// teslamate-import brings an existing TeslaMate installation's history
// into teslalog's own SQLite database - the onboarding path for
// someone switching from TeslaMate to teslalog who doesn't want to
// lose years of drive/charge history in the process.
//
// Separate Go module (own go.mod), same reasoning as teslamate-sync:
// pgx/Postgres never becomes a dependency of teslalog's own binary.
// This is meant to run once (or occasionally, for a top-up), on
// whatever machine can reach the source Postgres - never on the Pi.
//
// Two ways to point it at a TeslaMate installation's data:
//
//	-pg-host/-pg-port/-pg-user/-pg-pass/-pg-db   connect to a live,
//	                                              already-running
//	                                              TeslaMate Postgres.
//
//	-dump path/to/backup.sql                     restore a pg_dump SQL
//	                                              file into a throwaway
//	                                              local Postgres
//	                                              container (needs
//	                                              Docker), import from
//	                                              that, then remove it -
//	                                              for someone who has an
//	                                              old backup but no
//	                                              TeslaMate running
//	                                              anymore.
//
// Every table is read defensively via information_schema introspection
// (see introspect.go) rather than a hardcoded column list, since
// TeslaMate's schema has changed across versions (e.g. ascent/descent
// were added to drives in a 2025 migration) - a column this tool
// expects but an older/newer install doesn't have degrades to NULL
// instead of failing the whole import.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
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
	sqlitePath := flag.String("sqlite", "", "path to teslalog's SQLite database (created if it doesn't exist)")
	pgHost := flag.String("pg-host", "localhost", "source Postgres host (live-connection mode)")
	pgPort := flag.String("pg-port", "5432", "source Postgres port")
	pgUser := flag.String("pg-user", "teslamate", "source Postgres user")
	pgPass := flag.String("pg-pass", "", "source Postgres password")
	pgDB := flag.String("pg-db", "teslamate", "source Postgres database name")
	dumpFile := flag.String("dump", "", "path to a pg_dump SQL file instead of a live connection (requires Docker)")
	flag.Parse()

	if *sqlitePath == "" {
		log.Fatal("-sqlite is required, e.g. -sqlite ./tesla.db")
	}

	ctx := context.Background()
	var dsn string

	if *dumpFile != "" {
		var cleanup func()
		var err error
		dsn, cleanup, err = restoreDumpToEphemeralPostgres(ctx, *dumpFile)
		must(err)
		defer cleanup()
	} else {
		if *pgPass == "" {
			log.Fatal("-pg-pass is required in live-connection mode (or use -dump instead)")
		}
		dsn = fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable", *pgUser, *pgPass, *pgHost, *pgPort, *pgDB)
	}

	pg, err := pgx.Connect(ctx, dsn)
	must(err)
	defer pg.Close(ctx)

	sq, err := sql.Open("sqlite3", "file:"+*sqlitePath)
	must(err)
	defer sq.Close()
	must(applySQLiteSchema(sq))

	start := time.Now()
	vehicleID, err := importCar(ctx, pg, sq)
	must(err)
	driveIDs, err := importDrives(ctx, pg, sq, vehicleID)
	must(err)
	must(importPositions(ctx, pg, sq, vehicleID, driveIDs))
	sessionIDs, err := importChargingProcesses(ctx, pg, sq, vehicleID)
	must(err)
	must(importCharges(ctx, pg, sq, sessionIDs))
	must(importStates(ctx, pg, sq, vehicleID))
	must(importUpdates(ctx, pg, sq, vehicleID))

	fmt.Printf("import complete in %s\n", time.Since(start).Round(time.Millisecond))
}

func applySQLiteSchema(sq *sql.DB) error {
	pragmas := []string{"PRAGMA journal_mode = WAL;", "PRAGMA foreign_keys = ON;"}
	for _, p := range pragmas {
		if _, err := sq.Exec(p); err != nil {
			return fmt.Errorf("apply pragma %q: %w", p, err)
		}
	}
	if _, err := sq.Exec(sqliteSchema); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}

// restoreDumpToEphemeralPostgres starts a throwaway Postgres container
// (needs Docker installed and on PATH), restores dumpPath into it with
// psql, and returns a DSN pointing at it plus a cleanup func that
// removes the container. This exists for someone who has an old
// pg_dump backup lying around but no TeslaMate actually running
// anymore to connect to live - everywhere else in this tool, and in
// teslamate-sync, talks to a real running Postgres instead of trying
// to hand-parse pg_dump's SQL text (which varies by version/flags and
// isn't worth a bespoke parser when Postgres itself already knows how
// to read its own dump format).
func restoreDumpToEphemeralPostgres(ctx context.Context, dumpPath string) (dsn string, cleanup func(), err error) {
	const containerName = "teslamate-import-ephemeral-pg"
	const password = "import"

	fmt.Println("starting a throwaway Postgres container to restore the dump into...")
	runCmd := exec.CommandContext(ctx, "docker", "run", "-d", "--rm",
		"--name", containerName,
		"-e", "POSTGRES_PASSWORD="+password,
		"-p", "127.0.0.1:55432:5432",
		"postgres:16",
	)
	if out, err := runCmd.CombinedOutput(); err != nil {
		return "", nil, fmt.Errorf("docker run failed (is Docker installed and running?): %w\n%s", err, out)
	}
	cleanup = func() {
		fmt.Println("removing throwaway Postgres container...")
		_ = exec.Command("docker", "rm", "-f", containerName).Run()
	}

	dsn = fmt.Sprintf("postgres://postgres:%s@127.0.0.1:55432/postgres?sslmode=disable", password)

	// Wait for it to accept connections - a fresh Postgres container
	// takes a few seconds to initialize before it's ready.
	ready := false
	for i := 0; i < 30; i++ {
		conn, err := pgx.Connect(ctx, dsn)
		if err == nil {
			conn.Close(ctx)
			ready = true
			break
		}
		time.Sleep(time.Second)
	}
	if !ready {
		cleanup()
		return "", nil, fmt.Errorf("Postgres container did not become ready in time")
	}

	// Pre-create a "teslamate" database as the restore target. Most
	// pg_dump backups (taken without --create) assume the target
	// database already exists and contain no CREATE DATABASE/\connect
	// of their own; a dump that DOES include those statements (e.g. a
	// pg_dumpall) just executes them fine on top of this and switches
	// database itself mid-restore, so this is a safe default either way.
	adminConn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("connect to prepare restore target: %w", err)
	}
	_, err = adminConn.Exec(ctx, "CREATE DATABASE teslamate")
	adminConn.Close(ctx)
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("create restore target database: %w", err)
	}

	fmt.Println("restoring dump (this can take a while for a large history)...")
	restoreCmd := exec.CommandContext(ctx, "docker", "exec", "-i", containerName, "psql", "-U", "postgres", "-d", "teslamate")
	dumpReader, err := os.Open(dumpPath)
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("open dump file: %w", err)
	}
	defer dumpReader.Close()
	restoreCmd.Stdin = dumpReader
	if out, err := restoreCmd.CombinedOutput(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("restoring dump failed: %w\n%s", err, out)
	}

	return fmt.Sprintf("postgres://postgres:%s@127.0.0.1:55432/teslamate?sslmode=disable", password), cleanup, nil
}
