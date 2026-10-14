package store

import (
	"testing"
	"time"
)

func TestAlertLedgerAndQueries(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	last, err := st.LastAlertSent("new-issue", "front|fp")
	if err != nil || last != 0 {
		t.Fatalf("empty ledger must report 0: %d %v", last, err)
	}
	if err := st.RecordAlert("ops", "new-issue", "front|fp", now.Unix()); err != nil {
		t.Fatal(err)
	}
	last, _ = st.LastAlertSent("new-issue", "front|fp")
	if last != now.Unix() {
		t.Fatalf("record/lookup mismatch %d", last)
	}
	n, err := st.ChannelAlertCount("ops", now.Add(-time.Hour).Unix())
	if err != nil || n != 1 {
		t.Fatalf("channel count %d %v", n, err)
	}
	if n, _ := st.ChannelAlertCount("other", now.Add(-time.Hour).Unix()); n != 0 {
		t.Fatal("channel isolation")
	}
	pruned, err := st.PruneAlertsBefore(now.Add(-24*time.Hour).Unix(), 100)
	if err != nil || pruned != 0 {
		t.Fatalf("young alert must survive: %d %v", pruned, err)
	}
	if _, err := st.PruneAlertsBefore(now.Add(time.Hour).Unix(), 100); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.ChannelAlertCount("ops", 0); n != 0 {
		t.Fatal("prune must clear the ledger")
	}
}

func TestNewIssueAndCountQueries(t *testing.T) {
	st := newTestStore(t)
	st.MaxOccPerMin.Store(0)
	id, err := st.RecordError("front", "TypeError", "x undefined", "fp-n", "production", "error", nil)
	if err != nil {
		t.Fatal(err)
	}
	evts, err := st.NewIssueEvents(time.Now().Add(-time.Hour).Unix(), []string{"front"})
	if err != nil || len(evts) != 1 || evts[0].ID != id {
		t.Fatalf("new-issue query: %+v %v", evts, err)
	}
	if evts, _ := st.NewIssueEvents(time.Now().Add(-time.Hour).Unix(), []string{"backend"}); len(evts) != 0 {
		t.Fatal("project scoping")
	}
	if evts, _ := st.NewIssueEvents(time.Now().Add(time.Hour).Unix(), nil); len(evts) != 0 {
		t.Fatal("future cutoff must be empty")
	}
	for i := 0; i < 5; i++ {
		if _, err := st.RecordError("front", "TypeError", "x undefined", "fp-n", "production", "error", nil); err != nil {
			t.Fatal(err)
		}
	}
	breaches, err := st.CountBreaches(time.Now().Add(-time.Hour).Unix(), 5, []string{"front"})
	if err != nil || len(breaches) != 1 || breaches[0].N < 5 {
		t.Fatalf("count breach: %+v %v", breaches, err)
	}
	if b, _ := st.CountBreaches(time.Now().Add(-time.Hour).Unix(), 1000, nil); len(b) != 0 {
		t.Fatal("high threshold must not breach")
	}
}

func TestSetResolvedStampsAndBackdates(t *testing.T) {
	st := newTestStore(t)
	id, err := st.RecordError("front", "E", "x", "fp-r", "production", "error", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetResolved(id, true); err != nil {
		t.Fatal(err)
	}
	iss, err := st.Issue(id)
	if err != nil || !iss.Resolved {
		t.Fatalf("must be resolved: %v %v", iss, err)
	}
	back := time.Now().Add(-time.Hour).Unix()
	if err := st.SetResolvedAt(id, back); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordError("front", "E", "x", "fp-r", "production", "error", nil); err != nil {
		t.Fatal(err)
	}
	evts, err := st.Regressions(nil)
	if err != nil || len(evts) != 1 || evts[0].ID != id {
		t.Fatalf("regression must match: %+v %v", evts, err)
	}
	iss, _ = st.Issue(id)
	if iss.Resolved {
		t.Fatal("Regressions must reopen")
	}
	if evts, _ := st.Regressions(nil); len(evts) != 0 {
		t.Fatal("reopen consumes the predicate — no double fire")
	}
}
