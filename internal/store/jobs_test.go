package store

import (
	"testing"
	"time"
)

func TestRecordJobAggregates(t *testing.T) {
	st := newTestStore(t)
	if err := st.RecordJob("backend", "command", "quests:reset-stale-progress", "", 2.5, true, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordJob("backend", "command", "quests:reset-stale-progress", "", 4.5, true, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordJob("backend", "command", "quests:reset-stale-progress", "", 1.0, false, 1); err != nil {
		t.Fatal(err)
	}
	rows, err := st.JobLeaderboard("", 24, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("same name must group into one aggregate, got %d", len(rows))
	}
	j := rows[0]
	if j.Runs != 3 || j.Failures != 1 || j.Failures24h != 1 {
		t.Fatalf("runs=%d failures=%d fail24=%d", j.Runs, j.Failures, j.Failures24h)
	}
	if j.LastStatus != "failed" {
		t.Fatalf("last_status=%q", j.LastStatus)
	}
	if j.LastOKAt.IsZero() {
		t.Fatal("last_ok_at must hold the newest successful run")
	}
	if j.AvgMs < 2600 || j.AvgMs > 2700 {
		t.Fatalf("avg=%f want ~2666", j.AvgMs)
	}
	if j.LastMs != 1000 {
		t.Fatalf("last=%f want 1000", j.LastMs)
	}
	if err := st.RecordJob("backend", "command", "quests:reset-stale-progress", "", 3.0, true, 1); err != nil {
		t.Fatal(err)
	}
	rows, _ = st.JobLeaderboard("", 24, 50)
	if rows[0].LastStatus != "ok" || rows[0].Runs != 4 || rows[0].Failures24h != 1 {
		t.Fatalf("after ok: status=%q runs=%d fail24=%d", rows[0].LastStatus, rows[0].Runs, rows[0].Failures24h)
	}
}

func TestJobQueueColumnAndStaleWindow(t *testing.T) {
	st := newTestStore(t)
	if err := st.RecordJob("backend", "queue", "App\\\\Jobs\\\\X", "", 1, true, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordJob("backend", "queue", "App\\\\Jobs\\\\X", "high", 1, false, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordJob("backend", "queue", "App\\\\Jobs\\\\X", "", 1, true, 1); err != nil {
		t.Fatal(err)
	}
	rows, err := st.JobLeaderboard("", 24, 50)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Queue != "high" {
		t.Fatalf("queue must stick once known, got %q", rows[0].Queue)
	}
	if _, err := st.wdb.Exec(`UPDATE job_stats SET fail_window_start = ?`, time.Now().Unix()-86401); err != nil {
		t.Fatal(err)
	}
	rows, _ = st.JobLeaderboard("", 24, 50)
	if rows[0].Failures24h != 0 {
		t.Fatalf("stale window must display 0, got %d", rows[0].Failures24h)
	}
	if err := st.RecordJob("backend", "queue", "App\\\\Jobs\\\\X", "", 1, false, 1); err != nil {
		t.Fatal(err)
	}
	rows, _ = st.JobLeaderboard("", 24, 50)
	if rows[0].Failures24h != 1 {
		t.Fatalf("failure after stale window must reopen at 1, got %d", rows[0].Failures24h)
	}
}

func TestJobLastOKAtHeartbeatLookup(t *testing.T) {
	st := newTestStore(t)
	got, err := st.JobLastOKAt("backend", "command", "never:seen")
	if err != nil || got != 0 {
		t.Fatalf("missing row must report 0: %v %d", err, got)
	}
	if err := st.RecordJob("backend", "command", "chapters:publish-scheduled", "", 1, false, 1); err != nil {
		t.Fatal(err)
	}
	got, _ = st.JobLastOKAt("backend", "command", "chapters:publish-scheduled")
	if got != 0 {
		t.Fatalf("failed-only history has no ok baseline, got %d", got)
	}
	if err := st.RecordJob("backend", "command", "chapters:publish-scheduled", "", 1, true, 1); err != nil {
		t.Fatal(err)
	}
	got, _ = st.JobLastOKAt("backend", "command", "chapters:publish-scheduled")
	if got == 0 {
		t.Fatal("successful run must set the heartbeat baseline")
	}
	if err := st.RecordJob("backend", "command", "chapters:publish-scheduled", "", 1, false, 1); err != nil {
		t.Fatal(err)
	}
	got2, _ := st.JobLastOKAt("backend", "command", "chapters:publish-scheduled")
	if got2 != got {
		t.Fatal("a later failure must not move last_ok_at")
	}
}
