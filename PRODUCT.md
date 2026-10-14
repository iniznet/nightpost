# PRODUCT.md

> Inferred from the explicit brief and repo code (no interview was conducted);
> assumptions are labeled. Correct anything wrong and this file becomes the
> product context of record.

## What this is

fenrirealm-monitor is the first-party observability sink for the Fenrir Realm
platform (iniznet/fenrirealm-tracker#145): one static Go binary ingesting
exceptions, logs and performance profiles from every first-party stack — Laravel
11 backend (PHP), SvelteKit reader frontend and SvelteKit admin panel
(JavaScript/TypeScript), plus the Drive sync worker (Python) via its
Sentry-compatible DSN path if ever needed.

The M6 program (iniznet/fenrirealm-tracker#205) productizes this service into
**Nightpost** (nightpost.dev) — a self-hosted, one-binary observability product
for PHP + JS stacks. M6.1 (IA consolidation) is the UX foundation and must land
before any fork.

## Who uses it

The fenrirealm developers, on-call or investigating after a deploy. Sessions
are short and goal-directed: "something is slow/broken right now — which
defect, where, since when".

## Product success

- An on-call developer identifies the current top defect and its last
  occurrence in under 10 seconds from the dashboard.
- A stack trace on the detail page names the failing frame for BOTH stack
  flavors we store: PHP (Sentry frames) and JavaScript (first-party reports).
- Performance regressions surface as route latency (request profiles) and
  function self-time (callgraph hotspots) without opening a spreadsheet.
- The sink survives production volume: bounded retention, no unbounded
  growth, ingest never blocked by UI work.
- A reader asking "why is my data missing?" gets an answer from the page
  footer, not from a support thread.

## Surface map (Operate mode)

Two consolidated sections, each with tabs, plus the standalone surfaces. Every
page carries the same persistent toolbar (project · time range · env) and the
same footer ingest-health strip.

**Errors** — `/errors`
- *Issues* (default): error-class defect list, filters, trend sparklines,
  keyset pagination, per-fingerprint detail at `/issues/{id}`. Log-type
  pseudo-issues are folded out of this stream.
- *Logs*: the Monolog stream (type=log) with level / channel / env filters and
  a 15 s refresh, cross-linked with Issues.

**Performance** — `/performance`
- *Endpoints* (default): request profiles aggregated by route (count · p50 ·
  p95 · total · avg · max), each route drilling into the raw samples behind it
  and from there into a cross-service trace.
- *Traces*: one row per cross-service request — legs, total window, a
  has-tree badge for the call-tree drill-down, and the depth-cap note.
- *Hotspots*: self-time leaderboard over recent callgraphs, with an
  app-vs-framework scope filter, proportional bars, a real file column and
  pagination.

**Standalone** — `/` dashboard (triage cards per project: open issues, errors,
new-since-release, p95 delta vs the previous window, error spark, deep links
into both sections) · `/queries` slow statements · `/jobs` queue/command
health · `/orgs` organizations (create an org, move projects under it) ·
`/keys` per-project ingest keys · `/settings` operator settings.

History: the pre-M6.1 paths (`/issues`, `/logs`, `/profiles`, `/slowest`,
`/traces`, `/traces/{id}`) keep serving the same content (Expand/Contract).

## Settings (M6.3)

Operator-tunable values live in the **database**, edited at `/settings`: the
retention window, the prune interval, the per-fingerprint occurrence cap, the
profiles kept per project, and the ingest rate/burst. The process flags are
**import-once defaults** — a key is seeded only when absent, so a value saved
in the UI wins from then on, across restarts and redeploys. The retention loop
re-reads its knobs every cycle (marked *live* on the page); the ingest
rate/burst are built into the shaper at boot (marked *restart*), and the page
says so rather than pretending otherwise.

## Tenancy (M6.2)

An **org** groups projects; every project carries an additive `org_id` and
every project not explicitly assigned belongs to `Default`. Assignment never
touches a project's key, so no producer is reconfigured to become tenanted —
the ingest surface stays project-grain and budgets stay per project (the
profile keep-window is per project, so a noisy sibling cannot evict another
org's profiles and traces). Dashboard accounts get a role per org
(`owner` / `admin` / `viewer`), seeded from the M4 single admin as owner of
`Default`. Selecting an org in the toolbar scopes the project list,
dashboard and keys pages; the org hierarchy is NOT an authentication
boundary yet — role enforcement lands with M6.3's settings pages.

## Scale: the single-node ceiling (M6.4)

Every ingest write serializes through ONE SQLite WAL writer, so "how many apps
can one binary take?" has a measurable answer rather than a vibe. Measured on
this repo's benchmark dataset (20,620 issues, 196,500 occurrences, 60,000
profiles, ~24 MB on disk):

| Operation | Measured |
|---|---|
| Ingest writes (profiles) | **6,500-9,500 events/s** (2,000 writes in 219-305ms) |
| `Issues` (errors, newest 50) | 5ms |
| `Issues` (logs, newest 50) | 28ms |
| `IssueTotal` / `LogLevels` | 3.5ms / 9ms |
| `HourBuckets` (24h, the slowest) | 132ms |
| `Profiles` (newest 50) | 0.5ms |
| `Traces` (24h) | 6ms |
| Full retention cycle | ~2.9s |

Reproduce with `MONITOR_SCALE=1 go test ./internal/store/ -run TestScaleLatency -v`
(env-gated so CI never pays for it; `MONITOR_SCALE_DB=<path>` writes a real file
so you can point a server at the same data).

The number to plan with is the **write** ceiling: roughly one order of magnitude
below 10k events/s on commodity hardware, and it does not improve with more
cores because there is one writer. That is the honest limit of the
one-binary-one-file design, and it is far above what a mid-size application
produces (a 500 req/s app sampling 5% is ~25 events/s). Past it, the answer is
more instances with different projects, not a bigger instance.

**Hourly rollups were considered and NOT built.** The phase was conditional on
measured dashboard slowness, and the measurement says the dashboard is fine: one
query exceeds 100ms, everything else is under 30ms. Materializing hourly buckets
would have added a write-path cost, a second source of truth and a rebuild job to
fix a 132ms read. The one slow query was fixed instead by letting it drive from
`issues` through the covering index already on the table (213ms -> 132ms), after
measuring that a new index traded 31% of ingest throughput for it and was
rejected.

## Retention, and what bounds the `issues` table (M6.4)

Retention bounds raw volume by age and by per-issue cap, and now also bounds the
issue table itself:

- **Occurrences** are deleted past the retention window, and capped per issue so
  one high-frequency fingerprint cannot flood the table.
- **Profiles** keep a per-project window, so one noisy project cannot evict
  another's profiles.
- **Issues are archived, never deleted.** The defect list is history, so a row
  whose occurrences retention has already removed, and which has not been seen
  since, is marked archived: it leaves the live lists and counts, but its count
  and first-seen date survive. This exists because every distinct log message is
  an `issues` row — a chatty app produced 20,000 of them in a single day of real
  volume — so without it the table grows for the life of the install.
- **An archived issue that RECURS is un-archived** by the next occurrence of the
  same fingerprint. A one-way archive would hide a returning defect forever,
  which is worse than an unbounded table.
- **Archiving never removes a project.** Project discovery reads issues without
  the archive guard on purpose, so a project whose issues are all archived stays
  listable at `/keys` — otherwise archiving could lock an operator out of
  rotating its key.

## Install posture (M6.5)

Three things a self-hosted install must get right by default, because getting
them wrong is silent rather than loud:

- **The dashboard session key survives restarts.** Point
  `-session-secret-file` at a mounted path and the generated key is persisted
  there (0600). Without it the key is random per boot and every deploy logs the
  admins out.
- **A recovered password is never written to a log.** `-reset-admin` writes it
  to a 0600 file and names that path; container stdout lands in the json-file
  log, which would keep the credential for the log's whole rotation window.
- **The browser allowlist is per project.** A tenant's front end is a different
  origin, so the allowlist lives on the project row (edit it at `/keys`) and
  `-cors-origin` is only the seed for a row that is empty. A wildcard is
  refused: these routes accept unauthenticated browser posts.

### Health contract

Two endpoints, two different jobs — pointing a check at the wrong one is the
usual way this goes wrong:

| Endpoint | Kind | Checks | Answers |
|---|---|---|---|
| `GET /up` | liveness | nothing but "the process answers" | always `200` |
| `GET /ready` | readiness | a real database query | `200 {"status":"ok","db":"ok","revision":"…"}` or `503` with the error |

`/up` never touches the database on purpose: a container whose database is
briefly unavailable should not be killed and restarted into the same problem.
Gate traffic on `/ready`; restart on `/up`.

### Joining traces without a Nightpost client

Point any OpenTelemetry-instrumented service at an ingest route and propagate
the W3C `traceparent` header — no SDK, no payload field. Precedence is the
payload's own `trace_id`, then `X-Monitor-Trace-Id`, then `traceparent`. A
malformed `traceparent` is ignored rather than guessed at, so it can never
group unrelated requests under one key.

## Ingress posture (M6.10)

Ingest is safe behind the docker network, but it is hardened for public
ingress because one property cannot be relied on forever: the `{project}` path
segment is chosen by the caller. Three ceilings make a distinct-name flood
pointless — a **global** ceiling across all projects, a **per-source-address**
ceiling, and a **bounded** auto-provision budget (past `-max-auto-projects` a
new unsigned name is refused instead of minting a project row).
`-require-ingest-keys` turns unsigned ingest off entirely for a deployment
exposed through a reverse proxy. `X-Forwarded-For` is deliberately not
trusted, so behind a proxy the operator sets `-ingress-ip-rate 0` and the
global ceiling does the work. The footer strip counts every leg.

## Access posture

The dashboard is auth-gated by an HMAC session cookie with first-run `/setup`
that closes permanently once the first admin exists; `/up` and the ingest
routes stay unauthenticated-by-design and are gated by per-project ingest keys.
Multi-tenancy (org → project hierarchy) is M6.2, and public-ingress hardening
(per-project CORS, auto-provision throttling) is M6.10 — both tracked in
iniznet/fenrirealm-tracker#205. Never serve the dashboard over plain HTTP at a
public ingress without TLS.

## Out of scope (deliberate)

- No APM rewrite, no external SaaS dependencies.
- No runtime JavaScript in the UI (server-rendered HTML + inline CSS only).
- Producers (backend/front/admin) are documented in their own repos; this
  service only owns the wire contracts it serves.
