package alerts

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iniznet/nightpost/internal/store"
)

type fakeSender struct {
	mu   sync.Mutex
	msgs []string
	fail bool
}

func (f *fakeSender) Send(title, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("boom")
	}
	f.msgs = append(f.msgs, title)
	return nil
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.msgs)
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "alerts.db"))
	if err != nil {
		t.Fatal(err)
	}
	st.MaxOccPerMin.Store(0)
	t.Cleanup(func() { st.Close() })
	return st
}

func baseCfg(rules []Rule) *Config {
	return &Config{
		Channels: []Channel{{ID: "test", Type: "discord", Webhook: "http://127.0.0.1:9/x", HourlyCap: 50, DailyCeiling: 200}},
		Rules:    rules,
	}
}

func armed(t *testing.T, st *store.Store, cfg *Config) (*Dispatcher, *fakeSender) {
	t.Helper()
	d := NewDispatcher(st, cfg, "")
	f := &fakeSender{}
	d.Senders["test"] = f
	return d, f
}

func TestNewIssueCooldownExactlyOnce(t *testing.T) {
	st := testStore(t)
	now := time.Now()
	if _, err := st.RecordError("front", "TypeError", "x is undefined", "fp-new", "production", "error", nil); err != nil {
		t.Fatal(err)
	}
	// Window wider than the 60m cooldown so the re-alert tick still matches
	// the rule and only the cooldown gate decides delivery.
	d, f := armed(t, st, baseCfg([]Rule{{Type: "new-issue", Channels: []string{"test"}, WindowMinutes: 120}}))

	if sent := d.Tick(now); len(sent) != 1 || f.count() != 1 {
		t.Fatalf("first tick must fire exactly once: %d sent, %d msgs", len(sent), f.count())
	}
	if sent := d.Tick(now.Add(time.Minute)); len(sent) != 0 || f.count() != 1 {
		t.Fatalf("inside cooldown must stay silent: %d sent", len(sent))
	}
	if sent := d.Tick(now.Add(61 * time.Minute)); len(sent) != 1 || f.count() != 2 {
		t.Fatalf("after cooldown the same issue may re-alert once: %d sent", len(sent))
	}
	if sent := d.Tick(now.Add(62 * time.Minute)); len(sent) != 0 {
		t.Fatalf("immediate re-tick must be cooled down again: %d sent", len(sent))
	}
}

func TestSendFailureRetriesNextTick(t *testing.T) {
	st := testStore(t)
	if _, err := st.RecordError("front", "E", "boom", "fp-retry", "production", "error", nil); err != nil {
		t.Fatal(err)
	}
	d, f := armed(t, st, baseCfg([]Rule{{Type: "new-issue", Channels: []string{"test"}, WindowMinutes: 15}}))
	f.fail = true
	now := time.Now()
	if sent := d.Tick(now); len(sent) != 0 || f.count() != 0 {
		t.Fatal("failed send must not count or record")
	}
	if _, err := st.LastAlertSent("new-issue", "front|fp-retry"); err != nil {
		t.Fatal(err)
	}
	last, _ := st.LastAlertSent("new-issue", "front|fp-retry")
	if last != 0 {
		t.Fatal("failed send must leave the cooldown ledger empty")
	}
	f.fail = false
	if sent := d.Tick(now.Add(time.Minute)); len(sent) != 1 || f.count() != 1 {
		t.Fatalf("retry on next tick must deliver exactly once: %d/%d", len(sent), f.count())
	}
}

func TestRegressionReopensAndAlerts(t *testing.T) {
	st := testStore(t)
	now := time.Now()
	id, err := st.RecordError("backend", "QueryException", "deadlock", "fp-reg", "production", "error", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetResolvedAt(id, now.Add(-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordError("backend", "QueryException", "deadlock", "fp-reg", "production", "error", nil); err != nil {
		t.Fatal(err)
	}
	d, f := armed(t, st, baseCfg([]Rule{{Type: "regression", Channels: []string{"test"}}}))

	sent := d.Tick(now)
	if len(sent) != 1 || !strings.HasPrefix(sent[0].Title, "Regressed in backend") {
		t.Fatalf("regression must fire once: %+v", sent)
	}
	iss, err := st.Issue(id)
	if err != nil {
		t.Fatal(err)
	}
	if iss.Resolved {
		t.Fatal("regression pass must reopen the issue")
	}
	if sent := d.Tick(now.Add(time.Minute)); len(sent) != 0 || f.count() != 1 {
		t.Fatalf("reopen consumes the predicate; no double alert: %d", len(sent))
	}
}

func TestCountWindowThreshold(t *testing.T) {
	st := testStore(t)
	fp := store.ForException("QueryException", "user 42 missing", "App\\Controller:index")
	for i := 0; i < 60; i++ {
		if _, err := st.RecordError("backend", "QueryException", fmt.Sprintf("user %d missing", i), fp, "production", "error", nil); err != nil {
			t.Fatal(err)
		}
	}
	d, f := armed(t, st, baseCfg([]Rule{{Type: "count-window", Channels: []string{"test"}, WindowMinutes: 15, Threshold: 50}}))
	now := time.Now()
	sent := d.Tick(now)
	if len(sent) != 1 || !strings.Contains(sent[0].Title, "60 occurrences") {
		t.Fatalf("60>=50 must breach: %+v", sent)
	}
	if f.count() != 1 {
		t.Fatalf("msgs %d", f.count())
	}
	if sent := d.Tick(now.Add(time.Minute)); len(sent) != 0 {
		t.Fatalf("cooldown must suppress the breach: %d", len(sent))
	}
}

func TestP95LatencyRule(t *testing.T) {
	st := testStore(t)
	for i := 0; i < 20; i++ {
		if _, err := st.RecordProfile(store.ProfileInput{ProjectKey: "front", Type: store.ProfileTypeRequest, Name: "GET /slow", DurationMs: 2500, Environment: "production"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.RecordProfile(store.ProfileInput{ProjectKey: "front", Type: store.ProfileTypeRequest, Name: "GET /fast", DurationMs: 10, Environment: "production"}); err != nil {
		t.Fatal(err)
	}
	d, _ := armed(t, st, baseCfg([]Rule{{Type: "p95-latency", Channels: []string{"test"}, Projects: []string{"front"}, ThresholdMs: 1000}}))
	sent := d.Tick(time.Now())
	if len(sent) != 1 || !strings.Contains(sent[0].Title, "GET /slow") {
		t.Fatalf("only the slow route must breach: %+v", sent)
	}
}

func TestChannelHourlyCap(t *testing.T) {
	st := testStore(t)
	for _, fp := range []string{"fp-a", "fp-b", "fp-c"} {
		if _, err := st.RecordError("front", "E", "x", fp, "production", "error", nil); err != nil {
			t.Fatal(err)
		}
	}
	cfg := baseCfg([]Rule{{Type: "new-issue", Channels: []string{"test"}, WindowMinutes: 15}})
	cfg.Channels[0].HourlyCap = 2
	d, f := armed(t, st, cfg)
	sent := d.Tick(time.Now())
	if len(sent) != 2 || f.count() != 2 {
		t.Fatalf("hourly cap 2 must clip to two sends: %d/%d", len(sent), f.count())
	}
}
