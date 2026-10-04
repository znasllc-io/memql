// The MemQL brand, as far as a VS Code webview can carry it (memql#4196;
// re-keyed onto the appearance setting in memql#4419).
//
// ONE token module for every panel. Canonical brand roles and the brighter
// dark editor surfaces live next door in palette.ts as DATA, because a
// theme-JSON generator cannot
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
//     eight panels is weight without leverage -- the editor already renders a
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

import { DARK, LIGHT, PALETTE_KEYS, type PaletteKey } from "./palette.js";
import { kitStyles } from "./ui/kitStyles.js";

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
  "accent-subtle": "var(--vscode-editor-background)",
  "on-accent": "var(--vscode-editor-background)",
  "on-accent-hover": "var(--vscode-editor-background)",
  focus: "var(--vscode-focusBorder)",
  ok: "var(--vscode-charts-green, var(--vscode-foreground))",
  warn: "var(--vscode-editorWarning-foreground, var(--vscode-foreground))",
  "warn-subtle": "var(--vscode-editor-background)",
  danger: "var(--vscode-errorForeground)",
  "danger-subtle": "var(--vscode-editor-background)",
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
 *
 * The page kit's stylesheet (ui/kitStyles.ts) rides at the end, so every
 * document that carries the brand can use every `mq-*` component with no
 * wiring of its own.
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

  /* Metrics, not colours: no theme may move a box or change how fast anything
     moves. 26px is VS Code's own control line, so a MemQL button sits level
     with the editor's. Reduced motion zeroes the one duration, which is the
     whole opt-out for every transition that reads it. */
  body {
    --memql-control-h: 26px;
    --memql-radius: 4px;
    --memql-radius-lg: 6px;
    --memql-motion-dur: 160ms;
    --memql-motion-ease: cubic-bezier(0.2, 0, 0, 1);
  }
  @media (prefers-reduced-motion: reduce) {
    body { --memql-motion-dur: 0ms; }
  }

  /* Native controls, selection popups and scrollbars follow the palette in
     force, not the editor's. The page's own scrollbar belongs to <html>,
     which the body-scoped tokens do not reach, so it is named from the
     palette directly. High contrast keeps VS Code's. */
  body { color-scheme: light; }
  body[data-memql-theme="light"] { color-scheme: light; }
  body[data-memql-theme="dark"] { color-scheme: dark; }
  body.vscode-high-contrast { color-scheme: dark; }
  body.vscode-high-contrast-light { color-scheme: light; }
  html:has(> body[data-memql-theme="light"]) { color-scheme: light;
    scrollbar-color: ${LIGHT["border-strong"]} ${LIGHT.bg}; }
  html:has(> body[data-memql-theme="dark"]) { color-scheme: dark;
    scrollbar-color: ${DARK["border-strong"]} ${DARK.bg}; }

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
  button:focus-visible { outline: 2px solid var(--memql-focus); outline-offset: 1px; }

  /* ---- the page kit (src/webview/ui/kit.ts): every mq-* component ---- */
${kitStyles()}`;
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
