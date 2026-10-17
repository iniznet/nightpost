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
  "seconds": 3.2, "ok": true, "count": 1, "last_trace": "…" }
```

`kind` is `command` or `queue` (inferred when absent). Repeated runs aggregate;
a failure flips the aggregate's last status.

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
