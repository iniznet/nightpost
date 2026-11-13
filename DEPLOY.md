# Deploy

Two audiences: an operator standing up one instance, and an operator exposing
it to the internet. Read the second half before doing the first on anything
public.

## Install (one machine, docker)

### From the published image

The release pipeline publishes a multi-arch (amd64 + arm64) image to GHCR on
every `v*` tag. No clone, no build, no network of ours:

```sh
docker run -d --name nightpost -p 8000:8000 -v np-data:/data \
  ghcr.io/iniznet/nightpost:latest
curl -fsS http://127.0.0.1:8000/ready
```

Then open `http://<host>:8000` and finish first-run `/setup`.

`latest` only ever moves on a tag, never on a push to `main`. A non-public
package needs `docker login ghcr.io` once on the host.

Use `-p 127.0.0.1:8000:8000` instead when the host is public and you reach the
dashboard through a tunnel. Ingest shares that port; read "Exposing it publicly"
below before you leave it open.

The same image through compose, on a host that is not ours:

```sh
git clone https://github.com/iniznet/nightpost && cd nightpost
docker compose -f docker-compose.quickstart.yml up -d
docker compose -f docker-compose.quickstart.yml exec monitor \
  wget -qO- http://127.0.0.1:8000/ready
```

### From source

```sh
git clone https://github.com/iniznet/nightpost && cd nightpost
docker build -t nightpost:latest .
docker run -d --name nightpost -p 8000:8000 -v np-data:/data nightpost:latest
```

### Which compose file runs where

| File | Host | What it does |
|---|---|---|
| `docker-compose.quickstart.yml` | yours | published image, one published port, one named volume |
| `docker-compose.yml` | ours | builds locally, joins the external `dokploy-network`, publishes no host port |

`docker-compose.yml` is the operator deployment. A clean host has no
`dokploy-network`, so `docker compose up` there stops at once with "network
dokploy-network declared as external, but could not be found". Use the
quickstart file on your own machine.

The dashboard session key is generated on first boot and persisted at
`/data/session.key` (0600) inside the volume — the image's `CMD` sets
`--session-secret-file` — so a restart or a redeploy does not log the admins
out. Without that flag the key would be random per boot.

`docker-compose.yml` publishes **no host port** on purpose — every consumer
reaches the service by name over the docker network, and host bindings fight
other projects on a shared node. The quickstart file publishes one, because a
first install has no docker network to ride. To reach the UI on our node ad hoc:

```sh
docker exec nightpost-production wget -qO- http://127.0.0.1:8000/up
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
| Laravel timing / MonitorClient | `NIGHTPOST_ENDPOINT` |
| SvelteKit reporter | `NIGHTPOST_URL` |

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
-require-registered      # a POST cannot MINT a project by naming it
-require-ingest-keys     # a project that has a key must present it
```

Together these close the two halves of the same door. `-require-registered`
answers **401** to a project nobody registered, so the caller-chosen `{project}`
name stops being a resource the attacker allocates; `-require-ingest-keys`
answers **401** to unsigned traffic on projects that have a key. Neither breaks
a running producer: a project that already exists without a key stays admitted
(that is the Expand/Contract path), and only *new* names are refused.

Prefer `-require-registered` first if you must stage the change — it is the
narrower refusal, and the bounded auto-provision budget exists to make a flood
pointless rather than to make unsigned ingest safe. This product build flips
`-require-registered`'s default to on; pass `-require-registered=false`
to restore the legacy auto-provision posture for an existing install.

Then tune the ceilings for the real traffic shape:

| Flag | Meaning |
|---|---|
| `-ingress-global-rate` / `-ingress-global-burst` | ceiling across **all** projects. A DoS ceiling, not a fairness limit — set it well above real aggregate volume |
| `-ingress-ip-rate` / `-ingress-ip-burst` | per source address. **Set `-ingress-ip-rate 0` behind a proxy**, where every request shares the proxy address |
| `-max-auto-projects` | how many unsigned projects may be auto-provisioned at all |
| `-require-registered` | refuse a project nobody registered, instead of auto-provisioning it |
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

## Upgrading to schema v2 — `monitor --fresh`

**Schema v2 is breaking.** A v1 volume is not migrated in place; `monitor --fresh`
is the documented upgrade path. The journeys model adds services, routes, spans,
journeys, journey_hops and sessions.

`monitor --fresh` deletes the database files before it opens the store:

| File | Why it goes, in this order |
|---|---|
| `monitor.db-wal` | sidecars first: deleting the main file while a WAL lives can resurrect stale pages |
| `monitor.db-shm` | same reason, and it is gone before the file it shares |
| `monitor.db` | the v1 schema itself |

The next boot creates the v2 schema from scratch. Nothing is carried across.

**What it destroys:**

- every issue and every occurrence behind it;
- every log line, every profile and every hotspot;
- every slow-query aggregate, job row and release marker;
- and with them the keys, organisations, admin users and saved settings.

An install whose history matters does not run this command. Decide that before
you type it, not after.

Run it once, on an install whose data you have accepted losing:

```sh
# compose: stop first, so one writer owns the volume
docker compose stop monitor
docker compose run --rm monitor --fresh
docker compose up -d

# bare docker; the image's WORKDIR is /data, so -db defaults to the volume
docker run --rm -v np-data:/data ghcr.io/iniznet/nightpost:latest --fresh

# from source
./monitor --fresh -db ./monitor.db
```

`--fresh` deletes, then boots normally. Both container forms above run the
service in the foreground.

Stop them with Ctrl-C at the ready line, then start the container for real.
From source, the flag on a normal start is enough.

- Use it when the volume predates v2, or when a store is corrupt past a restore.
- Do not use it as a routine upgrade: later migrations are additive and run at boot.
- A rollback is this same operation — the previous image against a fresh volume.

## Recovering access

```sh
docker compose exec monitor monitor -db /data/monitor.db -reset-admin you@example.com
docker compose exec monitor cat /data/admin-reset.txt   # mode 0600, read then delete
```

The password is written to a file and the path is logged; it is never printed,
because container stdout lands in the json-file log and would keep the
credential for that log's whole rotation window.
