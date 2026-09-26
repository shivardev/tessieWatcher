// Package dbserver exposes a self-hosted SQLite replica over the same small
// HTTP query contract used by teslalog's incremental sync and browser viewer.
package dbserver

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"

	"teslalog/internal/storage"
	"teslalog/internal/webui"
)

type Config struct {
	DatabasePath string
	DatabaseID   string
	Addr         string
	Token        string
}

type Server struct {
	cfg   Config
	store *storage.Store
	read  *sql.DB
}

func New(cfg Config) (*Server, error) {
	if cfg.DatabasePath == "" || cfg.DatabaseID == "" || cfg.Token == "" {
		return nil, fmt.Errorf("database path, database id, and token are required")
	}
	store, err := storage.Open(cfg.DatabasePath)
	if err != nil {
		return nil, err
	}
	// A replica receives changes from the Pi. It must not create a second
	// outbound outbox for the same rows.
	rows, err := store.DB().Query(`SELECT name FROM sqlite_master WHERE type='trigger' AND name LIKE 'cloud_sync_%'`)
	if err != nil {
		store.Close()
		return nil, err
	}
	var triggers []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			store.Close()
			return nil, err
		}
		triggers = append(triggers, name)
	}
	rows.Close()
	for _, name := range triggers {
		if _, err := store.DB().Exec(`DROP TRIGGER IF EXISTS "` + strings.ReplaceAll(name, `"`, `""`) + `"`); err != nil {
			store.Close()
			return nil, err
		}
	}
	read, err := sql.Open("sqlite3", "file:"+cfg.DatabasePath+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		store.Close()
		return nil, err
	}
	read.SetMaxOpenConns(8)
	return &Server{cfg: cfg, store: store, read: read}, nil
}

func (s *Server) Close() error {
	_ = s.read.Close()
	return s.store.Close()
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.health)
	mux.HandleFunc("/v1/databases/", s.query)
	if app, err := webui.Handler("/app"); err == nil {
		mux.Handle("/app", app)
		mux.Handle("/app/", app)
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/app/", http.StatusTemporaryRedirect)
		})
	}
	return s.cors(mux)
}

func (s *Server) Run(ctx context.Context) error {
	httpServer := &http.Server{Addr: s.cfg.Addr, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- httpServer.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdown)
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

func (s *Server) authorized(r *http.Request) bool {
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return len(got) == len(s.cfg.Token) && subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Token)) == 1
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var ok int
	if err := s.read.QueryRowContext(r.Context(), `SELECT 1`).Scan(&ok); err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, map[string]any{"status": "ok", "database_id": s.cfg.DatabaseID})
}

func (s *Server) query(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	prefix := "/v1/databases/" + s.cfg.DatabaseID + "/query"
	if r.URL.Path != prefix {
		http.NotFound(w, r)
		return
	}
	var input struct {
		Query string `json:"query"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := decoder.Decode(&input); err != nil || strings.TrimSpace(input.Query) == "" {
		http.Error(w, "invalid query", http.StatusBadRequest)
		return
	}
	if isRead(input.Query) {
		s.readQuery(w, r, input.Query)
		return
	}
	result, err := s.store.DB().ExecContext(r.Context(), input.Query)
	if err != nil {
		writeError(w, err)
		return
	}
	affected, _ := result.RowsAffected()
	writeJSON(w, map[string]any{"columns": []string{}, "rows": [][]any{}, "rowCount": affected})
}

func isRead(query string) bool {
	upper := strings.ToUpper(strings.TrimSpace(query))
	return strings.HasPrefix(upper, "SELECT") || strings.HasPrefix(upper, "WITH") || strings.HasPrefix(upper, "PRAGMA") || strings.HasPrefix(upper, "EXPLAIN")
}

func (s *Server) readQuery(w http.ResponseWriter, r *http.Request, query string) {
	rows, err := s.read.QueryContext(r.Context(), query)
	if err != nil {
		writeError(w, err)
		return
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		writeError(w, err)
		return
	}
	values := make([][]any, 0)
	for rows.Next() {
		row := make([]any, len(columns))
		ptrs := make([]any, len(columns))
		for i := range row {
			ptrs[i] = &row[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			writeError(w, err)
			return
		}
		for i, v := range row {
			if b, ok := v.([]byte); ok {
				row[i] = string(b)
			}
		}
		values = append(values, row)
	}
	if err := rows.Err(); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]any{"columns": columns, "rows": values, "rowCount": len(values)})
}

func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization,Content-Type")
		next.ServeHTTP(w, r)
	})
}

func writeError(w http.ResponseWriter, err error) {
	w.WriteHeader(http.StatusBadRequest)
	writeJSON(w, map[string]string{"error": err.Error()})
}
func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Warn("write server response", "error", err)
	}
}
