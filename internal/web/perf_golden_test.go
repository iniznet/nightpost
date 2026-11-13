package web

import (
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/iniznet/nightpost/internal/store"
)

// This file is the gate for the six performance pages. Every page is rendered
// in three seeded states — empty, single hop, gap — and compared against a file
// in testdata/golden. Each page also carries one explicit marker assertion, so a
// golden re-recorded without the gap row, the coverage fraction or the P95
// header still fails: the golden is a regression test, not a snapshot of today.

var updateGolden = flag.Bool("update-golden", false, "rewrite the golden HTML files")

// perfFixture is the seeded fleet every golden state starts from: two services
// in one project, the routes they own, and the ids a page needs to be addressed.
type perfFixture struct {
	FrontService int64
	BackService  int64
	FrontRoute   int64
	BackRoute    int64
	TraceSingle  string
	TraceGap     string
}

// seedPerf writes one golden state through the store, never through the HTTP
// surface, so the test proves the page renders what the store holds.
//
// The three states are:
//   - empty: the fleet registered, nothing measured yet.
//   - single: one trace that is one hop, fully reported (coverage 1/1).
//   - gap: one trace whose second hop the callee never reported (coverage 1/2).
func seedPerf(t *testing.T, st *store.Store, state string) perfFixture {
	t.Helper()
	fx := perfFixture{TraceSingle: "t-single", TraceGap: "t-gap"}

	var err error
	if fx.FrontService, err = st.UpsertService("demo", "front", "sveltekit", "production", true); err != nil {
		t.Fatalf("seed front service: %v", err)
	}
	if fx.BackService, err = st.UpsertService("demo", "backend", "laravel", "production", true); err != nil {
		t.Fatalf("seed backend service: %v", err)
	}
	if fx.FrontRoute, err = st.UpsertRoute(fx.FrontService, "GET", "/", "http"); err != nil {
		t.Fatalf("seed front route: %v", err)
	}
	if fx.BackRoute, err = st.UpsertRoute(fx.BackService, "GET", "/api/home", "http"); err != nil {
		t.Fatalf("seed backend route: %v", err)
	}
	if state == "empty" {
		return fx
	}

	now := time.Now()
	trace := fx.TraceSingle
	spans := []store.TraceSpan{{
		TraceID: trace, SpanID: "s-root", ServiceID: fx.FrontService, RouteID: fx.FrontRoute,
		Kind: "document", Name: "GET /", StartMs: 0, DurationMs: 812, Status: "ok", CreatedAt: now,
	}}
	if state == "gap" {
		trace = fx.TraceGap
		spans[0].TraceID = trace
		// The caller recorded the call it made and the peer that owed it, and the
		// callee recorded nothing: this is the gap the dashed chip names.
		spans = append(spans, store.TraceSpan{
			TraceID: trace, SpanID: "s-call", ParentSpanID: "s-root",
			ServiceID: fx.FrontService, RouteID: fx.BackRoute, Kind: "client",
			Name: "fetch /api/home", StartMs: 40, DurationMs: 600, Status: "ok",
			PeerServiceID: fx.BackService, CreatedAt: now,
		})
	}
	if err := st.InsertSpans(spans); err != nil {
		t.Fatalf("seed spans: %v", err)
	}
	if err := st.RecomputeJourneys(now.Add(-time.Hour), now.Add(time.Hour)); err != nil {
		t.Fatalf("recompute journeys: %v", err)
	}

	// Profiles give the Routes page its latency and the drill-down a sample to
	// open; the callgraph profile gives the request page its attribution table.
	if _, err := st.RecordProfile(store.ProfileInput{
		ProjectKey: "demo", Type: store.ProfileTypeRequest, Name: "/", DurationMs: 812,
		TraceID: trace, ServiceID: fx.FrontService, RouteID: fx.FrontRoute,
		Environment: "production",
	}); err != nil {
		t.Fatalf("seed front profile: %v", err)
	}
	if _, err := st.RecordProfile(store.ProfileInput{
		ProjectKey: "demo", Type: store.ProfileTypeRequest, Name: "/api/home", DurationMs: 600,
		TraceID: trace, ServiceID: fx.BackService, RouteID: fx.BackRoute,
		Environment: "production",
	}); err != nil {
		t.Fatalf("seed backend profile: %v", err)
	}
	if _, err := st.RecordProfile(store.ProfileInput{
		ProjectKey: "demo", Type: store.ProfileTypeCallgraph, Name: "demo @ node-1", DurationMs: 812,
		Hotspots: []store.Hotspot{
			{Function: "SeriesController::show", File: "/app/Http/Controllers/SeriesController.php", SelfMs: 240, Calls: 3},
			{Function: "ChapterList::visible", File: "/app/Domain/ChapterList.php", SelfMs: 96, Calls: 2},
		},
	}); err != nil {
		t.Fatalf("seed callgraph: %v", err)
	}

	if err := st.RecordRelease("demo", "v1.2.3", "production"); err != nil {
		t.Fatalf("seed release: %v", err)
	}
	for _, sess := range []store.Session{
		{ProjectKey: "demo", Release: "v1.2.3", SessionID: "sess-ok-1", Status: "ok", StartedAt: now, DurationMs: 1400},
		{ProjectKey: "demo", Release: "v1.2.3", SessionID: "sess-ok-2", Status: "ok", StartedAt: now, DurationMs: 900},
		{ProjectKey: "demo", Release: "v1.2.2", SessionID: "sess-bad-1", Status: "crashed", StartedAt: now, DurationMs: 300},
	} {
		if err := st.UpsertSession(sess); err != nil {
			t.Fatalf("seed session: %v", err)
		}
	}

	if err := st.RecordJob("demo", "queue", "nightly-report", "default", 1.5, true, 1, ""); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	// A job that promised a run two hours ago and never delivered it is the state
	// the Jobs page exists to show, so the seed produces it the way the ticker
	// does: register the promise, then let the sweep stamp it.
	if err := st.SetJobSchedule("demo", "queue", "nightly-report", "0 3 * * *", now.Add(-2*time.Hour).Unix(), 300); err != nil {
		t.Fatalf("seed job schedule: %v", err)
	}
	if err := st.RecordJob("demo", "command", "cache:warm", "", 0.8, true, 1, ""); err != nil {
		t.Fatalf("seed command: %v", err)
	}
	if _, err := st.MarkMissedJobs(now); err != nil {
		t.Fatalf("seed missed sweep: %v", err)
	}
	return fx
}

