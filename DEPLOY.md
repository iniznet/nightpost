# Deploy

Two audiences: an operator standing up one instance, and an operator exposing
it to the internet. Read the second half before doing the first on anything
public.

## Install (one machine, docker)

### From the published image

The release pipeline publishes a multi-arch (amd64 + arm64) image to GHCR on
every `v*` tag — `ghcr.io/iniznet/fenrirealm-monitor`. To use it instead of
building locally, point compose at the image:

```yaml
services:
  monitor:
    image: ghcr.io/iniznet/fenrirealm-monitor:latest
    # ...the rest of docker-compose.yml unchanged
```

`latest` only ever moves on a tag, never on a push to `main`. A non-public
package needs `docker login ghcr.io` once on the host.

### From source

```sh
git clone <repo> && cd fenrirealm-monitor
printf 'APP_ENV=production\n' > .env
docker compose up -d --build
docker compose exec monitor wget -qO- http://127.0.0.1:8000/ready
```

The dashboard session key is generated on first boot and persisted at
`/data/session.key` (0600) inside the volume — the image's `CMD` sets
`--session-secret-file` — so a restart or a redeploy does not log the admins
out. Without that flag the key would be random per boot.

Then open the UI (through the docker network or a temporary port override) and
finish first-run `/setup`.

`docker-compose.yml` publishes **no host port** on purpose — every consumer
reaches the service by name over the docker network, and host bindings fight
other projects on a shared node. To reach the UI ad hoc:

```sh
docker exec fenrirealm-monitor-production wget -qO- http://127.0.0.1:8000/up
```

### Senders are never enabled silently

The #145 discipline: **every sender is off until an operator switches it on.**
An install enables nothing, and no `docker compose up` starts shipping
telemetry anywhere. Enabling a sender is always one explicit line in the
producer's environment:

| Producer | Variable |
|---|---|
| Laravel logs | `LOG_STACK` gains `monitor` |
| Laravel exceptions | `SENTRY_LARAVEL_DSN` |
| xhprof callgraphs | `PROFILER_ENDPOINT` |
| Laravel timing / MonitorClient | `MONITOR_ENDPOINT` |
| SvelteKit reporter | `MONITOR_URL` |

Removing the variable turns the sender off again. Nothing is inferred, and no
installer writes these for you.

### Things to set before you rely on it

| Setting | Why |
|---|---|
| `-session-secret-file /data/session.key` | already set by the image's `CMD`; without it the session key is random per boot and every deploy logs the admins out |
| `-cookie-secure` | set it when TLS terminates at a proxy: the connection here is then plain HTTP, so the cookie cannot be auto-marked Secure |
| `-alerts-config /run/secrets/alerts.json` | mounted file, channels + rules (§ alerts) |
| a named volume | the SQLite file *is* the retention policy |
| `-retention-days` | occurrences older than this are pruned; issues are kept |

## Exposing it publicly

The dashboard and ingest have different risk profiles. Expose ingest; be
deliberate about the dashboard (it is authenticated, but it is still a single
admin surface).

### TLS is not optional

The session cookie is `HttpOnly` + `SameSite=Lax`, and `Secure` when the
request itself is TLS. Behind a TLS-terminating proxy the connection to this
server is plain HTTP, so that cannot be detected — set **`-cookie-secure`** to
declare it. It is deliberately not inferred from `X-Forwarded-Proto`: that
header is caller-controlled, and trusting it would let a plain-HTTP request
mint a cookie the browser then sends in the clear.

### Proxy rules

Expose `/ingest/*` and the `/api/*` compatibility routes — nothing else needs
to be public. Keep `/up` and `/ready` reachable by the orchestrator only.

```caddy
monitor.example.com {
    @ingest path /ingest/* /api/*
    reverse_proxy @ingest 127.0.0.1:8000

    @health path /up /ready
    reverse_proxy @health 127.0.0.1:8000

    # the dashboard stays on the private network
    respond 404
}
```

```nginx
server {
    listen 443 ssl;
    server_name monitor.example.com;

    location /ingest/ { proxy_pass http://127.0.0.1:8000; }
    location /api/    { proxy_pass http://127.0.0.1:8000; }
    location = /up    { proxy_pass http://127.0.0.1:8000; }
    location = /ready { proxy_pass http://127.0.0.1:8000; }
    # dashboards are reached over the tunnel/private network, not here
}
```

### Mandatory once ingest is public

```sh
-require-ingest-keys
```

Unsigned ingest then answers 401: with the surface public, an unauthenticated
POST that auto-provisions a project is an open door, and the bounded
auto-provision budget exists to make the flood pointless — not to make unsigned
ingest safe.

Then tune the ceilings for the real traffic shape:

| Flag | Meaning |
|---|---|
| `-ingress-global-rate` / `-ingress-global-burst` | ceiling across **all** projects. A DoS ceiling, not a fairness limit — set it well above real aggregate volume |
| `-ingress-ip-rate` / `-ingress-ip-burst` | per source address. **Set `-ingress-ip-rate 0` behind a proxy**, where every request shares the proxy address |
| `-max-auto-projects` | how many unsigned projects may be auto-provisioned at all |
| `-ingest-rate` / `-ingest-burst` | per-project fairness budget |

`X-Forwarded-For` is deliberately **not** trusted: the header is caller-
controlled, and honouring it would let a flood mint a fresh per-address bucket
per request. Behind a proxy the global ceiling is the real limit.

### Per-project browser origins

An in-page reporter needs CORS, and the allowlist is per project: set it at
`/keys` for each tenant. `-cors-origin` only seeds a project whose row is empty.
A wildcard is refused — these routes accept unauthenticated browser posts. An
origin that is present but not on the list answers 403 instead of silently
dropping the report.

## Health contract

| Endpoint | Kind | Checks | Answers |
|---|---|---|---|
| `GET /up` | liveness | the process answers | `200`, never touches the database |
| `GET /ready` | readiness | a real database query | `200 {"status":"ok","db":"ok","revision":"…"}` or `503` |

Point **restart** logic at `/up` and **traffic** gating at `/ready`. `/up`
ignores the database on purpose: a container whose store is briefly unavailable
must not be killed and restarted into the same problem. Both are unauthenticated
and both stay minimal — no tenant figures.

## Upgrading

```sh
git pull
docker compose up -d --build     # migrations run at boot
docker compose exec monitor wget -qO- http://127.0.0.1:8000/ready
```

- **Migrations are additive and run automatically at boot** (`ALTER TABLE ADD
  COLUMN`, `CREATE TABLE IF NOT EXISTS`, with a `PRAGMA user_version` guard).
  No manual step, and a downgrade ignores the new columns rather than failing.
- **Settings saved in the UI win over flags.** CLI flags are import-once
  defaults: they seed a key that has never been saved, and after that the stored
  value is authoritative — including across this file being re-applied. To
  force a flag value, change it on `/settings`.
- **Back up the volume before upgrading.** SQLite in WAL mode plus a running
  writer means a copy of `monitor.db` alone can be torn; copy
  `monitor.db`, `-wal` and `-shm` together, or stop the container first.
- **Removed things stay removed.** The pre-#145 observability sink was deleted
  outright: a stale `LOG_STACK` entry naming it now fails log resolution. Check
  producer env for a leftover sink entry when upgrading an older install.

## Recovering access

```sh
docker compose exec monitor monitor -db /data/monitor.db -reset-admin you@example.com
docker compose exec monitor cat /data/admin-reset.txt   # mode 0600, read then delete
```

The password is written to a file and the path is logged; it is never printed,
because container stdout lands in the json-file log and would keep the
credential for that log's whole rotation window.
