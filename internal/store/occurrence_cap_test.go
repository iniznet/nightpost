package store

import "testing"

func TestOccurrenceCapCountAndDiscard(t *testing.T) {
	st := newTestStore(t)
	st.MaxOccPerMin = 2

	fp := ForException("LoopError", "hydration loop", "app:render")
	var id int64
	for i := 0; i < 5; i++ {
		got, err := st.RecordError("front", "LoopError", "hydration loop", fp, "production", "error", map[string]any{"i": i})
		if err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		id = got
	}

	issue, err := st.Issue(id)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if issue.Count != 5 {
		t.Errorf("issue count must track every report, got %d", issue.Count)
	}
	occs, err := st.Occurrences(id, 100)
	if err != nil {
		t.Fatalf("occurrences: %v", err)
	}
	if len(occs) != 2 {
		t.Errorf("expected 2 stored occurrences under cap, got %d", len(occs))
	}
	if got := st.DroppedOccurrences(); got != 3 {
		t.Errorf("expected 3 dropped, got %d", got)
	}

	// A different fingerprint keeps its own budget.
	other := ForException("OtherError", "unrelated defect", "app:other")
	oid, err := st.RecordError("front", "OtherError", "unrelated defect", other, "production", "error", nil)
	if err != nil {
		t.Fatalf("record other: %v", err)
	}
	occs, err = st.Occurrences(oid, 100)
	if err != nil {
		t.Fatalf("other occurrences: %v", err)
	}
	if len(occs) != 1 {
		t.Errorf("independent fingerprint budget violated: got %d occurrences", len(occs))
	}
}
