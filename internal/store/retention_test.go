package store

import (
	"time"
	"testing"
)

func seedOccurrences(t *testing.T, st *Store, issueID int64, count int, ageDays int) {
	t.Helper()
	seen := time.Now().AddDate(0, 0, -ageDays).Unix()
	for i := 0; i < count; i++ {
		if _, err := st.db.Exec(`INSERT INTO occurrences (issue_id, type, payload, seen_at) VALUES (?, 'error', NULL, ?)`, issueID, seen-int64(i)); err != nil {
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