// Package main is the reference port for the Nightpost wire spec v1 plus the
// v2 producer half: service identity, the server span at the trace root with
// one client span per outbound call, the schedule a job promises, and the
// session beacon sent at exit.
//
// It is deliberately a single file with ZERO dependencies: the spec has no
// SDK-shaped problem in it, so a language's port is an afternoon of work —
// build the JSON, set one header, POST it. Read WIRE_SPEC.md beside this file;
// testdata/wire/ holds the vectors this client is checked against.
//
// Two rules are contract rather than courtesy, and both are structural here:
//
//  1. OFF BY DEFAULT. New returns nil when nothing is configured, and every
//     method is a no-op on a nil receiver, so a caller never branches on
//     "is telemetry enabled?" — and an installer cannot silently switch a
//     sender on .
//  2. NEVER COST THE REQUEST. Every send is bounded, and the failure of a
//     telemetry call is never returned into application control flow.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ClientVersion identifies this port to the server (X-Nightpost-Client).
const ClientVersion = "nightpost-go/1.0"

// Client is the whole SDK: where to send, how to authenticate, who is
// sending, and a bound HTTP client. An empty endpoint makes New return nil
// (see rule 1 above).
type Client struct {
	endpoint string       // e.g. http://nightpost:8000
	socket   string       // e.g. nightpost:9913, for the Monolog line
	key      string       // ingest key; "" while the project is unsigned
	service  string       // v2 producer identity name, never empty
	env      string       // v2 producer environment; "" until one is named
	http     *http.Client // never unbounded: a stuck send must not hold a worker
}

// New builds a client. Both endpoints are optional and independently inert.
//
// The service name starts as the binary's own name (see DefaultServiceName):
// a v1 payload with no service is filed under the project key, which tells an
// operator nothing about which process sent it.
func New(endpoint, socket, key string) *Client {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if endpoint == "" && socket == "" {
		return nil
	}
	return &Client{
		endpoint: endpoint,
		socket:   socket,
		key:      key,
		service:  DefaultServiceName(),
		http:     &http.Client{Timeout: 5 * time.Second},
	}
}

// NewFromEnv reads the producer environment: NIGHTPOST_ENDPOINT,
// NIGHTPOST_SOCKET, NIGHTPOST_KEY, and the v2 identity NIGHTPOST_SERVICE /
// NIGHTPOST_ENV. One namespace only — a legacy name is inert by design, so a
// half-migrated host reports nothing instead of posting to a target nobody
// meant to configure.
func NewFromEnv() *Client {
	env := func(name string) string { return strings.TrimSpace(os.Getenv(name)) }
	c := New(
		env("NIGHTPOST_ENDPOINT"),
		env("NIGHTPOST_SOCKET"),
		env("NIGHTPOST_KEY"),
	)
	c.SetIdentity(env("NIGHTPOST_SERVICE"), env("NIGHTPOST_ENV"))
	return c
}

// DefaultServiceName is the identity used when neither a flag nor
// NIGHTPOST_SERVICE names one: the binary's own file name. Deriving it means
// an install reports the process it was actually built as, and a copy of this
// port names itself with no edit.
func DefaultServiceName() string {
	if len(os.Args) > 0 {
		base := filepath.Base(strings.TrimSpace(os.Args[0]))
		if base != "" && base != "." && base != string(filepath.Separator) {
			return base
		}
	}
	return "nightpost-go"
}