// goldenPage is one page under test: its path template, and the markers that
// must be in the body of each state whatever the golden says.
type goldenPage struct {
	name    string
	path    string
	markers map[string][]string
}

var goldenPages = []goldenPage{
	{
		name: "journeys", path: "/performance?project=demo",
		markers: map[string][]string{
			"empty":  {"No journeys in this range"},
			"single": {"P95", "Hop chain", "1/1"},
			"gap":    {"P95", "Coverage", "1/2", "hop-gap"},
		},
	},
	{
		name: "routes", path: "/performance/routes?project=demo",
		markers: map[string][]string{
			"empty":  {"No routes in this range"},
			"single": {"P95", "Coverage", "/api/home"},
			"gap":    {"Called by", "0/1"},
		},
	},
	{
		name: "route", path: "/performance/routes/{route}?project=demo",
		markers: map[string][]string{
			"empty":  {"Distribution", "No journey walks this route"},
			"single": {"Distribution", "Journeys through it", "Slowest samples", "Queries", "Jobs", "Errors"},
			"gap":    {"hop 1", "0/1"},
		},
	},
	{
		name: "request", path: "/performance/requests/{trace}",
		markers: map[string][]string{
			"empty":  {"trace not found"},
			"single": {"services", "hops", "Attribution", "SeriesController::show"},
			"gap":    {"row-gap", "not reported", "docs/producers/laravel.md"},
		},
	},
	{
		name: "releases", path: "/releases?project=demo",
		markers: map[string][]string{
			"empty":  {"No release markers in this range"},
			"single": {"Crash-free", "Adoption", "v1.2.3"},
			"gap":    {"Sessions", "Adoption"},
		},
	},
	{
		name: "jobs", path: "/jobs?project=demo",
		markers: map[string][]string{
			"empty":  {"No job telemetry in this range"},
			"single": {"Schedule", "Expected next", "missed"},
			"gap":    {"missed", "nightly-report"},
		},
	},
}

