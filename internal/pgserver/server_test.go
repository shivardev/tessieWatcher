package pgserver

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAuthenticationCanBeExplicitlyDisabled(t *testing.T) {
	server := &Server{cfg: Config{AuthDisabled: true}}
	req := httptest.NewRequest(http.MethodPost, "/v1/databases/teslalog/query", nil)
	if !server.authorized(req) {
		t.Fatal("expected tokenless requests to be authorized when authentication is disabled")
	}
}

func TestAuthenticationStillDefaultsToRequired(t *testing.T) {
	server := &Server{cfg: Config{Token: "secret"}}
	req := httptest.NewRequest(http.MethodPost, "/v1/databases/teslalog/query", nil)
	if server.authorized(req) {
		t.Fatal("expected a tokenless request to remain unauthorized by default")
	}
}

func TestPostgresCompatibleSQL(t *testing.T) {
	input := `SELECT CASE WHEN is_dc_fast_charge THEN 1 ELSE 0 END,
CAST(strftime('%s', start_time) AS INTEGER) * 1000,
1 AS "efficiency_mi" FROM charging_sessions GROUP BY 1 HAVING "efficiency_mi" IS NOT NULL`
	got := postgresCompatibleSQL(input)
	want := `SELECT CASE WHEN is_dc_fast_charge <> 0 THEN 1 ELSE 0 END,
CAST(strftime('%s', start_time) AS BIGINT) * 1000,
1 AS "efficiency_mi" FROM charging_sessions GROUP BY 1`
	if got != want {
		t.Fatalf("rewrite:\n%s\nwant:\n%s", got, want)
	}
}
