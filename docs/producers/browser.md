# Producer: browser (`@nightpost/js`)

The browser half of `@nightpost/js` sends two things: one routed error event and one session beacon.
Read it if you report from a page rather than a server, or you are explaining why a page hop has no children.
It is inert without `NIGHTPOST_URL`; the wire fields are in [WIRE_SPEC.md](../../WIRE_SPEC.md).

## Environment

| Variable | Turns on |
|---|---|
| `NIGHTPOST_URL` | the endpoint the page posts to; unset means every call below is a no-op |
| `NIGHTPOST_PROJECT_KEY` | the project, defaulting to `front` |
| `NIGHTPOST_KEY` | the key from `/keys`, sent as `x-monitor-key` |
| `NIGHTPOST_SERVICE` | the service name this origin reports as |

`NIGHTPOST_SERVICE` falls back to the host label of the page's origin, then to
the project key. A raw URL is never used as a name.

The error route is CORS-enabled per project. Add the origin at `/keys`; an
unlisted origin answers 403 rather than losing the report.

## The routed error payload

`reportBrowserError` posts one `POST /ingest/{project}/error`.

- `message` is required. An empty message sends nothing at all.
- `route` is the normalised pathname, never the URL the reader was on.
- `url` is origin plus pathname. The query string and the fragment are dropped
  first, because a token or a row id in either is exactly what must not travel.
- `service` names this hop, so the error is attributable without a second lookup.
- `stack`, `environment`, `release` and `user_id` are optional and only sent when
  the caller supplies them.

Normalisation collapses id-shaped segments to `[id]`.

- Id-shaped means bare digits, uuids, eight-plus hex keys, ulids, and number-bearing slugs like `chapter-12`.
- A plain word such as `/tags/fantasy` stays, because a false positive merges two real routes.
- The result is capped at 255 characters, the server's field limit.

Two readers on `/chapters/7` and `/chapters/8` are one route. One defect is one
issue, and one hop is one chart line.

## The session beacon

`installSession` wires one beacon to the page lifecycle.

- The key is a random id kept in `sessionStorage` under `nightpost_session_id`,
  so every page load of one tab reports the same session. It is never a reader
  identity; a storage-disabled page falls back to a per-page key.
- A completed navigation reports `ok`. An unhandled `error` or
  `unhandledrejection` reports `crashed`. A page that ends before its navigation
  completed reports `abnormal`.
- The beacon is sent on `pagehide` through `navigator.sendBeacon`, with a
  `keepalive` fetch where the browser has none. Nothing in the page awaits it.
- The server answers 204 and is idempotent on `(project, session_id)`, so a retry
  updates the outcome instead of counting the session twice.

## Why the browser sends no spans

- The browser is the entry of its own trace, and it has no children to report:
  the servers it calls own their spans.
- A server that wants the browser's trace forwards the `traceparent`, and its own
  span lands as the child.
- The error payload carries `route` and `service` and no `spans[]`. Ingest writes
  a span batch only when a payload carries spans, so a browser error is a
  triage record, not a hop.
- A hop needs a server span to become a row. That span comes from the render, in
  [sveltekit.md](sveltekit.md).

Read [../architecture/perf-model.md](../architecture/perf-model.md) for coverage
and how a missing child renders.
