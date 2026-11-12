package store

import (
	"fmt"
	"sort"
	"testing"
	"time"
)

// seedSpan writes one span row the way ingest would: the service and the route
// are upserted first, so the materialiser reads exactly the rows a real payload
// leaves behind. A span with an empty route is an outbound call the caller did not
// name; a span with an empty parent is that trace's root.
func seedSpan(t *testing.T, st *Store, trace, spanID, parent, kind, routePattern string, startMs, durationMs float64, peer string) {
	t.Helper()
	svcID, err := st.UpsertService("app", "backend", "php", "production", false)
	if err != nil {
		t.Fatalf("upsert service: %v", err)
	}
	var routeID int64
	if routePattern != "" {
		routeID, err = st.UpsertRoute(svcID, "GET", routePattern, kind)
		if err != nil {
			t.Fatalf("upsert route: %v", err)
		}
	}
	var peerID int64
	if peer != "" {
		peerID, err = st.UpsertService("app", peer, "php", "production", false)
		if err != nil {
			t.Fatalf("upsert peer service: %v", err)
		}
	}
	if err := st.InsertSpans([]TraceSpan{{
		TraceID: trace, SpanID: spanID, ParentSpanID: parent, ServiceID: svcID,
		RouteID: routeID, Kind: kind, Name: routePattern, StartMs: startMs,
		DurationMs: durationMs, Status: "ok", PeerServiceID: peerID, CreatedAt: time.Now(),
	}}); err != nil {
		t.Fatalf("insert span: %v", err)
	}
}

// journeyWindow is the window a test recomputes over: wide enough to catch a
// span stored a millisecond ago on either side of a slow clock.
func journeyWindow() (time.Time, time.Time) {
	now := time.Now()
	return now.Add(-time.Hour), now.Add(time.Minute)
}

func recomputeJourneys(t *testing.T, st *Store) {
	t.Helper()
	from, to := journeyWindow()
	if err := st.RecomputeJourneys(from, to); err != nil {
		t.Fatalf("recompute journeys: %v", err)
	}
}

