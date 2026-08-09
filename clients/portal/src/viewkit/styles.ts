// One-time injection of view-kit's own stylesheet.
//
// view-kit ships its CSS as a STRING export rather than a .css file (see the
// long note at the top of sdk/ts-viewkit/src/styles.ts): the package has no
// DOM and no bundler assumptions, so it hands the sheet to the host and lets
// the host decide how to install it. A webview interpolates it into a
// nonce-carrying <style>; a bundler-based host like this one appends a
// <style> element.
//
// THIS IS WHY THE PORTAL'S CSP CARRIES `style-src 'self' 'unsafe-inline'`
// (component/portal/csp.go). A <style> element created at runtime counts as
// an inline style block, so a bare `style-src 'self'` drops the sheet and the
// row list renders unstyled. `script-src` stays strict -- there is no inline
// script anywhere in the portal, which is the property that actually matters.
// Identity's web UI made the same trade for the same reason
// (component/identity/web/csp.go).

import { viewKitStyles } from "@znasllc-io/memql-view-kit";

const STYLE_ID = "memql-view-kit-styles";

// ensureViewKitStyles installs the sheet once per document. Idempotent by the
// element id rather than by a module-level boolean: a test that remounts the
// tree, or a hot reload that re-evaluates this module, would otherwise append
// a duplicate sheet each time.
export function ensureViewKitStyles(): void {
  const doc = globalThis.document;
  if (!doc || doc.getElementById(STYLE_ID)) return;
  const style = doc.createElement("style");
  style.id = STYLE_ID;
  style.textContent = viewKitStyles;
  doc.head.appendChild(style);
}
