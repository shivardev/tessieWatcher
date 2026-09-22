// Package cloudsync replicates teslalog's local SQLite data to Layerbase so a
// browser viewer can query it without downloading the whole database.
//
// Layerbase is SQLite 3 with an HTTP endpoint that runs one SQL statement
// per request (POST {base}/v1/databases/{id}/query, Bearer key, body
// {"query": "..."}). Push remains available for a one-time bootstrap.
// Normal daemon operation uses the durable trigger-backed outbox in
// incremental.go, small UPSERT batches, cloud acknowledgement, and
// crash-safe retries.
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
	"math/rand"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
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
	workers  int
	endpoint string
}

// New builds a Client. maxBody is held below Layerbase's HTTP query limit
// (10 KB per request), with margin for the JSON envelope and escaping.
// workers is how many batch inserts run concurrently, so a bulk load of
// hundreds of thousands of tiny 10 KB batches finishes in minutes rather
// than the ~half hour a strictly serial push would take.
func New(cfg Config) *Client {
	return &Client{
		cfg:      cfg,
		http:     &http.Client{Timeout: 2 * time.Minute},
		maxBody:  8_000,
		workers:  4,
		endpoint: strings.TrimRight(cfg.BaseURL, "/") + "/v1/databases/" + cfg.DatabaseID + "/query",
	}
}

// Exec runs a single SQL statement on the cloud database, retrying with
// exponential backoff on the transient failures a shared cloud endpoint
// produces under a bulk load - rate limits (429 / "rate limit"), 5xx, and
// network blips. A Retry-After header, when present, overrides the
// backoff. Permanent errors (a bad statement, auth) return immediately.
func (c *Client) Exec(ctx context.Context, statement string) error {
	payload, err := json.Marshal(map[string]string{"query": statement})
	if err != nil {
		return err
	}
	backoff := 400 * time.Millisecond
	for attempt := 0; attempt < 10; attempt++ {
		retryable, wait, e := c.attempt(ctx, payload)
		if e == nil {
			return nil
		}
		err = e
		if !retryable {
			return err
		}
		if wait <= 0 {
			wait = backoff
		}
		wait += time.Duration(rand.Int63n(int64(250 * time.Millisecond))) // jitter
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
		if backoff < 8*time.Second {
			backoff *= 2
		}
	}
	return err
}

// attempt makes one request. It returns whether the failure is worth
// retrying and how long to wait first (from Retry-After, else 0).
func (c *Client) attempt(ctx context.Context, payload []byte) (retryable bool, wait time.Duration, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return false, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return true, 0, fmt.Errorf("layerbase request: %w", err) // network blip: retry
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return false, 0, nil
	}
	if secs, e := strconv.Atoi(resp.Header.Get("Retry-After")); e == nil && secs > 0 {
		wait = time.Duration(secs) * time.Second
	}
	msg := errorMessage(resp)
	retryable = resp.StatusCode == http.StatusTooManyRequests ||
		resp.StatusCode/100 == 5 ||
		strings.Contains(strings.ToLower(msg), "rate limit")
	return retryable, wait, fmt.Errorf("layerbase query: %s", msg)
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
		if err := client.Exec(ctx, idempotentCreate(statement)); err != nil {
			// A table/index left by a previous partial push is fine - the
			// point is that the object exists, not that we created it now.
			if strings.Contains(strings.ToLower(err.Error()), "already exists") {
				continue
			}
			return fmt.Errorf("create schema object: %w", err)
		}
	}
	return rows.Err()
}

// idempotentCreate re-inserts the "IF NOT EXISTS" clause that SQLite strips
// out of the CREATE text it stores in sqlite_master, so replaying the
// schema onto a cloud database that already has some of it (e.g. a
// re-run after a partial push) does not fail with "already exists".
var (
	createRe      = regexp.MustCompile(`(?i)^(\s*CREATE\s+(?:TABLE|VIEW|TRIGGER|(?:UNIQUE\s+)?INDEX)\s+)`)
	ifNotExistsRe = regexp.MustCompile(`(?i)\bIF\s+NOT\s+EXISTS\b`)
)

func idempotentCreate(statement string) string {
	if ifNotExistsRe.MatchString(statement) {
		return statement
	}
	return createRe.ReplaceAllString(statement, "${1}IF NOT EXISTS ")
}

