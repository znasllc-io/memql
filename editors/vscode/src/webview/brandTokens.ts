// The MemQL brand, as far as a VS Code webview can carry it (memql#4196;
// re-keyed onto the appearance setting in memql#4419).
//
// ONE token module for every panel. The palette is memql.io's exactly -- the
// same hexes the portal redesign (memql#4177) ships -- and since memql#4419 it
// lives next door in palette.ts as DATA, because a theme-JSON generator cannot
// read a template literal (see that file's header). This module COMPOSES the
// CSS from it; it no longer holds a hex of its own.
//
// WHICH PALETTE APPLIES IS MEMQL'S DECISION NOW, NOT THE EDITOR'S. The dark
// block used to select on `body.vscode-dark` -- the class VS Code stamps from
// the EDITOR's theme -- which made `memql.appearance` unimplementable: the
// cascade had no input but the editor. It now selects on
// `body[data-memql-theme="dark"]`, which each panel host stamps from
// appearance.ts's resolver. Do not add the old class back beside the new
// attribute: whichever rule came last would win, so a forced-light panel under
// a dark editor would flip to dark with nothing to say why.
//
// High contrast defers wholesale to VS Code's own variables and IGNORES the
// setting: a themed green is not worth an accessibility regression. Those
// rules keep keying on the `vscode-high-contrast` classes VS Code itself
// stamps, and appearance.ts stamps no attribute in that case, so exactly one
// opinion reaches the cascade.
//
// WHAT VS CODE DOES NOT ALLOW, stated so nobody re-litigates it:
//   - No bundled fonts. A webview under `default-src 'none'` loads no font
//     files, and shipping Inter/JetBrains Mono/Squada One in the VSIX for
//     seven panels is weight without leverage -- the editor already renders a
//     good UI face (--vscode-font-family) and the USER'S chosen editor
//     monospace (--vscode-editor-font-family), which is the two-voice split.
//     Squada One (display moments) has no equivalent here; weight and size
//     carry the hierarchy instead.
//   - No external stylesheet. The CSP is `style-src 'nonce-...'`, kept strict
//     on purpose; this module is a string every panel inlines under its nonce.
//
// The tree items are OUT of scope for these tokens by design: tree rows are
// drawn by the workbench and take ThemeIcon + ThemeColor only, so they speak
// VS Code's `charts.*` vocabulary (green = healthy, red = error, yellow =
// needs attention, purple = in progress) and never a hardcoded hex.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import { escapeHtml } from "@znasllc-io/memql-view-kit";

import { DARK, LIGHT, PALETTE_KEYS, type PaletteKey } from "./palette.js";

/**
 * What each token becomes under either high-contrast theme.
 *
 * Typed as a TOTAL record over PaletteKey, which is the point: adding a
 * palette token without deciding its high-contrast behaviour is a compile
 * error rather than a custom property that silently resolves to nothing in the
 * one theme where legibility matters most.
 */
const HIGH_CONTRAST: Readonly<Record<PaletteKey, string>> = {
  bg: "var(--vscode-editor-background)",
  surface: "var(--vscode-editor-background)",
  raised: "var(--vscode-editor-background)",
  border: "var(--vscode-contrastBorder, var(--vscode-panel-border))",
  "border-strong": "var(--vscode-contrastBorder, var(--vscode-panel-border))",
  fg: "var(--vscode-foreground)",
  muted: "var(--vscode-foreground)",
  subtle: "var(--vscode-descriptionForeground)",
  accent: "var(--vscode-focusBorder)",
  "accent-deep": "var(--vscode-focusBorder)",
  "on-accent": "var(--vscode-editor-background)",
  "on-accent-hover": "var(--vscode-editor-background)",
  danger: "var(--vscode-errorForeground)",
  "data-number": "var(--vscode-foreground)",
  "data-string": "var(--vscode-foreground)",
};

/**
 * One theme's `--memql-*` declarations, in PALETTE_KEYS order.
 *
 * Emitted from the shared key list rather than written out per block, so the
 * three blocks below cannot fall out of step: a token added to palette.ts
 * appears in all three, or the total-record types stop compiling.
 */
function declarations(values: Readonly<Record<PaletteKey, string>>): string {
  return PALETTE_KEYS.map((key) => `    --memql-${key}: ${values[key]};`).join("\n");
}

