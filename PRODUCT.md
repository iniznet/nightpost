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
health · `/keys` per-project ingest keys.

History: the pre-M6.1 paths (`/issues`, `/logs`, `/profiles`, `/slowest`,
`/traces`, `/traces/{id}`) keep serving the same content (Expand/Contract).

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