// onlyJourney returns the single materialised journey of a project, failing when
// the pass produced a different number: one distinct path must be one row.
func onlyJourney(t *testing.T, st *Store) Journey {
	t.Helper()
	got, err := st.Journeys("app", "", 50)
	if err != nil {
		t.Fatalf("journeys: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("materialised %d journeys, want 1: %+v", len(got), got)
	}
	return got[0]
}

// TestJourneyP95MatchesDirectAggregate is the no-drift test: the p50/p95 the
// materialiser stores must equal the same numbers recomputed straight from the
// spans. If they can diverge, every performance page is a guess.
func TestJourneyP95MatchesDirectAggregate(t *testing.T) {
	st := newTestStore(t)
	const traces = 20
	var expected []float64
	for i := 0; i < traces; i++ {
		duration := float64(100 * (i + 1))
		trace := fmt.Sprintf("trace-%02d", i)
		seedSpan(t, st, trace, fmt.Sprintf("span%012d", i), "", "server", "GET /orders/{id}", 0, duration, "")
		expected = append(expected, duration)
	}
	recomputeJourneys(t, st)
	journey := onlyJourney(t, st)
	if journey.Samples != traces {
		t.Fatalf("samples = %d, want %d", journey.Samples, traces)
	}

	// Recompute the same percentiles from the stored spans, not from the
	// materialiser's internals, with the documented index rule.
	var direct []float64
	for i := 0; i < traces; i++ {
		spans, err := st.TraceSpans(fmt.Sprintf("trace-%02d", i))
		if err != nil {
			t.Fatalf("trace spans: %v", err)
		}
		if len(spans) != 1 {
			t.Fatalf("trace %d carries %d spans, want 1", i, len(spans))
		}
		direct = append(direct, spans[0].DurationMs)
	}
	sort.Float64s(direct)
	wantP50 := direct[len(direct)*50/100]
	wantP95 := direct[len(direct)*95/100]
	if journey.P50Ms != wantP50 {
		t.Errorf("stored p50 = %v, direct aggregate = %v", journey.P50Ms, wantP50)
	}
	if journey.P95Ms != wantP95 {
		t.Errorf("stored p95 = %v, direct aggregate = %v", journey.P95Ms, wantP95)
	}
}

// TestJourneyMaterialisationIsIdempotent pins the property the five-minute
// ticker depends on: a second pass over the same window updates the same rows
// instead of minting a second journey or a second hop list.
func TestJourneyMaterialisationIsIdempotent(t *testing.T) {
	st := newTestStore(t)
	seedSpan(t, st, "trace-a", "span0000000001", "", "server", "GET /reader/{slug}", 0, 500, "")
	seedSpan(t, st, "trace-a", "span0000000002", "span0000000001", "client", "GET /api/series/{id}", 200, 120, "")
	seedSpan(t, st, "trace-b", "span0000000003", "", "server", "GET /reader/{slug}", 0, 700, "")
	seedSpan(t, st, "trace-b", "span0000000004", "span0000000003", "client", "GET /api/series/{id}", 100, 200, "")

	recomputeJourneys(t, st)
	first := onlyJourney(t, st)
	firstHops, err := st.JourneyHops(first.ID)
	if err != nil {
		t.Fatalf("hops: %v", err)
	}
	recomputeJourneys(t, st)
	second := onlyJourney(t, st)
	secondHops, err := st.JourneyHops(second.ID)
	if err != nil {
		t.Fatalf("hops: %v", err)
	}

	if second.ID != first.ID || second.PatternKey != first.PatternKey {
		t.Fatalf("a second pass minted a new journey: %d -> %d", first.ID, second.ID)
	}
	if second.Samples != 2 || second.HopCount != 2 {
		t.Fatalf("second pass changed the aggregates: %+v", second)
	}
	if len(secondHops) != len(firstHops) {
		t.Fatalf("hop list changed length: %d -> %d", len(firstHops), len(secondHops))
	}
	for i := range firstHops {
		if firstHops[i] != secondHops[i] {
			t.Errorf("hop %d drifted: %+v -> %+v", i, firstHops[i], secondHops[i])
		}
	}
}

// TestUnreportedHopBecomesGap is the gap contract: an outbound span that names a
// peer, with no child span in the trace, becomes exactly one 'unreported' hop and
// drops the journey's coverage below 100%.
func TestUnreportedHopBecomesGap(t *testing.T) {
	st := newTestStore(t)
	seedSpan(t, st, "trace-gap", "span000000000a", "", "server", "GET /reader/{slug}", 0, 900, "")
	seedSpan(t, st, "trace-gap", "span000000000b", "span000000000a", "client", "", 120, 400, "backend")

	recomputeJourneys(t, st)
	journey := onlyJourney(t, st)
	hops, err := st.JourneyHops(journey.ID)
	if err != nil {
		t.Fatalf("hops: %v", err)
	}
	if len(hops) != 2 {
		t.Fatalf("hops = %d, want 2 (entry + gap): %+v", len(hops), hops)
	}
	unreported := 0
	for _, hop := range hops {
		if hop.Status == "unreported" {
			unreported++
		}
	}
	if unreported != 1 {
		t.Fatalf("unreported hops = %d, want exactly 1: %+v", unreported, hops)
	}
	coverage, err := st.JourneyCoverage(journey.ID)
	if err != nil {
		t.Fatalf("coverage: %v", err)
	}
	if coverage.Total != 2 || coverage.Reported != 1 {
		t.Fatalf("coverage = %+v, want 1/2", coverage)
	}
	if coverage.Pct() >= 100 || coverage.Complete() {
		t.Fatalf("a gap must read below 100%%: %v", coverage.Pct())
	}
	if coverage.String() != "1/2" {
		t.Fatalf("coverage label = %q, want 1/2", coverage.String())
	}

	// The rule's own input sees the same gap: one path, one unreported hop.
	gaps, err := st.UnreportedJourneys(time.Now().Add(-time.Hour).Unix(), []string{"app"})
	if err != nil {
		t.Fatalf("unreported journeys: %v", err)
	}
	if len(gaps) != 1 || gaps[0].Unreported != 1 || gaps[0].Hops != 2 {
		t.Fatalf("gap report = %+v, want one path with 1 of 2 hops missing", gaps)
	}
}

// TestJourneyCoverageCompleteWithoutGaps pins the other half: a trace whose
// outbound call WAS answered is fully covered, and the answering child span is
// the hop — the caller's own span is never drawn twice.
func TestJourneyCoverageCompleteWithoutGaps(t *testing.T) {
	st := newTestStore(t)
	seedSpan(t, st, "trace-ok", "span000000000c", "", "server", "GET /reader/{slug}", 0, 900, "")
	seedSpan(t, st, "trace-ok", "span000000000d", "span000000000c", "client", "", 100, 500, "backend")
	seedSpan(t, st, "trace-ok", "span000000000e", "span000000000d", "server", "GET /api/series/{id}", 120, 300, "")

	recomputeJourneys(t, st)
	journey := onlyJourney(t, st)
	coverage, err := st.JourneyCoverage(journey.ID)
	if err != nil {
		t.Fatalf("coverage: %v", err)
	}
	if !coverage.Complete() || coverage.Total != 2 || coverage.Pct() != 100 {
		t.Fatalf("a fully reported trace must be 100%% covered: %+v", coverage)
	}
	hops, err := st.JourneyHops(journey.ID)
	if err != nil {
		t.Fatalf("hops: %v", err)
	}
	for _, hop := range hops {
		if hop.Status != "ok" {
			t.Fatalf("a covered trace must stay all-ok: %+v", hops)
		}
	}
}

// TestJourneyEntryKindSplitsPageFromAPI pins the entry rule the plan names: a
// server root materialises as an api journey, a browser root as a page journey,
// and the two never share a pattern key.
func TestJourneyEntryKindSplitsPageFromAPI(t *testing.T) {
	st := newTestStore(t)
	seedSpan(t, st, "trace-api", "span000000000f", "", "server", "GET /api/series/{id}", 0, 100, "")
	seedSpan(t, st, "trace-page", "span0000000009", "", "browser", "GET /reader/{slug}", 0, 100, "")

	recomputeJourneys(t, st)
	api, err := st.Journeys("app", EntryKindAPI, 10)
	if err != nil {
		t.Fatalf("api journeys: %v", err)
	}
	page, err := st.Journeys("app", EntryKindPage, 10)
	if err != nil {
		t.Fatalf("page journeys: %v", err)
	}
	if len(api) != 1 || len(page) != 1 {
		t.Fatalf("api=%d page=%d, want one each", len(api), len(page))
	}
	if api[0].PatternKey == page[0].PatternKey {
		t.Fatal("an api journey and a page journey must not share a pattern key")
	}
}

// TestJourneyHotspotAttributionKeepsAppFrames pins the attribution default: the
// leaderboard names this team's functions, and the dependency and runtime frames
// are still reachable when the page asks for every frame.
func TestJourneyHotspotAttributionKeepsAppFrames(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.RecordProfile(ProfileInput{
		ProjectKey: "app", Type: ProfileTypeCallgraph, Name: "GET /reader/{slug}", DurationMs: 1000,
		Hotspots: []Hotspot{
			{Function: "Illuminate\\Foundation\\Application::handle", File: "/srv/vendor/laravel/framework/src/Illuminate/Foundation/Application.php", SelfMs: 700},
			{Function: "runtime.goexit", File: "/usr/local/go/src/runtime/asm_amd64.s", SelfMs: 100},
			{Function: "App\\Http\\Controllers\\ChapterController::show", File: "/srv/app/Http/Controllers/ChapterController.php", SelfMs: 200},
		},
	}); err != nil {
		t.Fatalf("record profile: %v", err)
	}

	app, err := st.HotspotAttribution("app", HotspotAppFrames, 100)
	if err != nil {
		t.Fatalf("app frames: %v", err)
	}
	if len(app) != 1 || app[0].Function != "App\\Http\\Controllers\\ChapterController::show" {
		t.Fatalf("app-frame leaderboard = %+v, want only the controller", app)
	}
	all, err := st.HotspotAttribution("app", HotspotAllFrames, 100)
	if err != nil {
		t.Fatalf("all frames: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("all-frame leaderboard kept %d rows, want 3: %+v", len(all), all)
	}
	// Ranking is by self time, so the vendor frame is still first when the full
	// list is asked for: filtering must not reorder anything.
	if all[0].Function != "Illuminate\\Foundation\\Application::handle" {
		t.Fatalf("full list is out of order: %+v", all)
	}
}
