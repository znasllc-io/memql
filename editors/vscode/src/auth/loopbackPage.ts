// The page a browser lands on when it hands the sign-in back to the editor.
//
// WHY IT WAITS FOR THE EXCHANGE. The loopback listener used to answer the
// browser the moment the callback arrived, with "Signed in to MemQL" -- before
// the editor had redeemed the code. The redemption can still fail (a refused
// exchange, an unreachable token endpoint), and then the browser said "signed
// in" while the editor said it was not. So the listener now holds the response
// until the flow reports how the exchange went (loopback.ts `finish`), and this
// module renders one of two honest pages: signed in, or not finished.
//
// WHAT IT CAN REFERENCE. Nothing: it is served from a throwaway loopback port
// that is gone a moment later, so it carries its own styles and the MemQL mark
// inline. It follows the browser's light or dark preference; the gallery
// pins one (`scheme`) so both can be photographed.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import { brandMarkSvg } from "../webview/brandTokens.js";
import { DARK, LIGHT, type Palette } from "../webview/palette.js";

/** How the exchange went: the browser is told only after it settles. */
export type CallbackOutcome = "success" | "failure";

export interface LoopbackPageOptions {
  /** Pins a colour scheme instead of following the browser (the gallery). */
  scheme?: "light" | "dark";
  /** The CSP nonce for the one style block. */
  nonce?: string;
}

const COPY: Readonly<Record<CallbackOutcome, { title: string; line: string }>> = {
  // "your editor", not a product name: the same extension runs in VS Code and
  // in Cursor, and this tab cannot tell which one is waiting for it.
  success: { title: "You're signed in", line: "You can close this tab and return to your editor." },
  failure: { title: "Sign-in didn't finish", line: "Return to your editor to try again." },
};

function vars(p: Palette): string {
  return `--bg:${p.bg};--surface:${p.surface};--border:${p.border};--fg:${p.fg};--muted:${p.muted};--accent:${p.accent};--danger:${p.danger};`;
}

/** The whole document, ready to write to the held response. */
export function loopbackPage(outcome: CallbackOutcome, options: LoopbackPageOptions = {}): string {
  const copy = COPY[outcome];
  const nonce = options.nonce ?? "memql";
  const light = vars(LIGHT);
  const dark = vars(DARK);
  const scheme =
    options.scheme === "dark"
      ? `:root{${dark}color-scheme:dark;}`
      : options.scheme === "light"
        ? `:root{${light}color-scheme:light;}`
        : `:root{${light}color-scheme:light dark;}@media (prefers-color-scheme: dark){:root{${dark}}}`;
  return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'nonce-${nonce}';">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>${copy.title}</title>
<style nonce="${nonce}">
${scheme}
html,body{margin:0;padding:0;height:100%;}
body{background:var(--bg);color:var(--fg);font:15px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif;display:flex;align-items:center;justify-content:center;}
main{box-sizing:border-box;width:100%;max-width:24rem;margin:16px;padding:32px 28px;background:var(--surface);border:1px solid var(--border);border-radius:10px;text-align:center;}
.mark{color:var(--accent);display:flex;justify-content:center;margin:0 0 18px;}
main[data-outcome="failure"] .mark{color:var(--muted);}
h1{font-size:1.2rem;font-weight:600;margin:0 0 6px;}
p{margin:0;color:var(--muted);}
</style>
</head>
<body>
<main data-outcome="${outcome}">
<div class="mark">${brandMarkSvg(40)}</div>
<h1>${copy.title}</h1>
<p>${copy.line}</p>
</main>
</body>
</html>`;
}
