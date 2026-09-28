// Package pgserver exposes PostgreSQL to the teslalog WebUI through the same
// authenticated HTTP contract used by the remote query backend.
package pgserver

import (
	"context"
	"crypto/subtle"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"teslalog/internal/webui"
)

//go:embed schema.sql
var schemaSQL string

type Config struct {
	DSN, DatabaseID, Addr, Token string
}

type Server struct {
	cfg Config
	db  *sql.DB
}

var efficiencyHaving = regexp.MustCompile(`(?i)\s+HAVING\s+"efficiency_[^"]+"\s+IS\s+NOT\s+NULL`)

// postgresCompatibleSQL handles the few SQLite expressions still emitted by
// the shared dashboard catalog. Keeping this at the API boundary lets one SPA
// query both engines while PostgreSQL-native dashboard endpoints are developed.
func postgresCompatibleSQL(query string) string {
	query = strings.ReplaceAll(query, "CASE WHEN is_dc_fast_charge THEN", "CASE WHEN is_dc_fast_charge <> 0 THEN")
	query = strings.ReplaceAll(query, " AS INTEGER) * 1000", " AS BIGINT) * 1000")
	return efficiencyHaving.ReplaceAllString(query, "")
}

func New(ctx context.Context, cfg Config) (*Server, error) {
	if cfg.DSN == "" || cfg.DatabaseID == "" || cfg.Token == "" {
		return nil, fmt.Errorf("PostgreSQL DSN, database id, and token are required")
	}
	db, err := sql.Open("pgx", cfg.DSN)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize PostgreSQL schema: %w", err)
	}
	return &Server{cfg: cfg, db: db}, nil
}

func (s *Server) Close() error { return s.db.Close() }

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
	if err := s.db.PingContext(r.Context()); err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, map[string]any{"status": "ok", "database_id": s.cfg.DatabaseID, "engine": "postgresql"})
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
	if r.URL.Path != "/v1/databases/"+s.cfg.DatabaseID+"/query" {
		http.NotFound(w, r)
		return
	}
	var input struct {
		Query string `json:"query"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input); err != nil || strings.TrimSpace(input.Query) == "" {
		http.Error(w, "invalid query", http.StatusBadRequest)
		return
	}
	input.Query = postgresCompatibleSQL(input.Query)
	if isRead(input.Query) {
		s.readQuery(w, r, input.Query)
		return
	}
	result, err := s.db.ExecContext(r.Context(), input.Query)
	if err != nil {
		writeError(w, err)
		return
	}
	affected, _ := result.RowsAffected()
	writeJSON(w, map[string]any{"columns": []string{}, "rows": [][]any{}, "rowCount": affected})
}

func isRead(query string) bool {
	upper := strings.ToUpper(strings.TrimSpace(query))
	return strings.HasPrefix(upper, "SELECT") || strings.HasPrefix(upper, "WITH") || strings.HasPrefix(upper, "EXPLAIN")
}

func (s *Server) readQuery(w http.ResponseWriter, r *http.Request, query string) {
	rows, err := s.db.QueryContext(r.Context(), query)
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
		row, ptrs := make([]any, len(columns)), make([]any, len(columns))
		for i := range row {
			ptrs[i] = &row[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			writeError(w, err)
			return
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
		slog.Warn("write PostgreSQL response", "error", err)
	}
}
