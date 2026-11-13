# Retiring a project or an org

What an operator can destroy, what survives it, and why none of it runs inside a request.

## Shape

| Verb | Deletes | Keeps |
|---|---|---|
| **Clear data** | every event of one project: occurrences, profiles, spans, journeys + hops, issues, query_stats, job_stats, releases, sessions, raw_envelopes, routes, services | the project row, its ingest key, its CORS origin, its org membership |
| **Delete project** | the above, then the project row | nothing — the key stops authenticating |
| **Delete org** | refused while it still holds projects; deleting each project first is the cascade | every other org and project |

`projects` is one app/repo. A service is a producer identity inside a project and is never retired on its own.

## Invariants

- **Never in a request path.** The single WAL writer is shared with ingest, so a purge is queued (`purges`) and the retention ticker drains one bounded statement per cycle (500 rows). This is the same rule the retention loop follows; a synchronous DELETE would stall the writers that feed the dashboard.
- **The confirmation is typed.** The operator types the exact project key (or org slug). A mismatch deletes nothing and writes an audit line saying so.
- **A half-purged project still renders.** Every step is a separate statement, so the page must tolerate a project whose rows are partly gone; the project row leaves last.
- **Journeys go before routes.** A journey points at route ids across services; deleting routes first would leave rows pointing at nothing. A journey that walked the purged project is removed, not rendered with a dead link.
- **Every attempt is audited.** `admin_actions` holds actor, action, target and detail — refusals included. A purge with no audit line is a bug.
- **No one-click cascade for an org.** Deleting its projects one confirmed click at a time is the cascade; one button that removes an org and everything under it is the mistake this rule prevents.

## Failure modes

| Symptom | Cause | Fix |
|---|---|---|
| "a purge is already queued for this project" | a second request while one is in flight | wait for the ticker; `/orgs` shows the progress |
| counts unchanged after a click | the typed text did not match | the page says so; nothing was deleted |
| project still listed after *Delete* | the ticker has not reached the last step | `/orgs` shows `purging (n/13)` |

## Operations

The purge shares the retention ticker (`-prune-interval`, default 10 minutes), so a large project drains over several cycles. There is no separate command: the queue is the interface, and every step is logged as `purge <project>: step drained`.
