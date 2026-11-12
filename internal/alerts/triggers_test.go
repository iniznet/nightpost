package alerts

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/iniznet/nightpost/internal/store"
)

// The seven triggers of the derived layer. Each test drives one rule from the
// rows a producer would have left and asserts what the rule MATCHED, so a rule
// that stops matching — or matches something it should not — fails here rather
// than on a page nobody is watching.

func TestTriggerNewIssueFiresOncePerTarget(t *testing.T) {
	st := testStore(t)
	now := time.Now()
	if _, err := st.RecordError("front", "TypeError", "x is undefined", "fp-new", "production", "error", nil); err != nil {
		t.Fatal(err)
	}
	d, f := armed(t, st, baseCfg([]Rule{{Type: "new-issue", Channels: []string{"test"}, Projects: []string{"front"}, WindowMinutes: 120}}))
	sent := d.Tick(now)
	if len(sent) != 1 || sent[0].Rule != "new-issue" || sent[0].TargetKey != "front|fp-new" {
		t.Fatalf("new-issue trigger = %+v, want one event for front|fp-new", sent)
	}
	if f.count() != 1 {
		t.Fatalf("delivered %d messages, want 1", f.count())
	}
}

func TestTriggerEscalationCountsInsideTheWindow(t *testing.T) {
	st := testStore(t)
	for i := 0; i < 60; i++ {
		if _, err := st.RecordError("backend", "QueryException", fmt.Sprintf("user %d missing", i), "fp-hot", "production", "error", nil); err != nil {
			t.Fatal(err)
		}
	}
	d, _ := armed(t, st, baseCfg([]Rule{{Type: "count-window", Channels: []string{"test"}, Projects: []string{"backend"}, WindowMinutes: 15, Threshold: 50}}))
	sent := d.Tick(time.Now())
	if len(sent) != 1 || !strings.Contains(sent[0].Title, "60 occurrences") {
		t.Fatalf("escalation trigger = %+v, want one breach naming 60 occurrences", sent)
	}
}

