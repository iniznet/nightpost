package store

import (
	"testing"
	"time"
)

func ageIssue(t *testing.T, st *Store, id int64, days int) {
	t.Helper()
	old := time.Now().AddDate(0, 0, -days).Unix()
	if _, err := st.wdb.Exec(`UPDATE issues SET last_seen_at = ? WHERE id = ?`, old, id); err != nil {
		t.Fatalf("age issue: %v", err)
	}
	if _, err := st.wdb.Exec(`UPDATE occurrences SET seen_at = ? WHERE issue_id = ?`, old, id); err != nil {
		t.Fatalf("age occurrences: %v", err)
	}
}

// TestArchiveStaleIssuesHidesAndRestores is the M6.4 archive contract: an
// archived issue leaves the live view, and a RECURRENCE brings it back. A
// one-way archive would hide a returning defect forever, which is worse than
// leaving the table unbounded.
func TestArchiveStaleIssuesHidesAndRestores(t *testing.T) {
	st := newTestStore(t)
	fp := ForException("Ex", "boom", "f")
	id, err := st.RecordError("app", "Ex", "boom", fp, "prod", "error", nil)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	ageIssue(t, st, id, 40)

	n, err := st.ArchiveStaleIssues(30)
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	if n != 1 {
		t.Fatalf("archived %d issues, want 1", n)
	}
	// Idempotent: a second pass over an already-archived row changes nothing.
	if again, err := st.ArchiveStaleIssues(30); err != nil || again != 0 {
		t.Errorf("second pass archived %d (%v), want 0", again, err)
	}

	// Hidden from the list AND from every count that feeds the same view, or the
	// dashboard would contradict itself.
	if rows, err := st.Issues(IssueQuery{Project: "app", Limit: 10}); err != nil || len(rows) != 0 {
		t.Errorf("archived issue still listed: %+v %v", rows, err)
	}
	if total, err := st.IssueTotal("app"); err != nil || total != 0 {
		t.Errorf("IssueTotal = %d (%v), want 0", total, err)
	}
	if open, err := st.OpenIssueCount("app"); err != nil || open != 0 {
		t.Errorf("OpenIssueCount = %d (%v), want 0", open, err)
	}

	// The PROJECT must stay discoverable: archiving may not drop a project from
	// /keys, or an operator loses the ability to issue it a key.
	projects, err := st.Projects()
	if err != nil {
		t.Fatalf("projects: %v", err)
	}
	found := false
	for _, p := range projects {
		if p == "app" {
			found = true
		}
	}
	if !found {
		t.Errorf("archiving removed the project from discovery: %v", projects)
	}

	// A recurrence un-archives the row.
	if _, err := st.RecordError("app", "Ex", "boom", fp, "prod", "error", nil); err != nil {
		t.Fatalf("recurrence: %v", err)
	}
	rows, err := st.Issues(IssueQuery{Project: "app", Limit: 10})
	if err != nil || len(rows) != 1 {
		t.Fatalf("recurrence did not restore the issue: %+v %v", rows, err)
	}
}

// TestArchiveStaleIssuesKeepsActiveIssues pins the other direction: recency is
// required, so a live issue is never archived no matter how old its first sight.
func TestArchiveStaleIssuesKeepsActiveIssues(t *testing.T) {
	st := newTestStore(t)
	// Recently seen (default): must survive.
	if _, err := st.RecordError("app", "Ex", "boom", ForException("Ex", "boom", "a"), "prod", "error", nil); err != nil {
		t.Fatalf("record: %v", err)
	}
	// Last seen long ago, but it still HAS a recent occurrence: must survive too.
	id, err := st.RecordError("app", "Ex2", "boom2", ForException("Ex2", "boom2", "b"), "prod", "error", nil)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if _, err := st.wdb.Exec(`UPDATE issues SET last_seen_at = ? WHERE id = ?`, time.Now().AddDate(0, 0, -90).Unix(), id); err != nil {
		t.Fatalf("age: %v", err)
	}

	n, err := st.ArchiveStaleIssues(30)
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	if n != 0 {
		t.Errorf("archived %d active issues, want 0", n)
	}
	if total, _ := st.IssueTotal("app"); total != 2 {
		t.Errorf("IssueTotal = %d, want 2", total)
	}
}
