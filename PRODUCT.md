# PRODUCT.md

> Inferred from the explicit brief and repo code (no interview was conducted);
> assumptions are labeled. Correct anything wrong and this file becomes the
> product context of record.

## What this is

fenrirealm-monitor is the first-party observability sink for the Fenrir Realm
platform (iniznet/fenrirealm-tracker#145): one static Go binary ingesting
exceptions, logs and performance profiles from every first-party stack —
Laravel 11 backend (PHP), SvelteKit reader frontend and SvelteKit admin panel
(JavaScript/TypeScript), plus the Drive sync worker (Python) via its
Sentry-compatible DSN path if ever needed.

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

## Surface map (Operate mode)

- `/` — dashboard: per-project totals, 24h error trend, slowest routes,
  log volume.
- `/issues` — defect list with filters, trend sparklines, keyset pagination.
- `/issues/{id}` — issue detail: occurrences, decoded stack traces, raw
  payload.
- `/logs` — the Monolog stream (type=log) with level/channel/env filters,
  auto-refresh.
- `/profiles`, `/profiles/{id}` — request profiles with span waterfall and
  callgraph hotspots.
- `/slowest` — self-time leaderboard aggregated over recent callgraphs.

## Out of scope (deliberate)

- No multi-user auth: the UI binds loopback / docker-network only.
- No APM rewrite, no external SaaS dependencies.
- Producers (backend/front/admin) are documented in their own repos; this
  service only owns the wire contracts it serves.
