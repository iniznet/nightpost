# Docs

No site generator, deliberately. Five pages of markdown render fine on GitHub,
and adding a static-site toolchain would mean a build step, a config file and a
set of dependencies to maintain for no reader benefit. GitHub's renderer plus the
cross-links below is the docs site. Revisit that only if the page count or the
need for search makes it false.

**Hydrate-free is a product decision, not a limitation** — and the trigger for
changing it is named here so it cannot happen by accident: v1 ships zero
JavaScript. *If* live log tailing arrives and genuinely needs it, that is the
point to decide a tiny client bundle — in the open, as a decision, never as a
side effect of "it was easier with a framework". Everything else on these pages
must stay server-rendered.

| Page | Read it when |
|---|---|
| [../WIRE_SPEC.md](../WIRE_SPEC.md) | you are implementing a producer, porting a client, or asking whether a payload is legal |
| [../DEPLOY.md](../DEPLOY.md) | you are installing, exposing, upgrading or recovering an instance |
| [write-your-own-sdk.md](write-your-own-sdk.md) | you want a client for a language nobody has written one for yet |
| [producers/laravel.md](producers/laravel.md) | the producer is Laravel |
| [producers/sveltekit.md](producers/sveltekit.md) | the producer is SvelteKit or Node |
| [producers/webhook.md](producers/webhook.md) | the producer is anything else, or you just want curl |

Two things this folder does NOT have yet, and why: a **screenshot gallery**
(the UI is server-rendered HTML and the screenshots are captured from a live
instance, not generated in CI) and a **quickstart GIF** (same reason). Until
then the README quickstart is the onboarding path, and it is verified: a clean
directory, the real binary, the shipped container flags.
