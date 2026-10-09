package store

import (
	"fmt"
	"testing"
	"time"
)

func TestRecordReleaseCapAndLatest(t *testing.T) {
	st := newTestStore(t)
	for i := 1; i <= 105; i++ {
		if err := st.RecordRelease("backend", fmt.Sprintf("v%03d", i), "production"); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := st.rdb.QueryRow(`SELECT COUNT(*) FROM releases WHERE project_key = 'backend'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 100 {
		t.Fatalf("table must be hard-capped at 100 newest, got %d", n)
	}
	mark, err := st.LatestRelease("backend", "")
	if err != nil || mark == nil || mark.Version != "v105" {
		t.Fatalf("latest=%+v err=%v", mark, err)
	}
	if err := st.RecordRelease("backend", "s001", "staging"); err != nil {
		t.Fatal(err)
	}
	mark, _ = st.LatestRelease("backend", "production")
	if mark == nil || mark.Version != "v105" {
		t.Fatalf("env filter must ignore staging: %+v", mark)
	}
	if m, err := st.LatestRelease("nosuch", ""); err != nil || m != nil {
		t.Fatalf("unknown project must be nil,nil: %v %+v", err, m)
	}
}

func TestReleasesWithHealthEstimate(t *testing.T) {
	st := newTestStore(t)
	if err := st.RecordRelease("backend", "v9", "production"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := st.RecordError("backend", "Boom", fmt.Sprintf("m%d", i), fmt.Sprintf("fp-h%d", i), "production", "error", nil); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 10; i++ {
		if _, err := st.RecordProfile(ProfileInput{ProjectKey: "backend", Name: "/x", DurationMs: 30, Environment: "production"}); err != nil {
			t.Fatal(err)
		}
	}
	rels, err := st.ReleasesWithHealth("backend", 5)
	if err != nil || len(rels) != 1 {
		t.Fatalf("rels=%d err=%v", len(rels), err)
	}
	h := rels[0]
	if h.Errors24h != 3 || h.SampledReqs24h != 10 {
		t.Fatalf("errors=%d reqs=%d", h.Errors24h, h.SampledReqs24h)
	}
	if h.CrashFree != "70.0%" {
		t.Fatalf("crash-free must be conservative estimate, got %q", h.CrashFree)
	}
}

func TestIssuesSinceFirstSeenFilter(t *testing.T) {
	st := newTestStore(t)
	oldID, err := st.RecordError("backend", "Old", "pre-deploy", "fp-old", "production", "error", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.wdb.Exec(`UPDATE issues SET first_seen_at = ? WHERE id = ?`, time.Now().Unix()-7200, oldID); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordRelease("backend", "v2", "production"); err != nil {
		t.Fatal(err)
	}
	mark, err := st.LatestRelease("backend", "production")
	if err != nil || mark == nil {
		t.Fatalf("mark=%+v err=%v", mark, err)
	}
	if _, err := st.RecordError("backend", "New", "post-deploy", "fp-new2", "production", "error", nil); err != nil {
		t.Fatal(err)
	}
	rows, err := st.Issues(IssueQuery{Status: "open", SinceFirstSeen: mark.At.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Title != "post-deploy" {
		t.Fatalf("new_since filter must keep only post-release rows, got %+v", rows)
	}
	all, _ := st.Issues(IssueQuery{Status: "open"})
	if len(all) != 2 {
		t.Fatalf("zero SinceFirstSeen must be a no-op filter, got %d", len(all))
	}
}
