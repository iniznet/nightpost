# Licensing and the IP decision (M6.6)

## Decision

**Open-core.** The self-hosted server in this repository is **AGPL-3.0**
(decided 2026-10-21, operator decision on iniznet/fenrirealm-tracker#278 -
revised from the earlier Apache-2.0 plan before publication). The producer
packages (`nightpost/laravel` on Composer, `@nightpost/*` on npm) are **MIT**.
The managed control plane (M6.9 - billing, tenant provisioning, limits) is a
separate product, is not in this repository, and is therefore not covered by
this grant.

`LICENSE` is the verbatim AGPL-3.0 text. `NOTICE` carries the copyright line
and the third-party component list (the attribution the licence asks for on
distribution).

## Why open-core, and why these two licences

The pitch is anti-SaaS: the wedge is that the sender costs nothing when it is
off, and that the whole thing is one binary you can run yourself. Giving the
server away permissively *is* the acquisition channel, so charging for the
binary would undercut the argument. The paid product is the managed plane, not
the binary.

AGPL-3.0 for the server (operator decision, replacing the earlier Apache-2.0
plan) because the network-copyleft clause (section 13) closes the one hole the
anti-SaaS pitch leaves open: under a permissive licence, a competitor can take
the whole binary and offer a hosted Nightwatch-class service without sharing
their changes. AGPL makes hosted competitors play by the same rule the product
preaches - run it yourself, share what you change - while self-hosting stays
free and unchanged. MIT for the packages because a package is adopted or it is
not, and nothing in a package needs a copyleft clause.

## Alternatives considered

| Option | Why not |
|---|---|
| **BUSL / FSL** | Resale protection, but "source-available" is a confusing label to explain to the framework communities this targets and it blocks distro packaging outright. |
| **Proprietary** | Slowest possible community growth, and it contradicts the "run it yourself" pitch. |
| **Apache-2.0** (the earlier plan) | Leaves the hosted-service loophole open: a competitor could run a hosted Nightwatch-class service off this code and share nothing. Superseded by AGPL-3.0 at the operator's decision. |
| **MIT everywhere** | Same loophole, and no copyleft protection on the server at all. |

The AGPL trade is accepted on purpose: some commercial integrators will pass on
an AGPL server. That is the filter working - the integrators this product wants
are the ones self-hosting it, and self-hosting stays free under AGPL.

## Dependency licence audit

"Every dependency of the shipped binary is permissive" is verifiable rather than
asserted, because the link set is exact:

```sh
go list -deps ./cmd/monitor | grep '\.' | grep -v '^github.com/iniznet/nightpost'
```

That lists every third-party package actually compiled into the binary, which is
a strictly smaller set than the module graph (`go list -m all` reports 26
modules). Every linked component is permissive:

| Component | Licence |
|---|---|
| `modernc.org/sqlite` (+ `libc`, `mathutil`, `memory`, `vtab`) | BSD-3-Clause |
| `github.com/dustin/go-humanize` | MIT |
| `github.com/mattn/go-isatty` | MIT |
| `github.com/ncruces/go-strftime` | MIT |
| `github.com/remyoudompheng/bigfft` | BSD-3-Clause |
| `golang.org/x/sys` | BSD-3-Clause |
| vendored `golang.org/x/crypto`, `/net`, `/text` | BSD-3-Clause (Go toolchain) |

**The one weak-copyleft module in the graph does not ship.**
`github.com/hashicorp/golang-lru/v2` is MPL-2.0, but it appears only in the
module graph (a build-time dependency of the sqlite driver's tooling) and never
in `go list -deps ./cmd/monitor`. MPL-2.0 is file-level copyleft, so it would
have been distributable anyway while its source stayed available - but it is not
in the artifact, so the question does not arise. Re-run the command above
whenever a dependency is added: that check, not this paragraph, is what keeps
the claim true.

## What this grant does NOT cover

- **The brand.** `Nightpost`, `nightpost.dev` and the logo are trademarks;
  AGPL-3.0 does not grant trademark rights. A fork must rename.
- **The managed control plane** (M6.9). Not in this repository.
- **The fenrirealm production instance.** Its data, deployment env and Dokploy
  configuration are private.

## Decided: DCO (2026-10-21)

AGPL-3.0 makes contributions inbound=outbound, which is all a
permissively licensed project strictly needs. But it also means every outside
contribution is irrevocable under the *current* licence: if a relicence were
ever wanted (say to BUSL, on the evidence of a reseller), that contributor's
permission would be required and, without a CLA, unobtainable.

So this is the one hard-to-reverse decision left in the licence, and it is
cheapest to make **now**, while the copyright holders are still only us:

- **Do nothing (inbound=outbound).** Simplest, best for drive-by contributions;
  relicensing is off the table forever after the first outside patch.
- **DCO sign-off.** Still permissive and low friction, and it at least documents
  provenance.
- **CLA.** Keeps relicensing possible; adds friction and a document to maintain.

**Decided: DCO** (2026-10-21, operator decision on iniznet/fenrirealm-tracker#278)
- it preserves the audit trail at near-zero friction, given relicensing is not
currently intended. The sign-off requirement lives in `CONTRIBUTING.md`;
relicensing is now off the table after the first outside patch.

## The irreversible step

Committing these files into a **private** repository changes nothing for anyone
outside it: no grant has been made, and the files can be removed.

**Publishing the fork under this licence is the irreversible act.** Copyright
grants to distributed copies cannot be recalled, and AGPL-3.0 adds the section
13 network obligation: anyone offering this server as a service must offer its
source to the users of that service - the clause the product strategy now
relies on. Confirm this decision before the repository or the packages become
public, not after.

**Decision recorded 2026-10-21** (operator, iniznet/fenrirealm-tracker#278):
the fork repository and the producer packages go PUBLIC. The AGPL-3.0 grant
takes effect the moment the visibility flips; a Packagist/npm publish later
extends the same grant, it does not create a new one.

## Fork-time actions (M6.7) — completion ledger

The section below was written before the fork, in future tense. Each line now
carries its status, so the list stays an accurate record instead of a stale
to-do.

1. **Copyright holder and year in `NOTICE`** — DONE. `NOTICE` reads
   `nightpost ... Copyright 2026 iniznet`.
2. **Go module path rename** — DONE. `module github.com/iniznet/nightpost`
   across go.mod, imports and the dependency-audit grep (iniznet/nightpost#1).
3. **Repository rename plus the `fenrirealm-*` brand scrub** — DONE for the
   repository: the product repo is `iniznet/nightpost` (a private mirror that
   goes public; GitHub cannot fork a repo into the org that owns it). The
   string scrub landed in the same PR (#1). Tracker citations
   (`iniznet/fenrirealm-tracker#N`) stay on purpose: provenance, not brand.
4. **Producer packages** — BUILT, publication pending. `nightpost/laravel`
   (v0.1.0), `@nightpost/js`, `@nightpost/sveltekit` exist as repositories
   under iniznet; the visibility flip below publishes them. A Packagist/npm
   registry publish is optional later and does not change the grant.
5. **Flip `-require-registered` to default-on** — DONE (iniznet/nightpost#1);
   legacy posture via `-require-registered=false`.
6. **`CONTRIBUTING.md` with the DCO line** — DONE (2026-10-21): DCO chosen,
   sign-off required, relicensing closed after the first outside patch.
