package cloudsync

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Settings are the user-editable replication settings persisted in the Pi's
// SQLite database. Secrets intentionally stay out of this record; trusted-LAN
// deployments need none, while authenticated installs keep using the existing
// environment variable.
type Settings struct {
	Enabled    bool          `json:"enabled"`
	BaseURL    string        `json:"base_url"`
	DatabaseID string        `json:"database_id"`
	Interval   time.Duration `json:"-"`
	BatchSize  int           `json:"batch_size"`
}

type SettingsJSON struct {
	Enabled         bool   `json:"enabled"`
	BaseURL         string `json:"base_url"`
	DatabaseID      string `json:"database_id"`
	IntervalSeconds int64  `json:"interval_seconds"`
	BatchSize       int    `json:"batch_size"`
}

func (s Settings) JSON() SettingsJSON {
	return SettingsJSON{s.Enabled, s.BaseURL, s.DatabaseID, int64(s.Interval.Seconds()), s.BatchSize}
}

func SettingsFromJSON(s SettingsJSON) Settings {
	return Settings{Enabled: s.Enabled, BaseURL: s.BaseURL, DatabaseID: s.DatabaseID, Interval: time.Duration(s.IntervalSeconds) * time.Second, BatchSize: s.BatchSize}
}

func normalizeSettings(s Settings) (Settings, error) {
	s.BaseURL = strings.TrimRight(strings.TrimSpace(s.BaseURL), "/")
	s.DatabaseID = strings.TrimSpace(s.DatabaseID)
	if s.Interval <= 0 {
		s.Interval = 15 * time.Minute
	}
	if s.BatchSize <= 0 {
		s.BatchSize = 500
	}
	if !s.Enabled {
		return s, nil
	}
	parsed, err := url.Parse(s.BaseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return s, fmt.Errorf("server URL must be a complete http:// or https:// address")
	}
	if s.DatabaseID == "" {
		return s, fmt.Errorf("database id is required")
	}
	return s, nil
}

func loadOrSeedSettings(ctx context.Context, db *sql.DB, defaults Settings) (Settings, error) {
	defaults, err := normalizeSettings(defaults)
	if err != nil && defaults.Enabled {
		return defaults, err
	}
	_, err = db.ExecContext(ctx, `INSERT OR IGNORE INTO cloud_sync_settings
		(id,enabled,base_url,database_id,interval_seconds,batch_size) VALUES(1,?,?,?,?,?)`,
		boolInt(defaults.Enabled), defaults.BaseURL, defaults.DatabaseID, int64(defaults.Interval.Seconds()), defaults.BatchSize)
	if err != nil {
		return Settings{}, err
	}
	return readSettings(ctx, db)
}

func readSettings(ctx context.Context, db *sql.DB) (Settings, error) {
	var s Settings
	var enabled int
	var seconds int64
	err := db.QueryRowContext(ctx, `SELECT enabled,base_url,database_id,interval_seconds,batch_size FROM cloud_sync_settings WHERE id=1`).
		Scan(&enabled, &s.BaseURL, &s.DatabaseID, &seconds, &s.BatchSize)
	s.Enabled, s.Interval = enabled != 0, time.Duration(seconds)*time.Second
	return s, err
}

func saveSettings(ctx context.Context, db *sql.DB, s Settings) error {
	_, err := db.ExecContext(ctx, `UPDATE cloud_sync_settings SET enabled=?,base_url=?,database_id=?,interval_seconds=?,batch_size=? WHERE id=1`,
		boolInt(s.Enabled), s.BaseURL, s.DatabaseID, int64(s.Interval.Seconds()), s.BatchSize)
	return err
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// Controller owns exactly one scheduler and can replace it safely when the
// admin page changes its destination. Configure validates connectivity before
// persisting an enabled destination, so a typo cannot strand the worker.
type Controller struct {
	ctx               context.Context
	dbPath, apiKey    string
	onSettingsChanged func()
	mu                sync.Mutex
	settings          Settings
	cancel            context.CancelFunc
}

func NewController(ctx context.Context, dbPath, apiKey string, defaults Settings, onSettingsChanged func()) (*Controller, error) {
	db, err := OpenLocal(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	settings, err := loadOrSeedSettings(ctx, db, defaults)
	if err != nil {
		return nil, err
	}
	c := &Controller{ctx: ctx, dbPath: dbPath, apiKey: apiKey, settings: settings, onSettingsChanged: onSettingsChanged}
	c.restartLocked()
	return c, nil
}

func (c *Controller) Settings() Settings {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.settings
}

func (c *Controller) Configure(ctx context.Context, next Settings) error {
	next, err := normalizeSettings(next)
	if err != nil {
		return err
	}
	if next.Enabled {
		probe, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := New(Config{BaseURL: next.BaseURL, DatabaseID: next.DatabaseID, APIKey: c.apiKey}).Exec(probe, `SELECT 1`); err != nil {
			return fmt.Errorf("server connection test failed: %w", err)
		}
	}
	db, err := OpenLocal(c.dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := saveSettings(ctx, db, next); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.settings = next
	c.restartLocked()
	return nil
}

func (c *Controller) restartLocked() {
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	if !c.settings.Enabled {
		db, err := OpenLocal(c.dbPath)
		if err == nil {
			_, _ = db.Exec(`UPDATE cloud_sync_status SET sync_state='disabled',manual_sync_requested=0 WHERE id=1`)
			db.Close()
		}
		return
	}
	workerCtx, cancel := context.WithCancel(c.ctx)
	c.cancel = cancel
	s := c.settings
	go Scheduler{DBPath: c.dbPath, Config: Config{BaseURL: s.BaseURL, DatabaseID: s.DatabaseID, APIKey: c.apiKey}, Interval: s.Interval, BatchSize: s.BatchSize, OnSettingsChanged: c.onSettingsChanged}.Start(workerCtx)
}