var (
	relClockRe  = regexp.MustCompile("(just now|in <1m|in [0-9]+[mhd]|[0-9]+[mhd] ago)")
	dbSizeRe    = regexp.MustCompile("[0-9]+[.][0-9] MB")
	routeHrefRe = regexp.MustCompile("/performance/routes/[0-9]+")
	// The jobs table stamps an exact wall clock into a title attribute. Without
	// this the golden pins the minute the suite happened to run and fails on the
	// next one, which is the defect the helper exists to prevent.
	stampRe = regexp.MustCompile(`title="[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}"`)
)

// normaliseGolden rewrites the values that legitimately differ between two runs
// of the same seed: a relative clock, the database file size in the footer, and
// the route ids a fresh database numbers. Everything else stays pinned, which is
// what makes the file a regression test rather than a snapshot of today.
func normaliseGolden(body string) string {
	body = relClockRe.ReplaceAllString(body, "T")
	body = dbSizeRe.ReplaceAllString(body, "SIZE")
	body = routeHrefRe.ReplaceAllString(body, "/performance/routes/RID")
	body = stampRe.ReplaceAllString(body, `title="STAMP"`)
	return body
}

// TestNormaliseGoldenKeepsStructure is the helper's own guard: a clock or a
// file size that slipped through would make every golden fail on a slow run.
func TestNormaliseGoldenKeepsStructure(t *testing.T) {
	in := "href=\"/performance/routes/12?project=demo\" 4.2 MB just now 3h ago in 5m"
	got := normaliseGolden(in)
	want := "href=\"/performance/routes/RID?project=demo\" SIZE T T T"
	if got != want {
		t.Fatalf("normaliseGolden = %q, want %q", got, want)
	}
}

func TestGoldenPerformancePages(t *testing.T) {
	for _, page := range goldenPages {
		for _, state := range []string{"empty", "single", "gap"} {
			t.Run(page.name+"/"+state, func(t *testing.T) {
				srv, st := newTestServer(t)
				fx := seedPerf(t, st, state)

				path := strings.ReplaceAll(page.path, "{route}", strconv.FormatInt(fx.FrontRoute, 10))
				trace := fx.TraceSingle
				if state == "gap" {
					trace = fx.TraceGap
				}
				path = strings.ReplaceAll(path, "{trace}", trace)

				resp := mustGet(t, srv.URL+path)
				body := readAll(t, resp)

				// The markers are asserted against the live body, not the golden, so a
				// re-recorded golden cannot hide a missing gap row, coverage fraction
				// or P95 header.
				for _, want := range page.markers[state] {
					if !strings.Contains(body, want) {
						t.Errorf("%s/%s: body is missing %q", page.name, state, want)
					}
				}
				// The product thesis is a dashboard that works with JavaScript off,
				// so a script tag on any of these pages is a regression by itself.
				if strings.Contains(strings.ToLower(body), "<script") {
					t.Errorf("%s/%s: page carries a script tag", page.name, state)
				}

				golden := filepath.Join("testdata", "golden", page.name+"_"+state+".html")
				got := normaliseGolden(body)
				if *updateGolden {
					if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
						t.Fatalf("create golden dir: %v", err)
					}
					if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
						t.Fatalf("write golden: %v", err)
					}
					return
				}
				raw, err := os.ReadFile(golden)
				if err != nil {
					t.Fatalf("read golden %s: %v (run: go test ./internal/web -run Golden -update-golden)", golden, err)
				}
				if want := string(raw); got != want {
					t.Errorf("%s/%s: rendered page differs from %s\n%s",
						page.name, state, golden, firstDiff(want, got))
				}
			})
		}
	}
}

// firstDiff names the line where two renderings part company, so a failure is
// readable without dumping two full pages into the log.
func firstDiff(want, got string) string {
	wantLines, gotLines := strings.Split(want, "\n"), strings.Split(got, "\n")
	limit := len(wantLines)
	if len(gotLines) < limit {
		limit = len(gotLines)
	}
	for i := 0; i < limit; i++ {
		if wantLines[i] != gotLines[i] {
			return "line " + strconv.Itoa(i+1) + "\n  golden: " + wantLines[i] + "\n  live:   " + gotLines[i]
		}
	}
	return "line " + strconv.Itoa(limit+1) + " (length differs)"
}

