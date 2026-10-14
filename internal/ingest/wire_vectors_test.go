package ingest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iniznet/fenrirealm-monitor/internal/store"
)

// wireVector is one golden vector from testdata/wire: the canonical payload for
// an ingest route plus the assertion that it lands in the store.
//
// These fixtures ARE the frozen wire contract (WIRE_SPEC.md), so a community
// port can run the same files against its own implementation — spec drift then
// fails a test here instead of failing a request in production.
type wireVector struct {
	Spec      int               `json:"spec"`
	Name      string            `json:"name"`
	Transport string            `json:"transport"`
	Route     string            `json:"route"`
	Project   string            `json:"project"`
	Headers   map[string]string `json:"headers"`
	Body      json.RawMessage   `json:"body"`
	BodyLines []string          `json:"body_lines"`
	Expect    struct {
		Status    int     `json:"status"`
		Assert    string  `json:"assert"`
		Trace     string  `json:"trace"`
		Count     int     `json:"count"`
		Type      string  `json:"type"`
		Name      string  `json:"name"`
		Duration  float64 `json:"duration_ms"`
		Exception string  `json:"exception_class"`
		Message   string  `json:"message"`
		SQL       string  `json:"sql"`
		Kind      string  `json:"kind"`
		Version   string  `json:"version"`
		Channel   string  `json:"channel"`
		Level     string  `json:"level"`
	} `json:"expect"`
}

func TestWireVectors(t *testing.T) {
	dir := filepath.Join("..", "..", "testdata", "wire")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	ran := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		ran++
		t.Run(strings.TrimSuffix(name, ".json"), func(t *testing.T) {
			runWireVector(t, filepath.Join(dir, name))
		})
	}
	if ran == 0 {
		t.Fatal("no wire vectors were executed — a frozen spec must stay executable")
	}
}

func runWireVector(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read vector: %v", err)
	}
	var v wireVector
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("parse vector: %v", err)
	}
	if v.Spec != 1 {
		t.Fatalf("vector declares spec %d; this build implements spec 1", v.Spec)
	}
	st := newBenchStore(t)

	// The socket leg is a transport, not an HTTP route: the same JSON line.
	if v.Transport == "tcp" {
		(&Monolog{Store: st}).ingestLine(v.Body)
		assertWireLog(t, st, v)
		return
	}

	body := v.Body
	if len(v.BodyLines) > 0 {
		body = []byte(strings.Join(v.BodyLines, "\n"))
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/ingest/"+v.Project+"/"+vectorLeg(v.Route), strings.NewReader(string(body)))
	req.SetPathValue("project", v.Project)
	for k, val := range v.Headers {
		req.Header.Set(k, val)
	}
	wireHandler(t, v.Route, st)(rec, req)

	if rec.Code != v.Expect.Status {
		t.Fatalf("status = %d, want %d (body %.200s)", rec.Code, v.Expect.Status, rec.Body.String())
	}
	switch v.Expect.Assert {
	case "none":
		// A rejected payload is a contract too: it must not be stored.
	case "profile":
		assertWireProfile(t, st, v)
	case "issue":
		assertWireIssue(t, st, v)
	case "query":
		assertWireQuery(t, st, v)
	case "job":
		assertWireJob(t, st, v)
	case "release":
		assertWireRelease(t, st, v)
	default:
		t.Fatalf("unknown assertion %q — a vector that asserts nothing is not a vector", v.Expect.Assert)
	}
}

// vectorLeg turns a documented route into the handler's path segment.
func vectorLeg(route string) string {
	parts := strings.Split(strings.Trim(route, "/"), "/")
	return parts[len(parts)-1]
}

