// Package main is the reference port for the Nightpost wire spec v1.
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
//     sender on (#145).
//  2. NEVER COST THE REQUEST. Every send is bounded, and the failure of a
//     telemetry call is never returned into application control flow.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// ClientVersion identifies this port to the server (X-Nightpost-Client).
const ClientVersion = "nightpost-go/1.0"

// Client is the whole SDK: where to send, how to authenticate, and a bound
// HTTP client. An empty endpoint makes New return nil (see rule 1 above).
type Client struct {
	endpoint string       // e.g. http://nightpost:8000
	socket   string       // e.g. nightpost:9913, for the Monolog line
	key      string       // ingest key; "" while the project is unsigned
	http     *http.Client // never unbounded: a stuck send must not hold a worker
}

// New builds a client. Both endpoints are optional and independently inert.
func New(endpoint, socket, key string) *Client {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if endpoint == "" && socket == "" {
		return nil
	}
	return &Client{
		endpoint: endpoint,
		socket:   socket,
		key:      key,
		http:     &http.Client{Timeout: 5 * time.Second},
	}
}

// NewFromEnv reads the dual-read environment (WIRE_SPEC: NIGHTPOST_* first,
// then the legacy MONITOR_* name), so a producer migrates by adding a variable
// rather than by rewriting configuration.
func NewFromEnv() *Client {
	first := func(names ...string) string {
		for _, n := range names {
			if v := strings.TrimSpace(os.Getenv(n)); v != "" {
				return v
			}
		}
		return ""
	}
	return New(
		first("NIGHTPOST_ENDPOINT", "MONITOR_ENDPOINT"),
		first("NIGHTPOST_SOCKET", "MONITOR_SOCKET"),
		first("NIGHTPOST_KEY", "MONITOR_KEY"),
	)
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

// ErrorReport is the browser/first-party error route's payload.
type ErrorReport struct {
	Message     string `json:"message"`
	Stack       string `json:"stack,omitempty"`
	URL         string `json:"url,omitempty"`
	Route       string `json:"route,omitempty"`
	Environment string `json:"environment,omitempty"`
	Release     string `json:"release,omitempty"`
	UserID      string `json:"user_id,omitempty"`
}

// Span is one timed child of a profile.
type Span struct {
	Name       string  `json:"name"`
	DurationMs float64 `json:"duration_ms"`
	StartMs    float64 `json:"start_ms,omitempty"`
}

// Profile is a request, job or command timing.
type Profile struct {
	Name        string  `json:"name"`
	DurationMs  float64 `json:"duration_ms"`
	Environment string  `json:"environment,omitempty"`
	Type        string  `json:"type,omitempty"` // request|job|command|callgraph
	TraceID     string  `json:"trace_id,omitempty"`
	Spans       []Span  `json:"spans,omitempty"`
}

// Report sends an error. It is a no-op on an inert client.
func (c *Client) Report(ctx context.Context, project string, r ErrorReport) error {
	return c.post(ctx, project, "error", r)
}

// Profile sends a timing profile.
func (c *Client) Profile(ctx context.Context, project string, p Profile) error {
	return c.post(ctx, project, "profile", p)
}

// Query sends a slow-query sample. The server FINGERPRINTS the SQL it stores
// (numeric literals become <n>), so send the real statement.
func (c *Client) Query(ctx context.Context, project, sql string, ms float64, connection string) error {
	return c.post(ctx, project, "query", map[string]any{
		"sql": sql, "time_ms": ms, "connection": connection,
	})
}

// Job sends a job or command run.
func (c *Client) Job(ctx context.Context, project, name, kind, queue string, seconds float64, ok bool) error {
	return c.post(ctx, project, "job", map[string]any{
		"name": name, "kind": kind, "queue": queue, "seconds": seconds, "ok": ok,
	})
}

// Release sends a deploy marker, so a regression has a timestamp to line up
// against.
func (c *Client) Release(ctx context.Context, project, version, environment string) error {
	return c.post(ctx, project, "release", map[string]any{
		"version": version, "environment": environment,
	})
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
