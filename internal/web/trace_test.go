package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/iniznet/fenrirealm-monitor/internal/ingest"
	"github.com/iniznet/fenrirealm-monitor/internal/store"
)

func TestBuildTraceViewAnchorsBackendInsideFetchWindow(t *testing.T) {
	front := store.Profile{
		ProjectKey: "front", Name: "GET /api/new/[...path]", DurationMs: 500,
		Spans: []store.Span{
			{Name: "load data", DurationMs: 320, StartMs: 20, Tags: map[string]any{"leg": "backend"}},
			{Name: "render", DurationMs: 140, StartMs: 350},
		},
	}
	back := store.Profile{
		ProjectKey: "backend", Name: "GET v2.chapters.showChapter", DurationMs: 290,
		Spans: []store.Span{{Name: "request", DurationMs: 290, StartMs: 0}},
	}
	v := buildTraceView([]store.Profile{front, back})
	if v.TotalMs < 490 {
		t.Errorf("total = %v, want >= 490 (render ends at 490)", v.TotalMs)
	}
	if len(v.Rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(v.Rows))
	}
	// Backend request bar must sit INSIDE the fetch window [20, 340].
	req := v.Rows[2]
	if req.StartMs < 20 || req.StartMs+req.DurationMs > 360 {
		t.Errorf("backend leg anchored at %v+%v, want inside fetch window", req.StartMs, req.DurationMs)
	}
	if req.Project != "backend" {
		t.Errorf("row project = %s", req.Project)
	}
}

func TestBuildTraceViewLegacySequentialFallback(t *testing.T) {
	front := store.Profile{ProjectKey: "front", Name: "GET /p", DurationMs: 200,
		Spans: []store.Span{{Name: "request", DurationMs: 200}}}
	back := store.Profile{ProjectKey: "backend", Name: "GET v2.x", DurationMs: 90,
		Spans: []store.Span{{Name: "request", DurationMs: 90}}}
	v := buildTraceView([]store.Profile{front, back})
	if v.TotalMs < 290 {
		t.Errorf("total = %v, want >= 290 (legacy back-to-back layout)", v.TotalMs)
	}
	if v.Rows[1].StartMs != 200 {
		t.Errorf("legacy leg anchored at %v, want 200", v.Rows[1].StartMs)
	}
}

func TestTracePageRenders(t *testing.T) {
	srv, st := newTestServer(t)
	if _, err := st.RecordProfile(store.ProfileInput{
		ProjectKey: "front", Type: store.ProfileTypeRequest, Name: "GET /p", DurationMs: 500, TraceID: "t-1",
		Spans: []store.Span{{Name: "backend fetch", DurationMs: 300, StartMs: 20, Tags: map[string]any{"leg": "backend"}}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	resp, err := http.Get(srv.URL + "/traces/t-1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %.200s", resp.StatusCode, string(body))
	}
	for _, want := range []string{"t-1", "backend fetch", "track"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("trace page missing %q", want)
		}
	}
}

func TestTracePageNotFound(t *testing.T) {
	srv, _ := newTestServer(t)
	resp, err := http.Get(srv.URL + "/traces/nope")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestIngestProfileAcceptsTraceID(t *testing.T) {
	_, st := newTestServer(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ingest/{project}/profile", ingest.ProfileHandler(st))
	srv := httptest.NewServer(mux)
	body := `{"name":"GET /p","duration_ms":500,"trace_id":"t-9","spans":[{"name":"fetch","duration_ms":300,"start_ms":20,"tags":{"leg":"backend"}}]}`
	resp, err := http.Post(srv.URL+"/ingest/front/profile", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ingest status %d", resp.StatusCode)
	}
	legs, err := st.ProfilesByTrace("t-9", 20)
	if err != nil || len(legs) != 1 {
		t.Fatalf("legs: %v %d", err, len(legs))
	}
	if legs[0].Spans[0].StartMs != 20 {
		t.Errorf("ingest dropped start_ms: %+v", legs[0].Spans[0])
	}
}

func TestBuildTraceViewGroupsHotspotsPerLeg(t *testing.T) {
	front := store.Profile{ProjectKey: "front", Name: "GET /p", DurationMs: 500,
		Spans: []store.Span{{Name: "backend fetch", DurationMs: 300, StartMs: 20, Tags: map[string]any{"leg": "backend"}}}}
	back := store.Profile{ProjectKey: "backend", Name: "GET v2.x", DurationMs: 290, Hotspots: []store.Hotspot{
		{Function: "Illuminate\\Database\\Connection::select", SelfMs: 200, Calls: 2},
		{Function: "main()", SelfMs: 100},
	}}
	v := buildTraceView([]store.Profile{front, back})
	if len(v.HotGroups) != 1 {
		t.Fatalf("hot groups = %d, want 1 (front leg has no hotspots)", len(v.HotGroups))
	}
	g := v.HotGroups[0]
	if g.Project != "backend" || len(g.Rows) != 2 {
		t.Fatalf("group = %+v", g)
	}
	if g.Rows[0].Width != "100.0" || g.Rows[1].Width != "50.0" {
		t.Errorf("bars not scaled to the worst offender: %+v", g.Rows)
	}
}

func TestTracePageRendersHotspots(t *testing.T) {
	srv, st := newTestServer(t)
	if _, err := st.RecordProfile(store.ProfileInput{
		ProjectKey: "front", Type: store.ProfileTypeRequest, Name: "GET /p", DurationMs: 500, TraceID: "t-hot",
		Spans: []store.Span{{Name: "backend fetch", DurationMs: 300, StartMs: 20, Tags: map[string]any{"leg": "backend"}}},
	}); err != nil {
		t.Fatalf("seed front: %v", err)
	}
	if _, err := st.RecordProfile(store.ProfileInput{
		ProjectKey: "backend", Type: store.ProfileTypeCallgraph, Name: "Fenrir Realm @ node-1", DurationMs: 290, TraceID: "t-hot",
		Hotspots: []store.Hotspot{{Function: "Illuminate\\Database\\Connection::select", SelfMs: 200, Calls: 2}},
	}); err != nil {
		t.Fatalf("seed callgraph: %v", err)
	}
	resp, err := http.Get(srv.URL + "/traces/t-hot")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	text := string(body)
	for _, want := range []string{"hotspots (self time)", "Connection::select"} {
		if !strings.Contains(text, want) {
			t.Errorf("trace page missing %q", want)
		}
	}
}
