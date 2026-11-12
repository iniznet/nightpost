package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/iniznet/nightpost/internal/store"
)

func TestPrettifyFn(t *testing.T) {
	cases := map[string]string{
		"Illuminate\\Database\\Connection::{closure:Illuminate\\Database\\Connection::select():398}": "Connection::select():398 (closure)",
		"findOne": "findOne",
		"App\\Providers\\AppServiceProvider::{closure:{closure:App\\Providers\\AppServiceProvider::boot():1055}:1082}": "AppServiceProvider::boot():1055 (closure ×2)",
		"Illuminate\\Pipeline\\Pipeline::{closure:{closure:Illuminate\\Pipeline\\Pipeline::carry():184}:185}@12":       "Pipeline::carry():184 (closure ×2)@12",
		"A::{closure:{closure:{closure:B::m():7}:8}:9}":                                                                "B::m():7 (closure ×3)",
	}
	for in, want := range cases {
		if got := prettifyFn(in); got != want {
			t.Errorf("prettifyFn(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCollapseNotFound(t *testing.T) {
	issues := []store.Issue{
		{ID: 1, Title: "real failure", Count: 2},
		{ID: 2, Title: "Not found: /robots.txt", Count: 900},
		{ID: 3, Title: "Not found: /search", Count: 40},
		{ID: 4, Title: "Not found: /icons/icon-512x512.png", Count: 7},
	}
	out := collapseNotFound(issues)
	if len(out) != 2 {
		t.Fatalf("expected 2 rows after collapse, got %d", len(out))
	}
	group := out[1]
	if group.ID != 0 || !strings.Contains(group.Title, "3 paths") || group.Count != 947 {
		t.Errorf("group row = %+v", group)
	}
	if out[0].ID != 1 {
		t.Errorf("real issue misplaced: %+v", out[0])
	}

	if got := collapseNotFound([]store.Issue{{ID: 1, Title: "boom"}}); len(got) != 1 {
		t.Errorf("no-op collapse changed the list")
	}
}

// The performance surface moved from a merged index plus tabs to a journey-first
// page and an id-addressed route detail (journeys increment). This test follows
// the pages: the routes index ranks a route and names who calls it, and the
// drill-down renders the six sections for one route only.
func TestPerfRoutesIndexAndDrilldown(t *testing.T) {
	srv, st := newTestServer(t)
	fx := seedPerf(t, st, "gap")

	body := readAll(t, mustGet(t, srv.URL+"/performance/routes?project=demo"))
	for _, want := range []string{"P95", "/api/home", "Called by", "Coverage"} {
		if !strings.Contains(body, want) {
			t.Errorf("routes index missing %q", want)
		}
	}

	id := strconv.FormatInt(fx.FrontRoute, 10)
	body = readAll(t, mustGet(t, srv.URL+"/performance/routes/"+id+"?project=demo"))
	for _, want := range []string{
		"Distribution", "Journeys through it", "Slowest samples", "Queries", "Jobs", "Errors", "hop 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("route detail missing section %q", want)
		}
	}
	// The detail is one route: the heading names it, and the chain that walks
	// through it is shown as a chain, not as two competing detail pages.
	if !strings.Contains(body, "<title>/ — nightpost</title>") {
		t.Errorf("route detail is not titled for its own route: %.300s", body)
	}
	if strings.Count(body, "Distribution") != 1 {
		t.Errorf("route detail rendered more than one distribution section")
	}
}

func TestIssuesCollapseNotFoundEndToEnd(t *testing.T) {
	srv, st := newTestServer(t)
	for i := 0; i < 3; i++ {
		if _, err := st.RecordError("backend", "E", "Not found: /p"+fmt.Sprint(i), "nf"+fmt.Sprint(i), "prod", "error", nil); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if _, err := st.RecordError("backend", "E", "real failure", "rf1", "prod", "error", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	resp, err := http.Get(srv.URL + "/issues?project=backend")
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp)
	if strings.Count(body, "Not found: /p") != 0 {
		t.Errorf("collapsed view still lists individual 404 paths")
	}
	if !strings.Contains(body, "3 paths on this page") {
		t.Errorf("collapsed row missing: %.400s", body)
	}
	if !strings.Contains(body, "real failure") {
		t.Errorf("real issue buried by collapse")
	}
}

func TestCollapseNotFoundOrder(t *testing.T) {
	now := time.Now()
	issues := []store.Issue{
		{ID: 1, Title: "Not found: /a", Count: 10, LastSeenAt: now},
		{ID: 2, Title: "real", Count: 1, LastSeenAt: now},
		{ID: 3, Title: "Not found: /b", Count: 5, LastSeenAt: now},
	}
	out := collapseNotFound(issues)
	if len(out) != 2 || out[0].ID != 0 {
		t.Fatalf("group should take the first not-found slot: %+v", out)
	}
	if out[1].ID != 2 {
		t.Errorf("surviving row misplaced: %+v", out[1])
	}
}
