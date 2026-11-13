# Producer: Go (reference)

`examples/nightpost-go` is the reference producer: one package, the standard library, no dependency to vendor.
Read it when you are porting the wire contract to a new language, or want a journey you can watch arrive.
The payload shapes are [WIRE_SPEC.md](../../WIRE_SPEC.md); this page is the half a producer owns.

## Environment and flags

| Variable | Turns on |
|---|---|
| `NIGHTPOST_ENDPOINT` | the HTTP ingest base URL, e.g. `http://127.0.0.1:8000` |
| `NIGHTPOST_SOCKET` | the Monolog TCP leg, `host:9913`; independent of the HTTP leg |
| `NIGHTPOST_KEY` | the ingest key from `/keys` |
| `NIGHTPOST_SERVICE` | the identity every payload reports as |
| `NIGHTPOST_ENV` | the environment every payload reports as |

- `New` returns `nil` when neither endpoint nor socket is set, and every method is
  a no-op on a nil receiver. A caller never branches on "is telemetry on?".
- Identity precedence is flag, then `NIGHTPOST_*`, then the binary's own file name.
  An unset flag never erases the env value, and a payload never arrives anonymous.
- An empty `NIGHTPOST_ENV` is not sent at all: the server then keeps the v1 path
  rather than filing the process under an environment it never claimed.
- `NIGHTPOST_*` is the only namespace. A legacy name is inert by design, so a
  half-migrated host reports nothing rather than posting somewhere nobody meant.

## What one run sends

The demo makes four calls, and each one is a different lesson.

| Call | Route | Carries |
|---|---|---|
| `Release` | `/ingest/{project}/release` | the deploy id, so a regression has a timestamp |
| `Profile` | `/ingest/{project}/profile` | the server root span and its client children |
| `JobWithSchedule` | `/ingest/{project}/job` | one run plus the promise it keeps |
| `Session` | `/ingest/{project}/session` | how the run ended, sent at exit |

`Log` is the fifth leg the client offers: one newline-delimited record over
`NIGHTPOST_SOCKET`, used when an install wants the Monolog stream too.

## Root span and children

- The profile's own span is the ROOT of the trace: no parent, `start_ms` 0, kind
  `server`. The server builds it from `name`, `duration_ms` and the payload's span id.
- `Tracer.Begin(kind, name, route, peer)` opens one child. `End(status)` closes it
  and computes `start_ms` as an offset from the tracer's own start.
- A client span names its callee through `peer_service`, so a callee that never
  reported still renders as a named gap.
- `NewTracer("")` mints a trace id. Pass one in when an upstream already
  propagated a trace; `TraceIDFromTraceparent` reads the W3C header for that.
- `start_ms` is always relative. Two hosts with skewed clocks still order
  correctly, and no install has the clock sync an absolute join would need.

## The schedule it promises

`JobWithSchedule` adds three fields to the run's row, and they are the whole
difference between a heartbeat and a check.

- `schedule_cron` is the expression the producer claims.
- `next_expected_at` is unix seconds, computed by the producer. The server never
  parses a cron: the producer is the only party that knows when it fires next.
- `grace_seconds` is how long past that time the monitor waits before stamping
  the run `missed`. A stamped run alerts once.

The demo derives its next fire time with `nextFire`, five-minute arithmetic
rather than a parser, because that is the honest size of the contract.

## The session beacon

- One beacon per process, sent at exit from a deferred call, with the producer's
  own random key from `RandomHex(8)`. It is never a reader or user identifier.
- The server is idempotent on `(project, session_id)`, so a retry updates the
  outcome instead of counting the session twice. It answers 204.
- `status` is one of `ok`, `crashed`, `abnormal`. Crash-free is a read over these
  rows on `/releases`, never a number this client computes.

## Run it against a live monitor

```sh
go run ./examples/nightpost-go            # NIGHTPOST_ENDPOINT must be set
go test ./examples/nightpost-go           # the wire shapes, no server needed
```

- Every send is bounded by a five-second client, and a failed send is logged, never returned.
- Telemetry must never cost the request it measures.
- `Client.Go` sends in the background and recovers a panic, so a bug here cannot take the host down.

The run reports under project `demo`, so `/performance` shows one `api` journey:
`/demo`, then `GET /up` as a gap.

The gap is honest while nothing reports that callee. Row construction is in
[../architecture/perf-model.md](../architecture/perf-model.md).
