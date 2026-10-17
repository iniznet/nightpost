package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/iniznet/fenrirealm-monitor/internal/ingest"
	"github.com/iniznet/fenrirealm-monitor/internal/store"
)

// newServer runs the REAL ingest handlers, so this test proves the reference
// client speaks the frozen wire spec rather than proving it agrees with itself.
func newServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "wire.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ingest/{project}/profile", ingest.ProfileHandler(st))
	mux.HandleFunc("POST /ingest/{project}/error", ingest.ErrorHandler(st))
	mux.HandleFunc("POST /ingest/{project}/query", ingest.QueryHandler(st))
	mux.HandleFunc("POST /ingest/{project}/job", ingest.JobHandler(st))
	mux.HandleFunc("POST /ingest/{project}/release", ingest.ReleaseHandler(st))
	return httptest.NewServer(mux), st
}

func TestInertClientIsSafeToCall(t *testing.T) {
	var c *Client // the "not configured" case, as a nil receiver
	ctx := context.Background()
	if err := c.Profile(ctx, "p", Profile{Name: "x", DurationMs: 1}); err != nil {
		t.Errorf("nil client must be a no-op, got %v", err)
	}
	if err := c.Log(ctx, LogRecord{Message: "x"}); err != nil {
		t.Errorf("nil client Log must be a no-op, got %v", err)
	}
	c.Go(func() error { return nil }) // must not panic

	if got := New("", "", ""); got != nil {
		t.Error("an unconfigured New must return nil — that IS the off-by-default posture")
	}
}

func TestNewFromEnvDualRead(t *testing.T) {
	t.Setenv("MONITOR_ENDPOINT", "http://legacy:8000")
	t.Setenv("NIGHTPOST_ENDPOINT", "http://brand:8000")
	if c := NewFromEnv(); c == nil || c.endpoint != "http://brand:8000" {
		t.Errorf("NIGHTPOST_* must win the dual read, got %+v", c)
	}
	t.Setenv("NIGHTPOST_ENDPOINT", "")
	if c := NewFromEnv(); c == nil || c.endpoint != "http://legacy:8000" {
		t.Errorf("MONITOR_* must be the fallback, got %+v", c)
	}
	t.Setenv("MONITOR_ENDPOINT", "")
	t.Setenv("MONITOR_SOCKET", "")
	if c := NewFromEnv(); c != nil {
		t.Error("no environment at all must build an inert client")
	}
}

func TestReferenceClientLandsEveryRoute(t *testing.T) {
	srv, st := newServer(t)
	defer srv.Close()
	c := New(srv.URL, "", "s3cret-key")
	ctx := context.Background()

	if err := c.Profile(ctx, "demo", Profile{Name: "GET /demo", DurationMs: 12.5, Spans: []Span{{Name: "db", DurationMs: 3}}}); err != nil {
		t.Fatalf("profile: %v", err)
	}
	if err := c.Report(ctx, "demo", ErrorReport{Message: "boom", Stack: "at x:1"}); err != nil {
		t.Fatalf("report: %v", err)
	}
	if err := c.Query(ctx, "demo", "select * from chapters where id = 7", 123.5, "mysql"); err != nil {
		t.Fatalf("query: %v", err)
	}
	if err := c.Job(ctx, "demo", "quests:reset", "command", "default", 3.2, true); err != nil {
		t.Fatalf("job: %v", err)
	}
	if err := c.Release(ctx, "demo", "2026.10.9-a1b2c3", "production"); err != nil {
		t.Fatalf("release: %v", err)
	}

	// Assert against the store: the payloads were UNDERSTOOD, not merely
	// accepted with a 200.
	rows, err := st.QueryLeaderboard("demo", 24, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("slow query not stored: %+v %v", rows, err)
	}
	if rows[0].SQL != "select * from chapters where id = <n>" {
		t.Errorf("stored sql = %q, want the fingerprinted form", rows[0].SQL)
	}
	if jobs, err := st.JobLeaderboard("demo", 24, 10); err != nil || len(jobs) != 1 {
		t.Errorf("job not stored: %+v %v", jobs, err)
	}
	if mark, err := st.LatestRelease("demo", ""); err != nil || mark == nil || mark.Version != "2026.10.9-a1b2c3" {
		t.Errorf("release not stored: %+v %v", mark, err)
	}
	if issues, err := st.Issues(store.IssueQuery{Project: "demo", Limit: 5}); err != nil || len(issues) == 0 {
		t.Errorf("error report not stored: %+v %v", issues, err)
	}
	if profiles, err := st.Profiles("demo", 10); err != nil || len(profiles) == 0 {
		t.Errorf("profile not stored: %+v %v", profiles, err)
	}
}

// TestLogLegReachesTheStore exercises the socket transport, which is a
// transport difference only — the same JSON record.
func TestLogLegReachesTheStore(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "log.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()

	// Reserve a free port, release it, and hand the address to the server: the
	// Monolog leg takes an address, not a listener.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	addr := probe.Addr().String()
	probe.Close()

	mon := &ingest.Monolog{Store: st}
	go func() { _ = mon.ListenAndServe(addr) }()
	defer mon.Stop()

	c := New("", addr, "")
	var lastErr error
	for i := 0; i < 40; i++ {
		lastErr = c.Log(context.Background(), LogRecord{
			ProjectKey: "demo", Channel: "security", LevelName: "info",
			Message: "reference client log line",
		})
		if lastErr == nil {
			break
		}
		time.Sleep(25 * time.Millisecond) // wait for the listener
	}
	if lastErr != nil {
		t.Fatalf("log: %v", lastErr)
	}

	for i := 0; i < 40; i++ {
		if levels, err := st.LogLevels("demo"); err == nil {
			for _, l := range levels {
				if l.Level == "info" && l.Count >= 1 {
					return
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Error("the log line never reached the store")
}