/**
 * The progress bar's width, as 101 rules rather than an inline style
 * (memql#4454).
 *
 * NOT A STYLISTIC CHOICE. Every panel here runs under
 * `style-src 'nonce-<...>'` with no `'unsafe-inline'`, and a nonce cannot
 * apply to a style ATTRIBUTE -- only to a `<style>` element. So
 * `style="width: 42%"` is not merely discouraged on this surface, it is
 * DROPPED by the browser, and the bar would render empty at every value with
 * nothing in any log to say why. Widening the CSP for one bar is the wrong
 * trade on a page that also renders capability stderr.
 *
 * GENERATED, not written out. A hand-maintained table of a hundred rules is a
 * table with a gap in it, and the gap is a percentage at which the bar
 * silently renders empty.
 */
function percentRules(): string {
  const rules: string[] = [];
  for (let percent = 0; percent <= 100; percent += 1) {
    rules.push(`  .run-bar-fill[data-percent="${percent}"] { width: ${percent}%; }`);
  }
  return rules.join("\n");
}

/**
 * The palette + shared component classes, inlined by every panel under its
 * CSP nonce.
 *
 * Light is the default; `body[data-memql-theme="dark"]` -- the attribute the
 * panel host stamps from `memql.appearance` and the editor's kind -- flips to
 * the dark palette; both high-contrast classes remap every token onto VS
 * Code's own variables and ignore the setting entirely.
 *
 * Light being the DEFAULT rather than a stamped case is deliberate: a panel
 * rendered before a theme could be resolved, or by a code path that forgot to
 * stamp, gets a readable light page rather than an unstyled one.
 */
