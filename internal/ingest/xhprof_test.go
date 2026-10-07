package ingest

import (
    "net/http"
    "net/http/httptest"
    "strings"
    "testing"

    "github.com/iniznet/fenrirealm-monitor/internal/store"
)

// foldTrace math, asserted exactly (microseconds in -> ms out):
//   main() inclusive 1,000,000us; children Foo::bar 600,000us and
//   str_repeat 300,000us; Foo::bar itself calls Baz::qux 200,000us.
//   self: Foo::bar 400ms, str_repeat 300ms, Baz::qux 200ms, main 100ms.
func TestXHProfHandlerFoldsSelfTime(t *testing.T) {
    st := newBenchStore(t)

    body := `{"profile":{
        "main()":{"ct":1,"wt":1000000},
        "main()==>App\\Foo::bar":{"ct":1,"wt":600000,"cpu":0,"mu":0,"pmu":0},
        "App\\Foo::bar==>App\\Baz::qux":{"ct":3,"wt":200000},
        "main()==>str_repeat":{"ct":1,"wt":300000}
    },"tags":{"env":"production","host":"node-1"},"app_name":"fenrirealm","hostname":"node-1","date":1728300000}`

    req := httptest.NewRequest(http.MethodPost, "/api/profiler/store", strings.NewReader(body))
    req.SetPathValue("project", "")
    rec := httptest.NewRecorder()
    XHProfHandler(st)(rec, req)

    if rec.Code != http.StatusOK {
        t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
    }

    profiles, err := st.Profiles("backend", 10)
    if err != nil || len(profiles) != 1 {
        t.Fatalf("expected 1 profile, got %d err %v", len(profiles), err)
    }
    p := profiles[0]
    if p.Type != store.ProfileTypeCallgraph {
        t.Fatalf("type = %s", p.Type)
    }
    if p.Environment != "production" || !strings.HasPrefix(p.Name, "fenrirealm") {
        t.Fatalf("env/name: %q %q", p.Environment, p.Name)
    }
    if p.DurationMs != 1000 {
        t.Fatalf("duration = %v ms, want 1000", p.DurationMs)
    }

    want := []struct{ fn string; ms float64 }{
        {"App\\Foo::bar", 400},
        {"str_repeat", 300},
        {"App\\Baz::qux", 200},
        {"main()", 100},
    }
    if len(p.Hotspots) != len(want) {
        t.Fatalf("hotspots = %+v, want %d rows", p.Hotspots, len(want))
    }
    for i, w := range want {
        if p.Hotspots[i].Function != w.fn || p.Hotspots[i].SelfMs != w.ms {
            t.Errorf("hotspot %d = %+v, want %s @ %v ms", i, p.Hotspots[i], w.fn, w.ms)
        }
    }
    if p.Hotspots[2].Calls != 3 {
        t.Errorf("Baz::qux calls = %d, want 3", p.Hotspots[2].Calls)
    }
}

func TestXHProfHandlerRejectsGarbage(t *testing.T) {
    st := newBenchStore(t)

    req := httptest.NewRequest(http.MethodPost, "/api/profiler/store", strings.NewReader(`{"profile":{}}`))
    rec := httptest.NewRecorder()
    XHProfHandler(st)(rec, req)

    if rec.Code != http.StatusBadRequest {
        t.Fatalf("expected 400, got %d", rec.Code)
    }
}