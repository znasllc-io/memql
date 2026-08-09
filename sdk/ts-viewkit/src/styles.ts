// The stylesheet for view-kit's own markup.
//
// view-kit emits a class contract (`vk-row`, `vk-key`, `vk-cycle`, ...), so
// view-kit owns what those classes MEAN. Shipping the markup without the
// styles left every consumer re-authoring the whole sheet and re-deriving the
// semantics -- what distinguishes `vk-cycle` from `vk-empty-value`, whether
// `vk-row-tertiary` is quieter than `vk-row-secondary` -- from the class name
// alone. Two consumers (the VS Code panel and the portal) doing that
// independently is two divergent answers, and the divergence starts on day
// one, not in some future refactor.
//
// This is a STRING. view-kit does not touch the DOM (see vnode.ts): there is
// no `document`, no `<style>` injection, no adoptedStyleSheets. A webview
// consumer interpolates it into a nonce-carrying `<style>`; a bundler-based
// consumer can inject it however it likes. That keeps the package renderable
// under `node --test` with no jsdom and no browser.
//
// THEMING: every color is `var(--vk-*, <fallback>)`. A host themes view-kit by
// defining the `--vk-*` tokens (the VS Code panel maps them onto `--vscode-*`
// in one small block); a host that defines nothing still gets a legible,
// self-contained rendering from the fallbacks. view-kit deliberately does NOT
// reach for host-specific variables itself -- `--vscode-foreground` in this
// file is exactly the coupling the package exists to avoid.
//
// SCOPE: view-kit-owned classes ONLY. Page chrome -- toolbars, panes, the
// two-column grid, buttons, error banners -- belongs to the consumer, which
// knows its own layout. Adding it here would make view-kit dictate page
// structure it cannot see.

// CHART COLOUR AND THE TWO-TIER TOKEN. Everything else here is theme-neutral:
// a grey at 15% alpha and `inherit` are correct on any background, so one
// fallback serves light and dark. A chart palette cannot do that -- a hue
// legible on white is not legible on near-black -- so the series slots carry
// TWO tiers:
//
//   --vk-chart-1           the host override. Wins whenever the host sets it.
//   --vk-chart-1-default   view-kit's own answer, redefined per theme in the
//                          blocks at the bottom of this sheet. Internal: a
//                          host never sets it, and it is not in the list
//                          below.
//
// Marks read `var(--vk-chart-1, var(--vk-chart-1-default))`, so a host that
// themes view-kit still wins, and a host that does nothing gets a validated
// palette that follows prefers-color-scheme. A host rendering dark chrome on
// a light OS (a VS Code dark theme, say) has two ways out: map its own tokens
// onto --vk-chart-*, or pass `{ theme: "dark" }` to the renderer, which
// stamps data-vk-theme on the figure.

// The tokens a host may define. Documented as a list so a consumer can theme
// view-kit completely without reading the sheet.
export const VIEW_KIT_CSS_VARIABLES = [
  "--vk-fg",
  "--vk-muted-fg",
  "--vk-border",
  "--vk-hover-bg",
  "--vk-selected-bg",
  "--vk-selected-fg",
  // Chart + map. The six categorical series slots are a FIXED ORDER: that
  // order is what keeps adjacent series distinguishable under colour-vision
  // deficiency, so a host overriding them should re-validate the whole set
  // rather than swap two slots for taste.
  "--vk-chart-1",
  "--vk-chart-2",
  "--vk-chart-3",
  "--vk-chart-4",
  "--vk-chart-5",
  "--vk-chart-6",
  "--vk-chart-other",
  "--vk-chart-grid",
  "--vk-chart-axis",
  // The colour painted BETWEEN adjacent fills (the 2px gap on pie slices).
  // It has to match the surface the chart sits on, which only the host knows.
  "--vk-chart-surface",
] as const;

