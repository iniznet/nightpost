package store

import (
	"fmt"
	"testing"
	"time"
)

func TestHourBucketsAlignsTrailingWindow(t *testing.T) {
	st := newTestStore(t)

	fp := ForException("E", "boom <n>", "A:b")
	id, err := st.RecordError("backend", "E", "boom 1", fp, "prod", "error", nil)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	// One occurrence exactly now, one 3 hours ago.
	now := time.Now().Unix()
	if _, err := st.wdb.Exec(`UPDATE occurrences SET seen_at = ?`, now); err != nil {
		t.Fatalf("seed: %v", err)
	}
	past := now - 3*3600 - 60
	if _, err := st.wdb.Exec(`INSERT INTO occurrences (issue_id, type, payload, seen_at) VALUES (?, 'error', NULL, ?)`, id, past); err != nil {
		t.Fatalf("seed past: %v", err)
	}

	buckets, err := st.HourBuckets("backend", 24, "error")
	if err != nil {
		t.Fatalf("buckets: %v", err)
	}
	if len(buckets) != 24 {
		t.Fatalf("expected 24 buckets, got %d", len(buckets))
	}
	var total int64
	for _, v := range buckets {
		total += v
	}
	if total != 2 {
		t.Fatalf("expected total 2, got %d", total)
	}
	if buckets[len(buckets)-1] < 1 {
		t.Errorf("expected current-hour bucket >= 1, got %d", buckets[len(buckets)-1])
	}
}

func TestRequestStatsPercentiles(t *testing.T) {
	st := newTestStore(t)

	names := []string{"GET /a", "GET /a", "GET /a", "GET /b"}
	for i, n := range names {
		_, err := st.RecordProfile(ProfileInput{
			ProjectKey: "backend", Type: ProfileTypeRequest, Name: n,
			DurationMs: float64((i + 1) * 100),
		})
		if err != nil {
			t.Fatalf("record profile: %v", err)
		}
	}
	count, p50, p95, routes, err := st.RequestStats("backend", 24)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if count != 4 {
		t.Fatalf("expected 4, got %d", count)
	}
	if p50 <= 0 || p95 < p50 {
		t.Fatalf("expected sane percentiles, got p50=%v p95=%v", p50, p95)
	}
	if len(routes) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(routes))
	}
	if routes[0].Name != "GET /b" {
		t.Errorf("expected slowest route first (GET /b avg 400), got %s avg %v", routes[0].Name, routes[0].AvgMs)
	}
}

func TestIssueSparklinesGroupsPerIssue(t *testing.T) {
	st := newTestStore(t)

	idA, _ := st.RecordError("backend", "A", "a 1", ForException("A", "a 1", "x:y"), "prod", "error", nil)
	idB, _ := st.RecordError("backend", "B", "b 1", ForException("B", "b 1", "x:y"), "prod", "error", nil)

	spark, err := st.IssueSparklines("backend", 24)
	if err != nil {
		t.Fatalf("sparklines: %v", err)
	}
	sa, ok := spark[idA]
	if !ok {
		t.Fatalf("issue %d missing from sparklines", idA)
	}
	if len(sa) != 24 {
		t.Fatalf("expected 24 buckets, got %d", len(sa))
	}
	var sumA int64
	for _, v := range sa {
		sumA += v
	}
	if sumA != 1 {
		t.Errorf("expected issue A total 1, got %d", sumA)
	}
	if _, ok := spark[idB]; !ok {
		t.Errorf("issue B missing from sparklines")
	}
}

func TestProjectsAndEnvironments(t *testing.T) {
	st := newTestStore(t)

	st.RecordError("backend", "E", "m1", ForException("E", "m1", "a"), "production", "error", nil)
	st.RecordLog("front", "app", "info", "hello", nil, "production")
	st.RecordProfile(ProfileInput{ProjectKey: "admin", Type: ProfileTypeRequest, Name: "GET /", DurationMs: 10})

	projects, err := st.Projects()
	if err != nil {
		t.Fatalf("projects: %v", err)
	}
	want := fmt.Sprint([]string{"admin", "backend", "front"})
	if got := fmt.Sprint(projects); got != want {
		t.Errorf("projects = %v, want %v", got, want)
	}

	envs, err := st.Environments("backend")
	if err != nil {
		t.Fatalf("envs: %v", err)
	}
	if len(envs) != 1 || envs[0] != "production" {
		t.Errorf("envs = %v, want [production]", envs)
	}
}

func TestLogLevelsAndChannelColumn(t *testing.T) {
	st := newTestStore(t)

	st.RecordLog("backend", "auth", "warning", "login failed", nil, "production")
	st.RecordLog("backend", "app", "info", "boot", nil, "production")
	st.RecordLog("backend", "app", "info", "boot again", nil, "")

	levels, err := st.LogLevels("backend")
	if err != nil {
		t.Fatalf("levels: %v", err)
	}
	if len(levels) != 2 {
		t.Fatalf("expected 2 levels, got %d", len(levels))
	}
	if levels[0].Level != "info" || levels[0].Count != 2 {
		t.Errorf("expected info=2 first, got %+v", levels[0])
	}

	warn, err := st.Issues(IssueQuery{Project: "backend", Type: "log", Level: "warning", Channel: "auth"})
	if err != nil {
		t.Fatalf("filtered issues: %v", err)
	}
	if len(warn) != 1 || warn[0].Channel != "auth" {
		t.Fatalf("channel filter failed: %+v", warn)
	}
}
