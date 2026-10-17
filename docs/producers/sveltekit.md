# Producer: SvelteKit / Node

The reference wiring in this organisation lives in **fenrirealm-front**
(`src/lib/server/monitor.ts`, called from `src/hooks.server.ts`).

## Environment

| Variable | Turns on |
|---|---|
| `MONITOR_URL` | the endpoint the reporter posts to |
| `MONITOR_PROJECT_KEY` | the project this deployment reports as |
| `MONITOR_KEY` | the key from `/keys` |
| `MONITOR_SAMPLE_RATE` | request timing samples |
| `MONITOR_PROFILE_RATE` | deep samples |

Off by default: with `MONITOR_URL` unset the reporter is inert and the app runs
exactly as it did before.

## Server-side (SSR)

Post from the hook — the same shape the reference client uses:

```ts
await fetch(`${MONITOR_URL}/ingest/${PROJECT}/profile`, {
  method: 'POST',
  headers: {
    'content-type': 'application/json',
    'x-monitor-key': MONITOR_KEY,
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
`/keys` for the deployment's origin (e.g. `https://fenrirealm.com`). An origin
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