const coreStyles = `
.vk-rows { list-style: none; margin: 0; padding: 0; }

.vk-row {
  display: flex;
  gap: 8px;
  align-items: baseline;
  padding: 4px 6px;
  border-radius: 3px;
  cursor: pointer;
  color: var(--vk-fg, inherit);
}
.vk-row:hover { background: var(--vk-hover-bg, rgba(128, 128, 128, 0.15)); }
.vk-row[data-selected="true"] {
  background: var(--vk-selected-bg, rgba(128, 128, 128, 0.35));
  color: var(--vk-selected-fg, inherit);
}
/* The supporting slots set their own muted colour, which would survive the
   selection and leave them tinted for the UNselected background while sitting
   on the selected one. Hand them back to the row so the whole row reads as
   selected; they stay distinguishable by the opacity + size below. */
.vk-row[data-selected="true"] .vk-row-secondary,
.vk-row[data-selected="true"] .vk-row-tertiary {
  color: inherit;
}

/* The always-present slot: falls back to the row id, so it is the row's
   identity and carries full-strength contrast. */
.vk-row-primary { min-width: 0; overflow-wrap: anywhere; }

/* Supporting slots. Both are quieter than primary and equal to each other --
   @displayCard orders them by importance, not by weight. */
.vk-row-secondary,
.vk-row-tertiary {
  color: var(--vk-muted-fg, inherit);
  opacity: 0.7;
  font-size: 0.9em;
  min-width: 0;
  overflow-wrap: anywhere;
}

/* A status pill, pushed to the trailing edge. Carries data-status, so a host
   wanting per-value colour adds \`.vk-row-status[data-status="failed"]\` rules
   of its own -- view-kit stays value-agnostic. */
.vk-row-status {
  margin-left: auto;
  flex: none;
  font-size: 0.8em;
  opacity: 0.8;
  padding: 0 6px;
  border: 1px solid var(--vk-border, currentColor);
  border-radius: 8px;
}

/* "Nothing to show" -- an empty row set. Distinct from .vk-empty-value, which
   is an empty object/array INSIDE a row. */
.vk-empty {
  color: var(--vk-muted-fg, inherit);
  opacity: 0.6;
  padding: 8px 0;
}

.vk-detail { color: var(--vk-fg, inherit); }

/* One level of the recursive walk. The rule is the indent AND the guide line;
   nesting depth is legible without a disclosure control. */
.vk-nested {
  padding-left: 12px;
  border-left: 1px solid var(--vk-border, currentColor);
}

.vk-field { display: flex; gap: 8px; padding: 1px 0; align-items: baseline; }

.vk-key {
  flex: none;
  min-width: 8em;
  color: var(--vk-muted-fg, inherit);
  opacity: 0.7;
}

.vk-value { min-width: 0; overflow-wrap: anywhere; white-space: pre-wrap; }

/* The three "this is not data" markers. They share one presentation
   deliberately: each says the slot holds no value to read, and an operator
   scanning a detail pane should be able to skip all three by the same visual
   cue. The literal text ("null", "{}", "[circular]") is what distinguishes
   them. */
.vk-null,
.vk-empty-value,
.vk-cycle {
  color: var(--vk-muted-fg, inherit);
  opacity: 0.5;
  font-style: italic;
}

/* The cluster topology grid (cluster.ts). Auto-filling columns rather than a
   fixed count: the same grid is drawn into a narrow editor tab and a wide
   portal pane, and neither consumer should have to tell the renderer how many
   tiles fit. */
.vk-grid {
  list-style: none;
  margin: 0;
  padding: 0;
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(220px, 1fr));
  gap: 6px;
}

.vk-tile {
  display: flex;
  flex-wrap: wrap;
  gap: 4px 8px;
  align-items: baseline;
  padding: 6px 8px;
  border: 1px solid var(--vk-border, currentColor);
  border-radius: 3px;
  color: var(--vk-fg, inherit);
}

/* A dashed border for an orphan. Deliberately a SHAPE difference, not a colour
   one: colour on this surface is spent on health, and an orphan can be
   perfectly healthy -- it is simply serving from a release that is no longer
   current. It also survives a monochrome or high-contrast theme. */
.vk-tile[data-orphan="true"] { border-style: dashed; }

.vk-tile-name { font-weight: 600; min-width: 0; overflow-wrap: anywhere; }

/* Same pill treatment as .vk-row-status, and value-agnostic for the same
   reason: a host wanting green/amber/red writes rules against
   \`.vk-tile-health[data-health="degraded"]\`. */
.vk-tile-health {
  margin-left: auto;
  flex: none;
  font-size: 0.8em;
  opacity: 0.8;
  padding: 0 6px;
  border: 1px solid var(--vk-border, currentColor);
  border-radius: 8px;
}

/* Supporting lines. flex-basis 100% puts each on its own row inside the tile,
   so the identity line and the two release lines stack predictably however
   long the values are. */
.vk-tile-detail,
.vk-tile-meta {
  flex-basis: 100%;
  color: var(--vk-muted-fg, inherit);
  opacity: 0.7;
  font-size: 0.9em;
  min-width: 0;
  overflow-wrap: anywhere;
}

/* Name / detail / count tables -- the replica tally and a deployment's
   per-tier composition. */
.vk-tally { list-style: none; margin: 0; padding: 0; }

.vk-tally-row { display: flex; gap: 8px; align-items: baseline; padding: 2px 6px; }

.vk-tally-name { flex: none; min-width: 8em; }

.vk-tally-detail {
  color: var(--vk-muted-fg, inherit);
  opacity: 0.7;
  font-size: 0.9em;
  min-width: 0;
  overflow-wrap: anywhere;
}

/* Tabular figures so a column of counts scans as a column. */
.vk-tally-count {
  margin-left: auto;
  flex: none;
  font-variant-numeric: tabular-nums;
}

/* A flagged row drops the count's muting so the number that is WRONG is the
   one that reads loudest. */
.vk-tally-row[data-flagged="true"] .vk-tally-count { font-weight: 600; }

/* The \`[flag]\` marker -- \`[orphan]\`, \`[under-replica]\`. Dashed to echo the
   orphan tile, and value-agnostic: the literal text inside is what says which
   flag it is. */
.vk-flag {
  flex: none;
  font-size: 0.8em;
  padding: 0 4px;
  border: 1px dashed var(--vk-border, currentColor);
  border-radius: 3px;
  opacity: 0.9;
}

/* The \`[current]\` marker inside a vk-row. Quieter than .vk-flag: it marks the
   NORMAL state (exactly one deployment is current), so it should not read as a
   warning the way an orphan flag does. */
.vk-row-flag { flex: none; font-size: 0.8em; opacity: 0.9; }
`;