export function brandStyleBlock(): string {
  return `
  /* ---- MemQL brand tokens (memql#4196; palette = memql.io / memql#4177,
         and since memql#4419 it lives in palette.ts) ---- */
  body {
${declarations(LIGHT)}
  }
  body[data-memql-theme="dark"] {
${declarations(DARK)}
  }
  /* High contrast is VS Code's contract, not ours: every token defers, and the
     appearance setting does not get a vote. */
  body.vscode-high-contrast, body.vscode-high-contrast-light {
${declarations(HIGH_CONTRAST)}
  }

  /* view-kit rides the same tokens, so its lists and tables wear the brand. */
  body {
    --vk-fg: var(--memql-fg);
    --vk-muted-fg: var(--memql-muted);
    --vk-border: var(--memql-border);
    --vk-hover-bg: var(--memql-raised);
    --vk-selected-bg: var(--memql-raised);
    --vk-selected-fg: var(--memql-fg);
    --vk-subtle-bg: var(--memql-raised);
    --vk-mono-font: var(--vscode-editor-font-family, ui-monospace, monospace);
  }

  /* ---- the shared base every panel composes ---- */
  body { font-family: var(--vscode-font-family); color: var(--memql-fg);
         background: var(--memql-bg); margin: 0; padding: 16px 20px; }
  h1 { font-size: 1.2em; margin: 0 0 4px; letter-spacing: 0.01em; }
  h2 { font-size: 1.02em; margin: 18px 0 6px; }
  .lede { color: var(--memql-muted); margin: 0 0 16px; }
  .hint { color: var(--memql-muted); margin-top: 3px; }

  .brand-head { display: flex; align-items: center; gap: 10px;
                border-bottom: 1px solid var(--memql-border);
                padding-bottom: 10px; margin-bottom: 14px; }
  .brand-head .memql-mark { color: var(--memql-accent); flex: 0 0 auto; }
  .brand-head h1 { margin: 0; flex: 1 1 auto; }
  .brand-head .head-actions { display: flex; gap: 8px; }
  .brand-head .brand-name { font-weight: 700; letter-spacing: 0.02em;
                            flex: 1 1 auto; }

  /* The two voices: the editor face for chrome, the editor's monospace for
     GRAPH DATA -- ids, counts, timestamps, field values -- with the site's
     data tints. */
  .data { font-family: var(--vscode-editor-font-family, ui-monospace, monospace); }
  .data-number { color: var(--memql-data-number); }
  .data-string { color: var(--memql-data-string); }

  .actions { display: flex; gap: 8px; margin-top: 16px; flex-wrap: wrap; }
  button.primary, button.secondary {
    font: inherit; padding: 4px 12px; cursor: pointer; border-radius: 3px;
    border: 1px solid transparent; }
  button.primary { background: var(--memql-accent); color: var(--memql-on-accent); }
  button.primary:hover { background: var(--memql-accent-deep); color: var(--memql-on-accent-hover); }
  button.secondary { background: var(--memql-raised); color: var(--memql-fg);
                     border-color: var(--memql-border-strong); }
  button.secondary:hover { border-color: var(--memql-accent); }
  button.destructive { color: var(--memql-danger); }
  button:focus-visible { outline: 2px solid var(--memql-accent); outline-offset: 1px; }

  .badge { display: inline-block; border: 1px solid var(--memql-border-strong);
           border-radius: 999px; padding: 1px 8px; font-size: 0.85em;
           color: var(--memql-muted); }
  .badge.ok { color: var(--memql-accent); border-color: var(--memql-accent); }

  .panel-box { background: var(--memql-surface); border: 1px solid var(--memql-border);
               border-radius: 6px; padding: 12px 14px; }

  .preflight-heading { margin-top: 18px; }
  .preflight { list-style: none; margin: 6px 0 0; padding: 0; }
  .preflight-item { display: flex; gap: 8px; align-items: baseline;
                    padding: 4px 0; border-bottom: 1px solid var(--memql-border); }
  .preflight-item:last-child { border-bottom: none; }
  .preflight-mark { flex: 0 0 3.2em; font-size: 0.78em; font-weight: 700;
                    letter-spacing: 0.06em; color: var(--memql-accent); }
  .preflight-item.attention .preflight-mark { color: var(--memql-data-string); }
  .preflight-label { flex: 0 0 10em; font-weight: 600; }
  .preflight-detail { color: var(--memql-muted); }

  .boundary { color: var(--memql-muted); border-top: 1px solid var(--memql-border);
              margin-top: 18px; padding-top: 10px; font-size: 0.92em; }

  /* ---- the branded run block (memql#4454) ----
     HERE RATHER THAN IN EITHER PANEL. The wizard and the Deployments page run
     the same graph through the same renderer, and two copies of this would be
     two answers to what a MemQL install LOOKS like -- which is the thing the
     epic is trying to make one answer. */
  .run-block { display: flex; flex-direction: column; align-items: center;
               text-align: center; gap: 12px; padding: 22px 0 18px; }
  .run-block .memql-mark { color: var(--memql-accent); }
  .run-bar { width: 100%; max-width: 460px; height: 6px; border-radius: 999px;
             background: var(--memql-raised); overflow: hidden; }
  .run-bar-fill { height: 100%; background: var(--memql-accent); border-radius: 999px;
                  transition: width 240ms ease; width: 0; }
  .run-message { margin: 0; color: var(--memql-fg); }
  .run-position { color: var(--memql-muted); }
  .steps-heading { margin-top: 20px; }
  /* The record, not the headline: quieter than the block above it. */
  .step-list { font-size: 0.94em; }
${percentRules()}
  /* Before runStarted seeds the list there is no total, so the bar says
     "something is happening" rather than claiming 0%. */
  .run-bar-fill.indeterminate { width: 40%;
                                animation: memql-run-indeterminate 1.4s ease-in-out infinite; }
  @keyframes memql-run-indeterminate {
    0%   { transform: translateX(-100%); }
    100% { transform: translateX(250%); }
  }

  /* ---- the disclosure, shared by the run log and Diagnostics (memql#4455) ---- */
  .disclosure { margin-top: 18px; border-top: 1px solid var(--memql-border);
                padding-top: 10px; }
  .disclosure-toggle { font: inherit; background: none; border: none; padding: 4px 0;
                       color: var(--memql-accent); cursor: pointer; }
  .disclosure-toggle:hover { text-decoration: underline; }
  .disclosure-toggle[disabled] { color: var(--memql-muted); cursor: default;
                                 text-decoration: none; }
  .disclosure-toggle:focus-visible { outline: 2px solid var(--memql-accent);
                                     outline-offset: 2px; }
  .disclosure-pane { margin-top: 8px; }
  /* SCROLLABLE AND BOUNDED. An install writes hundreds of lines; a pane that
     grew with them would push everything above it -- including the actions row
     this epic just moved to the top -- off the screen. */
  .log-pane { max-height: 40vh; overflow-y: auto;
              background: var(--memql-raised); border: 1px solid var(--memql-border);
              border-radius: 4px; padding: 8px 10px; }
  .log-step + .log-step { margin-top: 10px; border-top: 1px solid var(--memql-border);
                          padding-top: 10px; }
  .log-step-name { color: var(--memql-muted); font-size: 0.9em; margin-bottom: 3px; }
  .log-step[data-status="failed"] .log-step-name { color: var(--memql-danger); }
  .log-step-output { margin: 0; white-space: pre-wrap; word-break: break-word;
                     font-size: 0.9em; color: var(--memql-fg); }

  /* REDUCED MOTION IS HONOURED, and it takes the indeterminate animation with
     it: a bar that cannot state a percentage still must not pulse at somebody
     who asked the system to stop moving. It keeps its width, so it still reads
     as "in progress" rather than as "empty". */
  @media (prefers-reduced-motion: reduce) {
    .run-bar-fill { transition: none; }
    .run-bar-fill.indeterminate { animation: none; }
  }
`;
}

