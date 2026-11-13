package store

import (
	"testing"
	"time"
)

func recordGrouped(t *testing.T, st *Store, project, fingerprint, source, message string) int64 {
	t.Helper()
	id, err := st.RecordGroupedError(GroupedError{
		ProjectKey:     project,
		ExceptionClass: "E",
		Message:        message,
		Fingerprint:    fingerprint,
		GroupSource:    source,
		Environment:    "production",
		Level:          "error",
		Payload:        map[string]any{"message": message},
	})
	if err != nil {
		t.Fatalf("record grouped %q: %v", fingerprint, err)
	}
	return id
}

func occurrenceCount(t *testing.T, st *Store, issueID int64) int64 {
	t.Helper()
	var n int64
	if err := st.rdb.QueryRow(`SELECT COUNT(*) FROM occurrences WHERE issue_id = ?`, issueID).Scan(&n); err != nil {
		t.Fatalf("count occurrences: %v", err)
	}
	return n
}

func listedIDs(t *testing.T, st *Store, q IssueQuery) []int64 {
	t.Helper()
	rows, err := st.Issues(q)
	if err != nil {
		t.Fatalf("issues: %v", err)
	}
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

// A fingerprint rule is the operator's override: the first rule that matches
// wins, and {{ default }} is the only place a producer key can reach the stored
// key.
func TestMatchFingerprintRuleWinsAndSubstitutesDefault(t *testing.T) {
	cfg := GroupingConfig{FingerprintRules: []FingerprintRule{
		{Match: "error.type", Pattern: "^Validation", Fingerprint: "validation"},
		{Match: "error.message", Pattern: "locked$", Fingerprint: "{{ default }}-locked"},
	}}
	validation := map[string]any{"error": map[string]any{"type": "ValidationException", "message": "chapter 4 invalid"}}
	if key, ok := cfg.MatchFingerprintRule(validation, "producer-key"); !ok || key != "validation" {
		t.Fatalf("rule key = %q/%v, want validation/true", key, ok)
	}

	locked := map[string]any{"error": map[string]any{"type": "QueryException", "message": "chapter locked"}}
	if key, ok := cfg.MatchFingerprintRule(locked, "producer-key"); !ok || key != "producer-key-locked" {
		t.Fatalf("default substitution = %q/%v, want producer-key-locked/true", key, ok)
	}

	// A field the event does not carry never matches, so a rule about one
	// producer field cannot quietly fire on every event.
	missing := map[string]any{"error": map[string]any{"type": "OtherException", "message": "nope"}}
	if key, ok := cfg.MatchFingerprintRule(missing, "producer-key"); ok {
		t.Fatalf("unmatched event produced key %q", key)
	}

	// A malformed pattern is skipped, not fatal, and does not shadow the rule
	// behind it.
	broken := GroupingConfig{FingerprintRules: []FingerprintRule{
		{Match: "error.type", Pattern: "(", Fingerprint: "broken"},
		{Match: "error.type", Pattern: "Other", Fingerprint: "still-works"},
	}}
	if key, ok := broken.MatchFingerprintRule(missing, ""); !ok || key != "still-works" {
		t.Fatalf("rule after a malformed one = %q/%v, want still-works/true", key, ok)
	}
}

// Merging hides the pair from the list and hands the history to the primary;
// unmerging puts both rows back on the list.
func TestMergeIssueMovesOccurrencesAndCollapsesThePair(t *testing.T) {
	st := newTestStore(t)
	st.MaxOccPerMin.Store(0)
	primary := recordGrouped(t, st, "app", "fp-primary", GroupSourceServer, "first defect")
	duplicate := recordGrouped(t, st, "app", "fp-duplicate", GroupSourceServer, "same defect, other key")
	recordGrouped(t, st, "app", "fp-duplicate", GroupSourceServer, "same defect, other key")

	if got := len(listedIDs(t, st, IssueQuery{Project: "app", Limit: 10})); got != 2 {
		t.Fatalf("before merge the list holds %d issues, want 2", got)
	}
	if err := st.MergeIssue(primary, duplicate); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if ids := listedIDs(t, st, IssueQuery{Project: "app", Limit: 10}); len(ids) != 1 || ids[0] != primary {
		t.Fatalf("after merge the list = %v, want only the primary %d", ids, primary)
	}
	if n := occurrenceCount(t, st, duplicate); n != 0 {
		t.Errorf("duplicate still owns %d occurrences, want 0", n)
	}
	if n := occurrenceCount(t, st, primary); n != 3 {
		t.Errorf("primary owns %d occurrences, want 3", n)
	}
	merged, err := st.Issue(primary)
	if err != nil || merged.Count != 3 {
		t.Fatalf("primary count = %v err=%v, want 3", merged, err)
	}

	// New history for the duplicate's key keeps landing on the primary, the
	// only row the list shows.
	folded := recordGrouped(t, st, "app", "fp-duplicate", GroupSourceServer, "same defect, other key")
	if folded != primary {
		t.Fatalf("a merged duplicate recorded as %d, want the primary %d", folded, primary)
	}

	if err := st.UnmergeIssue(duplicate); err != nil {
		t.Fatalf("unmerge: %v", err)
	}
	ids := listedIDs(t, st, IssueQuery{Project: "app", Limit: 10})
	if len(ids) != 2 {
		t.Fatalf("after unmerge the list = %v, want both issues", ids)
	}
	if state, err := st.IssueGrouping(duplicate); err != nil || state.MergedInto != 0 {
		t.Fatalf("duplicate still merged: %+v err=%v", state, err)
	}
	if err := st.UnmergeIssue(duplicate); err == nil {
		t.Error("unmerging an unmerged issue must be refused, not silently accepted")
	}
}

// A merge across projects would move one tenant's history into another's.
func TestMergeIssueRefusesForeignAndSelfMerge(t *testing.T) {
	st := newTestStore(t)
	primary := recordGrouped(t, st, "alpha", "fp-a", GroupSourceServer, "a")
	foreign := recordGrouped(t, st, "beta", "fp-b", GroupSourceServer, "b")
	if err := st.MergeIssue(primary, foreign); err == nil {
		t.Error("cross-project merge was accepted")
	}
	if err := st.MergeIssue(primary, primary); err == nil {
		t.Error("self merge was accepted")
	}
}

// An ignore-until stamp hides the issue from the default list while its count
// keeps growing, and lapses on its own.
func TestIssueIgnoreUntilHidesFromDefaultList(t *testing.T) {
	st := newTestStore(t)
	id := recordGrouped(t, st, "app", "fp-ignored", GroupSourceServer, "noisy defect")

	if err := st.SetIssueIgnoredUntil(id, time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatalf("ignore: %v", err)
	}
	if ids := listedIDs(t, st, IssueQuery{Project: "app", Limit: 10}); len(ids) != 0 {
		t.Fatalf("ignored issue still listed by default: %v", ids)
	}
	if ids := listedIDs(t, st, IssueQuery{Project: "app", Limit: 10, IncludeIgnored: true}); len(ids) != 1 {
		t.Fatalf("explicit include listed %v, want the ignored issue", ids)
	}

	// The mute is a read filter: ingest keeps counting underneath it.
	recordGrouped(t, st, "app", "fp-ignored", GroupSourceServer, "noisy defect")
	if issue, err := st.Issue(id); err != nil || issue.Count != 2 {
		t.Fatalf("ignored issue count = %v err=%v, want 2", issue, err)
	}

	if err := st.SetIssueIgnoredUntil(id, time.Now().Add(-time.Minute).Unix()); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if ids := listedIDs(t, st, IssueQuery{Project: "app", Limit: 10}); len(ids) != 1 {
		t.Fatalf("a lapsed ignore still hides the issue: %v", ids)
	}
}

func TestSetIssueAssigneeStoresAndClears(t *testing.T) {
	st := newTestStore(t)
	id := recordGrouped(t, st, "app", "fp-owned", GroupSourceServer, "defect")
	if err := st.SetIssueAssignee(id, 7); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if state, err := st.IssueGrouping(id); err != nil || state.AssigneeUserID != 7 {
		t.Fatalf("assignee = %+v err=%v, want 7", state, err)
	}
	if err := st.SetIssueAssignee(id, 0); err != nil {
		t.Fatalf("unassign: %v", err)
	}
	if state, err := st.IssueGrouping(id); err != nil || state.AssigneeUserID != 0 {
		t.Fatalf("assignee = %+v err=%v, want cleared", state, err)
	}
}

// The stored policy is what ingest reads back: a project that never stored one
// gets the documented default, and a stored one round-trips.
func TestGroupingConfigRoundTrip(t *testing.T) {
	st := newTestStore(t)
	def, err := st.GroupingConfig("app")
	if err != nil {
		t.Fatalf("default: %v", err)
	}
	if len(def.StackTraceRules.Ignore) == 0 || def.StackTraceRules.GroupBy != "app_frames" {
		t.Fatalf("default config = %+v, want app_frames with ignore globs", def.StackTraceRules)
	}

	cfg := GroupingConfig{
		FingerprintRules: []FingerprintRule{{Match: "error.type", Pattern: "Validation", Fingerprint: "validation"}},
		StackTraceRules:  StackTraceRule{GroupBy: "app_frames", Ignore: []string{"vendor/"}},
	}
	if err := st.SetGroupingConfig("app", cfg); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := st.SetGroupingConfig("app", GroupingConfig{FingerprintRules: []FingerprintRule{{Match: "error.type", Pattern: "(", Fingerprint: "x"}}}); err == nil {
		t.Error("a malformed rule pattern was accepted")
	}
	got, err := st.GroupingConfig("app")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(got.FingerprintRules) != 1 || got.FingerprintRules[0].Fingerprint != "validation" {
		t.Fatalf("rules = %+v, want the stored validation rule", got.FingerprintRules)
	}
	if got.StackTraceRules.GroupBy != "app_frames" || len(got.StackTraceRules.Ignore) != 1 {
		t.Fatalf("stack rules = %+v, want the stored app_frames policy", got.StackTraceRules)
	}
}
