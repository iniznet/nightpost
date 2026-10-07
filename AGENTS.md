# fenrirealm-monitor — Agent Instructions

One static Go binary ingesting exceptions, logs and performance profiles for every first-party Fenrir Realm stack (Laravel backend = PHP, SvelteKit front/admin = JavaScript). UI is server-rendered html/template with zero runtime JS.

### Project Invariants & Reusable Conventions

- **Templates are baked at build time**: pages live in `internal/web/templates/*.html` behind `go:embed` — after editing any template or handler-embedded asset, `go build` and restart the process before browser verification; a running old binary serves stale UI and will waste a verification round (iniznet/fenrirealm-tracker#148).
- **Ingest wire contracts are compat-locked**: `POST /ingest/{project}/sentry|profile|error|xhprof`, `/api/{project}/store|envelope` (Sentry SDK), `/api/profiler/store` (spiral profiler), TCP 9913 (Monolog newline-JSON) and `GET /up` must never change shape — producers live in other repos (Laravel `MonitorClient`, SvelteKit `lib/server/monitor.ts`) and ship against these paths (iniznet/fenrirealm-tracker#145).
- **Grouping fingerprints are frozen**: `store.ForException`/`ForLog` normalize volatile tokens; changing the algorithm re-groups every existing issue on deploy — treat both functions as release-pinned (iniznet/fenrirealm-tracker#141).
- **The store must stay bounded**: `RunRetention` (age-prune occurrences, cap 50/issue, keep newest 5000 profiles) runs on the background ticker from `cmd/monitor`; never delete issues (the defect list is the history), never run retention deletes in a request path, and keep every prune chunked — the SQLite connection is single-writer and shared with ingest.
- **Migrations are additive and idempotent**: new columns/indexes are applied at boot in `migrate()` and must ignore only `duplicate column`/`already exists` errors; never drop or rename (Expand/Contract).
- **CORS is opt-in**: only the first-party error/profile endpoints honor `-cors-origin`; browser preflight has its own `OPTIONS` registration because a POST pattern never matches preflight. Default off — server-only senders need no CORS headers.
- **UI stays hydrate-free**: server-rendered SVG (sparklines, waterfall, bars), meta-refresh only on the log stream, no client state — interactive JS belongs in the producer repos, not here.
