package ingest

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iniznet/fenrirealm-monitor/internal/store"
)

func newBenchStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestProfileHandlerRoundTrip(t *testing.T) {
	st := newBenchStore(t)

	body := `{"name":"GET /chapters/123","duration_ms":412.5,"environment":"production","spans":[{"name":"db: 14 queries","duration_ms":380.2,"tags":{"password":"hunter2","queries":14}}]}`
	req := httptest.NewRequest(http.MethodPost, "/ingest/backend/profile", strings.NewReader(body))
	req.SetPathValue("project", "backend")
	rec := httptest.NewRecorder()
	ProfileHandler(st)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	profiles, err := st.Profiles("backend", 10)
	if err != nil {
		t.Fatalf("profiles: %v", err)
	}
	if len(profiles) != 1 {
		t.Fatalf("expected 1 profile, got %d", len(profiles))
	}
	p := profiles[0]
	if p.Name != "GET /chapters/123" || p.DurationMs != 412.5 || p.Type != store.ProfileTypeRequest {
		t.Errorf("bad profile: %+v", p)
	}
	if len(p.Spans) != 1 || p.Spans[0].Tags["password"] != "[REDACTED]" {
		t.Errorf("span tags not sanitized: %+v", p.Spans)
	}
}

func TestProfileHandlerStoresHotspots(t *testing.T) {
	st := newBenchStore(t)

	body := `{"name":"GET /x","duration_ms":500,"type":"callgraph","hotspots":[{"function":"mod\u005c\u005cFoo::bar","self_ms":100,"file":""},{"function":"","self_ms":50}]}`
	req := httptest.NewRequest(http.MethodPost, "/ingest/backend/profile", strings.NewReader(body))
	req.SetPathValue("project", "backend")
	rec := httptest.NewRecorder()
	ProfileHandler(st)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	p, err := st.Profile(func() int64 { ps, _ := st.Profiles("backend", 5); return ps[0].ID }())
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	if p.Type != store.ProfileTypeCallgraph || len(p.Hotspots) != 1 {
		t.Fatalf("bad hotspots/type: %+v", p)
	}
}

func TestProfileHandlerKeepsJobAndCommandTypes(t *testing.T) {
	for _, typ := range []string{"job", "command", "callgraph"} {
		st := newBenchStore(t)
		body := `{"name":"schedule:run","duration_ms":1234.5,"type":"` + typ + `"}`
		req := httptest.NewRequest(http.MethodPost, "/ingest/backend/profile", strings.NewReader(body))
		req.SetPathValue("project", "backend")
		rec := httptest.NewRecorder()
		ProfileHandler(st)(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s leg: 200 expected, got %d: %s", typ, rec.Code, rec.Body.String())
		}
		rows, err := st.Profiles("backend", 10)
		if err != nil || len(rows) != 1 || rows[0].Type != typ {
			t.Fatalf("%s leg must survive the type coercion, got %+v err=%v", typ, rows, err)
		}
	}
	st := newBenchStore(t)
	req := httptest.NewRequest(http.MethodPost, "/ingest/backend/profile", strings.NewReader(`{"name":"x","duration_ms":1,"type":"bogus"}`))
	req.SetPathValue("project", "backend")
	rec := httptest.NewRecorder()
	ProfileHandler(st)(rec, req)
	rows, _ := st.Profiles("backend", 10)
	if len(rows) != 1 || rows[0].Type != store.ProfileTypeRequest {
		t.Fatalf("unknown type must coerce to request, got %+v", rows)
	}
}

func TestProfileHandlerRejectsGarbage(t *testing.T) {
	st := newBenchStore(t)

	req := httptest.NewRequest(http.MethodPost, "/ingest/backend/profile", strings.NewReader(`{"duration_ms":-5}`))
	req.SetPathValue("project", "backend")
	rec := httptest.NewRecorder()
	ProfileHandler(st)(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestErrorHandlerRecordsJSError(t *testing.T) {
	st := newBenchStore(t)

	body := `{"message":"Cannot read properties of undefined","stack":"TypeError\n    at SeriesCard.svelte:42:7","url":"https://fenrirealm.com/series/x","route":"/series/[slug]","environment":"production"}`
	req := httptest.NewRequest(http.MethodPost, "/ingest/front/error", strings.NewReader(body))
	req.SetPathValue("project", "front")
	rec := httptest.NewRecorder()
	ErrorHandler(st)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	issues, err := st.Issues(store.IssueQuery{Project: "front", Limit: 10})
	if err != nil {
		t.Fatalf("issues: %v", err)
	}
	if len(issues) != 1 || issues[0].ExceptionClass != "JSError" {
		t.Fatalf("expected JSError issue, got %+v", issues)
	}
}

func TestErrorHandlerRejectsEmpty(t *testing.T) {
	st := newBenchStore(t)

	req := httptest.NewRequest(http.MethodPost, "/ingest/front/error", strings.NewReader(`{"message":""}`))
	req.SetPathValue("project", "front")
	rec := httptest.NewRecorder()
	ErrorHandler(st)(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

// Client-sent trees round-trip; oversized ones are dropped without
// losing the profile row (#181 trust boundary).
func TestProfileIngestStoresSanitizedTree(t *testing.T) {
	st := newBenchStore(t)
	h := ProfileHandler(st)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ingest/{project}/profile", h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	body := `{"name":"GET /p","duration_ms":1000,"trace_id":"t-tree",
		"spans":[{"name":"request","duration_ms":1000}],
		"tree":[{"n":"root","s":100,"i":1000,"k":[{"n":"child","s":900,"i":900}]}]}`
	resp, err := http.Post(srv.URL+"/ingest/front/profile", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	legs, err := st.ProfilesByTrace("t-tree", 20)
	if err != nil || len(legs) != 1 {
		t.Fatalf("legs %d %v", len(legs), err)
	}
	if len(legs[0].Tree) != 1 || legs[0].Tree[0].N != "root" || len(legs[0].Tree[0].K) != 1 {
		t.Fatalf("stored tree = %+v", legs[0].Tree)
	}

	// Below-floor child folds into self; negative times clamp.
	body2 := `{"name":"GET /q","duration_ms":1000,
		"tree":[{"n":"r","s":-9,"i":1000,"k":[{"n":"a","s":990,"i":990},{"n":"tiny","s":1,"i":1}]}]}`
	resp2, err := http.Post(srv.URL+"/ingest/front/profile", "application/json", strings.NewReader(body2))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	profiles, _ := st.Profiles("front", 10)
	var q *store.Profile
	for i := range profiles {
		if profiles[i].Name == "GET /q" {
			q = &profiles[i]
		}
	}
	if q == nil || len(q.Tree) != 1 || len(q.Tree[0].K) != 1 || q.Tree[0].K[0].N != "a" {
		t.Fatalf("sanitize on ingest path failed: %+v", q)
	}
}
