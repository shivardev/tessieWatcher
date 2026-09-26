package cloudsync

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// bootstrapTables are the durable user-data tables copied when a receiver is
// empty. Sync bookkeeping is deliberately excluded: it belongs only to the
// collecting installation and must never be mirrored to the receiver.
var bootstrapTables = []string{
	"vehicles",
	"geofences",
	"geocode_cache",
	"states",
	"drives",
	"positions",
	"charging_sessions",
	"charging_samples",
	"battery_samples",
	"software_updates",
}

// EnsureBootstrap initializes a brand-new receiver with all existing local
// history before incremental replication begins. The remote marker is written
// only after every table succeeds, so interruption simply causes a safe,
// idempotent retry. A non-empty receiver without a marker is treated as a
// pre-existing/manual snapshot and adopted without overwriting it.
func EnsureBootstrap(ctx context.Context, local *sql.DB, cfg Config) error {
	client := New(cfg)
	if err := client.Exec(ctx, `CREATE TABLE IF NOT EXISTS teslalog_replication_meta (
		name TEXT PRIMARY KEY,
		completed_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("create replication metadata: %w", err)
	}
	rows, err := client.queryRows(ctx, `SELECT completed_at FROM teslalog_replication_meta WHERE name='initial_bootstrap'`)
	if err != nil {
		return err
	}
	if len(rows) != 0 {
		return nil
	}

	remoteVehicles, err := client.queryRows(ctx, `SELECT COUNT(*) AS count FROM vehicles`)
	if err != nil {
		return fmt.Errorf("inspect replication receiver: %w", err)
	}
	if len(remoteVehicles) == 0 || integer(remoteVehicles[0]["count"]) == 0 {
		for _, table := range bootstrapTables {
			if err := client.pushTable(ctx, local, table); err != nil {
				return fmt.Errorf("bootstrap table %q: %w", table, err)
			}
		}
	}

	completed := time.Now().UTC().Format(time.RFC3339Nano)
	return client.Exec(ctx, `INSERT INTO teslalog_replication_meta(name,completed_at) VALUES('initial_bootstrap',`+
		quoteString(completed)+`) ON CONFLICT(name) DO UPDATE SET completed_at=excluded.completed_at`)
}
