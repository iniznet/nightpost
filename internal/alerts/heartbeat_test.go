package alerts

import (
	"testing"
	"time"
)

func hbRule(cooldown int) Rule {
	return Rule{Type: "heartbeat", Channels: []string{"test"}, Projects: []string{"backend"},
		Command: "quests:reset-stale-progress", CadenceSeconds: 3600, CooldownMinutes: cooldown}
}

func TestHeartbeatSilentWhenFresh(t *testing.T) {
	st := testStore(t)
	if err := st.RecordJob("backend", "command", "quests:reset-stale-progress", "", 30, true, 1); err != nil {
		t.Fatal(err)
	}
	d := NewDispatcher(st, baseCfg([]Rule{hbRule(60)}), "")
	fake := &fakeSender{}
	d.Senders["test"] = fake
	if sent := d.Tick(time.Now()); len(sent) != 0 || fake.count() != 0 {
		t.Fatalf("fresh run must stay silent, got %d", fake.count())
	}
}

func TestHeartbeatFiresExactlyOnceWhenStale(t *testing.T) {
	st := testStore(t)
	if err := st.RecordJob("backend", "command", "quests:reset-stale-progress", "", 30, true, 1); err != nil {
		t.Fatal(err)
	}
	d := NewDispatcher(st, baseCfg([]Rule{hbRule(60)}), "")
	fake := &fakeSender{}
	d.Senders["test"] = fake
	// Age the baseline past cadence x grace (2h) by ticking from the future.
	future := time.Now().Add(4 * time.Hour)
	if sent := d.Tick(future); len(sent) != 1 || sent[0].Rule != "heartbeat" {
		t.Fatalf("stale command must alert once, got %+v", sent)
	}
	d.Tick(future.Add(time.Minute))
	d.Tick(future.Add(2 * time.Minute))
	if fake.count() != 1 {
		t.Fatalf("cooldown must hold further ticks to 1 alert, got %d", fake.count())
	}
}

func TestHeartbeatBootGraceAndRecovery(t *testing.T) {
	st := testStore(t)
	d := NewDispatcher(st, baseCfg([]Rule{hbRule(60)}), "")
	fake := &fakeSender{}
	d.Senders["test"] = fake
	if sent := d.Tick(time.Now()); len(sent) != 0 {
		t.Fatalf("unknown command at boot must respect grace, got %+v", sent)
	}
	d.startedAt = time.Now().Add(-3 * time.Hour)
	if sent := d.Tick(time.Now()); len(sent) != 1 {
		t.Fatalf("boot older than grace must alert, got %+v", sent)
	}
	if err := st.RecordJob("backend", "command", "quests:reset-stale-progress", "", 20, true, 1); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.JobLastOKAt("backend", "command", "quests:reset-stale-progress"); err != nil || ok == 0 {
		t.Fatal("recovery must set the baseline")
	}
}

func TestHeartbeatConfigValidation(t *testing.T) {
	bad := baseCfg([]Rule{{Type: "heartbeat", Channels: []string{"test"}, Projects: []string{"backend"}, CadenceSeconds: 3600}})
	if err := bad.validate(); err == nil {
		t.Fatal("heartbeat without command must be rejected")
	}
	bad2 := baseCfg([]Rule{hbRule(0)})
	bad2.Rules[0].CadenceSeconds = 0
	if err := bad2.validate(); err == nil {
		t.Fatal("heartbeat without cadence must be rejected")
	}
	bad3 := baseCfg([]Rule{hbRule(0)})
	bad3.Rules[0].Projects = nil
	if err := bad3.validate(); err == nil {
		t.Fatal("heartbeat without projects must be rejected")
	}
	if err := baseCfg([]Rule{hbRule(30)}).validate(); err != nil {
		t.Fatalf("valid heartbeat rejected: %v", err)
	}
}