// RandomHex mints n cryptorandom bytes as lowercase hex. A span id is 8 bytes
// (16 digits) and a trace id 16 (32 digits), which is the width the server
// accepts without repair; a session id is the producer's own random key and is
// never a reader identifier.
//
// The fallback exists because a telemetry id must never fail a request: if the
// entropy source is unavailable the clock still keeps two ids apart, and the
// value is trimmed to the exact width the server checks.
func RandomHex(n int) string {
	if n <= 0 {
		return ""
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err == nil {
		return hex.EncodeToString(buf)
	}
	fallback := fmt.Sprintf("%0*x", n*2, uint64(time.Now().UnixNano()))
	if len(fallback) > n*2 {
		fallback = fallback[len(fallback)-n*2:]
	}
	return fallback
}

// SetIdentity names this producer: the service the server files an event
// under, and the environment it runs in. An empty argument keeps the current
// value, so an unset flag can never erase NIGHTPOST_SERVICE or the
// binary-derived default.
func (c *Client) SetIdentity(service, env string) {
	if c == nil {
		return
	}
	if s := strings.TrimSpace(service); s != "" {
		c.service = s
	}
	if e := strings.TrimSpace(env); e != "" {
		c.env = e
	}
}

// Service is the identity this client reports as.
func (c *Client) Service() string {
	if c == nil {
		return ""
	}
	return c.service
}

// Env is the environment this client reports as ("" when none is named).
func (c *Client) Env() string {
	if c == nil {
		return ""
	}
	return c.env
}

// identify fills a payload's v2 identity from the client. An explicit value on
// the payload wins, so a caller can label one event for another service.
func (c *Client) identify(service, env string) (string, string) {
	if c == nil {
		return strings.TrimSpace(service), strings.TrimSpace(env)
	}
	if strings.TrimSpace(service) == "" {
		service = c.service
	}
	if strings.TrimSpace(env) == "" {
		env = c.env
	}
	return strings.TrimSpace(service), strings.TrimSpace(env)
}

// Go runs a send in the background, recovering panic so a telemetry bug can
// never take the host process down.
func (c *Client) Go(fn func() error) {
	if c == nil || fn == nil {
		return
	}
	go func() {
		defer func() { _ = recover() }()
		_ = fn()
	}()
}

// post sends one payload to one ingest route and returns the status error.
func (c *Client) post(ctx context.Context, project, leg string, body any) error {
	if c == nil || c.endpoint == "" {
		return nil
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	route := fmt.Sprintf("%s/ingest/%s/%s", c.endpoint, url.PathEscape(project), leg)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, route, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Nightpost-Client", ClientVersion)
	if c.key != "" {
		req.Header.Set("X-Monitor-Key", c.key)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return fmt.Errorf("nightpost %s: %s", leg, res.Status)
	}
	return nil
}

// ErrorReport is the browser/first-party error route's payload. Route is a
// route PATTERN, never a URL with ids in it; SpanID and TraceID let the report
// join the trace it happened inside.
type ErrorReport struct {
	Message     string `json:"message"`
	Stack       string `json:"stack,omitempty"`
	URL         string `json:"url,omitempty"`
	Route       string `json:"route,omitempty"`
	Environment string `json:"environment,omitempty"`
	Release     string `json:"release,omitempty"`
	UserID      string `json:"user_id,omitempty"`
	SpanID      string `json:"span_id,omitempty"`
	TraceID     string `json:"trace_id,omitempty"`
	Service     string `json:"service,omitempty"`
	Env         string `json:"env,omitempty"`
}

// Span is one timed child of a profile.
//
// StartMs is an OFFSET from the profile's own start, never a wall clock: the
// two ends of a trace run on different hosts, and no install has the clock
// sync an absolute join would need. Kind says what the span is; PeerService
// names the callee of a client span, so a hop that reported nothing still
// renders with a name instead of a blank.
type Span struct {
	SpanID       string  `json:"span_id,omitempty"`
	ParentSpanID string  `json:"parent_span_id,omitempty"`
	Name         string  `json:"name"`
	Kind         string  `json:"kind,omitempty"`
	Route        string  `json:"route,omitempty"`
	PeerService  string  `json:"peer_service,omitempty"`
	Status       string  `json:"status,omitempty"`
	StartMs      float64 `json:"start_ms,omitempty"`
	DurationMs   float64 `json:"duration_ms"`
}

// Profile is a request, job or command timing. SpanID is the payload's own
// span, and that span is the ROOT of the trace: it carries no parent, and a
// child that names none hangs off it on the server.
type Profile struct {
	Name        string  `json:"name"`
	DurationMs  float64 `json:"duration_ms"`
	Environment string  `json:"environment,omitempty"`
	Type        string  `json:"type,omitempty"` // request|job|command|callgraph
	Kind        string  `json:"kind,omitempty"` // request|server|job|command|…
	Route       string  `json:"route,omitempty"`
	SpanID      string  `json:"span_id,omitempty"`
	TraceID     string  `json:"trace_id,omitempty"`
	Spans       []Span  `json:"spans,omitempty"`
	Service     string  `json:"service,omitempty"`
	Env         string  `json:"env,omitempty"`
}

// Report sends an error. It is a no-op on an inert client.
func (c *Client) Report(ctx context.Context, project string, r ErrorReport) error {
	r.Service, r.Env = c.identify(r.Service, r.Env)
	return c.post(ctx, project, "error", r)
}

// Profile sends a timing profile, filling the v2 identity and span ids the
// caller left empty from the client's own.
func (c *Client) Profile(ctx context.Context, project string, p Profile) error {
	p.Service, p.Env = c.identify(p.Service, p.Env)
	return c.post(ctx, project, "profile", p)
}

// Query sends a slow-query sample. The server FINGERPRINTS the SQL it stores
// (numeric literals become <n>), so send the real statement.
func (c *Client) Query(ctx context.Context, project, sql string, ms float64, connection string) error {
	service, env := c.identify("", "")
	return c.post(ctx, project, "query", map[string]any{
		"sql": sql, "time_ms": ms, "connection": connection,
		"service": service, "env": env,
	})
}

// Job sends a job or command run: the v1-shaped call the wire vectors use,
// with no promise attached.
func (c *Client) Job(ctx context.Context, project, name, kind, queue string, seconds float64, ok bool) error {
	return c.JobWithSchedule(ctx, project, JobRun{
		Name: name, Kind: kind, Queue: queue, Seconds: seconds, OK: ok,
	})
}

// JobRun is one job or command completion.
type JobRun struct {
	Name     string
	Kind     string // command|queue; the server infers it when empty
	Queue    string
	Seconds  float64
	OK       bool
	Count    int64
	Trace    string // trace id of this run's profiled leg
	Schedule *Schedule
	Service  string
	Env      string
}

// Schedule is the promise a scheduled run makes. NextExpectedAt is unix
// seconds, never a parsed cron: the producer already knows when it will fire
// next, and a cron parser is a dependency this server's single-binary rule
// cannot pay for.
type Schedule struct {
	Cron           string `json:"schedule_cron,omitempty"`
	NextExpectedAt int64  `json:"next_expected_at,omitempty"`
	GraceSeconds   int64  `json:"grace_seconds,omitempty"`
}

// JobWithSchedule sends a run and registers the promise it makes, so /jobs can
// print a real next-due time and stamp the run as missed when the promise plus
// its grace elapsed with nothing arriving.
func (c *Client) JobWithSchedule(ctx context.Context, project string, run JobRun) error {
	service, env := c.identify(run.Service, run.Env)
	body := map[string]any{
		"name": run.Name, "kind": run.Kind, "queue": run.Queue,
		"seconds": run.Seconds, "ok": run.OK,
		"service": service, "env": env,
	}
	if run.Count > 0 {
		body["count"] = run.Count
	}
	if trace := strings.TrimSpace(run.Trace); trace != "" {
		body["trace"] = trace
	}
	if run.Schedule != nil {
		if cron := strings.TrimSpace(run.Schedule.Cron); cron != "" {
			body["schedule_cron"] = cron
		}
		if run.Schedule.NextExpectedAt > 0 {
			body["next_expected_at"] = run.Schedule.NextExpectedAt
		}
		if run.Schedule.GraceSeconds > 0 {
			body["grace_seconds"] = run.Schedule.GraceSeconds
		}
	}
	return c.post(ctx, project, "job", body)
}

// Release sends a deploy marker, so a regression has a timestamp to line up
// against.
func (c *Client) Release(ctx context.Context, project, version, environment string) error {
	service, env := c.identify("", "")
	return c.post(ctx, project, "release", map[string]any{
		"version": version, "environment": environment,
		"service": service, "env": env,
	})
}

// SessionBeacon is the v2 session outcome: how one run of this process ended.
// Route is a route PATTERN. SessionID is the producer's own random key and is
// idempotent on (project, session_id), so a retry at exit updates the outcome
// and never counts the session twice.
type SessionBeacon struct {
	SessionID  string `json:"session_id"`
	Release    string `json:"release,omitempty"`
	Status     string `json:"status"` // ok|crashed|abnormal
	Route      string `json:"route,omitempty"`
	DurationMs int64  `json:"duration_ms,omitempty"`
	Service    string `json:"service,omitempty"`
	Env        string `json:"env,omitempty"`
}

// Session sends the session outcome beacon. The server answers 204 and
// computes crash-free as a read; this route never folds a number.
func (c *Client) Session(ctx context.Context, project string, b SessionBeacon) error {
	b.Service, b.Env = c.identify(b.Service, b.Env)
	return c.post(ctx, project, "session", b)
}

// LogRecord is one Monolog line (the socket leg, not an HTTP route).
type LogRecord struct {
	ProjectKey  string         `json:"project_key"`
	Channel     string         `json:"channel"`
	LevelName   string         `json:"level_name"`
	Message     string         `json:"message"`
	Context     map[string]any `json:"context,omitempty"`
	Environment string         `json:"environment,omitempty"`
}

// Log writes one newline-delimited JSON record to the socket leg.
func (c *Client) Log(ctx context.Context, r LogRecord) error {
	if c == nil || c.socket == "" {
		return nil
	}
	if r.ProjectKey == "" {
		r.ProjectKey = "default"
	}
	if r.Channel == "" {
		r.Channel = "default"
	}
	if r.LevelName == "" {
		r.LevelName = "info"
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", c.socket)
	if err != nil {
		return err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetWriteDeadline(dl)
	} else {
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	}
	_, err = conn.Write(append(raw, '\n'))
	return err
}

// TraceIDFromTraceparent extracts the trace id from a W3C traceparent header,
// so a service that already propagates one can join the trace with no other
// change (the server also reads the header directly).
func TraceIDFromTraceparent(header string) string {
	parts := strings.Split(strings.TrimSpace(header), "-")
	if len(parts) < 3 || len(parts[1]) != 32 {
		return ""
	}
	return parts[1]
}

// Tracer measures one unit of work — a request, a job or a command — and the
// child spans inside it. start_ms is always an OFFSET from the tracer's own
// start, never a wall clock: two hosts with skewed clocks still order
// correctly, and the server reads only the relative value.
//
// The tracer's own span is the ROOT of the trace: it carries no parent, and
// the server builds it from the profile payload (name, duration, kind) with
// start_ms 0. Every span this tracer records is a child of that root.
type Tracer struct {
	TraceID string    // 32 lowercase hex; minted unless the caller had one
	SpanID  string    // the root span, i.e. the payload's own span
	start   time.Time // the request start every child's start_ms is relative to
	spans   []Span
}

// NewTracer starts one trace at this instant. An empty or malformed trace id
// is minted here: a producer that does not already propagate W3C trace
// context still needs an id its spans can join on.
func NewTracer(traceID string) *Tracer {
	traceID = strings.TrimSpace(traceID)
	if len(traceID) != 32 {
		traceID = RandomHex(16)
	}
	return &Tracer{
		TraceID: traceID,
		SpanID:  RandomHex(8),
		start:   time.Now(),
	}
}

// Begin opens one child span. kind says what the span is (server, client, db,
// cache, internal); peer names the callee of an outbound call, so an
// unreported hop renders with a name instead of a blank.
func (t *Tracer) Begin(kind, name, route, peer string) *Step {
	if t == nil {
		return &Step{}
	}
	return &Step{
		tracer:  t,
		started: time.Now(),
		span: Span{
			SpanID:      RandomHex(8),
			Name:        name,
			Kind:        kind,
			Route:       route,
			PeerService: peer,
		},
	}
}

// Spans is the batch to put on the profile payload, in the order they closed.
func (t *Tracer) Spans() []Span {
	if t == nil || len(t.spans) == 0 {
		return nil
	}
	out := make([]Span, len(t.spans))
	copy(out, t.spans)
	return out
}

// Step is one open child span. End computes its start_ms from the trace's own
// start, so no caller ever subtracts wall clocks across hosts.
type Step struct {
	tracer  *Tracer
	started time.Time
	span    Span
	closed  bool
}

// End closes the step and appends it to the trace. status is ok|error|unset;
// "" means ok. A negative offset or duration — a clock that moved backwards
// during the span — is clamped to zero rather than sent for the server to
// repair.
func (s *Step) End(status string) {
	if s == nil || s.tracer == nil || s.closed {
		return
	}
	s.closed = true
	s.span.StartMs = float64(s.started.Sub(s.tracer.start).Microseconds()) / 1000
	s.span.DurationMs = float64(time.Since(s.started).Microseconds()) / 1000
	if s.span.StartMs < 0 {
		s.span.StartMs = 0
	}
	if s.span.DurationMs < 0 {
		s.span.DurationMs = 0
	}
	if strings.TrimSpace(status) == "" {
		status = "ok"
	}
	s.span.Status = strings.ToLower(strings.TrimSpace(status))
	s.tracer.spans = append(s.tracer.spans, s.span)
}

// SpanID is the id this step was assigned, so a nested span can parent to it.
func (s *Step) SpanID() string {
	if s == nil {
		return ""
	}
	return s.span.SpanID
}
