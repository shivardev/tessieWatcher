// Package cloudsync pushes teslalog's local SQLite database up to a
// Layerbase cloud database over its HTTP query API, so a browser viewer
// can query the data in place instead of downloading the whole file.
//
// Layerbase is SQLite 3 with an HTTP endpoint that runs one SQL statement
// per request (POST {base}/v1/databases/{id}/query, Bearer key, body
// {"query": "..."}). This package uses only that endpoint: it recreates
// the schema from the source database's own sqlite_master, then copies
// every row as batched INSERT OR REPLACE statements kept under the API's
// 1 MiB body limit. INSERT OR REPLACE (keyed on each table's primary key)
// makes a re-push idempotent - re-running it updates changed rows and adds
// new ones - which is the seed for an incremental Pi->cloud sync.
//
// The daemon keeps writing to local SQLite regardless; this is a copy
// pushed on top, never the Pi's own source of truth, so an internet
// outage never costs a logged drive.
package cloudsync

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Config identifies the target cloud database. APIKey is a secret (sk_...);
// it is read from the environment/config on the Pi and never logged.
type Config struct {
	BaseURL    string
	DatabaseID string
	APIKey     string
}

// Client runs statements against one Layerbase database.
type Client struct {
	cfg      Config
	http     *http.Client
	maxBody  int
	endpoint string
}

// New builds a Client. maxBody is held below Layerbase's 1 MiB request
// limit with margin for the JSON envelope.
func New(cfg Config) *Client {
	return &Client{
		cfg:      cfg,
		http:     &http.Client{Timeout: 2 * time.Minute},
		maxBody:  900_000,
		endpoint: strings.TrimRight(cfg.BaseURL, "/") + "/v1/databases/" + cfg.DatabaseID + "/query",
	}
}

// Exec runs a single SQL statement on the cloud database. A non-2xx
// response is surfaced with the API's {"error": ...} message when present.
func (c *Client) Exec(ctx context.Context, statement string) error {
	body, err := json.Marshal(map[string]string{"query": statement})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("layerbase request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("layerbase query: %s", errorMessage(resp))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// errorMessage extracts the API's {"error": ...} text, falling back to the
// HTTP status when the body is not the documented JSON error shape.
func errorMessage(resp *http.Response) string {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	var parsed struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &parsed) == nil && parsed.Error != "" {
		return parsed.Error
	}
	return "HTTP " + strconv.Itoa(resp.StatusCode)
}

// Push copies the whole database at dbPath to the cloud: schema first
// (tables, then indexes), then every table's rows.
func Push(ctx context.Context, dbPath string, cfg Config) error {
	src, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return fmt.Errorf("open source database: %w", err)
	}
	defer src.Close()

	client := New(cfg)
	if err := createSchema(ctx, src, client); err != nil {
		return err
	}
	tables, err := tableNames(ctx, src)
	if err != nil {
		return err
	}
	for _, table := range tables {
		if err := client.pushTable(ctx, src, table); err != nil {
			return fmt.Errorf("copy table %q: %w", table, err)
		}
	}
	return nil
}

// createSchema replays the source database's own CREATE statements on the
// cloud. Reading them from sqlite_master (rather than a hard-coded DDL
// string) means the copy always matches the live schema, migrations
// included. Tables sort before indexes so an index never precedes its
// table.
func createSchema(ctx context.Context, src *sql.DB, client *Client) error {
	rows, err := src.QueryContext(ctx,
		`SELECT sql FROM sqlite_master
		 WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%'
		 ORDER BY (type = 'index'), rowid`)
	if err != nil {
		return fmt.Errorf("read schema: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var statement string
		if err := rows.Scan(&statement); err != nil {
			return err
		}
		if err := client.Exec(ctx, statement); err != nil {
			return fmt.Errorf("create schema object: %w", err)
		}
	}
	return rows.Err()
}

// tableNames lists the user tables to copy, vehicles first so any row that
// references it exists before the reference (harmless when the cloud has
// foreign-key enforcement off, correct if it does not).
func tableNames(ctx context.Context, src *sql.DB) ([]string, error) {
	rows, err := src.QueryContext(ctx,
		`SELECT name FROM sqlite_master
		 WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
		 ORDER BY (name = 'vehicles') DESC, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// pushTable streams one table to the cloud as batched INSERT OR REPLACE
// statements, flushing whenever the next row would push the request past
// the body limit.
func (c *Client) pushTable(ctx context.Context, src *sql.DB, table string) error {
	rows, err := src.QueryContext(ctx, `SELECT * FROM `+quoteIdent(table))
	if err != nil {
		return err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return err
	}

	quotedCols := make([]string, len(columns))
	for i, name := range columns {
		quotedCols[i] = quoteIdent(name)
	}
	prefix := "INSERT OR REPLACE INTO " + quoteIdent(table) +
		" (" + strings.Join(quotedCols, ",") + ") VALUES "

	var batch strings.Builder
	rowsInBatch := 0
	flush := func() error {
		if rowsInBatch == 0 {
			return nil
		}
		err := c.Exec(ctx, batch.String())
		batch.Reset()
		rowsInBatch = 0
		return err
	}

	scan := make([]any, len(columns))
	ptrs := make([]any, len(columns))
	for i := range scan {
		ptrs[i] = &scan[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		tuple := encodeTuple(scan)
		if rowsInBatch > 0 && batch.Len()+len(tuple)+1 > c.maxBody {
			if err := flush(); err != nil {
				return err
			}
		}
		if rowsInBatch == 0 {
			batch.WriteString(prefix)
		} else {
			batch.WriteByte(',')
		}
		batch.WriteString(tuple)
		rowsInBatch++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return flush()
}

// encodeTuple renders one scanned row as an SQL "(v1,v2,...)" literal.
func encodeTuple(values []any) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = literal(v)
	}
	return "(" + strings.Join(parts, ",") + ")"
}

// literal renders a scanned SQLite value as an inline SQL literal. The
// ncruces database/sql driver yields int64/float64/string/[]byte/nil (and
// occasionally bool/time), which covers every column teslalog stores.
func literal(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return "NULL"
		}
		// -1 precision is the shortest string that round-trips the float64
		// exactly, so coordinates and energies survive the copy unchanged.
		return strconv.FormatFloat(x, 'g', -1, 64)
	case bool:
		if x {
			return "1"
		}
		return "0"
	case []byte:
		return "X'" + hex.EncodeToString(x) + "'"
	case string:
		return quoteString(x)
	case time.Time:
		return quoteString(x.Format(time.RFC3339Nano))
	default:
		return quoteString(fmt.Sprint(x))
	}
}

func quoteString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
