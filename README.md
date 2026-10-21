# nightpost

Self-hosted observability ingest for any PHP + JS stack (and anything else
that speaks its wire spec): errors, request profiles, callgraphs, slow queries,
job/command runs, deploy markers and log lines — into one binary and one SQLite
file, with a zero-JavaScript dashboard.

Answering the questions that actually cost uptime: *what broke, when did it
start, which deploy was live, and why was it slow.*

## Quickstart

```sh
git clone <this repo> && cd nightpost
printf 'APP_ENV=production\n' > .env
docker compose up -d --build
docker compose exec monitor wget -qO- http://127.0.0.1:8000/ready
```

The compose file publishes **no host port** on purpose: consumers reach it by
service name, and host bindings fight other projects on a shared node. Open the
UI through the docker network, or a temporary port override, and finish first-run
`/setup`. Then issue an ingest key per producer at **`/keys`** — that page also
holds each project's browser-origin allowlist.

**Nothing ships telemetry until you say so.** Every sender is off until an
operator adds one environment line to a *producer*; an install enables nothing,
and no `docker compose up` starts sending anything anywhere (#145).

## Wire in a producer

| Producer | Guide |
|---|---|
| Laravel | [docs/producers/laravel.md](docs/producers/laravel.md) |
| SvelteKit / Node | [docs/producers/sveltekit.md](docs/producers/sveltekit.md) |
| Anything else | [docs/producers/webhook.md](docs/producers/webhook.md) and [WIRE_SPEC.md](WIRE_SPEC.md) |

Writing your own client is an afternoon, not a project: the spec has no
SDK-shaped problem in it. [examples/nightpost-go](examples/nightpost-go) is a
complete, zero-dependency reference port (~150 annotated lines) that is tested
against the real ingest handlers, and [docs/write-your-own-sdk.md](docs/write-your-own-sdk.md)
walks through it.

## Operate it

- [DEPLOY.md](DEPLOY.md) — install, reverse-proxy ingress, the exposure
  checklist, upgrades, recovery.
- Health: `GET /up` (liveness, never touches the database) and `GET /ready`
  (readiness, real query + the ingest-rejection counters).

## What this deliberately is not

No session replay. No SSO/SAML. No 100-package SDK matrix — depth in PHP and JS,
other languages over the wire spec. No mobile apps, no marketplace, no
multi-region hosted scale. Each of those was considered and refused; the depth in
two ecosystems is the wedge, not breadth.

## Docs

| | |
|---|---|
| [WIRE_SPEC.md](WIRE_SPEC.md) | the frozen producer/server contract (v1, additive-only) |
| [docs/README.md](docs/README.md) | docs index, and why there is no site generator |
| [DEPLOY.md](DEPLOY.md) | install, ingress, upgrade, recovery |
| [PRODUCT.md](PRODUCT.md) / [DESIGN.md](DESIGN.md) | what it is and how it is shaped |
| [AGENTS.md](AGENTS.md) | the invariants this codebase is held to |

## Licence

AGPL-3.0 ([LICENSE](LICENSE), [NOTICE](NOTICE)). Producer packages are MIT
(`nightpost/laravel`, `@nightpost/*`). Contributions are DCO-signed — see
[CONTRIBUTING.md](CONTRIBUTING.md). The grant does not cover the Nightpost
brand, the managed control plane (a separate product, not in this
repository), or the operators' production deployment. Components linked into
the shipped binary and their licences are listed in [NOTICE](NOTICE).
