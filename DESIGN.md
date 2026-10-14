# DESIGN.md

> Written for the M3 UI overhaul (iniznet/fenrirealm-tracker#148); extended by
> the M6.1 IA consolidation (iniznet/fenrirealm-tracker#205). The world:
> **precision ops** — a dense, calm, dark instrument panel; GitHub-dark
> adjacent but tokenized and self-consistent. Server-rendered, zero runtime
> JavaScript (except meta-refresh on the log stream), SVG drawn server-side.

## Tokens

- Surfaces: `--bg #0b0e14`, panel `#12161f`, raised `#171c26`, borders
  `#232a35` / `#2e3744`.
- Text: `#dce3ec` body, `#94a0b0` muted, `#7e8ba4` faint (12px meta only).
- Accent `#4d9fff` (links, bars, active nav); semantic: red `#ff6b5e`,
  yellow `#e3b341`, green `#4ade80`, purple `#b48cff` (hotspots).
- Type: Segoe UI Variable/system sans for prose; ui-monospace stack for
  code, data values, channels, routes — monospace is for data, never costume.
- Numbers in tables use tabular-nums; tables are the primary structure
  (this is an instrument, not a card wall).

## Navigation & context

- Topbar: sticky, brand lowercase mono, six section links (Dashboard, Errors,
  Performance, Queries, Jobs, Keys) with an accent underline on the active
  section — the section is derived from the page, never re-typed per template.
- Toolbar: sticky second row holding the ONE persistent context — org,
  project, time range (1h/24h/7d/30d, default 24h) and env. It is a single GET form
  posting to the current path, so changing context never loses the section.
  Every generated link carries the toolbar through `toolbar.Link`; the default
  range encodes away so short legacy links stay short. Pre-auth pages hide it.
- Tabs: a section's pages share one tab row (Errors: Issues | Logs;
  Performance: Endpoints | Traces | Hotspots). An unknown or empty tab
  resolves to the section default — never a 404.
- Project pills stay in the topbar for one-click switching; they carry the
  rest of the toolbar with them.

## Components

- Cards: used only for dashboard triage (per-project health) and as a
  raised panel; the projects landing page is a card grid, not a table.
- Badge: 11px, dim surface + colored text for levels (error/warning/info/
  debug, critical family maps to error red); `warn`/`ok` variants for state.
- Track+fill bars: 14px rounded tracks for waterfalls/hotspot shares; 6px
  inline bars inside tables. Bars inside a table cell keep the cell
  `nowrap` so a duration never wraps away from its bar.
- Sparkline: server-rendered polyline, 96x24, accent stroke; always labeled
  with what it counts. Latency deltas render as signed percent with a
  semantic color, never as an unlabeled arrow.
- Tabs: 2px accent bottom border on the active tab, 1px rule under the row.
- Health footer: the ingest-rejection counters (shaper 429 / keygate 401 /
  unsigned admits) beside the store figures, on every page — the answer to
  "why is my data missing?". It replaces the old per-page `store:` debug line.
- Filters: inline GET forms; selects inherit panel surface; hidden toolbar
  fields keep the context when a page filter is applied; pills for
  log-level, scope and view toggles.
- Tables: `display:block; overflow-x:auto`, so a wide table scrolls inside
  its own box and the page never gains a horizontal scrollbar.

## Motion

Hover states only (rows, pills, buttons, summary, tabs). No entrances, no
scroll effects — an ops tool must never move under an on-call reader.

## Bans (this surface)

No decorative gradients, no glass beyond the sticky topbar blur, no icon
fonts, no client-side state management, no light mode. Auto-refresh exists
only on the log stream (meta refresh), never on filtered lists.
