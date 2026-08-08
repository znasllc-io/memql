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
`;
