# DESIGN.md

> Rewritten for the triage-first shell (iniznet/fenrirealm-tracker#525). The world:
> **a triage instrument** — dense, dark, and ranked. The first screen answers one
> question: *which defect, where, since when*. Server-rendered, zero runtime
> JavaScript except the log stream's meta refresh, SVG drawn server-side.

## Type scale — five sizes, one owner each

| Size | Owns |
|---|---|
| 24px | the page title (`h1`) — the only 24px element on any page |
| 15px | the brand, a card title |
| 14px | body copy, table cells |
| 12px | meta: scope line, faint notes, empty states |
| 11px | `th`, badges, control labels (uppercase) |

Rules: an `h2` is a section label at 11px uppercase — never larger than the
`h1`, never competing with it. Data values use the mono stack. Numbers use
tabular figures. One focal block per page carries the only 24px number.

## Three levels, three treatments

1. **Section** — top bar, filled segment (`Now` · `Errors` · `Performance` ·
   `Configure`). Four destinations, because a data surface and a config surface
   are different jobs.
2. **Tab** — underline, inside the page head, below the `h1`. Errors:
   Exceptions | Logs. Configure: Keys | Organisations | Settings.
   Performance: Overview | Queries | Jobs. Overview is the merged route table
   and a route's detail carries its walk and hotspots (tracker #656); a
   statement shape and a queue drain outlive any one route, so they keep their
   own pages (tracker #658).
3. **Page** — the `h1`, plus the scope line.

A level never borrows another level's mark. The active state is `aria-current`,
not a colour alone. A read-only surface renders no action button: the detail page
says in one line why the action does not exist instead of showing a dead control.

## Chrome

- **One sticky bar, two lines.** Destinations on line one, scope on line two,
  inside one `header.appbar`. Nothing slides under anything: the old pair of
  sticky rows both at `top:0` is gone.
- **Scope is one control group.** Org is always present (it gates what a reader
  may see). Project, range and env render only on the sections they change —
  configuration pages show org alone, because the others do nothing there.
- **The scope is stated in words** under every `h1`: "backend · 24h ·
  production". A reader never reconstructs scope from four selects.
- **One filter form per page**, built from the shared `scopehidden` partial so
  `org` can never be dropped again. The page's own button reads "Apply
  filters"; the bar's button reads "Apply".
- **Drill paths carry their origin.** A row link sends `from=<list URL>`; the
  detail page's back link returns there, filters intact, and refuses an
  absolute target.

## Performance answers two questions in one place

The index ranks routes by accumulated wall clock. Each row states the other
service's p95 and the share of requests that carried its leg, so a missing leg
reads `not received` instead of a blank that looks fast. The route detail
walks the last twenty requests: front render, the spans inside it, the other
service's legs, then an explicit gap row when a step never reported. Steps are
scaled against the longest step because they nest, never summed.

## The landing page answers the question

`/` opens with a ranked block: **Top defects right now** — project · defect ·
level · count · trend · last seen, ranked by pain, across every project in
scope. Project cards become a one-line strip; the aggregates below each carry a
Project column. This is the 10-second answer PRODUCT.md promises, and it is the
one place the shell spends its emphasis.

## Components

- Badge: 11px, solid dark tint plus coloured text for levels. Solid, not alpha:
  a translucent fill reads as 1:1 contrast to a checker and to a low-vision
  reader.
- Track+fill bars: 14px for waterfalls and hotspots, 6px inline in tables.
- Sparkline: server-rendered polyline, 96×24, `role="img"` with a label.
- Tabs: 2px underline on `aria-current`.
- Confirm step: destructive key actions open a `<details class="confirm">` and
  need a second press. Zero JavaScript, two deliberate clicks.
- Health footer: ingest rejections beside store figures, on every page — the
  answer to "why is my data missing?".
- Empty state: names the next action and links it. Never a dead end. Never a
  cap without saying so.
- Failure: any page-level error renders inside the shell, with a retry and a
  way back.

## Responsive

One breakpoint at 720px. The sticky bar keeps both lines; scope controls reach
44px on touch. Wide tables scroll inside their own box, never the page.

## Motion

Hover states only. No entrances, no scroll effects — an ops tool must not move
under an on-call reader.

## Bans

No decorative gradients, no glass beyond the sticky bar's blur, no icon fonts,
no client-side state, no light mode. Auto-refresh only on the log stream. No
two controls for one decision (the duplicate project pills are gone).

## Known gap

`docs/screenshots/*` predate this shell. Regenerating them needs a human
browser session; the file names and capture paths stay the same.
