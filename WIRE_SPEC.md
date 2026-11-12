# Wire spec v1 (FROZEN)

The boundary between a producer and this server. Anything a producer sends is
public contract; the server's UI, storage and internals are not.

**Status: v1, frozen.** After publication, changes are **additive only** — new
optional fields, new optional routes, new optional query parameters. No renames,
no removed fields, no changed meanings. Third-party SDKs are consumers we cannot
redeploy on our schedule, so a breaking change means a `v2` path, never a `v1`
edit.

## Normative vectors

`testdata/wire/*.json` are **normative**. Each file is one canonical payload
plus the assertion that it lands in the store, and `internal/ingest/wire_vectors_test.go`
runs all of them against the real handlers. A community port can run the same
files: if your implementation disagrees with a vector, the vector wins until the
spec is versioned.

```
go test ./internal/ingest/ -run TestWireVectors
```

## Transports

| Transport | Endpoint | Notes |
|---|---|---|
| HTTP | `POST /ingest/{project}/…` | preferred |
| HTTP | `POST /api/{project}/store`, `/api/{project}/store/`, `/api/{project}/envelope` | Sentry SDK compatibility |
| HTTP | `POST /api/profiler/store`, `POST /api/{project}/profiler/store` | spiral-packages/profiler compatibility |
| TCP | `:9913`, newline-delimited JSON | Monolog logging leg |

`{project}` is the tenant key: a short slug, chosen by the operator, that scopes
every read and every retention window. It is **not** a secret.

## Authentication

| Mechanism | Header | Status |
|---|---|---|
| Ingest key | `X-Monitor-Key: <key>` | preferred |
| Bearer | `Authorization: Bearer <key>` | accepted |
| HTTP Basic | `Authorization: Basic base64(<project>:<key>)` | accepted; the xhprof/profiler leg of `spiral-packages` cannot set headers |
| Unsigned | — | legacy, deprecated; only while the project has no key |

A mismatch on a keyed project answers **401**. An unknown project is admitted as
unsigned only while the operator's auto-provision budget allows; past it the
answer is **429** and no project row is created.

## Trace context

Most specific first:

1. `trace_id` in the payload (or `tags.trace_id` for xhprof),
2. `X-Monitor-Trace-Id: <id>` header,
3. W3C `traceparent: 00-<32 hex trace-id>-<16 hex parent-id>-<2 hex flags>`.

An OpenTelemetry-instrumented service needs no SDK: propagate `traceparent` and
it joins the trace. A malformed `traceparent` is **ignored**, never guessed at —
a salvaged trace key would silently group unrelated requests. An absent trace id
stays empty.

## Client version

`X-Nightpost-Client: <name>/<version>` is optional and **currently unread**. It
is reserved so a package can send it from day one; a later spec revision may
start recording it without any package changing.

## Routes and payloads

### `POST /ingest/{project}/sentry` — error event

Body: a Sentry envelope (newline-delimited header/manifest/event) or a bare
event JSON. `level`, `exception.values[].type`, `exception.values[].value` and
the stack frames are read; `request`, `user`, `environment` and `release` are
carried as context. Frames are sanitized (paths collapsed, long values
truncated) before storage.

### `POST /ingest/{project}/error` — browser error report

```json
{ "message": "…", "stack": "…", "url": "…", "route": "…",
  "environment": "production", "release": "2026.10.9-a1b2c3", "user_id": "…" }
```

`message` is required. Stored as exception class `JSError`, fingerprinted on
`message` + the top stack line. This route is CORS-enabled per project.

### `POST /ingest/{project}/profile` — request / job / command profile

```json
{ "name": "GET /chapters/1", "duration_ms": 124, "environment": "production",
  "type": "request", "trace_id": "…",
  "spans":    [ { "name": "db", "duration_ms": 61, "start_ms": 0 } ],
  "hotspots": [ { "function": "App\\Foo::slow", "self_ms": 400, "calls": 3 } ],
  "tree":     [ { "n": "main()", "s": 100, "i": 500, "c": 1 } ] }
```

