# Producer: anything else (curl)

If your stack has no client and you do not want to write one, these are the
requests. Read [WIRE_SPEC.md](../WIRE_SPEC.md) for the full contract; the JSON
below is copied from the executable vectors in `testdata/wire/`.

Set the key once:

```sh
M=http://monitor:8000            # or https://nightpost.example.com
K=REPLACE_WITH_KEY_FROM_KEYS     # omit entirely while the project is unsigned
H='content-type: application/json'
```

**Deploy marker** — the cheapest useful thing you can send; it puts a line on
every chart so a regression has a timestamp to line up against.

```sh
curl -sS -X POST "$M/ingest/myapp/release" -H "$H" -H "x-monitor-key: $K" \
  -d '{"version":"2026.10.9-a1b2c3","environment":"production"}'
```

**Error**

```sh
curl -sS -X POST "$M/ingest/myapp/error" -H "$H" -H "x-monitor-key: $K" \
  -d '{"message":"user 42 not found","stack":"at index.php:42",
       "url":"https://example.com/chapters/1","route":"/chapters/[id]",
       "environment":"production","release":"2026.10.9-a1b2c3"}'
```

**Request timing** (add `spans` for a breakdown, `traceparent` to join a trace)

```sh
curl -sS -X POST "$M/ingest/myapp/profile" -H "$H" -H "x-monitor-key: $K" \
  -H 'traceparent: 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01' \
  -d '{"name":"GET /chapters/1","duration_ms":124,"environment":"production",
       "spans":[{"name":"db","duration_ms":61,"start_ms":0}]}'
```

**Slow query** — send the real statement; the server fingerprints it, so `id = 7`
is stored as `id = <n>` and no literal value survives into the store.

```sh
curl -sS -X POST "$M/ingest/myapp/query" -H "$H" -H "x-monitor-key: $K" \
  -d '{"sql":"select * from chapters where id = 7","time_ms":123.5,
       "connection":"mysql","environment":"production"}'
```

**Job / command run**

```sh
curl -sS -X POST "$M/ingest/myapp/job" -H "$H" -H "x-monitor-key: $K" \
  -d '{"name":"quests:reset-stale-progress","kind":"command","queue":"default",
       "seconds":3.2,"ok":true}'
```

**Log line** — the socket leg, not HTTP: newline-delimited JSON on TCP 9913.

```sh
printf '%s\n' '{"project_key":"myapp","channel":"security","level_name":"info",
  "message":"turnstile rejected","environment":"production"}' \
  | nc monitor 9913
```

## Reading the answers

| Code | Meaning |
|---|---|
| `200` / `202` | accepted and stored |
| `400` | malformed payload — refused, never stored |
| `401` | key mismatch, or the project is not registered |
| `403` | browser origin not on the project's allowlist |
| `422` | well-formed but invalid (empty `sql`, non-positive `time_ms`, missing `version`) |
| `429` | a rate ceiling — drop the event, do not retry into it |

Unknown JSON keys are **ignored on purpose**: ingest is a tolerant reader, so
your typo is dropped rather than rejected. Verify a new integration against
`testdata/wire/`, not against a status code.
