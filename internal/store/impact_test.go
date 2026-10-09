package store

import (
	"fmt"
	"strings"
	"testing"
)

func TestUserImpactExtractionAndCounts(t *testing.T) {
	st := newTestStore(t)
	id1, err := st.RecordError("front", "TypeError", "bad", "fp-imp", "production", "error",
		map[string]any{"user_id": float64(42)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordError("front", "TypeError", "bad", "fp-imp", "production", "error",
		map[string]any{"user": map[string]any{"id": float64(7)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordError("front", "TypeError", "bad", "fp-imp", "production", "error",
		map[string]any{"user_id": strings.Repeat("x", 65)}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordError("front", "TypeError", "bad", "fp-imp", "production", "error", nil); err != nil {
		t.Fatal(err)
	}
	counts, err := st.UserCounts([]int64{id1})
	if err != nil {
		t.Fatal(err)
	}
	if counts[id1] != 2 {
		t.Fatalf("42 + 7 are two distinct users, oversized id must be dropped, got %d", counts[id1])
	}
	// second issue with no ids at all is simply absent from the map
	id2, _ := st.RecordError("front", "TypeError", "other", "fp-imp2", "production", "error", map[string]any{})
	counts, _ = st.UserCounts([]int64{id1, id2})
	if _, bad := counts[id2]; bad {
		t.Fatal("issue without user ids must not report counts")
	}
	if extractUserID("not a map") != "" || extractUserID(map[string]any{"user_id": float64(-3)}) != "" {
		t.Fatal("non-scalar and non-positive ids must extract to empty")
	}
}

func TestRecordJobBatchCount(t *testing.T) {
	st := newTestStore(t)
	if err := st.RecordJob("backend", "command", "seals:expire", "", 2, false, 5); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordJob("backend", "command", "seals:expire", "", 1, true, 10); err != nil {
		t.Fatal(err)
	}
	rows, err := st.JobLeaderboard("", 24, 50)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	j := rows[0]
	if j.Runs != 15 || j.Failures != 5 || j.Failures24h != 5 {
		t.Fatalf("runs=%d failures=%d fail24=%d", j.Runs, j.Failures, j.Failures24h)
	}
	wantAvg := (2*5 + 1*10) / 15.0
	if fmt.Sprintf("%.3f", j.AvgMs/1000) != fmt.Sprintf("%.3f", wantAvg) {
		t.Fatalf("avg=%f want %f", j.AvgMs/1000, wantAvg)
	}
	if j.LastStatus != "ok" {
		t.Fatalf("batch flush of the newest group wins, got %q", j.LastStatus)
	}
}
