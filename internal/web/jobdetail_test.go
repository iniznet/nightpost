package web

import (
	"net/http"
	"strings"
	"testing"

	"github.com/iniznet/nightpost/internal/store"
)

// seedJobRun writes one sampled run of a job: a profile of kind job/command
// carrying its spans and hotspots, which is what the producer sends when it
// samples a run (iniznet/nightpost#4).
func seedJobRun(t *testing.T, st *store.Store, name string, spans []store.Span, hotspots []store.Hotspot) {
	t.Helper()
	_, err := st.RecordProfile(store.ProfileInput{
		ProjectKey: "backend",
		Type:       store.ProfileTypeCommand,
		Name:       name,
		DurationMs: 820.5,
		TraceID:    "a1b2c3d4e5f60718293a4b5c6d7e8f90",
		Spans:      spans,
		Hotspots:   hotspots,
	})
	if err != nil {
		t.Fatalf("seed job run: %v", err)
	}
}

// The drill-down exists to answer "which part of this mechanism is slow": the
// page must name the frames that carry the cost and link the run's waterfall.
func TestJobDetailPageShowsFramesAndWaterfallLink(t *testing.T) {
	srv, st := newTestServer(t)
	seedUI(t, st)
	if err := st.RecordJob("backend", "command", "chapters:unlock", "", 820.5, true, 1, ""); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	seedJobRun(t, st, "chapters:unlock",
		[]store.Span{{Name: "db", DurationMs: 760.0}, {Name: "redis", DurationMs: 12.0}},
		[]store.Hotspot{{Function: "App\\Service\\Unlock::run", File: "app/Service/Unlock.php", SelfMs: 41.0, Calls: 3}},
	)

	resp := mustGet(t, srv.URL+"/jobs/command/chapters:unlock?project=backend")
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	for _, want := range []string{
		"Where the time goes", // the aggregated frame table
		">db<",                 // the database hop, named
		"App\\Service\\Unlock::run",
		"/performance/requests/a1b2c3d4e5f60718293a4b5c6d7e8f90", // the waterfall link
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the job page is missing %q", want)
		}
	}
}

// A job with no sampled run still renders: the leaderboard counts every run,
// the drill-down counts only the sampled ones, and saying so is the difference
// between an honest page and an empty one.
func TestJobDetailPageSaysWhenNothingWasSampled(t *testing.T) {
	srv, st := newTestServer(t)
	seedUI(t, st)
	resp := mustGet(t, srv.URL+"/jobs/command/never:sampled?project=backend")
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, "No sampled run in this range") {
		t.Error("an unsampled job must say so, not render a blank table")
	}
	if !strings.Contains(body, "NIGHTPOST_JOB_SPAN_RATE") {
		t.Error("the empty state must name the setting that turns sampling on")
	}
}

// The empty /performance page used to claim nothing was there while its own
// footer counted thousands of profiles. It now names the count and the cause.
func TestJourneysEmptyStateNamesTheSpanlessProfiles(t *testing.T) {
	srv, st := newTestServer(t)
	// A producer on the old line: profiles arrive, no spans[]. seedUI's own
	// profile carries spans, so this one must be written explicitly.
	if _, err := st.RecordProfile(store.ProfileInput{
		ProjectKey: "backend", Type: store.ProfileTypeRequest,
		Name: "GET /chapters/{id}", DurationMs: 412.5,
	}); err != nil {
		t.Fatalf("seed spanless profile: %v", err)
	}
	resp := mustGet(t, srv.URL+"/performance?project=backend")
	body := readAll(t, resp)
	if !strings.Contains(body, "no spans") {
		t.Error("the empty journeys page must name the missing spans")
	}
	if !strings.Contains(body, "NIGHTPOST_SPANS=true") {
		t.Error("the empty state must name the setting that fixes it")
	}
}