`name` and a positive `duration_ms` are required (**400** otherwise). `type` is
one of `request` (default), `callgraph`, `job`, `command`.

### `POST /ingest/{project}/xhprof` — PHP callgraph

```json
{ "profile": { "main()": { "ct": 1, "wt": 500000 },
                "main()==>App\\Foo::slow": { "ct": 1, "wt": 400000 } },
  "tags": { "env": "production", "host": "node-1", "trace_id": "…" },
  "app_name": "…", "hostname": "…", "date": 1728300000 }
```

`wt` is microseconds. Key syntax is xhprof's own: `caller==>callee`, with `main()`
as the root. An empty fold (**400**) is refused.

### `POST /ingest/{project}/query` — slow query

```json
{ "sql": "select * from chapters where id = 7", "time_ms": 123.5,
  "connection": "mysql", "environment": "production" }
```

`sql` must be non-empty and `time_ms` positive (**422** otherwise). **The stored
form is a fingerprint**: standalone numeric literals are replaced (`id = 7`
becomes `id = <n>`) so `id = 5` and `id = 6` group into one aggregate and no
literal value survives into the store. Assert against the fingerprint, not the
input.

### `POST /ingest/{project}/job` — job or command run

```json
{ "name": "quests:reset-stale-progress", "kind": "command", "queue": "default",
  "seconds": 3.2, "ok": true, "count": 1, "last_trace": "…",
  "schedule_cron": "*/5 * * * *", "next_expected_at": 1794480000, "grace_seconds": 300 }
```

`kind` is `command` or `queue` (inferred when absent). Repeated runs aggregate;
a failure flips the aggregate's last status.

The three schedule fields are optional and independent of the run itself: the
producer states the promise, and the server needs no cron parser to honour it.
`next_expected_at` is unix seconds; a run that arrives after it plus
`grace_seconds` is marked missed by the sweep.

### `POST /ingest/{project}/release` — deploy marker

```json
{ "version": "2026.10.9-a1b2c3", "environment": "production" }
```

`version` is required (**422**). The newest 100 markers per project are kept;
release markers render as vertical lines on charts so a regression has a
timestamp to line up against.

### TCP `:9913` — Monolog log line

One JSON object per line:

```json
{ "project_key": "wire", "channel": "security", "level_name": "info",
  "message": "…", "context": { "…": "…" }, "environment": "production" }
```

`project_key` defaults to `default`, `channel` to `default`, `level_name` to
`info`. A line without `message` is dropped. The channel and level are
attribution: named channels ship themselves precisely so a diagnostic line is
not silently file-only.

### `POST /ingest/{project}/session` — session outcome (v2)

```json
{ "session_id": "…", "release": "2026.10.10-b2c3d4", "status": "ok|crashed|abnormal",
  "route": "/chapters/[id]", "duration_ms": 8120,
  "service": "front", "env": "production" }
```

`session_id` and `status` are required (**422**); `session_id` is the producer's
own random key, never a reader identifier. The beacon is idempotent on
`(project, session_id)`: a retry updates the outcome and never counts the
session twice. Answers **204**. Crash-free rate is a read over these rows, never
a number this route computes.

## v2 additions to every route

Every route above also accepts these OPTIONAL fields. Their absence is the v1
rollout path, never a rejection: a payload with no `service` is labelled with
the project key as its service name.

| Field | Routes | Meaning |
|---|---|---|
| `service` | sentry, error, profile, query, job, release, session | producer identity name |
| `env` | profile, query, job, release, session | producer environment |
| `kind` | profile | profile kind (`request`, `job`, `command`, …) |
| `route` | profile, error, session | route PATTERN, never a URL with ids |
| `span_id` | profile, error | the id of the payload's own span |
| `spans[]` | sentry, error, profile | the v2 span batch, below |
| `schedule_cron` | job | the cron expression the run promises, as display text |
| `next_expected_at` | job | unix seconds the next run is due; absent means unscheduled |
| `grace_seconds` | job | how late the run may be before a sweep marks it missed |

