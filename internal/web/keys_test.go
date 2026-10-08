package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/iniznet/fenrirealm-monitor/internal/ingest"
	"github.com/iniznet/fenrirealm-monitor/internal/store"
)

var flashRe = regexp.MustCompile(`class="mono">([0-9a-f]{48})<`)

func openKeysStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "keys.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestKeysPageRotateFlow(t *testing.T) {
	st := openKeysStore(t)
	if _, err := st.RecordError("alpha", "Ex", "boom", "fp", "production", "error", nil); err != nil {
		t.Fatal(err)
	}
	if err := st.ProvisionProjects(); err != nil {
		t.Fatal(err)
	}
	gate := ingest.NewKeyGate(st, time.Second)
	mux := http.NewServeMux()
	Register(mux, st, gate, nil)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/keys", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /keys: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "alpha") {
		t.Fatal("keys page must list provisioned projects")
	}
	if !strings.Contains(rec.Body.String(), "unsigned (legacy)") {
		t.Fatal("backfilled project must render as unsigned")
	}

	form := strings.NewReader("project=alpha")
	req := httptest.NewRequest(http.MethodPost, "/keys/rotate", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /keys/rotate: %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "shown once") {
		t.Fatal("rotation must flash the new key")
	}

	m := flashRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("rotation response must flash a 48-hex secret")
	}
	flashed := m[1]
	stored, err := st.ProjectIngestKey("alpha")
	if err != nil || stored != store.HashIngestKey(flashed) {
		t.Fatalf("store must hold the sha256 of the flashed secret, got %q (err %v)", stored, err)
	}
	if strings.Contains(body, stored) {
		t.Fatal("the stored hash must never appear in the page")
	}

	// The gate must reject the project's old (absent) key immediately after
	// rotation: Invalidate cleared the cache, so an unsigned POST is 401.
	// Route through a ServeMux so PathValue("project") resolves to "alpha".
	hh := ingest.WithKeyGate(gate, ingest.PathProject, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	ingestMux := http.NewServeMux()
	ingestMux.HandleFunc("POST /ingest/{project}/sentry", hh)
	r2 := httptest.NewRecorder()
	ingestMux.ServeHTTP(r2, httptest.NewRequest(http.MethodPost, "/ingest/alpha/sentry", nil))
	if r2.Code != http.StatusUnauthorized {
		t.Fatalf("rotated project must reject unsigned ingest, got %d", r2.Code)
	}
	r3 := httptest.NewRecorder()
	okReq := httptest.NewRequest(http.MethodPost, "/ingest/alpha/sentry", nil)
	okReq.Header.Set("X-Monitor-Key", flashed)
	ingestMux.ServeHTTP(r3, okReq)
	if r3.Code != http.StatusAccepted {
		t.Fatalf("correct key must be accepted after rotation, got %d", r3.Code)
	}
}

func TestKeysRotateRejectsCrossOrigin(t *testing.T) {
	st := openKeysStore(t)
	mux := http.NewServeMux()
	Register(mux, st, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/keys/rotate", strings.NewReader("project=alpha"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://evil.example")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin rotate must 403, got %d", rec.Code)
	}
	req2 := httptest.NewRequest(http.MethodPost, "/keys/rotate", strings.NewReader("project=alpha"))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.Header.Set("Origin", "http://example.com")
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("same-origin rotate must pass, got %d", rec2.Code)
	}
}

func TestKeysRotateRejectsEmptyProject(t *testing.T) {
	st := openKeysStore(t)
	mux := http.NewServeMux()
	Register(mux, st, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/keys/rotate", strings.NewReader("project="))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty project must 400, got %d", rec.Code)
	}
}
