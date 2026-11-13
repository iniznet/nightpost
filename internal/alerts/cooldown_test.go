package alerts

import (
	"testing"
	"time"
)

// TestCooldownOneAlertPerTargetPerWindow proves the contract the alerts_log
// ledger exists for: one alert per (rule, target) per cooldown window, no matter
// how many channels that rule names. It also pins the defect the fan-out had:
// the cooldown was checked inside the channel loop, so the first channel's own
// record silenced every channel after it.
func TestCooldownOneAlertPerTargetPerWindow(t *testing.T) {
	st := testStore(t)
	now := time.Now()
	if _, err := st.RecordError("front", "TypeError", "x is undefined", "fp-cool", "production", "error", nil); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Channels: []Channel{
			{ID: "a", Type: "discord", Webhook: "http://127.0.0.1:9/a"},
			{ID: "b", Type: "discord", Webhook: "http://127.0.0.1:9/b"},
		},
		Rules: []Rule{{Type: "new-issue", Channels: []string{"a", "b"}, Projects: []string{"front"},
			WindowMinutes: 120, CooldownMinutes: 60}},
	}
	d := NewDispatcher(st, cfg, "")
	first := &fakeSender{}
	second := &fakeSender{}
	d.Senders["a"] = first
	d.Senders["b"] = second

	if sent := d.Tick(now); len(sent) != 2 {
		t.Fatalf("first tick delivered to %d channel(s), want both: %+v", len(sent), sent)
	}
	if first.count() != 1 || second.count() != 1 {
		t.Fatalf("channel sends = %d/%d, want 1/1", first.count(), second.count())
	}
	last, err := st.LastAlertSent("new-issue", "front|fp-cool")
	if err != nil || last != now.Unix() {
		t.Fatalf("ledger = %d err=%v, want the tick's second", last, err)
	}

	if sent := d.Tick(now.Add(time.Minute)); len(sent) != 0 {
		t.Fatalf("inside the window the same target must stay silent: %+v", sent)
	}
	if first.count() != 1 || second.count() != 1 {
		t.Fatalf("cooldown leaked: %d/%d sends", first.count(), second.count())
	}
	if sent := d.Tick(now.Add(61 * time.Minute)); len(sent) != 2 {
		t.Fatalf("after the window the target re-alerts on every channel once: %+v", sent)
	}
	if first.count() != 2 || second.count() != 2 {
		t.Fatalf("post-window sends = %d/%d, want 2/2", first.count(), second.count())
	}
}

// TestSweepMissedRunMarksOnceAndAlertsOnce is the sweep contract: the ticker
// stamps a past-due scheduled run once, the rule fires once for it, and a second
// sweep inside the same window fires nothing.
func TestSweepMissedRunMarksOnceAndAlertsOnce(t *testing.T) {
	st := testStore(t)
	now := time.Now()
	if err := st.RecordJob("backend", "command", "quests:reset-stale-progress", "", 30, true, 1, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.SetJobSchedule("backend", "command", "quests:reset-stale-progress", "*/5 * * * *",
		now.Add(-10*time.Minute).Unix(), 60); err != nil {
		t.Fatal(err)
	}
	d, f := armed(t, st, baseCfg([]Rule{{Type: "missed_run", Channels: []string{"test"},
		Projects: []string{"backend"}, CooldownMinutes: 60}}))

	marked, err := st.MarkMissedJobs(now)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(marked) != 1 {
		t.Fatalf("sweep stamped %d row(s), want 1", len(marked))
	}
	if sent := d.Tick(now); len(sent) != 1 || sent[0].Rule != "missed_run" {
		t.Fatalf("first sweep must alert exactly once: %+v", sent)
	}
	if f.count() != 1 {
		t.Fatalf("delivered %d messages, want 1", f.count())
	}

	// Second sweep inside the window: nothing new to stamp, and the cooldown holds
	// the rule even though the row is still missed.
	marked, err = st.MarkMissedJobs(now.Add(time.Minute))
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if len(marked) != 0 {
		t.Fatalf("second sweep re-stamped %d row(s): a stamp is one-way", len(marked))
	}
	if sent := d.Tick(now.Add(time.Minute)); len(sent) != 0 {
		t.Fatalf("a second sweep inside the cooldown must fire nothing: %+v", sent)
	}
	if f.count() != 1 {
		t.Fatalf("delivered %d messages after two sweeps, want 1", f.count())
	}

	// A run that lands clears the verdict, so the next absence can alert again.
	if err := st.RecordJob("backend", "command", "quests:reset-stale-progress", "", 30, true, 1, ""); err != nil {
		t.Fatal(err)
	}
	if missed, err := st.MissedJobs([]string{"backend"}); err != nil || len(missed) != 0 {
		t.Fatalf("a recovered job must leave the missed list: %+v %v", missed, err)
	}
}
