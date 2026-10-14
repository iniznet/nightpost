# Write your own SDK in an afternoon

The wire spec has no SDK-shaped problem in it. A port is: marshal JSON, set one
header, POST it — which is why there is no SDK matrix and why the official
packages are treated as ports of a spec rather than the definition of it.

The reference port is **[`examples/nightpost-go`](../examples/nightpost-go)**:
one file, zero dependencies, ~150 annotated lines, covering every route plus the
socket leg. Its test runs against the **real ingest handlers**, so it proves the
port and the server agree — not that the port agrees with itself.

## The four decisions a port has to make

1. **Where does configuration come from?** Read your brand's variable first, then
   the legacy name, so a producer migrates by adding a variable rather than by
   rewriting config:
   `NIGHTPOST_ENDPOINT` → `MONITOR_ENDPOINT`, `NIGHTPOST_KEY` → `MONITOR_KEY`.
2. **Off by default, structurally.** Not "documented as off" — *unconstructible
   when unconfigured*. The reference client returns `nil`, and every method is a
   no-op on a `nil` receiver, so calling code never branches on "is telemetry
   on?". An installer then cannot silently enable a sender, because there is
   nothing to enable.
3. **Never cost the request.** A bounded timeout (5s is plenty), failures
   swallowed, and a fire-and-forget helper that recovers panic. Telemetry that
   can page you about its own failure is worse than no telemetry.
4. **Join traces for free.** Propagate the W3C `traceparent` header; the server
   reads it and links your spans to theirs with no payload change.

## Checklist

- [ ] `POST /ingest/{project}/error` with `message` (+ optional `stack`, `url`,
      `route`, `environment`, `release`).
- [ ] `POST /ingest/{project}/profile` with `name` and a positive `duration_ms`.
- [ ] `X-Monitor-Key: <key>` when the project has a key (a keyed project answers
      **401** without one).
- [ ] `X-Nightpost-Client: <name>/<version>` — optional today, reserved so your
      client is already identifying itself when the server starts recording it.
- [ ] Handle `429` by dropping the event, never by retrying into the ceiling.
- [ ] Run `testdata/wire/` against your implementation before you call it done:
      those fixtures are the spec, and the real server is tested against the
      same files.

## The two mistakes that bite later

- **Sending literal values you should not.** The *server* fingerprints SQL
  (`id = 7` → `id = <n>`) so literals never survive into the store, but that is
  the server protecting you from yourself. Do not send request bodies, tokens or
  user identifiers in a payload hoping the other side will scrub them.
- **Treating an accepted request as a stored one.** A `200`/`202` means
  *accepted*, and ingest is a tolerant reader: unknown fields are ignored on
  purpose, so your typo'ed key is dropped rather than rejected. Assert against
  the golden vectors, not against a status code.
