// The MemQL brand, as far as a VS Code webview can carry it (memql#4196).
//
// ONE token module for every panel. The palette is memql.io's exactly -- the
// same hexes the portal redesign (memql#4177) ships -- selected dark/light by
// the theme class VS Code stamps on the webview body, so the panels read as
// the same product as the portal and the site while still flipping with the
// editor. High contrast defers wholesale to VS Code's own variables: a themed
// green is not worth an accessibility regression.
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

/**
 * The palette + shared component classes, inlined by every panel under its
 * CSP nonce. Light is the default; `body.vscode-dark` flips to the dark
 * palette; both high-contrast classes remap every token onto VS Code's own
 * variables.
 */
export function brandStyleBlock(): string {
  return `
  /* ---- MemQL brand tokens (memql#4196; palette = memql.io / memql#4177) ---- */
  body {
    --memql-bg: #f2f4ef;
    --memql-surface: #ffffff;
    --memql-raised: #e9ede6;
    --memql-border: #d6ddd4;
    --memql-border-strong: #c2cabf;
    --memql-fg: #14201a;
    --memql-muted: #586159;
    --memql-subtle: #7c847b;
    --memql-accent: #047d5a;
    --memql-accent-deep: #026842;
    --memql-on-accent: #ffffff;
    --memql-on-accent-hover: #ffffff;
    --memql-danger: #b42318;
    --memql-data-number: #0f766e;
    --memql-data-string: #b45309;
  }
  body.vscode-dark {
    --memql-bg: #07090a;
    --memql-surface: #0b1110;
    --memql-raised: #0e1311;
    --memql-border: #18231e;
    --memql-border-strong: #213029;
    --memql-fg: #e8e6dd;
    --memql-muted: #9ca395;
    --memql-subtle: #6c726a;
    --memql-accent: #5ccda7;
    --memql-accent-deep: #026842;
    --memql-on-accent: #052e21;
    --memql-on-accent-hover: #ffffff;
    --memql-danger: #f97066;
    --memql-data-number: #98ffe0;
    --memql-data-string: #cbb083;
  }
  /* High contrast is VS Code's contract, not ours: every token defers. */
  body.vscode-high-contrast, body.vscode-high-contrast-light {
    --memql-bg: var(--vscode-editor-background);
    --memql-surface: var(--vscode-editor-background);
    --memql-raised: var(--vscode-editor-background);
    --memql-border: var(--vscode-contrastBorder, var(--vscode-panel-border));
    --memql-border-strong: var(--vscode-contrastBorder, var(--vscode-panel-border));
    --memql-fg: var(--vscode-foreground);
    --memql-muted: var(--vscode-foreground);
    --memql-subtle: var(--vscode-descriptionForeground);
    --memql-accent: var(--vscode-focusBorder);
    --memql-accent-deep: var(--vscode-focusBorder);
    --memql-on-accent: var(--vscode-editor-background);
    --memql-on-accent-hover: var(--vscode-editor-background);
    --memql-danger: var(--vscode-errorForeground);
    --memql-data-number: var(--vscode-foreground);
    --memql-data-string: var(--vscode-foreground);
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
