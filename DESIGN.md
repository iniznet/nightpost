# DESIGN.md

> Written for the M3 UI overhaul (iniznet/fenrirealm-tracker#148). The world:
> **precision ops** — a dense, calm, dark instrument panel; GitHub-dark
> adjacent but tokenized and self-consistent. Server-rendered, zero runtime
> JavaScript (except meta-refresh on the log stream), SVG drawn server-side.

## Tokens

- Surfaces: `--bg #0b0e14`, panel `#12161f`, raised `#171c26`, borders
  `#232a35` / `#2e3744`.
- Text: `#dce3ec` body, `#94a0b0` muted, `#6b7891` faint (12px meta only).
- Accent `#4d9fff` (links, bars, active nav); semantic: red `#ff6b5e`,
  yellow `#e3b341`, green `#4ade80`, purple `#b48cff` (hotspots).
- Type: Segoe UI Variable/system sans for prose; ui-monospace stack for
  code, data values, channels, routes — monospace is for data, never costume.
- Numbers in tables use tabular-nums; tables are the primary structure
  (this is an instrument, not a card wall).

## Components

- Topbar: sticky, brand lowercase mono, section links with accent underline
  on active, project pills (mono, rounded, filled accent when active).
- Badge: 11px, dim surface + colored text for levels (error/warning/info/
  debug, critical family maps to error red).
- Track+fill bars: 14px rounded tracks for waterfalls/hotspot shares; 6px
  inline bars inside tables.
- Sparkline: server-rendered polyline, 96x24, accent stroke; always labeled
  with what it counts.
- Occurrence card: `<details>` with summary (time · kind · title), body of
  key/value grid + frame list + collapsible raw JSON.
- Filters: inline GET forms; selects inherit panel surface; pills for
  log-level filter.

## Motion

Hover states only (rows, pills, buttons, summary). No entrances, no scroll
effects — an ops tool must never move under an on-call reader.

## Bans (this surface)

No decorative gradients, no glass beyond the sticky topbar blur, no icon
fonts, no client-side state management, no light mode. Auto-refresh exists
only on the log stream (meta refresh), never on filtered lists.
