package cloudsync

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// queryRows executes a read query and normalizes the common Layerbase result
// shapes into named rows.
func (c *Client) queryRows(ctx context.Context, query string) ([]map[string]any, error) {
	payload, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("layerbase settings query: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("layerbase settings query: HTTP %d: %s", resp.StatusCode, string(body))
	}
	var raw any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode Layerbase settings: %w", err)
	}
	return namedRows(raw)
}

func namedRows(raw any) ([]map[string]any, error) {
	if object, ok := raw.(map[string]any); ok {
		if columns, cok := object["columns"].([]any); cok {
			if rows, rok := object["rows"].([]any); rok {
				out := make([]map[string]any, 0, len(rows))
				for _, rawRow := range rows {
					values, ok := rawRow.([]any)
					if !ok {
						continue
					}
					row := make(map[string]any, len(columns))
					for i, column := range columns {
						if i < len(values) {
							row[fmt.Sprint(column)] = values[i]
						}
					}
					out = append(out, row)
				}
				return out, nil
			}
		}
		for _, key := range []string{"rows", "results", "data"} {
			if nested, ok := object[key]; ok {
				return namedRows(nested)
			}
		}
	}
	if array, ok := raw.([]any); ok {
		out := make([]map[string]any, 0, len(array))
		for _, item := range array {
			row, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("unexpected Layerbase settings row shape")
			}
			out = append(out, row)
		}
		return out, nil
	}
	return nil, fmt.Errorf("unexpected Layerbase settings result shape")
}

// PullGeofences copies WebUI-managed geofences/pricing from Layerbase into
// the local source-of-truth cache used by the running logger.
func PullGeofences(ctx context.Context, dbPath string, cfg Config) error {
	db, err := OpenLocal(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	return PullGeofencesDB(ctx, db, cfg)
}

// PullGeofencesDB applies the small cloud-owned settings table to the local
// cache. It intentionally does not participate in telemetry replication.
func PullGeofencesDB(ctx context.Context, db *sql.DB, cfg Config) error {
	rows, err := New(cfg).queryRows(ctx, `SELECT id, name, latitude, longitude, radius_m,
		billing_type, cost_per_unit, session_fee FROM geofences ORDER BY id`)
	if err != nil {
		// An older cloud database will gain this table on the following push.
		if strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return nil
		}
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM geofences`); err != nil {
		return err
	}
	for _, row := range rows {
		if _, err := tx.ExecContext(ctx, `INSERT INTO geofences
			(id, name, latitude, longitude, radius_m, billing_type, cost_per_unit, session_fee)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, integer(row["id"]), textValue(row["name"]),
			number(row["latitude"]), number(row["longitude"]), number(row["radius_m"]),
			textValue(row["billing_type"]), nullableNumber(row["cost_per_unit"]), number(row["session_fee"])); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func textValue(v any) string { return fmt.Sprint(v) }
func number(v any) float64 {
	f, _ := strconv.ParseFloat(strings.ReplaceAll(fmt.Sprint(v), ",", ""), 64)
	return f
}
func integer(v any) int64 { return int64(number(v)) }
func nullableNumber(v any) any {
	if v == nil {
		return nil
	}
	return number(v)
}
