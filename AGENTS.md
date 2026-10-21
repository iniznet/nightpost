# nightpost — Agent Instructions

Nightpost is a self-hosted, single-node application monitor: one static Go
binary that ingests exceptions, logs and performance profiles from any
producer stack (first-party producers today: a Laravel backend, two SvelteKit
apps, Go examples) and renders a server-side dashboard with **zero runtime
JavaScript** — html/template, SVG sparklines, meta-refresh. Storage is a
single SQLite database (one WAL writer). The product thesis: the sender costs
nothing when it is off, the whole thing is one binary you run yourself, and
AGPL keeps hosted competitors honest. Licence split: see `LICENSE`,
`NOTICE`, `CONTRIBUTING.md` and README's Licence section.

### Repo layout

| Path | What lives there |
|---|---|
| `cmd/monitor` | the binary: flags, boot wiring, retention ticker, route order |
| `internal/ingest` | wire contracts: Sentry/error/profile/xhprof/Monolog-TCP, key gate, shaper, CORS, trace context |
| `internal/store` | SQLite schema, migrations, queries, retention, fingerprints |
| `internal/web` | SSR dashboard: handlers, params, auth, `templates/*.html` (go:embed) |
| `internal/alerts` | rule engine + channels (discord, email), file-configured |
| `testdata/wire/` | golden wire vectors — the executable half of `WIRE_SPEC.md` |
| `docs/producers/` | per-stack integration guides (Laravel, SvelteKit, webhook) |
| `examples/nightpost-go/` | minimal Go producer example |

### Commands

- Build + run: `go build ./... && ./monitor` (first run seeds an admin via `/setup`).
- Tests: `go test ./...` — all packages green is the local gate.
- Race detector runs in CI only (see the invariant below); locally its substitute is `internal/ingest/concurrency_test.go`.
- Wire-contract check: `go test ./internal/ingest -run TestWireVectors`.
- Dependency licence audit: `go list -deps ./cmd/monitor` (every entry must be permissive; see NOTICE).

### Standards pointers

- `WIRE_SPEC.md` — the frozen ingest boundary; additive-only once published.
- `DEPLOY.md` — install, reverse-proxy ingress checklist, health contract, upgrades.
- `README.md` — pitch, quick start, licence split.
- Producer repos ship against the wire contract; this repo never reaches into them.
