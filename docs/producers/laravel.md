# Producer: Laravel

The Laravel-side plumbing is the `nightpost/laravel` Composer package (the
monitor client, the profiler and the `monitor` logging channel). This page is
the operational half: what to set, and in what order.

## Order of operations

**Issue the key first, then set the producer's environment — in the same
window.** Rotating or revoking a key takes effect immediately, so the gap between
the two steps is a window where ingest is refused (`401`).

1. **`/keys`** — issue a key for the project (e.g. `my-laravel-app`).
2. Set the producer env (below).
3. Reload the producer. Nothing is enabled until step 2 lands.

## Environment

Everything is off by default. Each line turns one sender on.

| Variable | Turns on |
|---|---|
| `NIGHTPOST_ENABLED=true` | the Laravel-level sender at all (the master switch) |
| `NIGHTPOST_ENDPOINT` | where it posts |
| `NIGHTPOST_PROJECT_KEY` | the project this deployment reports as |
| `NIGHTPOST_INGEST_KEY` | the key from `/keys` |
| `LOG_STACK=…,monitor` | shipping log lines to the TCP socket |
| `NIGHTPOST_LOG_SOCKET_URL` | the socket address for that leg (`tcp://monitor:9913`) |
| `NIGHTPOST_LOG_LEVEL` | which levels the default stack ships |
| `SENTRY_LARAVEL_DSN` | exception events (sentry-laravel owns this name; the DSN path is the server's project bucket) |
| `PROFILER_ENDPOINT` | xhprof callgraphs (`http://backend@monitor:8000/api/profiler/store` — the URL user is the project, the password is the ingest key) |
| `XHPROF_ENABLED` | the extension-gated profiler at all |
| `NIGHTPOST_SAMPLE_RATE` | request timing samples |
| `NIGHTPOST_PROFILE_RATE` | deep code attribution samples |
| `NIGHTPOST_SLOW_QUERY_MS` | the slow-query threshold |
| `NIGHTPOST_JOB_CAPTURE` | queue/console job capture (batched; redacted by construction) |
| `NIGHTPOST_SERVICE` | the service name every payload reports as (default: the `APP_NAME` slug) |
| `NIGHTPOST_RELEASE` | the deploy id shipped on each session beacon |
| `NIGHTPOST_SESSION_BEACON` | one session-outcome POST per session that ends (off by default) |
| `NIGHTPOST_SPANS` | child spans for database, Redis and outbound HTTP on each request profile (off by default) |

## Three traps

- **The named channels do not ride `LOG_STACK`.** `LOG_STACK` only governs the
  default channel, which is why `security`, `auth` and `moderation` are built as
  stacks of their own with their own monitor leg. Setting `LOG_STACK=monitor`
  does **not** ship those three.
- **The xhprof leg needs the extension in the image.** `PROFILER_ENDPOINT` is
  inert on a host without `xhprof` loaded — the extension ships only in the
  production image, so a local run silently produces no callgraphs and that is
  correct behaviour, not a defect.
- **A stale `LOG_STACK` entry naming a removed channel fails log resolution.**
  When upgrading an older install, check the producer's env for a leftover
  observability sink before restarting it.

## Journeys (v2)

Four settings turn this deployment from an event source into a journey hop.

- **Identity.** Every payload gains `service` and `env`.
- `service` is `NIGHTPOST_SERVICE`, else the `APP_NAME` slug, else the project key.
- `env` is the running app environment: one declaration, never a second to drift.
- **Adopt.** A valid inbound `00-<32 hex>-<16 hex>-01` keeps its trace id.
- **Own the hop.** This request mints its own span id; the caller's span id becomes the parent.
- **Mint when unusable.** Missing, malformed, unsampled or a future version is minted here — never salvaged.
- **Propagate.** The profile carries `traceparent` and `span_id`; the response echoes the same header.
- **Child spans (`NIGHTPOST_SPANS`).** One span for database work, one for Redis, each a count plus total time.
- Forty queries are one hop with a cost, not forty rows that bury the waterfall.
- One span per outbound HTTP client call, capped at 20 per request, `peer_service` set to the target host.
- Every `start_ms` is an offset from the request start, never a wall clock.
- **Session beacon (`NIGHTPOST_SESSION_BEACON`).** One POST per session, keyed by the framework session id.
- A retry updates that one row instead of counting the session twice. A 5xx reports `abnormal`.
- A request without a session is skipped, never sent under an invented id. Needs a session driver.
- `crashed` never reaches this route: a process that died has nobody left to send it.
- **Scheduled runs.** The scheduled leg adds three fields to the `/job` row.
- `schedule_cron` is the task's own expression; `next_expected_at` is the scheduler's own next-run time.
- `grace_seconds` is the overlap window a mutex-guarded task declares, and 0 when unguarded.
- Without those three, `/jobs` has a heartbeat and no promise.

## What arrives

Errors and profiles land under the project you named; they show up on the
dashboard, and a request profile whose trace id matches a callgraph's stitches
the two into one waterfall. Send `traceparent` from anything upstream that
already has a trace context and the legs join automatically.

Read [../architecture/perf-model.md](../architecture/perf-model.md) when a
journey arrives with fewer hops than you expect, and
[../WIRE_SPEC.md](../../WIRE_SPEC.md) for every field this page names.