func wireHandler(t *testing.T, route string, st *store.Store) http.HandlerFunc {
	t.Helper()
	switch vectorLeg(route) {
	case "sentry":
		return SentryHandler(st)
	case "profile":
		return ProfileHandler(st)
	case "error":
		return ErrorHandler(st)
	case "xhprof":
		return XHProfHandler(st)
	case "query":
		return QueryHandler(st)
	case "job":
		return JobHandler(st)
	case "release":
		return ReleaseHandler(st)
	}
	t.Fatalf("no handler wired for route %q — a new ingest route must add a vector leg", route)
	return nil
}

func assertWireProfile(t *testing.T, st *store.Store, v wireVector) {
	t.Helper()
	legs, err := st.ProfilesByTrace(v.Expect.Trace, 20)
	if err != nil {
		t.Fatalf("byTrace: %v", err)
	}
	if len(legs) != v.Expect.Count {
		t.Fatalf("trace %s holds %d legs, want %d", v.Expect.Trace, len(legs), v.Expect.Count)
	}
	if v.Expect.Type != "" && legs[0].Type != v.Expect.Type {
		t.Errorf("type = %q, want %q", legs[0].Type, v.Expect.Type)
	}
	if v.Expect.Name != "" && legs[0].Name != v.Expect.Name {
		t.Errorf("name = %q, want %q", legs[0].Name, v.Expect.Name)
	}
	if v.Expect.Duration > 0 && legs[0].DurationMs != v.Expect.Duration {
		t.Errorf("duration = %v, want %v", legs[0].DurationMs, v.Expect.Duration)
	}
}

func assertWireIssue(t *testing.T, st *store.Store, v wireVector) {
	t.Helper()
	issues, err := st.Issues(store.IssueQuery{Project: v.Project, Limit: 10})
	if err != nil {
		t.Fatalf("issues: %v", err)
	}
	for _, is := range issues {
		if is.ExceptionClass == v.Expect.Exception && strings.Contains(is.Title, v.Expect.Message) {
			return
		}
	}
	t.Errorf("%s/%q was not stored; issues = %+v", v.Expect.Exception, v.Expect.Message, issues)
}

func assertWireQuery(t *testing.T, st *store.Store, v wireVector) {
	t.Helper()
	rows, err := st.QueryLeaderboard(v.Project, 24, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	if rows[0].TotalMs != v.Expect.Duration {
		t.Errorf("total_ms = %v, want %v", rows[0].TotalMs, v.Expect.Duration)
	}
	if v.Expect.SQL != "" && rows[0].SQL != v.Expect.SQL {
		t.Errorf("sql = %q, want %q", rows[0].SQL, v.Expect.SQL)
	}
}

func assertWireJob(t *testing.T, st *store.Store, v wireVector) {
	t.Helper()
	rows, err := st.JobLeaderboard(v.Project, 24, 50)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	if v.Expect.Kind != "" && rows[0].Kind != v.Expect.Kind {
		t.Errorf("kind = %q, want %q", rows[0].Kind, v.Expect.Kind)
	}
	if v.Expect.Name != "" && rows[0].Name != v.Expect.Name {
		t.Errorf("name = %q, want %q", rows[0].Name, v.Expect.Name)
	}
}

func assertWireRelease(t *testing.T, st *store.Store, v wireVector) {
	t.Helper()
	mark, err := st.LatestRelease(v.Project, "")
	if err != nil || mark == nil || mark.Version != v.Expect.Version {
		t.Fatalf("mark=%+v err=%v, want version %q", mark, err, v.Expect.Version)
	}
}

// assertWireLog checks the socket leg by level, which is what the ingest of a
// Monolog record is observable as.
func assertWireLog(t *testing.T, st *store.Store, v wireVector) {
	t.Helper()
	levels, err := st.LogLevels(v.Project)
	if err != nil {
		t.Fatalf("levels: %v", err)
	}
	for _, l := range levels {
		if l.Level == v.Expect.Level && l.Count >= 1 {
			return
		}
	}
	t.Errorf("no %q log stored; levels = %+v", v.Expect.Level, levels)
}
