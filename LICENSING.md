# Licensing and the IP decision (M6.6)

## Decision

**Open-core.** The self-hosted server in this repository is **Apache-2.0**. The
producer packages (`nightpost/laravel` on Composer, `@nightpost/*` on npm) are
**MIT**. The managed control plane (M6.9 - billing, tenant provisioning, limits)
is a separate product, is not in this repository, and is therefore not covered
by this grant.

`LICENSE` is the verbatim Apache-2.0 text. `NOTICE` carries the copyright line
and the third-party component list that Apache-2.0 section 4(d) requires on
distribution.

## Why open-core, and why these two licences

The pitch is anti-SaaS: the wedge is that the sender costs nothing when it is
off, and that the whole thing is one binary you can run yourself. Giving the
server away permissively *is* the acquisition channel, so charging for the
binary would undercut the argument. The paid product is the managed plane, not
the binary.

Apache-2.0 for the server rather than MIT because it carries an explicit patent
grant and a trademark clause (section 6), which matters for a company-backed
product others will redistribute. MIT for the packages because a package is
adopted or it is not, and nothing in a package needs a patent clause.

## Alternatives considered

| Option | Why not |
|---|---|
| **BUSL / FSL** | Resale protection, but "source-available" is a confusing label to explain to the framework communities this targets and it blocks distro packaging outright. Revisit only if a reseller actually appears - and a relicence first needs the CLA question below settled. |
| **Proprietary** | Slowest possible community growth, and it contradicts the "run it yourself" pitch. |
| **AGPL** | Would repel exactly the ecosystem this is built to sit beside, for protection the open-core split already provides (the managed plane stays closed). |
| **MIT everywhere** | Fine, but it gives up the patent grant on the server for no adoption gain. |

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
  Apache-2.0 section 6 grants no trademark rights. A fork must rename.
- **The managed control plane** (M6.9). Not in this repository.
- **The fenrirealm production instance.** Its data, deployment env and Dokploy
  configuration are private.

## Decided: DCO (2026-10-21)

Apache-2.0 section 5 makes contributions inbound=outbound, which is all a
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

**Publishing the fork under this licence is the irreversible act.** Apache-2.0
section 2 grants a perpetual, irrevocable copyright licence for every copy
distributed, so a published grant cannot be recalled. Confirm this decision
before the repository or the packages become public, not after.

## Actions at fork time (M6.7)

1. Copyright holder and year in `NOTICE`.
2. Go module path: `github.com/iniznet/nightpost` to the Nightpost path.
3. Repository rename plus the `fenrirealm-*` brand scrub (strings, install paths,
   docs).
4. Producer packages published as `nightpost/laravel` and `@nightpost/*`.
5. Flip `-require-registered` to default-on in the product build (M6.10).
6. Add `CONTRIBUTING.md` with the chosen DCO/CLA line.
