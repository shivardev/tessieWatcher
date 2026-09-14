package cloudsync

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	_ "github.com/ncruces/go-sqlite3/driver"
)

// recorder is a fake Layerbase query endpoint that captures every SQL
// statement posted to it.
type recorder struct {
	mu      sync.Mutex
	queries []string
}

func (r *recorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/databases/db1/query", func(w http.ResponseWriter, req *http.Request) {
		if got := req.Header.Get("Authorization"); got != "Bearer sk_test" {
			t.Errorf("missing/wrong bearer token: %q", got)
		}
		body, _ := io.ReadAll(req.Body)
		var parsed struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Errorf("body is not {\"query\":...}: %v", err)
		}
		r.mu.Lock()
		r.queries = append(r.queries, parsed.Query)
		r.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})
	return httptest.NewServer(mux)
}

func (r *recorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.queries...)
}

func anyContains(items []string, substr string) bool {
	for _, item := range items {
		if strings.Contains(item, substr) {
			return true
		}
	}
	return false
}

func makeSource(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "src.db")
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(
		`CREATE TABLE IF NOT EXISTS t (id INTEGER PRIMARY KEY, name TEXT, amount REAL, raw BLOB, note TEXT)`,
	); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_t_name ON t(name)`); err != nil {
		t.Fatalf("index: %v", err)
	}
	// A quote to escape, an exact float, a blob, and a NULL.
	if _, err := db.Exec(`INSERT INTO t VALUES (1, 'O''Brien', 1.5, X'deadbeef', NULL)`); err != nil {
		t.Fatalf("insert 1: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO t VALUES (2, 'plain', 2.0, NULL, 'note')`); err != nil {
		t.Fatalf("insert 2: %v", err)
	}
	return path
}

func TestPushReplaysSchemaAndRowsFaithfully(t *testing.T) {
	rec := &recorder{}
	server := rec.server(t)
	defer server.Close()

	if err := Push(context.Background(), makeSource(t), Config{
		BaseURL: server.URL, DatabaseID: "db1", APIKey: "sk_test",
	}); err != nil {
		t.Fatalf("Push: %v", err)
	}

	queries := rec.all()
	if !anyContains(queries, "CREATE TABLE") || !anyContains(queries, `"t"`) {
		t.Fatalf("expected the table's CREATE to be replayed, got %v", queries)
	}
	if !anyContains(queries, "CREATE INDEX") {
		t.Fatalf("expected the index to be replayed")
	}
	// The single-quote must be doubled, the blob hex-encoded, NULL kept as
	// NULL - not '<nil>' or an empty string.
	if !anyContains(queries, `INSERT OR REPLACE INTO "t"`) {
		t.Fatalf("expected an INSERT OR REPLACE, got %v", queries)
	}
	if !anyContains(queries, `'O''Brien'`) {
		t.Fatalf("expected the embedded quote to be escaped")
	}
	if !anyContains(queries, `X'deadbeef'`) {
		t.Fatalf("expected the blob to be hex-encoded")
	}
	if !anyContains(queries, `1.5`) {
		t.Fatalf("expected the float to round-trip")
	}
	// The NULL row must carry a literal NULL, and the string row its value.
	if !anyContains(queries, "NULL)") {
		t.Fatalf("expected a NULL literal for the null column")
	}
	if !anyContains(queries, `'note'`) {
		t.Fatalf("expected the text value to be copied")
	}
}

func TestPushTableBatchesUnderTheBodyLimit(t *testing.T) {
	rec := &recorder{}
	server := rec.server(t)
	defer server.Close()

	// A source with several rows and a tiny body budget must split into
	// more than one INSERT, and every INSERT must stay within the budget.
	path := filepath.Join(t.TempDir(), "batch.db")
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE r (id INTEGER PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, err := db.Exec(`INSERT INTO r (v) VALUES ('xxxxxxxxxx')`); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	db.Close()

	client := New(Config{BaseURL: server.URL, DatabaseID: "db1", APIKey: "sk_test"})
	client.maxBody = 120 // force several batches

	src, err := sql.Open("sqlite3", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatalf("open ro: %v", err)
	}
	defer src.Close()
	if err := client.pushTable(context.Background(), src, "r"); err != nil {
		t.Fatalf("pushTable: %v", err)
	}

	inserts := 0
	for _, q := range rec.all() {
		if strings.HasPrefix(q, "INSERT OR REPLACE INTO") {
			inserts++
			if len(q) > client.maxBody {
				t.Fatalf("a batch exceeded the body limit: %d bytes", len(q))
			}
		}
	}
	if inserts < 2 {
		t.Fatalf("expected the rows to be split across multiple batches, got %d", inserts)
	}
}
