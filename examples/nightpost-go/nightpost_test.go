package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iniznet/nightpost/internal/ingest"
	"github.com/iniznet/nightpost/internal/store"
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
	mux.HandleFunc("POST /ingest/{project}/session", ingest.SessionHandler(st))
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

func TestNewFromEnvReadsOneNamespace(t *testing.T) {
	t.Setenv("MONITOR_ENDPOINT", "http://legacy:8000")
	t.Setenv("NIGHTPOST_ENDPOINT", "http://brand:8000")
	if c := NewFromEnv(); c == nil || c.endpoint != "http://brand:8000" {
		t.Errorf("NIGHTPOST_* must be read, got %+v", c)
	}
	t.Setenv("NIGHTPOST_ENDPOINT", "")
	if c := NewFromEnv(); c != nil {
		t.Error("a legacy MONITOR_* name must be inert — no environment is an inert client")
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

// TestIdentityDefaultsAndPrecedence pins the three sources a producer may name
// itself from: flag, NIGHTPOST_* variable, and the binary-derived default.
// Flag > env > derived, and an unset flag never erases the variable.
func TestIdentityDefaultsAndPrecedence(t *testing.T) {
	t.Setenv("NIGHTPOST_ENDPOINT", "http://env:8000")
	t.Setenv("NIGHTPOST_SERVICE", "env-service")
	t.Setenv("NIGHTPOST_ENV", "staging")

	c := NewFromEnv()
	if c == nil {
		t.Fatal("NewFromEnv must build a client when the endpoint is set")
	}
	if c.Service() != "env-service" || c.Env() != "staging" {
		t.Fatalf("env identity = %q/%q, want env-service/staging", c.Service(), c.Env())
	}

	// An empty flag is "not given": it keeps the variable NewFromEnv read.
	c.SetIdentity("", "")
	if c.Service() != "env-service" || c.Env() != "staging" {
		t.Errorf("empty SetIdentity erased the env identity: %q/%q", c.Service(), c.Env())
	}
	c.SetIdentity("flag-service", "production")
	if c.Service() != "flag-service" || c.Env() != "production" {
		t.Errorf("flag identity = %q/%q, want flag-service/production", c.Service(), c.Env())
	}

	// No variable, no flag: the binary names itself rather than reporting
	// anonymously under the project key.
	t.Setenv("NIGHTPOST_SERVICE", "")
	t.Setenv("NIGHTPOST_ENV", "")
	fresh := New("http://brand:8000", "", "")
	if fresh == nil {
		t.Fatal("New must build a client for an http endpoint")
	}
	if fresh.Service() != DefaultServiceName() || fresh.Service() == "" {
		t.Errorf("default service = %q, want the binary-derived %q", fresh.Service(), DefaultServiceName())
	}
	if fresh.Env() != "" {
		t.Errorf("env = %q, want empty when nothing names one", fresh.Env())
	}
}

// TestIdentityLandsInTheStore proves the payloads carry the identity, not just
// that the client holds it.
func TestIdentityLandsInTheStore(t *testing.T) {
	srv, st := newServer(t)
	defer srv.Close()
	c := New(srv.URL, "", "")
	c.SetIdentity("nightpost-go-ref", "production")

	if err := c.Profile(context.Background(), "demo", Profile{Name: "GET /demo", DurationMs: 4}); err != nil {
		t.Fatalf("profile: %v", err)
	}
	svc, err := st.ServiceByName("demo", "nightpost-go-ref")
	if err != nil || svc == nil {
		t.Fatalf("service row missing: %+v %v", svc, err)
	}
	if svc.Env != "production" {
		t.Errorf("service env = %q, want production", svc.Env)
	}
}

// TestTracerReportsServerRootAndClientChild is the trace half of the contract:
// the payload's own span is the ROOT (kind server, start 0, no parent), and
// every outbound call is a client child carrying the callee's name.
func TestTracerReportsServerRootAndClientChild(t *testing.T) {
	srv, st := newServer(t)
	defer srv.Close()
	c := New(srv.URL, "", "")
	c.SetIdentity("nightpost-go-ref", "production")

	tracer := NewTracer("")
	if len(tracer.TraceID) != 32 || len(tracer.SpanID) != 16 {
		t.Fatalf("ids must be W3C width: trace %q span %q", tracer.TraceID, tracer.SpanID)
	}
	call := tracer.Begin("client", "GET /up", "GET /up", "nightpost")
	time.Sleep(2 * time.Millisecond)
	call.End("ok")

	if err := c.Profile(context.Background(), "demo", Profile{
		Name: "nightpost-go:demo", DurationMs: 5, Type: "command", Kind: "server",
		Route: "/demo", SpanID: tracer.SpanID, TraceID: tracer.TraceID,
		Environment: "production", Spans: tracer.Spans(),
	}); err != nil {
		t.Fatalf("profile: %v", err)
	}

	spans, err := st.TraceSpans(tracer.TraceID)
	if err != nil {
		t.Fatalf("trace: %v", err)
	}
	if len(spans) != 2 {
		t.Fatalf("spans = %d, want the root plus one client child", len(spans))
	}
	// Order inside a trace is (start_ms, span_id), so a child that began at the
	// same millisecond can sort before the root: pick them by identity, not by
	// index.
	var root, child = spans[0], spans[1]
	for _, sp := range spans {
		if sp.SpanID == tracer.SpanID {
			root = sp
		} else {
			child = sp
		}
	}
	if root.SpanID != tracer.SpanID {
		t.Fatalf("the payload's own span is not in the trace: %+v", spans)
	}
	services, err := st.Services("demo")
	if err != nil {
		t.Fatalf("services: %v", err)
	}
	peerName := ""
	for _, svc := range services {
		if child.PeerServiceID != 0 && svc.ID == child.PeerServiceID {
			peerName = svc.Name
		}
	}
	if root.SpanID != tracer.SpanID {
		t.Errorf("root span id = %q, want the payload's own %q", root.SpanID, tracer.SpanID)
	}
	if root.ParentSpanID != "" {
		t.Errorf("root parent = %q, want none — the payload's span IS the trace root", root.ParentSpanID)
	}
	if root.Kind != "server" {
		t.Errorf("root kind = %q, want server", root.Kind)
	}
	if root.StartMs != 0 {
		t.Errorf("root start_ms = %v, want 0", root.StartMs)
	}
	if child.ParentSpanID != tracer.SpanID {
		t.Errorf("child parent = %q, want the root %q", child.ParentSpanID, tracer.SpanID)
	}
	if child.Kind != "client" {
		t.Errorf("child kind = %q, want client", child.Kind)
	}
	if child.StartMs < 0 {
		t.Errorf("child start_ms = %v, want an offset from the request start", child.StartMs)
	}
	if child.DurationMs <= 0 {
		t.Errorf("child duration_ms = %v, want the measured call", child.DurationMs)
	}
	if peerName != "nightpost" {
		t.Errorf("peer service = %q, want nightpost", peerName)
	}
	if !strings.Contains(child.Attributes, "peer_service") {
		t.Errorf("attributes = %q, want the peer name on the span", child.Attributes)
	}
}

// TestJobRegistersItsSchedule is the promise /jobs renders: cron, the next
// expected fire time in unix seconds, and the grace the run gets.
func TestJobRegistersItsSchedule(t *testing.T) {
	srv, st := newServer(t)
	defer srv.Close()
	c := New(srv.URL, "", "")
	c.SetIdentity("nightpost-go-ref", "production")

	next := nextFire(time.Now())
	if err := c.JobWithSchedule(context.Background(), "demo", JobRun{
		Name: "nightpost-go:demo", Kind: "command", Seconds: 1.5, OK: true,
		Schedule: &Schedule{Cron: DemoCron, NextExpectedAt: next, GraceSeconds: DemoGraceSeconds},
	}); err != nil {
		t.Fatalf("job: %v", err)
	}

	jobs, err := st.JobLeaderboard("demo", 24, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %+v %v", jobs, err)
	}
	job := jobs[0]
	if job.Cron != DemoCron {
		t.Errorf("cron = %q, want %q", job.Cron, DemoCron)
	}
	if job.ExpectedNextAt != next {
		t.Errorf("next_expected_at = %d, want %d", job.ExpectedNextAt, next)
	}
	if job.GraceSeconds != DemoGraceSeconds {
		t.Errorf("grace_seconds = %d, want %d", job.GraceSeconds, DemoGraceSeconds)
	}
	if job.LastStatus != "ok" {
		t.Errorf("last status = %q, want ok", job.LastStatus)
	}
	if got := nextFire(time.Now()); got <= time.Now().Unix() || got%300 != 0 {
		t.Errorf("nextFire = %d, want a future 5-minute boundary", got)
	}
}

// TestSessionBeaconIsIdempotent is the exit beacon: one producer-owned random
// key, an outcome that a retry updates, and a crash-free read that counts the
// session exactly once.
func TestSessionBeaconIsIdempotent(t *testing.T) {
	srv, st := newServer(t)
	defer srv.Close()
	c := New(srv.URL, "", "")
	c.SetIdentity("nightpost-go-ref", "production")

	beacon := SessionBeacon{
		SessionID: "ref-session-1", Release: DemoRelease, Status: "ok",
		Route: "/demo", DurationMs: 12,
	}
	if err := c.Session(context.Background(), "demo", beacon); err != nil {
		t.Fatalf("session: %v", err)
	}
	sessions, crashed, crashFree, err := st.CrashFree("demo", DemoRelease)
	if err != nil {
		t.Fatalf("crash free: %v", err)
	}
	if sessions != 1 || crashed != 0 || crashFree != 1 {
		t.Fatalf("sessions/crashed/crashfree = %d/%d/%v, want 1/0/1", sessions, crashed, crashFree)
	}

	// The same key again is an update, never a second session.
	beacon.Status = "abnormal"
	if err := c.Session(context.Background(), "demo", beacon); err != nil {
		t.Fatalf("session retry: %v", err)
	}
	sessions, crashed, _, err = st.CrashFree("demo", DemoRelease)
	if err != nil {
		t.Fatalf("crash free: %v", err)
	}
	if sessions != 1 || crashed != 1 {
		t.Errorf("after retry sessions/crashed = %d/%d, want 1/1", sessions, crashed)
	}
}
