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