### `spans[]`

```json
{ "span_id": "16 hex", "parent_span_id": "16 hex", "kind": "db",
  "name": "db: 14 queries", "start_ms": 0, "duration_ms": 61,
  "status": "ok|error|unset", "route": "SELECT chapters", "peer_service": "cache" }
```

`start_ms` is an OFFSET from the payload's own start, **never a wall clock**:
producers run on different hosts and a join on absolute time would need clock
sync no install has. The payload's own span is the ROOT of its trace (no parent);
a child with no `parent_span_id` hangs off that root. `route` is upserted as the
service's route pattern, and `peer_service` names the callee so an unreported hop
can be rendered with a name instead of a blank.

Span fields are repaired rather than refused (a bad `span_id` is replaced, a
negative `start_ms` clamps to zero, an unknown `status` becomes `ok`), each with
its JSON path logged. A v2 span field must never turn a v1 payload into a
rejection.

### Trace identity (v2)

1. A stated `trace_id` (payload or `X-Monitor-Trace-Id`) is adopted.
2. A valid `traceparent` — exactly `00-<32 hex>-<16 hex>-01` — from a TRUSTED
   producer is adopted.
3. A valid `traceparent` from an untrusted producer is replaced by a minted id.
4. Absent or malformed means minted here.

The outcome is stored on every span as `{"trace_origin": "minted|adopted|replaced"}`.
Trust is granted per service by the operator, never claimed by the payload; a
payload with no `service` carries the project label and keeps the trace joining
the pre-v2 server already honoured.

### Admission (v2)

Beside the per-project token bucket and the coarse ingress guard, three live
settings apply, all re-read on the same refresh as the budget:

| Setting | Default | Meaning |
|---|---|---|
| `ingest_hourly` | `0` (off) | events per project per hour, beside the burst |
| `ingest_rate_errors` | `0` (off) | events/s for the errors class, its own bucket |
| `ingest_rate_profiles` | `0` (off) | events/s for the profiles class |
| `ingest_rate_logs` | `0` (off) | events/s for the logs class |
| `ingest_rate_spans` | `0` (off) | spans/s for a span batch (attached to `spans[]`) |

Every **429** carries `Retry-After` in seconds, computed from the dimension that
refused, and is decided before any body read. A span batch that does not fit its
budget is dropped while the event around it is still stored: the issue, log or
profile is the primary record.

### Digest validation and raw envelopes (v2)

| Setting | Default | Effect |
|---|---|---|
| `ingest.validate` | `strict` | `warn` logs an invalid field's JSON path (e.g. `spans[2].duration_ms`) and stores the event anyway |
| `ingest.keep_envelopes` | `0` | keep the last N raw bodies (project, received_at, route, accepted, error, body); the oldest beyond N is deleted on write |

`strict` is byte-for-byte the pre-v2 behaviour.

## OpenTelemetry (v2)

Two collector routes accept the OTLP **JSON** encoding — never protobuf, which
would add a generated-code dependency for data these routes already carry.

| Route | Envelope |
|---|---|
| `POST /v1/traces` | `resourceSpans[]` → `scopeSpans[]` → `spans[]` |
| `POST /v1/logs` | `resourceLogs[]` → `scopeLogs[]` → `logRecords[]` |

**Project.** The collector is configured with one endpoint, so the key travels as
`?project=` and falls back to the install's first configured project, then to
`default` — the same last resort the Monolog leg uses. The ingest key, when the
project has one, is presented exactly as on `/ingest/{project}/…`.

**Service identity.** `service.name` from the resource attributes is the
producer identity, admitted through the services table and falling back to the
project key when the resource carries none. `deployment.environment` (or
`deployment.environment.name`) is the environment.

