// The document every kit page is assigned in: the CSP, the brand and kit
// stylesheet under the nonce, the three regions, and the page script.
//
// ONE WRAPPER, so a panel adopting the kit writes its screens and nothing
// else. It is the natural `wrap` for LiveView:
//
//   new LiveView(target, (parts, screen) =>
//     pageDocument({ nonce, title, themeAttr: currentBodyThemeAttr(), screen, ...parts }))
//
// THE CSP IS THE SAME ONE EVERY PANEL ALREADY USES -- `default-src 'none'`,
// styles and the one script by nonce, nothing else -- which is also what
// keeps inline `style` attributes out: the kit never writes one.
//
// THE SCREEN KEY rides in a `<meta>` rather than on `<body>`, so the body tag
// stays exactly `<body data-memql-theme="...">` (test/panelAppearance.test.ts
// reads it) and the page script can tell a reload of the same screen, whose
// scroll it restores, from a different one.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import { escapeHtml } from "@znasllc-io/memql-view-kit";

import { brandStyleBlock } from "../brandTokens.js";
import { page } from "./kit.js";
import { PAGE_RUNTIME } from "./runtime.js";

export interface PageDocumentInput {
  /** The per-document nonce the CSP names. */
  nonce: string;
  /** The document title (the panel's tab title is set separately). */
  title: string;
  /** `bodyThemeAttr()` output, leading space included, or "" in high contrast. */
  themeAttr: string;
  /** The LiveView screen key; scroll is restored only for the same screen. */
  screen?: string;
  /** Posted when Escape is pressed anywhere on the page. */
  escapeAct?: string;
  /** Panel-local CSS: layout only, never colour (the kit and tokens own that). */
  styles?: string;
  head: string;
  body: string;
  actions: string;
}

/** A complete kit page, ready for `webview.html`. */
export function pageDocument(i: PageDocumentInput): string {
  const nonce = escapeHtml(i.nonce);
  const screen = i.screen === undefined ? "" : `\n<meta name="memql-screen" content="${escapeHtml(i.screen)}">`;
  const escapeAct = i.escapeAct === undefined ? "" : ` data-escape-act="${escapeHtml(i.escapeAct)}"`;
  return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta http-equiv="Content-Security-Policy"
      content="default-src 'none'; style-src 'nonce-${nonce}'; script-src 'nonce-${nonce}';">
<meta name="viewport" content="width=device-width, initial-scale=1">${screen}
<title>${escapeHtml(i.title)}</title>
<style nonce="${nonce}">
${brandStyleBlock()}
${i.styles ?? ""}
</style>
</head>
<body${i.themeAttr}${escapeAct}>
${page({ head: i.head, body: i.body, actionBar: i.actions })}
<script nonce="${nonce}">${PAGE_RUNTIME}</script>
</body>
</html>`;
}
