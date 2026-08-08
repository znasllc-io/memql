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

// The tokens a host may define. Documented as a list so a consumer can theme
// view-kit completely without reading the sheet.
export const VIEW_KIT_CSS_VARIABLES = [
  "--vk-fg",
  "--vk-muted-fg",
  "--vk-border",
  "--vk-hover-bg",
  "--vk-selected-bg",
  "--vk-selected-fg",
] as const;

export const viewKitStyles = `
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