**Spans.** Each span becomes a row of the `spans` table, the same table the
first-party producers write:

| Span field | Subscription |
|---|---|
| `traceId` / `spanId` / `parentSpanId` | exact OTLP width, lowercase hex (32/16/16); an all-zero parent means "no parent" |
| `kind` | `1` internal, `2` server, `3` client, `4` producer, `5` consumer |
| `status.code` | `2` error; anything else ok |
| `startTimeUnixNano` / `endTimeUnixNano` | the producer's own clock — see below |
| `attributes` | `http.route` (then `http.target`, `url.path`) is the route pattern the journey names the hop by; `peer.service` (then `net.peer.name`) names the callee |

`start_ms` is **relative to the earliest span in the same trace**, never an
absolute wall clock: two hosts with skewed clocks still order correctly, and the
journeys materialiser reads only the relative value.

**Refusal is whole-envelope.** A `traceId`, `spanId` or `parentSpanId` that is
not lowercase hex of the exact length answers **400** with a named error
(`otlp: malformed trace id`, `… span id`, `… parent span id`) and stores nothing:
a half-written trace draws a gap no reader can tell from a callee that never
reported.

**Retries are idempotent.** A span id already stored for that trace is skipped,
so a re-delivered batch never counts a hop twice. The response reports
`{"spans": n, "duplicates": n, "trace_ids": [...]}`; `/v1/logs` reports
`{"logs": n}`.

**Logs** map onto the Monolog path, so one log stream serves both producers:
`severityText` (lower-cased) is the level, `severityNumber` a fallback;
`body.stringValue` is the message; the `log.channel` attribute is the channel,
else the scope name; every other attribute is the context;
`timeUnixNano` is the clock the record was stamped with.

Both routes share the admission and raw-envelope rules above: the coarse guard
and the per-project bucket answer **429** with `Retry-After` before any body
read, the batch is charged against its item dimension (`spans`, `logs`), and
`ingest.keep_envelopes` captures the raw body under the routes `traces` and
`logs`.

## The fold contract

A callgraph arrives as a flat edge list; the server folds it into one canonical
form and both are stored:

```json
// TreeNode (the tree, canonical)
{ "n": "name", "s": 12.5, "i": 100.0, "c": 3, "k": [ …children… ] }
//   n = function name   s = SELF ms   i = INCLUSIVE ms
//   c = calls folded into the edge   k = children

// Hotspot (the flat ranking)
{ "function": "App\\Foo::slow", "file": "…", "self_ms": 400.0, "calls": 3 }
```

Rules that are part of the contract:

- `s` is self time: inclusive time minus the sum of expanded children.
- A node's children are sorted by self time, descending; hotspots are the top
  entries by self time.
- Missing child edges are folded into their parent rather than dropped, so the
  total is preserved: a producer may send a partial tree without distorting it.
- Times are milliseconds as floats, except xhprof input (`wt`, microseconds).

## Response codes

| Code | Meaning |
|---|---|
| `200` / `202` | accepted and stored |
| `400` | malformed payload — refused, never stored |
| `401` | ingest key mismatch |
| `403` | browser origin not on the project's allowlist |
| `422` | well-formed but semantically invalid (empty `sql`, non-positive `time_ms`, missing `version`) |
| `429` | a rate ceiling: the coarse global/per-address guard, or this project's own budget |
| `503` | readiness only — the store cannot answer |

A **tolerant reader** is part of the contract: unknown JSON keys are ignored, so
a newer producer degrades to a working request rather than a rejection. Nothing
in a response body is contract — only the status code.

## Compatibility promise

- Additive fields appear without notice; never key logic off the absence of a
  field that a later version adds.
- An unknown route, or an unknown `type`, is never a 500.
- `testdata/wire/` grows with every addition, so the executable spec always
  covers today's surface.
