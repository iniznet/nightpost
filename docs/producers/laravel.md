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
| `MONITOR_ENABLED=true` | the Laravel-level sender at all (the master switch) |
| `MONITOR_ENDPOINT` | where it posts |
| `MONITOR_PROJECT_KEY` | the project this deployment reports as |
| `MONITOR_INGEST_KEY` | the key from `/keys` |
| `LOG_STACK=…,monitor` | shipping log lines to the TCP socket |
| `MONITOR_LOG_SOCKET_URL` | the socket address for that leg (`tcp://monitor:9913`) |
| `MONITOR_LOG_LEVEL` | which levels the default stack ships |
| `SENTRY_LARAVEL_DSN` | exception events (sentry-laravel owns this name; the DSN path is the server's project bucket) |
| `PROFILER_ENDPOINT` | xhprof callgraphs (`http://backend@monitor:8000/api/profiler/store` — the URL user is the project, the password is the ingest key) |
| `XHPROF_ENABLED` | the extension-gated profiler at all |
| `MONITOR_SAMPLE_RATE` | request timing samples |
| `MONITOR_PROFILE_RATE` | deep code attribution samples |
| `MONITOR_SLOW_QUERY_MS` | the slow-query threshold |
| `MONITOR_JOB_CAPTURE` | queue/console job capture (batched; redacted by construction) |

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

## What arrives

Errors and profiles land under the project you named; they show up on the
dashboard, and a request profile whose trace id matches a callgraph's stitches
the two into one waterfall. Send `traceparent` from anything upstream that
already has a trace context and the legs join automatically.
