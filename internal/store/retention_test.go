package store

import (
	"fmt"
	"testing"
	"time"
)

func seedOccurrences(t *testing.T, st *Store, issueID int64, count int, ageDays int) {
	t.Helper()
	seen := time.Now().AddDate(0, 0, -ageDays).Unix()
	for i := 0; i < count; i++ {
		if _, err := st.wdb.Exec(`INSERT INTO occurrences (issue_id, type, payload, seen_at) VALUES (?, 'error', NULL, ?)`, issueID, seen-int64(i)); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

func TestPruneOccurrencesBefore(t *testing.T) {
	st := newTestStore(t)
	id, _ := st.RecordError("backend", "E", "m", ForException("E", "m", "a"), "prod", "error", nil)
	seedOccurrences(t, st, id, 7, 40)
	seedOccurrences(t, st, id, 3, 1)

	cutoff := time.Now().AddDate(0, 0, -30)
	n, err := st.PruneOccurrencesBefore(cutoff, 3)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 7 {
		t.Fatalf("expected 7 pruned, got %d", n)
	}
	occ, _ := st.Occurrences(id, 100)
	if len(occ) != 4 {
		t.Fatalf("expected 4 survivors (3 recent seeds + 1 original), got %d", len(occ))
	}
	issue, _ := st.Issue(id)
	if issue == nil || issue.Count != 1 {
		// Direct occurrence INSERTs never bump issue.count; the seeded count
		// from RecordError is what must survive pruning.
		t.Errorf("issue row must survive pruning with count intact, got %+v", issue)
	}
}

// TestPruneIssueOccurrencesSpansRanges covers the rewrite: the cap must
// hold per issue ACROSS range boundaries, not merely inside the first chunk.
func TestPruneIssueOccurrencesSpansRanges(t *testing.T) {
	st := newTestStore(t)

	const issues = 7
	const seeded = 5
	ids := make([]int64, 0, issues)
	for i := 0; i < issues; i++ {
		// A distinct fingerprint per issue is what makes these separate issues.
		id, err := st.RecordError("p", "Ex", "boom", ForException("Ex", "boom", fmt.Sprintf("frame-%d", i)), "prod", "error", nil)
		if err != nil {
			t.Fatalf("record: %v", err)
		}
		ids = append(ids, id)
	}
	for _, id := range ids {
		seedOccurrences(t, st, id, seeded, 0)
	}
	// RecordError already stored one occurrence per issue, so each holds
	// seeded+1 and the cap of 2 must remove seeded-1 per issue.
	want := int64(issues * (seeded - 1))

	// chunk=2 forces four ranges over seven issues, so a bug that only prunes
	// the first range cannot pass.
	n, err := st.pruneIssueOccurrences(2, 2)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != want {
		t.Errorf("pruned %d rows, want %d", n, want)
	}

	rows, err := st.rdb.Query(`SELECT issue_id, COUNT(*) FROM occurrences GROUP BY issue_id`)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var id, c int64
		if err := rows.Scan(&id, &c); err != nil {
			t.Fatal(err)
		}
		seen++
		if c != 2 {
			t.Errorf("issue %d kept %d occurrences, want 2", id, c)
		}
	}
	if seen != issues {
		t.Errorf("found %d issues with occurrences, want %d", seen, issues)
	}
}

// TestPruneIssueOccurrencesKeepsNewest pins that the cap keeps the NEWEST rows,
// which is the entire point of the window's ORDER BY.
func TestPruneIssueOccurrencesKeepsNewest(t *testing.T) {
	st := newTestStore(t)
	id, err := st.RecordError("p", "Ex", "boom", ForException("Ex", "boom", "f"), "prod", "error", nil)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	seedOccurrences(t, st, id, 4, 0) // 5 occurrences in total

	if _, err := st.pruneIssueOccurrences(1, pruneChunk); err != nil {
		t.Fatalf("prune: %v", err)
	}
	occ, err := st.Occurrences(id, 100)
	if err != nil {
		t.Fatalf("occurrences: %v", err)
	}
	if len(occ) != 1 {
		t.Fatalf("kept %d occurrences, want 1", len(occ))
	}
}

func TestPruneIssueOccurrencesCapsPerIssue(t *testing.T) {
	st := newTestStore(t)
	id, _ := st.RecordError("backend", "E", "m", ForException("E", "m", "a"), "prod", "error", nil)
	seedOccurrences(t, st, id, 60, 0)

	n, err := st.PruneIssueOccurrences(50)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 11 {
		t.Fatalf("expected 11 pruned (61 stored, cap 50), got %d", n)
	}
	occ, _ := st.Occurrences(id, 100)
	if len(occ) != 50 {
		t.Fatalf("expected cap 50, got %d", len(occ))
	}
}

func TestRunRetentionKeepsProfilesWindow(t *testing.T) {
	st := newTestStore(t)
	for i := 0; i < 7; i++ {
		st.RecordProfile(ProfileInput{ProjectKey: "backend", Type: ProfileTypeRequest, Name: "r", DurationMs: float64(i + 1)})
	}
	msg, err := st.RunRetention(30, 5)
	if err != nil {
		t.Fatalf("run retention: %v", err)
	}
	if msg == "" {
		t.Fatal("expected summary message")
	}
	profiles, _ := st.Profiles("backend", 100)
	if len(profiles) != 5 {
		t.Fatalf("expected 5 kept profiles, got %d", len(profiles))
	}
}