// TestPerfPagesRenderWithZeroParams is the I1 guard for the new surface: a page
// that needs a param to render is a broken page, so every list page is asked for
// with none and must answer with the shell.
func TestPerfPagesRenderWithZeroParams(t *testing.T) {
	srv, st := newTestServer(t)
	seedPerf(t, st, "gap")
	for _, path := range []string{"/performance", "/performance/routes", "/releases", "/jobs"} {
		resp := mustGet(t, srv.URL+path)
		body := readAll(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status %d", path, resp.StatusCode)
			continue
		}
		for _, want := range []string{"class=\"appbar\"", "<main class=\"wrap\" id=\"main\">", "perffilter"} {
			if !strings.Contains(body, want) {
				t.Errorf("GET %s: shell fragment %q missing", path, want)
			}
		}
	}
}

// TestPerfStylesheetIsServed proves the stylesheet the layout links is actually
// served from the binary. A golden compares the body, so a 404 on the CSS would
// leave every page unstyled and no golden would notice.
func TestPerfStylesheetIsServed(t *testing.T) {
	srv, _ := newTestServer(t)
	resp := mustGet(t, srv.URL+"/static/app.css")
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stylesheet status %d", resp.StatusCode)
	}
	for _, want := range []string{"--bg:#0b0d10", "--accent:#4a9eff", "--rowh:40px", ".perf-table"} {
		if !strings.Contains(body, want) {
			t.Errorf("stylesheet is missing %q", want)
		}
	}
}

// TestPerfHopChipNeverBlankOrDash pins the rule that replaces a bare dash: an
// unreported hop always prints something a reader can act on, and always links
// at the guide of the producer that owes it.
func TestPerfHopChipNeverBlankOrDash(t *testing.T) {
	// A gap whose route is known: the chip names the route and its service.
	known := hopChip(store.JourneyHop{Position: 1, RouteID: 7, Status: hopUnreported},
		map[int64]routeInfo{7: {Pattern: "/api/home", Service: "backend", Runtime: "laravel"}})
	if known.Label != "/api/home" || known.Doc != "/docs/producers/laravel.md" || !known.Gap {
		t.Fatalf("known gap chip = %+v", known)
	}
	// A gap with no route and no peer name still says what happened, in words.
	unknown := hopChip(store.JourneyHop{Position: 2, Status: hopUnreported}, nil)
	if strings.TrimSpace(unknown.Label) == "" || unknown.Label == "-" || unknown.Label == "\u2014" {
		t.Fatalf("unnamed gap rendered as %q", unknown.Label)
	}
	if unknown.Doc == "" {
		t.Fatal("a gap without a route must still link at a producer guide")
	}
	// A reported hop is never dashed.
	ok := hopChip(store.JourneyHop{Position: 0, RouteID: 7, Status: "ok"},
		map[int64]routeInfo{7: {Pattern: "/api/home", Service: "backend"}})
	if ok.Gap {
		t.Fatal("a reported hop must not render as a gap")
	}
}

// TestPerfCoverageIsAFraction guards the other half of the rule: coverage is
// reported/expected, never a percentage on its own.
func TestPerfCoverageIsAFraction(t *testing.T) {
	in := []journeyInput{{
		Journey:  store.Journey{EntryRouteID: 1, EntryKind: store.EntryKindPage, Samples: 2, P95Ms: 812},
		Hops:     []store.JourneyHop{{Position: 0, RouteID: 1, Status: "ok"}, {Position: 1, RouteID: 2, Status: hopUnreported}},
		Coverage: store.JourneyCoverage{Reported: 1, Total: 2},
	}}
	rows := buildJourneyRows(in, map[int64]routeInfo{
		1: {Pattern: "/", Service: "front", Project: "demo"},
		2: {Pattern: "/api/home", Service: "backend", Project: "demo"},
	})
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].Coverage != "1/2" {
		t.Fatalf("coverage = %q, want 1/2", rows[0].Coverage)
	}
	if len(rows[0].Hops) != 2 || !rows[0].Hops[1].Gap {
		t.Fatalf("hop chain = %+v, want the second hop drawn as a gap", rows[0].Hops)
	}
}
