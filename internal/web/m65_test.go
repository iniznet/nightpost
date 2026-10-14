package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/iniznet/fenrirealm-monitor/internal/ingest"
)

type fakeInvalidator struct{ dropped []string }

func (f *fakeInvalidator) Invalidate(project string) { f.dropped = append(f.dropped, project) }

// TestKeyOriginSavesPerProjectAllowlist covers the M6.5 page contract: the
// allowlist moved from one global flag onto the project row.
func TestKeyOriginSavesPerProjectAllowlist(t *testing.T) {
	st := openKeysStore(t)
	if _, err := st.RecordError("alpha", "Ex", "boom", "fp", "production", "error", nil); err != nil {
		t.Fatal(err)
	}
	if err := st.ProvisionProjects(); err != nil {
		t.Fatal(err)
	}
	inv := &fakeInvalidator{}
	mux := http.NewServeMux()
	Register(mux, st, ingest.NewKeyGate(st, time.Second), nil, WithCORSInvalidator(inv))

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/keys/origin", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	// A wildcard is refused outright: these routes accept unauthenticated
	// browser posts, so "*" would let any page write into the tenant's data.
	if rec := post("project=alpha&cors_origin=*"); rec.Code != http.StatusBadRequest {
		t.Fatalf("wildcard: got %d, want 400", rec.Code)
	}
	if got, _ := st.ProjectCORSOrigin("alpha"); got != "" {
		t.Errorf("a refused value must store nothing, got %q", got)
	}

	rec := post("project=alpha&cors_origin=https://alpha.example.com/")
	if rec.Code != http.StatusOK {
		t.Fatalf("save: got %d, want 200", rec.Code)
	}
	if got, _ := st.ProjectCORSOrigin("alpha"); got != "https://alpha.example.com" {
		t.Errorf("stored origin = %q (trailing slash should be normalized away)", got)
	}
	if len(inv.dropped) != 1 || inv.dropped[0] != "alpha" {
		t.Errorf("the cached CORS decision was not invalidated: %v", inv.dropped)
	}
	if !strings.Contains(rec.Body.String(), "Browser origin saved") {
		t.Error("a save must flash confirmation")
	}
}

func TestKeyOriginRejectsEmptyProject(t *testing.T) {
	st := openKeysStore(t)
	mux := http.NewServeMux()
	Register(mux, st, ingest.NewKeyGate(st, time.Second), nil)
	req := httptest.NewRequest(http.MethodPost, "/keys/origin", strings.NewReader("cors_origin=https://a.example.com"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400", rec.Code)
	}
}
