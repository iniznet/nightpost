package ingest

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iniznet/fenrirealm-monitor/internal/store"
)

func openQueryStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func postQuery(t *testing.T, st *store.Store, body string) int {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ingest/{project}/query", QueryHandler(st))
	req := httptest.NewRequest(http.MethodPost, "/ingest/backend/query", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code
}

func TestQueryHandler(t *testing.T) {
	st := openQueryStore(t)
	if code := postQuery(t, st, `{"sql":"select * from chapters where id = 7","time_ms":123.5,"environment":"production","connection":"mysql"}`); code != http.StatusAccepted {
		t.Fatalf("valid event must 202, got %d", code)
	}
	rows, err := st.QueryLeaderboard("backend", 24, 10)
	if err != nil || len(rows) != 1 || rows[0].TotalMs != 123.5 || rows[0].Connection != "mysql" {
		t.Fatalf("stored aggregate wrong: %+v %v", rows, err)
	}
	if code := postQuery(t, st, `{"sql":"","time_ms":5}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("empty sql must 422, got %d", code)
	}
	if code := postQuery(t, st, `{"sql":"select 1","time_ms":-2}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("negative time must 422, got %d", code)
	}
	if code := postQuery(t, st, `{"sql":"select 1","time_ms":0}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("zero time must 422, got %d", code)
	}
	if code := postQuery(t, st, `{"sql":"select 1"}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("missing time must 422, got %d", code)
	}
	if code := postQuery(t, st, "not-json"); code != http.StatusBadRequest {
		t.Fatalf("malformed json must 400, got %d", code)
	}
	if code := postQuery(t, st, `{"sql":"select 1 from x where y='`+strings.Repeat("z", 9000)+`'","time_ms":5}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("oversized sql must 422, got %d", code)
	}
}
