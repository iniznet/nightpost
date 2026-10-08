package store

import (
	"testing"
	"time"
)

func TestDashboardSummariesCoversEveryProjectInOnePass(t *testing.T) {
	st := newTestStore(t)

	fp := ForException("E", "boom", "A:b")
	if _, err := st.RecordError("backend", "E", "boom 1", fp, "prod", "error", nil); err != nil {
		t.Fatalf("record error: %v", err)
	}
	if _, err := st.RecordLog("front", "worker", "error", "queue stuck", nil, "prod"); err != nil {
		t.Fatalf("record log: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := st.RecordProfile(ProfileInput{
			ProjectKey: "backend", Type: ProfileTypeRequest, Name: "GET /a",
			DurationMs: float64(100 * (i + 1)),
		}); err != nil {
			t.Fatalf("record profile: %v", err)
		}
	}

	rows, err := st.DashboardSummaries(24)
	if err != nil {
		t.Fatalf("summaries: %v", err)
	}
	byProject := map[string]DashboardRow{}
	for _, r := range rows {
		byProject[r.Summary.Project] = r
	}
	b, ok := byProject["backend"]
	if !ok {
		t.Fatalf("backend row missing; projects: %v", rows)
	}
	if b.Summary.Errors24 != 1 {
		t.Errorf("backend errors24 = %d, want 1", b.Summary.Errors24)
	}
	if b.Summary.Requests24 != 3 {
		t.Errorf("backend requests24 = %d, want 3", b.Summary.Requests24)
	}
	if len(b.Trend) != 24 || b.Trend[23] < 1 {
		t.Errorf("backend trend = %v, want current-hour bucket >= 1", b.Trend)
	}
	f, ok := byProject["front"]
	if !ok {
		t.Fatalf("front row missing")
	}
	if f.Summary.Logs24 != 1 {
		t.Errorf("front logs24 = %d, want 1", f.Summary.Logs24)
	}
	if f.Summary.Requests24 != 0 {
		t.Errorf("front requests24 = %d, want 0", f.Summary.Requests24)
	}
	if len(f.Trend) != 24 {
		t.Errorf("front trend length = %d, want 24", len(f.Trend))
	}
}

func TestProfileRouteStatsAggregatesAndPainSorts(t *testing.T) {
	st := newTestStore(t)

	for _, ms := range []float64{300, 100, 200} {
		if _, err := st.RecordProfile(ProfileInput{
			ProjectKey: "backend", Type: ProfileTypeRequest, Name: "GET /a", DurationMs: ms,
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	if _, err := st.RecordProfile(ProfileInput{
		ProjectKey: "backend", Type: ProfileTypeRequest, Name: "GET /b", DurationMs: 1000,
	}); err != nil {
		t.Fatalf("record: %v", err)
	}

	routes, err := st.ProfileRouteStats("backend", ProfileTypeRequest, 24, 50)
	if err != nil {
		t.Fatalf("routes: %v", err)
	}
	if len(routes) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(routes))
	}
	b := routes[0]
	if b.Name != "GET /b" || b.TotalMs != 1000 || b.Count != 1 || b.P95Ms != 1000 {
		t.Errorf("pain order: first = %+v, want GET /b (total 1000 > 600)", b)
	}
	a := routes[1]
	if a.Name != "GET /a" {
		t.Fatalf("second route = %s, want GET /a", a.Name)
	}
	if a.Count != 3 || a.TotalMs != 600 || a.MaxMs != 300 || a.AvgMs != 200 {
		t.Errorf("GET /a agg = %+v", a)
	}
	if a.P95Ms != 300 {
		t.Errorf("GET /a p95 = %v, want 300", a.P95Ms)
	}
	if a.LastSeen.Before(time.Now().Add(-time.Minute)) {
		t.Errorf("GET /a last seen = %v, want now", a.LastSeen)
	}
}

func TestIssuesPainSortAndCursor(t *testing.T) {
	st := newTestStore(t)

	for i := 0; i < 3; i++ {
		if _, err := st.RecordLog("backend", "worker", "error", "not found: /robots.txt", nil, "prod"); err != nil {
			t.Fatalf("record log: %v", err)
		}
	}
	if _, err := st.RecordError("backend", "E", "real failure", ForException("E", "boom", "A:b"), "prod", "error", nil); err != nil {
		t.Fatalf("record error: %v", err)
	}

	page1, err := st.Issues(IssueQuery{Project: "backend", Status: "all", Sort: "pain", Limit: 1})
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1) != 1 {
		t.Fatalf("expected 1 row, got %d", len(page1))
	}
	last := page1[0]
	page2, err := st.Issues(IssueQuery{
		Project: "backend", Status: "all", Sort: "pain", Limit: 10,
		BeforeCount: last.Count, BeforeID: last.ID,
	})
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2) != 1 {
		t.Fatalf("expected 1 remaining row, got %d", len(page2))
	}
	for _, i := range page2 {
		if i.Count > last.Count || (i.Count == last.Count && i.ID > last.ID) {
			t.Errorf("cursor violated: page2 row count=%d id=%d after count=%d id=%d", i.Count, i.ID, last.Count, last.ID)
		}
	}
}
