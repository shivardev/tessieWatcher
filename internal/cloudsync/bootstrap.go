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
// idempotent retry. The UPSERT bootstrap also fills gaps in a receiver that
// was seeded from an older snapshot; linking can therefore never silently
// skip local history just because the receiver is non-empty.
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
	// Capture the local high-water mark before copying. Changes committed
	// while the copy is running receive a greater sequence and remain pending.
	// Everything at or below this mark is included in the successful UPSERT
	// bootstrap and must not be resent one row per HTTP request afterward.
	var highWater int64
	if err := local.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0) FROM cloud_sync_changes`).Scan(&highWater); err != nil {
		return fmt.Errorf("read bootstrap high-water mark: %w", err)
	}

	for _, table := range bootstrapTables {
		if err := client.pushTable(ctx, local, table); err != nil {
			return fmt.Errorf("bootstrap table %q: %w", table, err)
		}
	}

	completed := time.Now().UTC().Format(time.RFC3339Nano)
	if err := client.Exec(ctx, `INSERT INTO teslalog_replication_meta(name,completed_at) VALUES('initial_bootstrap',`+
		quoteString(completed)+`) ON CONFLICT(name) DO UPDATE SET completed_at=excluded.completed_at`); err != nil {
		return err
	}
	_, err = local.ExecContext(ctx, `UPDATE cloud_sync_changes SET state='synced',synced_at=? WHERE sequence<=?`, completed, highWater)
	if err != nil {
		return fmt.Errorf("acknowledge bootstrapped changes: %w", err)
	}
	return nil
}
