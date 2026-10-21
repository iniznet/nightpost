package ingest

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCORSAllowlistRefusesUnlistedOrigin(t *testing.T) {
	list := func(*http.Request) string { return "https://a.example.com, https://b.example.com" }
	called := false
	h := WithCORSSource(list, func(http.ResponseWriter, *http.Request) { called = true })

	req := httptest.NewRequest(http.MethodPost, "/ingest/front/error", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("unlisted origin: got %d, want 403", rec.Code)
	}
	if called {
		t.Error("an unlisted origin must not reach the handler")
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("a refused origin must never be echoed")
	}
}

func TestCORSAllowlistEchoesMatchedOrigin(t *testing.T) {
	list := func(*http.Request) string { return "https://a.example.com, https://b.example.com" }
	h := WithCORSSource(list, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	req := httptest.NewRequest(http.MethodPost, "/ingest/front/error", nil)
	req.Header.Set("Origin", "https://B.example.com") // match is case-insensitive
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://B.example.com" {
		t.Errorf("Allow-Origin = %q, want the request origin echoed", got)
	}
	if !containsFold(rec.Header().Get("Vary"), "origin") {
		t.Error("Vary: Origin is required once the allowlist is conditional")
	}
}

func TestPreflightSourceRefusesUnlistedOrigin(t *testing.T) {
	list := func(*http.Request) string { return "https://a.example.com" }
	req := httptest.NewRequest(http.MethodOptions, "/ingest/front/error", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	PreflightSource(list)(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("got %d, want 403", rec.Code)
	}
}

// TestCORSResolverInheritsInstallDefault is the contract: the row is the
// per-tenant truth, the -cors-origin flag is only the install-time seed.
func TestCORSResolverInheritsInstallDefault(t *testing.T) {
	rows := map[string]string{"beta": "https://beta.example.com"}
	lookup := func(project string) (string, error) { return rows[project], nil }
	r := NewCORSResolver("https://default.example.com", lookup, time.Minute)

	reqFor := func(project string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/ingest/"+project+"/error", nil)
		req.SetPathValue("project", project)
		return req
	}
	if got := r.OriginFor(reqFor("alpha")); got != "https://default.example.com" {
		t.Errorf("a project with no allowlist must inherit the install default, got %q", got)
	}
	if got := r.OriginFor(reqFor("beta")); got != "https://beta.example.com" {
		t.Errorf("beta = %q, want its own allowlist", got)
	}

	// An edit takes effect at once, not after the TTL.
	rows["beta"] = "https://new.example.com"
	if got := r.OriginFor(reqFor("beta")); got != "https://beta.example.com" {
		t.Errorf("expected the cached value before invalidation, got %q", got)
	}
	r.Invalidate("beta")
	if got := r.OriginFor(reqFor("beta")); got != "https://new.example.com" {
		t.Errorf("Invalidate did not refresh, got %q", got)
	}
}

func containsFold(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(haystack) > 0 && indexFold(haystack, needle) >= 0)
}

func indexFold(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			a, b := s[i+j], sub[j]
			if a >= 'A' && a <= 'Z' {
				a += 'a' - 'A'
			}
			if b >= 'A' && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
