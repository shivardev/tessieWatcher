// pgsync pulls a snapshot of teslalog's SQLite database from its portal
// (the same /download endpoint meant for opening in Grafana directly -
// see internal/portal/portal.go) and syncs it into a Postgres database
// laid out to match TeslaMate's own schema closely enough that
// TeslaMate's real, unmodified Grafana dashboards can be pointed at it.
//
// This never touches the Pi: it only ever makes an outbound HTTP GET to
// the portal's existing download route, over the LAN. It never touches
// TeslaMate's own "teslamate" database either - everything lands in a
// separate "teslalog" database in the same Postgres instance, so a
// wrong sync can never corrupt real TeslaMate history; re-running this
// is always safe, since every table is truncated and rebuilt from the
// SQLite snapshot each time.
//
// Meant to run in a loop (see -interval) as one more service in the
// same docker-compose stack that already runs Postgres+Grafana, not as
// a one-off script - see grafana/docker-compose.yml.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	_ "github.com/ncruces/go-sqlite3/driver"
)

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func main() {
	portalURL := flag.String("portal", "", "teslalog portal base URL, e.g. http://192.168.1.50:8080")
	pgHost := flag.String("pg-host", "localhost", "Postgres host")
	pgPort := flag.String("pg-port", "5432", "Postgres port")
	pgUser := flag.String("pg-user", "teslamate", "Postgres admin user")
	pgPass := flag.String("pg-pass", "", "Postgres admin password")
	interval := flag.Duration("interval", 0, "if set, sync repeatedly on this interval instead of once (e.g. 15m)")
	flag.Parse()

	if *portalURL == "" {
		log.Fatal("-portal is required, e.g. -portal http://192.168.1.50:8080")
	}
	if *pgPass == "" {
		log.Fatal("-pg-pass is required")
	}

	adminDSN := fmt.Sprintf("postgres://%s:%s@%s:%s/postgres?sslmode=disable", *pgUser, *pgPass, *pgHost, *pgPort)
	targetDSN := fmt.Sprintf("postgres://%s:%s@%s:%s/teslalog?sslmode=disable", *pgUser, *pgPass, *pgHost, *pgPort)

	runOnce := func() error {
		ctx := context.Background()
		start := time.Now()

		sqlitePath, err := fetchSnapshot(ctx, *portalURL)
		if err != nil {
			return fmt.Errorf("fetch snapshot: %w", err)
		}
		defer os.Remove(sqlitePath)

		if err := ensureDatabase(ctx, adminDSN); err != nil {
			return fmt.Errorf("ensure database: %w", err)
		}

		pg, err := pgx.Connect(ctx, targetDSN)
		if err != nil {
			return fmt.Errorf("connect target: %w", err)
		}
		defer pg.Close(ctx)

		if _, err := pg.Exec(ctx, schemaSQL); err != nil {
			return fmt.Errorf("create schema: %w", err)
		}

		if _, err := pg.Exec(ctx, `TRUNCATE charges, charging_processes, positions, drives, updates, states, addresses, cars RESTART IDENTITY CASCADE`); err != nil {
			return fmt.Errorf("truncate: %w", err)
		}

		db, err := sql.Open("sqlite3", "file:"+sqlitePath+"?mode=ro")
		if err != nil {
			return fmt.Errorf("open sqlite: %w", err)
		}
		defer db.Close()

		carID := syncCar(ctx, pg, db)
		addr := syncAddresses(ctx, pg, db)
		closedDrives := syncDrives(ctx, pg, db, carID, addr)
		syncPositions(ctx, pg, db, carID, closedDrives)
		closedSessions := syncChargingProcesses(ctx, pg, db, carID, addr)
		syncCharges(ctx, pg, db, closedSessions)
		syncStates(ctx, pg, db, carID)
		syncUpdates(ctx, pg, db, carID)

		fmt.Printf("sync complete in %s\n", time.Since(start).Round(time.Millisecond))
		return nil
	}

	if *interval <= 0 {
		must(runOnce())
		return
	}

	for {
		if err := runOnce(); err != nil {
			log.Printf("sync failed: %v", err)
		}
		time.Sleep(*interval)
	}
}

// fetchSnapshot downloads teslalog's portal's existing /download
// snapshot to a local temp file and returns its path. The portal
// already takes a safe, consistent, journal_mode=DELETE snapshot on
// every request (see internal/portal/portal.go's handleDownload) -
// this just consumes that, exactly as opening the URL in a browser
// would, no teslalog-side changes required.
func fetchSnapshot(ctx context.Context, portalURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, portalURL+"/download", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("portal returned %s", resp.Status)
	}

	f, err := os.CreateTemp("", "pgsync-snapshot-*.db")
	if err != nil {
		return "", err
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// ensureDatabase creates the "teslalog" Postgres database if it
// doesn't exist yet. Deliberately never touches "teslamate" - that
// database, and whatever TeslaMate's own Elixir app is doing to it
// concurrently, is never opened by this tool at all.
func ensureDatabase(ctx context.Context, adminDSN string) error {
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return err
	}
	defer admin.Close(ctx)

	var exists bool
	if err := admin.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname='teslalog')").Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err := admin.Exec(ctx, "CREATE DATABASE teslalog"); err != nil {
			return err
		}
		fmt.Println("created database teslalog")
	}
	return nil
}
