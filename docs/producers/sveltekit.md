# Producer: SvelteKit / Node

The reference wiring is a single server module (`src/lib/server/monitor.ts`,
called from `src/hooks.server.ts`); `@nightpost/sveltekit` packages the same
shape.

Read it before you wire a SvelteKit app, or when a render hop is missing.

## Environment

| Variable | Turns on |
|---|---|
| `NIGHTPOST_URL` | the endpoint the reporter posts to |
| `NIGHTPOST_PROJECT_KEY` | the project this deployment reports as |
| `NIGHTPOST_KEY` | the key from `/keys` |
| `NIGHTPOST_SAMPLE_RATE` | request timing samples |
| `NIGHTPOST_PROFILE_RATE` | deep samples |
| `NIGHTPOST_SERVICE` | the service every payload reports as (default: `@nightpost/sveltekit`) |
| `NIGHTPOST_ENV` | the environment (default: `APP_ENV`, then `production`) |
| `NIGHTPOST_RELEASE` | the deploy id shipped on each session beacon |

Off by default: with `NIGHTPOST_URL` unset the reporter is inert and the app runs
exactly as it did before. `NIGHTPOST_PROJECT_KEY` defaults to `front`.

## The hook is the wiring

`createHandle` owns the request half; install it once and stop calling the
reporters by hand.

```ts
// src/hooks.server.ts
import { createHandle, createHandleError } from '@nightpost/sveltekit';

export const handle = createHandle();
export const handleError = createHandleError();
```

Options and env are read per call, so one module serves every deployment.
An absent monitor resolves the request unchanged.

## Journeys (v2)

- **One render root span per request.** Kind `server`, name is the route
  pattern, `start_ms` is 0, status is `error` at a 5xx. Its duration is the
  request's duration, which is why the journey's total is the render's total.
- **The route is a pattern, never a URL.** SvelteKit's route id wins when the
  request matched one: `[id]`, `[[id]]`, `[...rest]` and `[[...rest]]` all
  collapse to `{param}`, and route groups like `(app)` are dropped. With only a
  pathname left, id-shaped segments collapse the same way. Two readers on
  `/chapters/7` and `/chapters/8` are one hop, and an id never travels.
- **Relayed fetches are child spans.** Every `event.fetch` is wrapped: its own
  span id, a `traceparent` of `00-<trace>-<span>-01` on the outbound request,
  and a span named after the callee's path pattern. A call that throws or answers
  5xx still records its span as `error`, so a backend that never reported shows
  as a gap rather than vanishing.
- **Traceparent is propagated both ways.** A valid inbound
  `00-<32 hex>-<16 hex>-01` is adopted: its trace id is kept, the render mints
  its own span id, and the caller's span id becomes the parent. Missing or
  malformed is minted here. The response carries `traceparent`, so a browser or
  proxy can join the same trace.
- **Session beacon.** The session key is the `np_session` cookie, minted when the
  request has none — `httpOnly`, `SameSite=Lax`, `Secure` on https. A completed
  navigation reports `ok`, a 5xx `abnormal`, a thrown render `crashed`. No
  cookie API means no beacon, never an invented id.

## Server-side (SSR)

The v1 shape still works, and the hook sends it.
Post by hand only for work the hook cannot see:

```ts
await fetch(`${NIGHTPOST_URL}/ingest/${PROJECT}/profile`, {
  method: 'POST',
  headers: {
    'content-type': 'application/json',
    'x-monitor-key': NIGHTPOST_KEY,
    'x-nightpost-client': 'my-app/1.0',
    traceparent: incomingTraceparent ?? ''
  },
  body: JSON.stringify({ name, duration_ms, environment, spans })
});
```

Keep it bounded (`AbortSignal.timeout(5000)`) and never let it reject into the
request: telemetry that can fail a page render is a regression, not a feature.

## Browser-side

A page reporter needs CORS, and the allowlist is **per project** — set it at
`/keys` for the deployment's origin (e.g. `https://app.example.com`). An origin
that is present but unlisted is refused `403` rather than silently dropped, so a
misconfigured origin shows up in the network tab instead of losing data quietly.
A wildcard is refused by the server; name each origin explicitly.

Keep the payload small and free of user content: `message`, `stack`, `url`,
`route`, `environment`, `release`. Do not send form values, tokens or identifiers
and hope the server scrubs them.

## Joining traces

If an upstream proxy or service already sets `traceparent`, forward it. The
server reads the W3C header directly, so a request profile and an xhprof
callgraph from different services fold into one waterfall with no payload change
and no shared SDK.

The header is adopted only from a trusted service.
An untrusted producer's `traceparent` is replaced by a minted id.

Why: [../architecture/perf-model.md](../architecture/perf-model.md).
Fields: [../WIRE_SPEC.md](../../WIRE_SPEC.md).