func TestTriggerReopenOnRegression(t *testing.T) {
	st := testStore(t)
	now := time.Now()
	id, err := st.RecordError("backend", "QueryException", "deadlock", "fp-reg", "production", "error", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Resolve first, then backdate the stamp: SetResolved stamps now, so the
	// reverse order would leave a resolution that a same-second occurrence cannot
	// move past.
	if err := st.SetResolved(id, true); err != nil {
		t.Fatal(err)
	}
	if err := st.SetResolvedAt(id, now.Add(-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordError("backend", "QueryException", "deadlock", "fp-reg", "production", "error", nil); err != nil {
		t.Fatal(err)
	}
	d, _ := armed(t, st, baseCfg([]Rule{{Type: "regression", Channels: []string{"test"}, Projects: []string{"backend"}}}))
	sent := d.Tick(now)
	if len(sent) != 1 || !strings.HasPrefix(sent[0].Title, "Regressed in backend") {
		t.Fatalf("reopen trigger = %+v, want one reopened issue", sent)
	}
	if issue, err := st.Issue(id); err != nil || issue.Resolved {
		t.Fatalf("the rule must reopen the issue: %+v %v", issue, err)
	}
}

func TestTriggerMissedRunNeedsTheSweepStamp(t *testing.T) {
	st := testStore(t)
	now := time.Now()
	if err := st.RecordJob("backend", "command", "quests:reset-stale-progress", "", 30, true, 1, ""); err != nil {
		t.Fatal(err)
	}
	// The promise is in the future: nothing is missing yet.
	if err := st.SetJobSchedule("backend", "command", "quests:reset-stale-progress", "0 * * * *", now.Add(time.Hour).Unix(), 60); err != nil {
		t.Fatal(err)
	}
	d, _ := armed(t, st, baseCfg([]Rule{{Type: "missed_run", Channels: []string{"test"}, Projects: []string{"backend"}}}))
	if sent := d.Tick(now); len(sent) != 0 {
		t.Fatalf("a run that is not yet due must stay silent: %+v", sent)
	}

	// Past the grace window, but unreported: the rule reads the sweep's stamp, not
	// the clock, so a stalled ticker cannot silently disable the alert.
	if err := st.SetJobSchedule("backend", "command", "quests:reset-stale-progress", "0 * * * *", now.Add(-time.Hour).Unix(), 60); err != nil {
		t.Fatal(err)
	}
	if sent := d.Tick(now); len(sent) != 0 {
		t.Fatalf("an unswept row has no verdict yet: %+v", sent)
	}
	if _, err := st.MarkMissedJobs(now); err != nil {
		t.Fatal(err)
	}
	sent := d.Tick(now)
	if len(sent) != 1 || sent[0].TargetKey != "backend|command|quests:reset-stale-progress" {
		t.Fatalf("missed-run trigger = %+v, want one event naming the command", sent)
	}
}

func TestTriggerQuerySlowOnWorstSample(t *testing.T) {
	st := testStore(t)
	for i := 0; i < 5; i++ {
		if _, err := st.RecordQuery("backend", "select * from series where slug = ?", 40, "mysql", "production"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.RecordQuery("backend", "select * from chapters where series_id = ?", 3200, "mysql", "production"); err != nil {
		t.Fatal(err)
	}
	d, _ := armed(t, st, baseCfg([]Rule{{Type: "query_slow", Channels: []string{"test"}, Projects: []string{"backend"}, WindowMinutes: 60, ThresholdMs: 1000}}))
	sent := d.Tick(time.Now())
	if len(sent) != 1 || !strings.Contains(sent[0].Title, "3200ms") {
		t.Fatalf("query_slow trigger = %+v, want only the 3200ms shape", sent)
	}
}

func TestTriggerSpanUnreportedOnMaterialisedGap(t *testing.T) {
	st := testStore(t)
	now := time.Now()
	svcID, err := st.UpsertService("backend", "backend", "php", "production", false)
	if err != nil {
		t.Fatal(err)
	}
	routeID, err := st.UpsertRoute(svcID, "GET", "GET /reader/{slug}", "server")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertJourney(store.Journey{
		PatternKey: "gap-key", EntryKind: store.EntryKindAPI, EntryRouteID: routeID,
		HopCount: 3, Samples: 4, P50Ms: 400, P95Ms: 900,
		FirstSeenAt: now.Add(-time.Minute), LastSeenAt: now,
	}, []store.JourneyHop{
		{Position: 0, RouteID: routeID, Status: "ok", P95Ms: 900, Share: 1},
		{Position: 1, Status: "unreported", P95Ms: 300, Share: 0.3},
		{Position: 2, Status: "ok", P95Ms: 200, Share: 0.2},
	}); err != nil {
		t.Fatal(err)
	}
	// A fully covered path must not fire: the rule is about gaps, not about
	// journeys existing.
	if _, err := st.UpsertJourney(store.Journey{
		PatternKey: "clean-key", EntryKind: store.EntryKindAPI, EntryRouteID: routeID,
		HopCount: 1, Samples: 4, P50Ms: 100, P95Ms: 200,
		FirstSeenAt: now.Add(-time.Minute), LastSeenAt: now,
	}, []store.JourneyHop{{Position: 0, RouteID: routeID, Status: "ok", P95Ms: 200, Share: 1}}); err != nil {
		t.Fatal(err)
	}

	d, _ := armed(t, st, baseCfg([]Rule{{Type: "span_unreported", Channels: []string{"test"}, Projects: []string{"backend"}, WindowMinutes: 60}}))
	sent := d.Tick(now)
	if len(sent) != 1 || sent[0].TargetKey != "backend|gap-key" {
		t.Fatalf("span_unreported trigger = %+v, want one event for the gap path", sent)
	}
	if !strings.Contains(sent[0].Body, "1 of 3") {
		t.Fatalf("body must report the gap as a fraction: %q", sent[0].Body)
	}
}

// seedRelease writes n sessions for one release, crashed of them abnormal, with a
// started_at that orders the releases oldest first.
func seedRelease(t *testing.T, st *store.Store, release string, n, crashed int, startedAt time.Time) {
	t.Helper()
	for i := 0; i < n; i++ {
		status := "ok"
		if i < crashed {
			status = "crashed"
		}
		if err := st.UpsertSession(store.Session{
			ProjectKey: "front", Release: release, SessionID: fmt.Sprintf("%s-%d", release, i),
			Status: status, StartedAt: startedAt, DurationMs: 1000,
		}); err != nil {
			t.Fatalf("upsert session: %v", err)
		}
	}
}

func TestTriggerReleaseRegressionOnCrashFreeDrop(t *testing.T) {
	st := testStore(t)
	now := time.Now()
	for i, release := range []string{"v1", "v2", "v3"} {
		seedRelease(t, st, release, 10, 0, now.Add(-time.Duration(4-i)*time.Hour))
	}
	// v4 crashes half of its sessions: a 50-point drop against a 100% baseline.
	seedRelease(t, st, "v4", 10, 5, now)

	d, _ := armed(t, st, baseCfg([]Rule{{Type: "release_regression", Channels: []string{"test"},
		Projects: []string{"front"}, Points: 3, DropPoints: 5}}))
	sent := d.Tick(now)
	if len(sent) != 1 || sent[0].TargetKey != "front|v4" {
		t.Fatalf("release_regression trigger = %+v, want one event for front|v4", sent)
	}
	if !strings.Contains(sent[0].Body, "50.0%") || !strings.Contains(sent[0].Body, "100.0%") {
		t.Fatalf("body must name both the rate and the baseline: %q", sent[0].Body)
	}

	// A smaller drop than the configured threshold stays silent.
	d2, _ := armed(t, st, baseCfg([]Rule{{Type: "release_regression", Channels: []string{"test"},
		Projects: []string{"front"}, Points: 3, DropPoints: 60}}))
	if sent := d2.Tick(now); len(sent) != 0 {
		t.Fatalf("a drop under the threshold must not fire: %+v", sent)
	}
}
