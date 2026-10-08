package ingest

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/iniznet/fenrirealm-monitor/internal/store"
)

type fakeKeys struct {
	mu      sync.Mutex
	keys    map[string]string
	ensured map[string]int
}

func (f *fakeKeys) ProjectIngestKey(key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.keys[key], nil
}

func (f *fakeKeys) EnsureProject(key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensured[key]++
	return nil
}

func (f *fakeKeys) set(key, val string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys[key] = val
}

func (f *fakeKeys) ensureCount(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ensured[key]
}

func post(gate *KeyGate, project, headerName, headerVal string) int {
	h := WithKeyGate(gate, PathProject, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	// Route through ServeMux so PathValue("project") is populated exactly as
	// in production; a raw httptest request leaves it empty.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ingest/{project}/sentry", h)
	req := httptest.NewRequest(http.MethodPost, "/ingest/"+project+"/sentry", nil)
	if headerName != "" {
		req.Header.Set(headerName, headerVal)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code
}

func TestKeyGateUnsignedLegacyAndKeyed(t *testing.T) {
	f := &fakeKeys{keys: map[string]string{"secured": store.HashIngestKey("s3cret")}, ensured: map[string]int{}}
	g := NewKeyGate(f, 0)

	if code := post(g, "open", "", ""); code != http.StatusAccepted {
		t.Fatalf("unsigned project must pass, got %d", code)
	}
	if n := f.ensureCount("open"); n != 1 {
		t.Fatalf("EnsureProject must run exactly once per project, ran %d", n)
	}
	if code := post(g, "open", "", ""); code != http.StatusAccepted {
		t.Fatalf("second unsigned request must pass, got %d", code)
	}
	if n := f.ensureCount("open"); n != 1 {
		t.Fatalf("EnsureProject re-ran per event (%d)", n)
	}

	if code := post(g, "secured", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("keyed project without header must 401, got %d", code)
	}
	if code := post(g, "secured", "X-Monitor-Key", "wrong"); code != http.StatusUnauthorized {
		t.Fatalf("wrong key must 401, got %d", code)
	}
	if code := post(g, "secured", "X-Monitor-Key", "s3cret"); code != http.StatusAccepted {
		t.Fatalf("X-Monitor-Key must authenticate, got %d", code)
	}
	if code := post(g, "secured", "Authorization", "Bearer s3cret"); code != http.StatusAccepted {
		t.Fatalf("Bearer must authenticate, got %d", code)
	}
	if code := post(g, "secured", "Authorization", "Token s3cret"); code != http.StatusUnauthorized {
		t.Fatalf("non-bearer scheme must 401, got %d", code)
	}
}

func TestKeyGateRotationInvalidates(t *testing.T) {
	f := &fakeKeys{keys: map[string]string{"p": store.HashIngestKey("v1")}, ensured: map[string]int{}}
	g := NewKeyGate(f, time.Hour)

	if code := post(g, "p", "X-Monitor-Key", "v1"); code != http.StatusAccepted {
		t.Fatalf("v1 must pass before rotation, got %d", code)
	}
	f.set("p", store.HashIngestKey("v2"))
	if code := post(g, "p", "X-Monitor-Key", "v1"); code != http.StatusAccepted {
		t.Fatalf("cached gate must keep serving v1 decision until Invalidate, got %d", code)
	}
	g.Invalidate("p")
	if code := post(g, "p", "X-Monitor-Key", "v1"); code != http.StatusUnauthorized {
		t.Fatalf("old key must die after Invalidate, got %d", code)
	}
	if code := post(g, "p", "X-Monitor-Key", "v2"); code != http.StatusAccepted {
		t.Fatalf("rotated key must pass after Invalidate, got %d", code)
	}
}

func TestKeyGateNilPassthrough(t *testing.T) {
	h := WithKeyGate(nil, PathProject, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	req := httptest.NewRequest(http.MethodPost, "/ingest/x/sentry", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("nil gate must pass through, got %d", rec.Code)
	}
}

func TestPresentedKeyForms(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	if got := presentedKey(req); got != "" {
		t.Fatalf("no headers must yield empty, got %q", got)
	}
	req.Header.Set("Authorization", "bearer  spaced ")
	if got := presentedKey(req); got != "spaced" {
		t.Fatalf("lowercase bearer + spaces must trim, got %q", got)
	}
	req.Header.Set("X-Monitor-Key", "hk")
	if got := presentedKey(req); got != "hk" {
		t.Fatalf("X-Monitor-Key must win, got %q", got)
	}
}
