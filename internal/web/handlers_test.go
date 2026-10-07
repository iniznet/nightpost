package web

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iniznet/fenrirealm-monitor/internal/store"
)

func newTestServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	mux := http.NewServeMux()
	Register(mux, st)
	return httptest.NewServer(mux), st
}

func seedUI(t *testing.T, st *store.Store) {
	t.Helper()
	fp := store.ForException("QueryException", "user 42 not found", "App\\\\Controller:index")
	_, err := st.RecordError("backend", "QueryException", "user 42 not found", fp, "production", "error", map[string]any{
		"level": "error",
		"exception": map[string]any{
			"values": []any{map[string]any{
				"type":  "QueryException",
				"value": "user 42 not found",
				"stacktrace": map[string]any{"frames": []any{
					map[string]any{"module": "App\\\\Http\\\\Controllers", "function": "index", "filename": "/app/Http/Controllers/X.php", "lineno": 42.0},
				}},
			}},
		},
		"request": map[string]any{"url": "https://x/api", "method": "GET"},
	})
	if err != nil {
		t.Fatalf("seed error: %v", err)
	}
	st.RecordLog("backend", "auth", "warning", "login failed", map[string]any{"user": "42"}, "production")
	_, err = st.RecordProfile(store.ProfileInput{
		ProjectKey: "backend", Type: store.ProfileTypeRequest, Name: "GET /chapters/123", DurationMs: 412.5,
		Spans: []store.Span{{Name: "db", DurationMs: 120.5}, {Name: "render", DurationMs: 80.0}},
	})
	if err != nil {
		t.Fatalf("seed profile: %v", err)
	}
}

func TestRoutesRenderWithData(t *testing.T) {
	srv, st := newTestServer(t)
	seedUI(t, st)

	for _, path := range []string{
		"/", "/?project=backend", "/issues", "/issues?project=backend&level=error&status=open",
		"/logs", "/logs?project=backend", "/profiles", "/profiles?project=backend&type=request", "/slowest?project=backend",
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		readAll(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status %d", path, resp.StatusCode)
		}
	}

	resp, _ := http.Get(srv.URL + "/?project=backend")
	body := readAll(t, resp)
	for _, want := range []string{"backend", "errors 24h", "GET /chapters/123"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}

	resp, _ = http.Get(srv.URL + "/issues")
	body = readAll(t, resp)
	if !strings.Contains(body, "user 42 not found") {
		t.Errorf("issues list missing seeded issue; body: %.400s", body)
	}
	if !strings.Contains(body, "login failed") {
		t.Errorf("issues list missing seeded log; body: %.400s", body)
	}

	resp, _ = http.Get(srv.URL + "/logs?project=backend")
	body = readAll(t, resp)
	if !strings.Contains(body, "login failed") {
		t.Error("logs page missing seeded log")
	}
	if !strings.Contains(body, "auth") {
		t.Error("logs page missing channel")
	}

	resp, _ = http.Get(srv.URL + "/profiles?project=backend&type=request")
	body = readAll(t, resp)
	if !strings.Contains(body, "GET /chapters/123") {
		t.Error("profiles page missing seeded profile")
	}
}

func TestIssueDetailAndResolve(t *testing.T) {
	srv, st := newTestServer(t)
	seedUI(t, st)
	issues, _ := st.Issues(store.IssueQuery{Project: "backend", Type: "error", Limit: 1})
	if len(issues) == 0 {
		t.Fatal("seed failed")
	}
	id := issues[0].ID

	resp, _ := http.Get(srv.URL + "/issues/" + fmt.Sprint(id))
	body := readAll(t, resp)
	for _, want := range []string{"QueryException", "/app/Http/Controllers/X.php", "raw payload", "https://x/api"} {
		if !strings.Contains(body, want) {
			t.Errorf("detail missing %q", want)
		}
	}

	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Post(srv.URL+"/issues/"+fmt.Sprint(id)+"/resolve", "", nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("resolve expected 303, got %d", resp.StatusCode)
	}
	issue, _ := st.Issue(id)
	if !issue.Resolved {
		t.Error("resolve did not persist")
	}
	resp2, _ := http.Get(srv.URL + "/issues/" + fmt.Sprint(id))
	body = readAll(t, resp2)
	if !strings.Contains(body, "Reopen") {
		t.Error("resolved detail should show Reopen action")
	}
}

func TestIssueDetailNotFound(t *testing.T) {
	srv, _ := newTestServer(t)
	resp, _ := http.Get(srv.URL + "/issues/99999")
	readAll(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}
