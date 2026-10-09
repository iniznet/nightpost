package ingest

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/iniznet/fenrirealm-monitor/internal/store"
)

func doRelease(t *testing.T, st *store.Store, body string) int {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ingest/{project}/release", ReleaseHandler(st))
	req := httptest.NewRequest("POST", "/ingest/backend/release", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code
}

func TestReleaseHandlerStoresMarker(t *testing.T) {
	st := openQueryStore(t)
	if code := doRelease(t, st, `{"version":"2026.10.9-a1b2c3","environment":"production"}`); code != http.StatusAccepted {
		t.Fatalf("got %d", code)
	}
	mark, err := st.LatestRelease("backend", "")
	if err != nil || mark == nil || mark.Version != "2026.10.9-a1b2c3" || mark.Environment != "production" {
		t.Fatalf("mark=%+v err=%v", mark, err)
	}
}

func TestReleaseHandlerRejectsGarbage(t *testing.T) {
	st := openQueryStore(t)
	if code := doRelease(t, st, `{"environment":"production"}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("missing version must 422, got %d", code)
	}
	if code := doRelease(t, st, `{"version":"`+strings.Repeat("v", 129)+`"}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("oversized version must 422, got %d", code)
	}
	if code := doRelease(t, st, `{"version"`); code != http.StatusBadRequest {
		t.Fatalf("malformed must 400, got %d", code)
	}
	mark, _ := st.LatestRelease("", "")
	if mark != nil {
		t.Fatal("rejected payloads must not store markers")
	}
}