/**
 * The 9-node graph mark, inline, sized for a panel header.
 *
 * THE SAME GEOMETRY as icons/memql-activity.svg -- the asset the Go test
 * cmd/memql-lsp/extensionicon_test.go pins against the PNG artwork. Inlined
 * rather than referenced because the CSP allows no image loads; currentColor
 * so the header tints it with --memql-accent.
 */
export function brandMarkSvg(sizePx: number): string {
  const size = Math.max(12, Math.round(sizePx));
  return `<svg class="memql-mark" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" width="${size}" height="${size}" aria-hidden="true">
<g fill="none" stroke="currentColor" stroke-width="0.78" stroke-linecap="round">
<line x1="13.09" y1="2.52" x2="4.30" y2="5.32"/><line x1="13.09" y1="2.52" x2="9.45" y2="6.83"/>
<line x1="13.09" y1="2.52" x2="19.70" y2="8.76"/><line x1="13.09" y1="2.52" x2="2.52" y2="14.24"/>
<line x1="4.30" y1="5.32" x2="9.45" y2="6.83"/><line x1="4.30" y1="5.32" x2="19.70" y2="8.76"/>
<line x1="4.30" y1="5.32" x2="2.52" y2="14.24"/><line x1="9.45" y1="6.83" x2="19.70" y2="8.76"/>
<line x1="9.45" y1="6.83" x2="2.52" y2="14.24"/><line x1="9.45" y1="6.83" x2="13.58" y2="16.07"/>
<line x1="19.70" y1="8.76" x2="13.58" y2="16.07"/><line x1="19.70" y1="8.76" x2="17.92" y2="17.85"/>
<line x1="19.70" y1="8.76" x2="9.40" y2="20.70"/><line x1="2.52" y1="14.24" x2="13.58" y2="16.07"/>
<line x1="2.52" y1="14.24" x2="9.40" y2="20.70"/><line x1="13.58" y1="16.07" x2="17.92" y2="17.85"/>
<line x1="13.58" y1="16.07" x2="9.40" y2="20.70"/><line x1="17.92" y1="17.85" x2="9.40" y2="20.70"/>
<line x1="17.92" y1="17.85" x2="21.52" y2="21.48"/>
</g>
<g fill="currentColor">
<circle cx="13.09" cy="2.52" r="1.53"/><circle cx="4.30" cy="5.32" r="1.53"/>
<circle cx="9.45" cy="6.83" r="1.53"/><circle cx="19.70" cy="8.76" r="1.53"/>
<circle cx="2.52" cy="14.24" r="1.53"/><circle cx="13.58" cy="16.07" r="1.53"/>
<circle cx="17.92" cy="17.85" r="1.53"/><circle cx="9.40" cy="20.70" r="1.53"/>
<circle cx="21.52" cy="21.48" r="1.53"/>
</g>
</svg>`;
}

/**
 * A panel's branded header row: the mark, the title, optional right-side
 * actions (already-rendered, trusted HTML from the caller -- the TITLE is
 * escaped here, the actions are the caller's own buttons).
 */
export function brandHeader(title: string, actionsHtml = ""): string {
  const actions = actionsHtml === "" ? "" : `<div class="head-actions">${actionsHtml}</div>`;
  return `<div class="brand-head">${brandMarkSvg(20)}<h1>${escapeHtml(title)}</h1>${actions}</div>`;
}

/**
 * The slim product strip for panels whose HEADING belongs to the screen
 * underneath (the wizard's screens each carry their own h1): the mark plus the
 * product name as a span, so the page keeps exactly one h1.
 */
export function brandStrip(label: string, actionsHtml = ""): string {
  const actions = actionsHtml === "" ? "" : `<div class="head-actions">${actionsHtml}</div>`;
  return `<div class="brand-head">${brandMarkSvg(18)}<span class="brand-name">${escapeHtml(label)}</span>${actions}</div>`;
}
