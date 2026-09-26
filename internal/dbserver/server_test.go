package dbserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestQueryAPIAndReplicaOutboxIsolation(t *testing.T) {
	s, err := New(Config{DatabasePath: filepath.Join(t.TempDir(), "server.db"), DatabaseID: "teslalog", Token: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	api := httptest.NewServer(s.Handler())
	defer api.Close()

	call := func(query, token string) *http.Response {
		body, _ := json.Marshal(map[string]string{"query": query})
		req, _ := http.NewRequest(http.MethodPost, api.URL+"/v1/databases/teslalog/query", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if resp := call("SELECT 1", "wrong"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d", resp.StatusCode)
	}
	upsert := `INSERT INTO vehicles(id,vin,display_name) VALUES(1,'VIN','Car') ON CONFLICT(id) DO UPDATE SET display_name=excluded.display_name`
	if resp := call(upsert, "secret"); resp.StatusCode != http.StatusOK {
		t.Fatalf("upsert status=%d", resp.StatusCode)
	}
	if resp := call(upsert, "secret"); resp.StatusCode != http.StatusOK {
		t.Fatalf("repeat upsert status=%d", resp.StatusCode)
	}
	resp := call(`SELECT id,vin,display_name FROM vehicles`, "secret")
	defer resp.Body.Close()
	var result struct {
		Rows     [][]any `json:"rows"`
		RowCount int     `json:"rowCount"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.RowCount != 1 || len(result.Rows) != 1 {
		t.Fatalf("result=%+v", result)
	}
	var changes int
	if err := s.store.DB().QueryRow(`SELECT COUNT(*) FROM cloud_sync_changes`).Scan(&changes); err != nil {
		t.Fatal(err)
	}
	if changes != 0 {
		t.Fatalf("replica generated %d outbound changes", changes)
	}
}

func TestHealthAndViewer(t *testing.T) {
	s, err := New(Config{DatabasePath: filepath.Join(t.TempDir(), "server.db"), DatabaseID: "teslalog", Token: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	api := httptest.NewServer(s.Handler())
	defer api.Close()
	req, _ := http.NewRequest(http.MethodGet, api.URL+"/health", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health=%d", resp.StatusCode)
	}
	resp, err = http.Get(api.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("viewer=%d", resp.StatusCode)
	}
}
