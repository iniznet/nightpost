# Licensing

## Decision

The self-hosted server in this repository is **AGPL-3.0**. The producer
packages (`nightpost/laravel` on Composer, `@nightpost/*` on npm) are **MIT**.
The managed control plane — billing, tenant provisioning, usage limits — is a
separate product, is not in this repository, and is therefore not covered by
this grant.

`LICENSE` is the verbatim AGPL-3.0 text. `NOTICE` carries the copyright line
and the third-party component list.

## Why these two licences

The pitch is anti-SaaS: the sender costs nothing when it is off, and the whole
thing is one binary you can run yourself. The paid product is the managed
plane, not the binary.

AGPL-3.0 for the server because the network-copyleft clause (section 13)
closes the one hole the anti-SaaS pitch would otherwise leave open: a
competitor could take the binary and offer a hosted service without sharing
their changes. AGPL makes hosted competitors play by the rule the product
preaches — run it yourself, share what you change — while self-hosting stays
free and unchanged. MIT for the packages because a package is adopted or it is
not, and nothing in a package needs a copyleft clause.

The trade is accepted on purpose: some commercial integrators will pass on an
AGPL server. That is the filter working — the integrators this product wants
are the ones self-hosting it.

## Alternatives considered

| Option | Why not |
|---|---|
| **BUSL / FSL** | Resale protection, but "source-available" is a confusing label to explain to the framework communities this targets and it blocks distro packaging outright. |
| **Proprietary** | Slowest possible community growth, and it contradicts the "run it yourself" pitch. |
| **Apache-2.0** | Leaves the hosted-service loophole open: a competitor could run a hosted service off this code and share nothing. |
| **MIT everywhere** | Same loophole, and no copyleft protection on the server at all. |

## Dependency licence audit

"Every dependency of the shipped binary is licence-compatible" is verifiable
rather than asserted, because the link set is exact:

```sh
go list -deps ./cmd/monitor | grep '\.' | grep -v '^github.com/iniznet/nightpost'
```

That lists every third-party package actually compiled into the binary, which
is a strictly smaller set than the module graph. Every linked component is
permissive:

| Component | Licence |
|---|---|
| `modernc.org/sqlite` (+ `libc`, `mathutil`, `memory`, `vtab`) | BSD-3-Clause |
| `github.com/dustin/go-humanize` | MIT |
| `github.com/mattn/go-isatty` | MIT |
| `github.com/ncruces/go-strftime` | MIT |
| `github.com/remyoudompheng/bigfft` | BSD-3-Clause |
| `golang.org/x/sys` | BSD-3-Clause |
| vendored `golang.org/x/crypto`, `/net`, `/text` | BSD-3-Clause (Go toolchain) |

The one weak-copyleft module in the graph (`github.com/hashicorp/golang-lru/v2`,
MPL-2.0) appears only in the module graph — a build-time dependency of the
sqlite driver's tooling — and never in `go list -deps ./cmd/monitor`, so it is
not in the shipped artifact. Re-run that command whenever a dependency is
added: the check, not this paragraph, keeps the claim true.

## What this licence does NOT cover

- **The brand.** `Nightpost`, `nightpost.dev` and the logo are trademarks;
  AGPL-3.0 grants no trademark rights. A fork must rename.
- **The managed control plane.** Billing, tenant provisioning and usage limits
  live outside this repository.
- **The operators' production deployment.** Its data, environment and
  deployment configuration are private.

## Contributions: DCO

Contributions are accepted under the Developer Certificate of Origin
(http://developercertificate.org) — see `CONTRIBUTING.md`. Every commit must
carry a `Signed-off-by:` line. This means contributions are inbound=outbound
under the current licence: after the first outside patch, a future relicence
would need every contributor's consent. That trade is accepted on purpose.

## The irreversible step

Committing these files into a private repository changes nothing for anyone
outside it: no grant has been made, and the files can be removed.

**Publishing under this licence is the irreversible act.** Copyright grants to
distributed copies cannot be recalled, and AGPL-3.0 adds the section 13
network obligation: anyone offering this server as a service must offer its
source to the users of that service. The operators have accepted this and the
repository publishes under AGPL-3.0; a later Packagist/npm publish of the MIT
packages extends the MIT grant and does not change the server's licence.