// The element library's stylesheet, appended to viewKitStyles below. Split
// into its own string only so the two halves stay readable; consumers see one
// sheet. Same rules apply: view-kit classes only, every colour through a
// --vk-* token with a fallback.
const elementStyles = `
/* ---- table ------------------------------------------------------------- */

.vk-table { width: 100%; border-collapse: collapse; color: var(--vk-fg, inherit); }

/* A header is a sort control. The cursor and the arrow slot are present on
   every column, not only the active one, so nothing shifts when the sort
   moves. */
.vk-table-head {
  text-align: left;
  font-weight: 600;
  padding: 4px 8px;
  cursor: pointer;
  white-space: nowrap;
  border-bottom: 1px solid var(--vk-border, currentColor);
}
.vk-table-head::after { content: ""; display: inline-block; width: 1em; opacity: 0.6; }
.vk-table-head[data-vk-sort-dir="asc"]::after { content: " ^"; }
.vk-table-head[data-vk-sort-dir="desc"]::after { content: " v"; }

.vk-table-row { cursor: pointer; }
.vk-table-row:hover { background: var(--vk-hover-bg, rgba(128, 128, 128, 0.15)); }
.vk-table-row[data-selected="true"] {
  background: var(--vk-selected-bg, rgba(128, 128, 128, 0.35));
  color: var(--vk-selected-fg, inherit);
}

.vk-table-cell {
  padding: 3px 8px;
  vertical-align: top;
  overflow-wrap: anywhere;
  border-bottom: 1px solid var(--vk-border, currentColor);
}

/* ---- calendar ---------------------------------------------------------- */

.vk-cal { color: var(--vk-fg, inherit); }
.vk-cal-title { font-weight: 600; padding: 4px 0; }

.vk-cal-weekdays,
.vk-cal-grid { display: grid; grid-template-columns: repeat(7, minmax(0, 1fr)); }

.vk-cal-weekday {
  padding: 2px 4px;
  font-size: 0.8em;
  color: var(--vk-muted-fg, inherit);
  opacity: 0.7;
}

.vk-cal-day {
  min-height: 64px;
  padding: 2px 4px;
  border: 1px solid var(--vk-border, currentColor);
  /* The grid is one continuous rule, not 35 boxed cells. */
  border-width: 0 1px 1px 0;
  min-width: 0;
}
/* Padding cells before the 1st and after the last: they keep the grid square
   without pretending to be days. */
.vk-cal-day-blank { opacity: 0.35; }

.vk-cal-daynum { font-size: 0.8em; color: var(--vk-muted-fg, inherit); opacity: 0.7; }

.vk-cal-event {
  display: flex;
  gap: 4px;
  align-items: baseline;
  padding: 1px 3px;
  margin-bottom: 2px;
  border-radius: 3px;
  font-size: 0.85em;
  cursor: pointer;
  background: var(--vk-hover-bg, rgba(128, 128, 128, 0.15));
  min-width: 0;
}
.vk-cal-event[data-selected="true"] {
  background: var(--vk-selected-bg, rgba(128, 128, 128, 0.35));
  color: var(--vk-selected-fg, inherit);
}
.vk-cal-time { flex: none; font-variant-numeric: tabular-nums; opacity: 0.75; }
.vk-cal-label { min-width: 0; overflow-wrap: anywhere; }
/* Both quieter than the label, and equal to each other -- same rule the row
   list applies to its supporting slots. */
.vk-cal-span,
.vk-cal-detail { color: var(--vk-muted-fg, inherit); opacity: 0.7; min-width: 0; }
.vk-cal-overflow {
  padding: 4px 0;
  font-size: 0.85em;
  color: var(--vk-muted-fg, inherit);
  opacity: 0.7;
}

/* ---- checklist --------------------------------------------------------- */

.vk-checklist { color: var(--vk-fg, inherit); }
.vk-checklist-summary {
  padding: 2px 0 6px;
  font-size: 0.85em;
  color: var(--vk-muted-fg, inherit);
  opacity: 0.7;
}
.vk-checklist-items { list-style: none; margin: 0; padding: 0; }
.vk-checklist-group {
  padding: 6px 0 2px;
  font-size: 0.8em;
  text-transform: uppercase;
  letter-spacing: 0.04em;
  color: var(--vk-muted-fg, inherit);
  opacity: 0.7;
}
.vk-checklist-item {
  display: flex;
  gap: 8px;
  align-items: baseline;
  padding: 2px 4px;
  border-radius: 3px;
  cursor: pointer;
}
.vk-checklist-item:hover { background: var(--vk-hover-bg, rgba(128, 128, 128, 0.15)); }
.vk-checklist-item[data-selected="true"] {
  background: var(--vk-selected-bg, rgba(128, 128, 128, 0.35));
  color: var(--vk-selected-fg, inherit);
}
.vk-checklist-box { flex: none; font-family: monospace; }
.vk-checklist-label { min-width: 0; overflow-wrap: anywhere; }
/* A struck-through, faded line is the second signal that an item is done --
   the box glyph is the first, so the state never rests on one cue. */
.vk-checklist-item[data-vk-done="true"] .vk-checklist-label {
  text-decoration: line-through;
  opacity: 0.6;
}
.vk-checklist-due {
  margin-left: auto;
  flex: none;
  font-size: 0.85em;
  font-variant-numeric: tabular-nums;
  color: var(--vk-muted-fg, inherit);
  opacity: 0.7;
}

/* ---- timeline ---------------------------------------------------------- */

.vk-timeline { list-style: none; margin: 0; padding: 0; color: var(--vk-fg, inherit); }
.vk-timeline-date {
  padding: 8px 0 2px;
  font-size: 0.8em;
  text-transform: uppercase;
  letter-spacing: 0.04em;
  color: var(--vk-muted-fg, inherit);
  opacity: 0.7;
}
.vk-timeline-entry {
  display: flex;
  gap: 8px;
  align-items: baseline;
  padding: 2px 4px;
  border-radius: 3px;
  cursor: pointer;
}
.vk-timeline-entry:hover { background: var(--vk-hover-bg, rgba(128, 128, 128, 0.15)); }
.vk-timeline-entry[data-selected="true"] {
  background: var(--vk-selected-bg, rgba(128, 128, 128, 0.35));
  color: var(--vk-selected-fg, inherit);
}
.vk-timeline-time {
  flex: none;
  min-width: 3.5em;
  font-variant-numeric: tabular-nums;
  color: var(--vk-muted-fg, inherit);
  opacity: 0.7;
}
/* The rail dot. Decorative: the time beside it says the same thing. */
.vk-timeline-marker {
  flex: none;
  width: 7px;
  height: 7px;
  border-radius: 50%;
  align-self: center;
  background: var(--vk-border, currentColor);
}
.vk-timeline-label { min-width: 0; overflow-wrap: anywhere; }
.vk-timeline-detail { color: var(--vk-muted-fg, inherit); opacity: 0.7; min-width: 0; }

/* ---- stat tiles -------------------------------------------------------- */

.vk-stats { display: flex; flex-wrap: wrap; gap: 12px; color: var(--vk-fg, inherit); }
.vk-stat {
  flex: 1 1 8em;
  min-width: 0;
  padding: 8px 12px;
  border: 1px solid var(--vk-border, currentColor);
  border-radius: 4px;
}
/* The hero number: the one thing on the tile a reader should see first. */
.vk-stat-value { font-size: 1.8em; line-height: 1.1; font-variant-numeric: tabular-nums; }
.vk-stat-label { font-size: 0.85em; overflow-wrap: anywhere; }
.vk-stat-sub {
  font-size: 0.8em;
  color: var(--vk-muted-fg, inherit);
  opacity: 0.7;
  overflow-wrap: anywhere;
}

/* ---- board ------------------------------------------------------------- */

.vk-board {
  display: flex;
  gap: 12px;
  align-items: flex-start;
  overflow-x: auto;
  color: var(--vk-fg, inherit);
}
.vk-board-column { flex: 0 0 14em; min-width: 0; }
.vk-board-heading {
  display: flex;
  gap: 8px;
  align-items: baseline;
  padding: 2px 4px;
  border-bottom: 1px solid var(--vk-border, currentColor);
}
.vk-board-heading-label { min-width: 0; overflow-wrap: anywhere; font-weight: 600; }
.vk-board-count {
  margin-left: auto;
  flex: none;
  font-size: 0.8em;
  color: var(--vk-muted-fg, inherit);
  opacity: 0.7;
}
.vk-board-cards { list-style: none; margin: 0; padding: 4px 0; }
.vk-board-card {
  display: flex;
  flex-direction: column;
  gap: 2px;
  padding: 4px 6px;
  margin-bottom: 4px;
  border: 1px solid var(--vk-border, currentColor);
  border-radius: 3px;
  cursor: pointer;
}
.vk-board-card:hover { background: var(--vk-hover-bg, rgba(128, 128, 128, 0.15)); }
.vk-board-card[data-selected="true"] {
  background: var(--vk-selected-bg, rgba(128, 128, 128, 0.35));
  color: var(--vk-selected-fg, inherit);
}
.vk-board-card-label { overflow-wrap: anywhere; }
.vk-board-card-detail {
  font-size: 0.85em;
  color: var(--vk-muted-fg, inherit);
  opacity: 0.7;
  overflow-wrap: anywhere;
}

/* ---- charts + map: the shared visual system ---------------------------- */

/* The figure owns the palette: the legend swatches are HTML siblings of the
   <svg>, so the tokens have to be defined above both. */
.vk-chart-figure {
  color: var(--vk-fg, inherit);
  --vk-chart-1-default: #2a78d6;
  --vk-chart-2-default: #eb6834;
  --vk-chart-3-default: #1baf7a;
  --vk-chart-4-default: #eda100;
  --vk-chart-5-default: #e87ba4;
  --vk-chart-6-default: #008300;
  --vk-chart-other-default: #7a7a75;
  --vk-chart-grid-default: rgba(128, 128, 128, 0.25);
  --vk-chart-axis-default: rgba(128, 128, 128, 0.55);
  --vk-chart-surface-default: #fcfcfb;
}
/* The dark steps are the same eight hues re-stepped for a dark surface, not a
   different palette, and they are validated as a set against it. The :not()
   guard lets an explicit light stamp beat an OS preference for dark. */
@media (prefers-color-scheme: dark) {
  .vk-chart-figure:not([data-vk-theme="light"]) {
    --vk-chart-1-default: #3987e5;
    --vk-chart-2-default: #d95926;
    --vk-chart-3-default: #199e70;
    --vk-chart-4-default: #c98500;
    --vk-chart-5-default: #d55181;
    --vk-chart-6-default: #008300;
    --vk-chart-other-default: #8a8a84;
    --vk-chart-grid-default: rgba(160, 160, 160, 0.22);
    --vk-chart-axis-default: rgba(160, 160, 160, 0.5);
    --vk-chart-surface-default: #1a1a19;
  }
}
/* The explicit stamp wins in both directions -- a host that knows it is dark
   must not depend on the viewer's OS agreeing. */
.vk-chart-figure[data-vk-theme="dark"] {
  --vk-chart-1-default: #3987e5;
  --vk-chart-2-default: #d95926;
  --vk-chart-3-default: #199e70;
  --vk-chart-4-default: #c98500;
  --vk-chart-5-default: #d55181;
  --vk-chart-6-default: #008300;
  --vk-chart-other-default: #8a8a84;
  --vk-chart-grid-default: rgba(160, 160, 160, 0.22);
  --vk-chart-axis-default: rgba(160, 160, 160, 0.5);
  --vk-chart-surface-default: #1a1a19;
}

.vk-chart { display: block; width: 100%; height: auto; }

/* One hue definition per slot, applied to fill (svg marks), stroke (lines)
   and background (the HTML legend swatch), so a mark and its swatch can never
   disagree. */
.vk-chart-s1 {
  fill: var(--vk-chart-1, var(--vk-chart-1-default));
  stroke: var(--vk-chart-1, var(--vk-chart-1-default));
  background: var(--vk-chart-1, var(--vk-chart-1-default));
}
.vk-chart-s2 {
  fill: var(--vk-chart-2, var(--vk-chart-2-default));
  stroke: var(--vk-chart-2, var(--vk-chart-2-default));
  background: var(--vk-chart-2, var(--vk-chart-2-default));
}
.vk-chart-s3 {
  fill: var(--vk-chart-3, var(--vk-chart-3-default));
  stroke: var(--vk-chart-3, var(--vk-chart-3-default));
  background: var(--vk-chart-3, var(--vk-chart-3-default));
}
.vk-chart-s4 {
  fill: var(--vk-chart-4, var(--vk-chart-4-default));
  stroke: var(--vk-chart-4, var(--vk-chart-4-default));
  background: var(--vk-chart-4, var(--vk-chart-4-default));
}
.vk-chart-s5 {
  fill: var(--vk-chart-5, var(--vk-chart-5-default));
  stroke: var(--vk-chart-5, var(--vk-chart-5-default));
  background: var(--vk-chart-5, var(--vk-chart-5-default));
}
.vk-chart-s6 {
  fill: var(--vk-chart-6, var(--vk-chart-6-default));
  stroke: var(--vk-chart-6, var(--vk-chart-6-default));
  background: var(--vk-chart-6, var(--vk-chart-6-default));
}
/* The seventh-and-beyond fold. Neutral on purpose: "Other" is not an identity
   and must not look like one. */
.vk-chart-other {
  fill: var(--vk-chart-other, var(--vk-chart-other-default));
  stroke: var(--vk-chart-other, var(--vk-chart-other-default));
  background: var(--vk-chart-other, var(--vk-chart-other-default));
}

/* Mark rules come AFTER the slot rules so their fill/stroke overrides land. */
.vk-chart-bar { stroke: none; }
.vk-chart-line { fill: none; stroke-width: 2; stroke-linejoin: round; stroke-linecap: round; }
.vk-chart-dot { stroke: none; }
/* The 2px stroke is the GAP between adjacent slices, painted in the surface
   colour -- not an outline. */
.vk-chart-slice {
  stroke: var(--vk-chart-surface, var(--vk-chart-surface-default));
  stroke-width: 2;
}

/* The proportion rail. One line tall and stretched to its container's width:
   the rail IS the population, so it must not letterbox at a narrow width the
   way an aspect-preserving chart would. The height is pinned here rather than
   left to the viewBox because .vk-chart's \`height: auto\` would scale it with
   the width and turn a 14-unit rail into a 60px band on a wide pane. */
.vk-chart-rail { height: 14px; }
.vk-chart-rail-seg { stroke: none; }

.vk-chart-grid { stroke: var(--vk-chart-grid, var(--vk-chart-grid-default)); stroke-width: 1; }
.vk-chart-axis { stroke: var(--vk-chart-axis, var(--vk-chart-axis-default)); stroke-width: 1; }

/* Text wears text tokens, never the series colour: a coloured mark beside a
   label carries the identity, the label carries the words. */
.vk-chart-tick {
  fill: var(--vk-muted-fg, currentColor);
  opacity: 0.75;
  font-size: 9px;
}
.vk-chart-value { fill: var(--vk-fg, currentColor); font-size: 9px; }

.vk-chart-legend {
  list-style: none;
  margin: 0;
  padding: 4px 0 0;
  display: flex;
  flex-wrap: wrap;
  gap: 4px 12px;
  font-size: 0.85em;
}
.vk-chart-legend-item { display: flex; gap: 6px; align-items: center; min-width: 0; }
.vk-chart-swatch { flex: none; width: 10px; height: 10px; border-radius: 2px; }
.vk-chart-legend-label { min-width: 0; overflow-wrap: anywhere; }

.vk-map { display: block; width: 100%; height: auto; }
.vk-map-graticule {
  stroke: var(--vk-chart-grid, var(--vk-chart-grid-default));
  stroke-width: 0.5;
  fill: none;
}
.vk-map-axis {
  stroke: var(--vk-chart-axis, var(--vk-chart-axis-default));
  stroke-width: 0.75;
  fill: none;
}
.vk-map-point { stroke: var(--vk-chart-surface, var(--vk-chart-surface-default)); stroke-width: 1; }
.vk-map-point[data-selected="true"] { stroke: var(--vk-fg, currentColor); stroke-width: 1.5; }
`;

// One sheet for consumers: the core row/detail contract plus the element
// library. Two exports would mean two <style> installs to keep in sync.
export const viewKitStyles = coreStyles + elementStyles;
