package store

import (
	"math"
	"testing"
	"time"
)

// TestRequestP95DeltaWindows pins the triage figure's window semantics: the
// trailing window's p95 beside the preceding equal-length window's, both
// time-bounded (the 70100-wedge guard).
func TestRequestP95DeltaWindows(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	seed := func(ms float64, age time.Duration) {
		t.Helper()
		if _, err := st.wdb.Exec(`INSERT INTO profiles
			(project_key, type, name, duration_ms, environment, trace_id, spans, hotspots, tree, created_at)
			VALUES ('backend','request','GET /x',?,'production','','[]','[]','[]',?)`,
			ms, now.Add(-age).Unix()); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	for i := 0; i < 20; i++ {
		seed(100, 30*time.Hour) // previous window (24h..48h ago)
	}
	for i := 0; i < 20; i++ {
		seed(200, time.Hour) // current window
	}
	d, err := st.RequestP95Delta("backend", 24)
	if err != nil {
		t.Fatalf("delta: %v", err)
	}
	if d.Prev != 100 || d.Now != 200 {
		t.Fatalf("delta = %+v, want prev=100 now=200", d)
	}
	if got := d.DeltaPct(); math.Abs(got-100) > 0.01 {
		t.Errorf("DeltaPct() = %v, want 100", got)
	}
	if got := (LatencyDelta{Now: 7}).DeltaPct(); got != 0 {
		t.Errorf("no-baseline DeltaPct() = %v, want 0", got)
	}
}

// TestTracesIndexFoldsLegsAndBoundsWindow covers the /traces index aggregate:
// legs fold by trace id, the has-tree flag is read from the stored tree, the
// root is the earliest leg, and rows outside the window are excluded.
func TestTracesIndexFoldsLegsAndBoundsWindow(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.RecordProfile(ProfileInput{ProjectKey: "front", Type: ProfileTypeRequest, Name: "GET /chapters", DurationMs: 300, TraceID: "t1"}); err != nil {
		t.Fatalf("leg1: %v", err)
	}
	if _, err := st.RecordProfile(ProfileInput{ProjectKey: "backend", Type: ProfileTypeCallgraph, Name: "GET /api/chapters", DurationMs: 250, TraceID: "t1", Tree: []TreeNode{{N: "main()", S: 20, I: 250}}}); err != nil {
		t.Fatalf("leg2: %v", err)
	}
	if _, err := st.wdb.Exec(`INSERT INTO profiles
		(project_key, type, name, duration_ms, environment, trace_id, spans, hotspots, tree, created_at)
		VALUES ('front','request','GET /old',10,'production','t0','[]','[]','[]',?)`,
		time.Now().Add(-72*time.Hour).Unix()); err != nil {
		t.Fatalf("old: %v", err)
	}

	rows, err := st.Traces("", 24, 50)
	if err != nil {
		t.Fatalf("traces: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d traces, want 1 (window bound)", len(rows))
	}
	got := rows[0]
	if got.TraceID != "t1" || got.Legs != 2 || !got.HasTree {
		t.Errorf("trace row = %+v", got)
	}
	if got.RootName != "GET /chapters" {
		t.Errorf("root = %q, want the earliest leg", got.RootName)
	}
	if got.Projects != "backend,front" && got.Projects != "front,backend" {
		t.Errorf("projects = %q", got.Projects)
	}
	if got.TotalMs != 550 {
		t.Errorf("total = %v, want 550", got.TotalMs)
	}
}

// TestRouteAggCarriesP50 pins the Endpoints tab's p50 column.
func TestRouteAggCarriesP50(t *testing.T) {
	st := newTestStore(t)
	for i := 1; i <= 100; i++ {
		if _, err := st.RecordProfile(ProfileInput{ProjectKey: "backend", Type: ProfileTypeRequest, Name: "GET /x", DurationMs: float64(i)}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	rows, err := st.ProfileRouteStats("backend", ProfileTypeRequest, 24, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("route stats: %v rows=%d", err, len(rows))
	}
	if rows[0].P50Ms != 51 || rows[0].P95Ms != 96 {
		t.Errorf("percentiles = p50 %v p95 %v, want 51 / 96", rows[0].P50Ms, rows[0].P95Ms)
	}
}

func TestOpenIssuesSince(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.RecordError("backend", "E", "old", "fp-old", "production", "error", nil); err != nil {
		t.Fatalf("old: %v", err)
	}
	if _, err := st.RecordError("backend", "E", "new", "fp-new", "production", "error", nil); err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := st.wdb.Exec(`UPDATE issues SET first_seen_at = ? WHERE fingerprint = 'fp-old'`, time.Now().Add(-48*time.Hour).Unix()); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	n, err := st.OpenIssuesSince("backend", time.Now().Add(-24*time.Hour).Unix())
	if err != nil {
		t.Fatalf("since: %v", err)
	}
	if n != 1 {
		t.Errorf("OpenIssuesSince = %d, want 1", n)
	}
	if all, err := st.OpenIssuesSince("backend", 0); err != nil || all != 2 {
		t.Errorf("OpenIssuesSince(0) = %d (%v), want 2", all, err)
	}
}