// tableNames lists the user tables to copy in foreign-key dependency order
// - every referenced table before the tables that reference it - because
// the cloud enforces foreign keys, so e.g. charging_samples must not be
// inserted before charging_sessions, nor positions before drives. Order is
// a post-order DFS over each table's foreign_key_list.
func tableNames(ctx context.Context, src *sql.DB) ([]string, error) {
	names, err := scanStrings(ctx, src,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	known := make(map[string]bool, len(names))
	for _, n := range names {
		known[n] = true
	}

	deps := make(map[string][]string, len(names))
	for _, t := range names {
		refs, err := scanColumn(ctx, src, `PRAGMA foreign_key_list(`+quoteIdent(t)+`)`, "table")
		if err != nil {
			return nil, err
		}
		for _, ref := range refs {
			if ref != t && known[ref] {
				deps[t] = append(deps[t], ref)
			}
		}
	}

	ordered := make([]string, 0, len(names))
	state := make(map[string]int) // 0 unseen, 1 visiting, 2 done
	var visit func(string)
	visit = func(t string) {
		if state[t] != 0 {
			return // done, or a cycle we break to avoid recursing forever
		}
		state[t] = 1
		for _, dep := range deps[t] {
			visit(dep)
		}
		state[t] = 2
		ordered = append(ordered, t)
	}
	for _, t := range names {
		visit(t)
	}
	return ordered, nil
}

// scanStrings runs a single-column query and returns the column's values.
func scanStrings(ctx context.Context, db *sql.DB, query string) ([]string, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// scanColumn runs a query and returns one named column's values, used for
// PRAGMA results whose full row shape we don't want to hard-code.
func scanColumn(ctx context.Context, db *sql.DB, query, column string) ([]string, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	idx := -1
	for i, c := range cols {
		if c == column {
			idx = i
		}
	}
	if idx < 0 {
		return nil, nil
	}
	var out []string
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		if s, ok := cells[idx].(string); ok {
			out = append(out, s)
		} else if b, ok := cells[idx].([]byte); ok {
			out = append(out, string(b))
		}
	}
	return out, rows.Err()
}

// pushTable streams one table to the cloud as batched INSERT OR REPLACE
// statements. Because the HTTP query cap is only 10 KB per request, each
// batch is tiny and there are many, so batches are fanned out to a pool of
// workers: INSERT OR REPLACE is idempotent and order-independent, so
// running many concurrently is safe and turns a ~half-hour serial push
// into a few minutes.
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

	jobs := make(chan string, c.workers*2)
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	var firstErr error
	fail := func(e error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = e
			cancel()
		}
		mu.Unlock()
	}
	var wg sync.WaitGroup
	for range c.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for b := range jobs {
				if err := c.Exec(workerCtx, b); err != nil {
					fail(err)
				}
			}
		}()
	}

	var batch strings.Builder
	rowsInBatch := 0
	send := func() {
		if rowsInBatch == 0 {
			return
		}
		select {
		case jobs <- batch.String():
		case <-workerCtx.Done():
		}
		batch.Reset()
		rowsInBatch = 0
	}

	scan := make([]any, len(columns))
	ptrs := make([]any, len(columns))
	for i := range scan {
		ptrs[i] = &scan[i]
	}
	for rows.Next() {
		if workerCtx.Err() != nil {
			break
		}
		if err := rows.Scan(ptrs...); err != nil {
			fail(err)
			break
		}
		tuple := encodeTuple(scan)
		if rowsInBatch > 0 && batch.Len()+len(tuple)+1 > c.maxBody {
			send()
		}
		if rowsInBatch == 0 {
			batch.WriteString(prefix)
		} else {
			batch.WriteByte(',')
		}
		batch.WriteString(tuple)
		rowsInBatch++
	}
	send()
	close(jobs)
	wg.Wait()

	if firstErr != nil {
		return firstErr
	}
	return rows.Err()
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
	escaped := strings.ReplaceAll(s, "'", "''")
	if !strings.Contains(escaped, ";") {
		return "'" + escaped + "'"
	}
	// Layerbase's HTTP query API splits a batch on ';' WITHOUT respecting
	// string literals, so a semicolon inside a value (e.g. an OSM road name
	// "I 75;US 11;US 64") truncates the statement and errors. Emit each ';'
	// via char(59) concatenation so no literal ';' appears in the SQL text,
	// while SQLite still reconstructs the exact original string.
	parts := strings.Split(escaped, ";")
	for i, p := range parts {
		parts[i] = "'" + p + "'"
	}
	return strings.Join(parts, "||char(59)||")
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
