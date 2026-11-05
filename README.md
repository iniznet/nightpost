# nightpost

Self-hosted observability ingest for any PHP + JS stack (and anything else
that speaks its wire spec): errors, request profiles, callgraphs, slow queries,
job/command runs, deploy markers and log lines — into one binary and one SQLite
file, with a zero-JavaScript dashboard.

Answering the questions that actually cost uptime: *what broke, when did it
start, which deploy was live, and why was it slow.*

## Quickstart

One command, on a host with docker and nothing else:

```sh
docker run -d --name nightpost -p 8000:8000 -v np-data:/data \
  ghcr.io/iniznet/nightpost:latest
curl -fsS http://127.0.0.1:8000/ready     # 200 when the store answers
```

Open `http://<host>:8000` and finish first-run `/setup`. Then issue one ingest
key per producer at **`/keys`** — that page also holds each project's
browser-origin allowlist.

Compose works too, from a clean host:

```sh
git clone https://github.com/iniznet/nightpost && cd nightpost
docker compose -f docker-compose.quickstart.yml up -d
```

Build it yourself, still without compose:

```sh
git clone https://github.com/iniznet/nightpost && cd nightpost
docker build -t nightpost:latest .
docker run -d --name nightpost -p 8000:8000 -v np-data:/data nightpost:latest
```

`docker-compose.yml` in this repo is **our** operator deployment, not yours. It
builds locally, joins an external `dokploy-network`, and publishes no host port.
A clean host has no such network, so a bare `docker compose up` fails there on
the first step. Use `docker-compose.quickstart.yml`, or the one-liner above.

The image contract: state lives in `/data` (a named volume, and the session key
inside it), the UI listens on `8000`, and nothing ships telemetry on its own.

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
| [AGENTS.md](AGENTS.md) | project explainer, layout, commands, standards pointers |

## Licence

AGPL-3.0 ([LICENSE](LICENSE), [NOTICE](NOTICE)). Producer packages are MIT
(`nightpost/laravel`, `@nightpost/*`). Contributions are welcome — see
[CONTRIBUTING.md](CONTRIBUTING.md). The grant does not cover the Nightpost
brand, the managed control plane (a separate product, not in this
repository), or the operators' production deployment. Components linked into
the shipped binary and their licences are listed in [NOTICE](NOTICE).
