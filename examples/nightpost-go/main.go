// Command nightpost-go demonstrates the reference client against a real
// server. Run it with NIGHTPOST_ENDPOINT pointed at an instance.
//
// It is a producer, so it does every half of the v2 contract a producer owns:
// it names itself (service + env), it reports a server span as the trace root
// with one client span per outbound call, it registers the schedule it keeps,
// and it sends one session beacon on the way out.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

const (
	// DemoProject scopes every read and retention window on the server side.
	DemoProject = "demo"
	// DemoRelease is stamped on the release marker and the session beacon, so
	// a later regression has both a deploy time and a crash-free rate to read.
	DemoRelease = "2026.10.9-a1b2c3"
	// DemoEnv is the environment this example claims to run in.
	DemoEnv = "production"
	// DemoJob is the schedule entry /jobs reads. One row, one promise.
	DemoJob = "nightpost-go:demo"
	// DemoCron is the schedule the example claims. The wire contract asks a
	// producer for the next expected FIRE TIME, not for a cron parser on the
	// server, so nextFire derives it from this constant below.
	DemoCron = "*/5 * * * *"
	// DemoGraceSeconds is how long after the promised time the run may still
	// arrive before the server stamps it missed.
	DemoGraceSeconds = 60
)

func main() {
	// Flag > NIGHTPOST_* > binary name. An unset flag never erases the env var
	// NewFromEnv already read, and the derived name is always there, so the
	// payloads never arrive anonymous.
	service := flag.String("service", "", "producer identity sent as `service` (default: NIGHTPOST_SERVICE, else the binary name)")
	env := flag.String("env", "", "producer environment sent as `env` (default: NIGHTPOST_ENV)")
	flag.Parse()

	c := NewFromEnv()
	if c == nil {
		log.Fatal("set NIGHTPOST_ENDPOINT — an unconfigured client is inert by design")
	}
	c.SetIdentity(*service, *env)
	// The demo claims the same environment its release marker does, so the
	// services list and the release line agree; -env/NIGHTPOST_ENV override it.
	// An empty env is not sent as a name at all — the server then keeps the
	// v1 path rather than filing this process under an environment it never
	// claimed.
	if c.Env() == "" {
		c.SetIdentity("", DemoEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// The session beacon goes out at exit, once, with the producer's own random
	// key. It is idempotent on (project, session_id), so a retry updates the
	// outcome instead of counting this process twice.
	sessionID := RandomHex(8)
	bootedAt := time.Now()
	defer func() {
		beacon := SessionBeacon{
			SessionID:  sessionID,
			Release:    DemoRelease,
			Status:     "ok",
			Route:      "/demo",
			DurationMs: time.Since(bootedAt).Milliseconds(),
		}
		if err := c.Session(ctx, DemoProject, beacon); err != nil {
			log.Printf("session: %v", err)
		}
	}()

	// A deploy marker, so a regression later has a timestamp to line up with.
	if err := c.Release(ctx, DemoProject, DemoRelease, DemoEnv); err != nil {
		log.Printf("release: %v", err)
	}

	// One traced unit of work. The timer starts here because start_ms on every
	// child is an offset from this instant, never a wall clock another host
	// would have to agree with.
	tracer := NewTracer("")
	started := time.Now()

	// One client span per outbound call, with the callee named. A real call:
	// the liveness route of the same instance, so the duration is measured
	// rather than invented.
	call := tracer.Begin("client", "GET /up", "GET /up", "nightpost")
	callStatus := "ok"
	if err := ping(ctx, c.endpoint); err != nil {
		callStatus = "error"
		log.Printf("outbound call: %v", err)
	}
	call.End(callStatus)

	elapsed := float64(time.Since(started).Microseconds()) / 1000
	// The server builds the trace ROOT from this payload: its own span, no
	// parent, start_ms 0. kind "server" is what makes it read as the server
	// side of the hop rather than as another client leg.
	if err := c.Profile(ctx, DemoProject, Profile{
		Name:        DemoJob,
		DurationMs:  elapsed,
		Type:        "command",
		Kind:        "server",
		Route:       "/demo",
		SpanID:      tracer.SpanID,
		TraceID:     tracer.TraceID,
		Environment: DemoEnv,
		Spans:       tracer.Spans(),
	}); err != nil {
		log.Printf("profile: %v", err)
	}

	// The same run, registered with the promise it keeps. Without these three
	// fields /jobs has a heartbeat but no schedule: it could say a job ran,
	// never that a run it promised never arrived.
	if err := c.JobWithSchedule(ctx, DemoProject, JobRun{
		Name:    DemoJob,
		Kind:    "command",
		Seconds: elapsed / 1000,
		OK:      callStatus == "ok",
		Trace:   tracer.TraceID,
		Schedule: &Schedule{
			Cron:           DemoCron,
			NextExpectedAt: nextFire(time.Now()),
			GraceSeconds:   DemoGraceSeconds,
		},
	}); err != nil {
		log.Printf("job: %v", err)
	}
}

// nextFire returns the next fire time this example promises, in unix seconds.
// It is derived from DemoCron arithmetic, not from a cron parser: the producer
// is the only party that knows when it will fire next, which is why the wire
// contract carries the time instead of asking the server to parse.
func nextFire(at time.Time) int64 {
	return at.UTC().Add(5 * time.Minute).Truncate(5 * time.Minute).Unix()
}

// ping makes one real outbound call so the client span stands for work that
// actually happened. Its failure is reported as a span status, never as a
// reason to fail the process: telemetry must not cost the request.
func ping(ctx context.Context, endpoint string) error {
	if endpoint == "" {
		return errors.New("no http endpoint configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/up", nil)
	if err != nil {
		return err
	}
	res, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	if res.StatusCode >= 300 {
		return fmt.Errorf("up: %s", res.Status)
	}
	return nil
}
