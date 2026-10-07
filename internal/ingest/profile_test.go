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

    issues, err := st.Issues("front", 10)
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