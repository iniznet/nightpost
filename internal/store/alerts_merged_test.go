package store

import (
	"testing"
	"time"
)

// TestAlertReadsCollapseMergedDuplicates is the regression test for the I3
// defect: a merged duplicate is not a defect of its own, so no rule may fire on
// one. Before the fix the alert reads returned a row the issue list refuses to
// show, and an operator got an alert for an issue they could not open.
//
// Each of the three reads gets its own arming, because they match on different
// predicates: a merged duplicate that is merely OPEN guards the new-issue read, a
// fresh occurrence on it guards the count-window read, and one that is armed to
// reopen guards the regression read.
func TestAlertReadsCollapseMergedDuplicates(t *testing.T) {
	st := newTestStore(t)
	st.MaxOccPerMin.Store(0)
	now := time.Now()
	since := now.Add(-time.Hour).Unix()

	primary, err := st.RecordError("front", "TypeError", "x is undefined", "fp-primary", "production", "error", nil)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := st.RecordError("front", "TypeError", "y is undefined", "fp-duplicate", "production", "error", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MergeIssue(primary, duplicate); err != nil {
		t.Fatal(err)
	}

	// The merged duplicate is still open, so only the fold keeps it out of the
	// new-issue read.
	if events, err := st.NewIssueEvents(since, []string{"front"}); err != nil {
		t.Fatalf("new issue events: %v", err)
	} else if len(events) != 1 || events[0].ID != primary {
		t.Fatalf("new-issue read = %+v, want only the primary %d", events, primary)
	}

	// A later occurrence against the duplicate's own fingerprint lands on its row
	// (plain ingest does not fold), which is what makes it countable again.
	if _, err := st.RecordError("front", "TypeError", "y is undefined", "fp-duplicate", "production", "error", nil); err != nil {
		t.Fatal(err)
	}
	if breaches, err := st.CountBreaches(since, 1, []string{"front"}); err != nil {
		t.Fatalf("count breaches: %v", err)
	} else if len(breaches) != 1 || breaches[0].Fingerprint != "fp-primary" {
		t.Fatalf("count-window read = %+v, want only the primary's fingerprint", breaches)
	}

	// Arm the duplicate for the reopen rule: resolved in the past, active again
	// since. Merging must not resurrect that verdict on the duplicate's row.
	if err := st.SetResolved(duplicate, true); err != nil {
		t.Fatal(err)
	}
	if err := st.SetResolvedAt(duplicate, now.Add(-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordError("front", "TypeError", "y is undefined", "fp-duplicate", "production", "error", nil); err != nil {
		t.Fatal(err)
	}
	if events, err := st.Regressions(nil); err != nil {
		t.Fatalf("regressions: %v", err)
	} else if len(events) != 0 {
		t.Fatalf("reopen read = %+v, want none: a merged duplicate is not a regression", events)
	}

	// The primary still fires: the fold hides the duplicate, it does not mute the
	// defect the duplicate was folded into.
	if err := st.SetResolved(primary, true); err != nil {
		t.Fatal(err)
	}
	if err := st.SetResolvedAt(primary, now.Add(-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordError("front", "TypeError", "x is undefined", "fp-primary", "production", "error", nil); err != nil {
		t.Fatal(err)
	}
	if events, err := st.Regressions(nil); err != nil {
		t.Fatalf("regressions: %v", err)
	} else if len(events) != 1 || events[0].ID != primary {
		t.Fatalf("regressions = %+v, want the primary to reopen", events)
	}
}
