// Every panel document inlines the brand tokens (memql#4422, D5).
//
// WHAT IT IS FOR. All nine panel classes wear the brand today. This gate is
// what keeps the tenth honest: a new panel that forgets `brandStyleBlock()`
// renders with VS Code's raw defaults, which does not look broken -- it looks
// like a different product, which is the exact complaint that opened this
// epic and the exact thing nobody notices in review.
//
// WHY A SOURCE CHECK AND NOT A RENDER OF ALL NINE. Nine panel classes take
// nine different dependency sets, several of which need a live
// ConnectionManager. A render-of-everything gate would in practice become a
// render-of-whatever-was-cheap gate, and the panels left out would be exactly
// where the next one goes wrong. So the sweep is over source -- and the last
// case in this file renders a real panel and finds real palette bytes on the
// page, which is what ties the source check to reality.
//
// WHY THIS PARTICULAR SOURCE CHECK -- D5 says "pick the one that cannot be
// satisfied vacuously and say why in a comment", so here is the why. Two
// weaker checks were available and both can pass while a panel ships
// unbranded:
//
//   * "the FILE imports brandStyleBlock" -- an import is not a call. A panel
//     could import it, never call it, and pass.
//   * "the FILE mentions brandStyleBlock()" -- a call is not an inlining, and
//     worse, it is per-file. automationPanel.ts and runPanel.ts each build TWO
//     documents; one of them could drop the block entirely and the file would
//     still mention it, once, on behalf of the other.
//
// So the unit here is the DOCUMENT, not the file: every `<!DOCTYPE html>` …
// `</html>` template in the tree must reach `brandStyleBlock()` through its
// OWN interpolations. One level of same-file indirection is resolved, because
// both two-document panels legitimately share a local `panelChrome()` helper
// that calls it -- refusing that would push them into duplicating the chrome,
// which is a worse outcome than the gate is worth.
//
// AND THE GATE TESTS ITSELF. `panelsMissingBrand()` is run over two synthetic
// fixtures below: one bare document it must reject, one indirect document it
// must accept. Without those, a detector that returned an empty list for any
// reason -- a changed suffix, a regex that stopped matching -- would report
// "all panels branded" forever while examining nothing.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";

import type { ExtensionContext } from "vscode";

import type { CatalogConstruct } from "../src/state/constructCatalog.js";
import { ConstructPanel } from "../src/webview/constructPanel.js";
import { DARK, LIGHT } from "../src/webview/palette.js";
import { ColorThemeKind, recorded, resetRecorded, setActiveColorTheme, writtenSettings } from "./support/vscodeStub.js";

const ROOT = path.resolve(__dirname, "..", "..");
const PANEL_DIR = path.join(ROOT, "src", "webview");

/** The call every document must reach, one way or another. */
const BRAND_CALL = "brandStyleBlock()";

/**
 * Read a source file so that a stray control byte cannot make it vanish.
 *
 * "latin1" decodes any byte sequence, so nothing here can throw or be skipped.
 * That is not paranoia: runPanel.ts carried two raw NUL bytes until memql#4422,
 * which made `file` call it "data" and made plain `grep` skip it SILENTLY --
 * and that is how this epic's design record came to say "all six webview
 * panels" when there are seven. A gate over panels must not be able to lose
 * one without saying so, which is also why the file list is asserted below.
 */
function read(file: string): string {
  return fs.readFileSync(file, "latin1");
}

/** Every `*Panel.ts` under src/webview, by name. */
function panelNames(): string[] {
  return fs
    .readdirSync(PANEL_DIR)
    .filter((name) => name.endsWith("Panel.ts"))
    .sort();
}

/**
 * The bodies of this file's own top-level `function name(...) { … }`
 * declarations, keyed by name.
 *
 * A brace-counting parse would be more precise; this takes the pragmatic
 * route, ending a body at the first line that is exactly `}` in column zero,
 * which is what this codebase's formatting always produces. The failure
 * direction is safe: a mis-parsed body means a helper LOOKS not to call
 * brandStyleBlock, so the gate fails loudly and a human looks.
 */
function localFunctions(source: string): Map<string, string> {
  const out = new Map<string, string>();
  const lines = source.split("\n");
  for (let i = 0; i < lines.length; i += 1) {
    const declaration = /^function\s+([A-Za-z_$][\w$]*)\s*\(/.exec(lines[i]);
    if (declaration === null) continue;
    const body: string[] = [];
    for (let j = i + 1; j < lines.length && lines[j] !== "}"; j += 1) body.push(lines[j]);
    out.set(declaration[1], body.join("\n"));
  }
  return out;
}

/**
 * Every full HTML document a source file builds.
 *
 * A document is a `<!DOCTYPE html>` … `</html>` span. Panels assign
 * `webview.html` with exactly this shape; a fragment module (the `*Screens.ts`
 * files) has no doctype and therefore contributes none, which is what makes
 * their exemption structural rather than a name on a list.
 */
function documents(source: string): string[] {
  const out: string[] = [];
  let from = 0;
  for (;;) {
    const start = source.indexOf("<!DOCTYPE html>", from);
    if (start < 0) break;
    const end = source.indexOf("</html>", start);
    out.push(source.slice(start, end < 0 ? source.length : end));
    from = end < 0 ? source.length : end + 1;
  }
  return out;
}

/** Does this document reach brandStyleBlock(), directly or via a local helper? */
function documentIsBranded(document: string, helpers: Map<string, string>): boolean {
  if (document.includes(BRAND_CALL)) return true;
  for (const call of document.matchAll(/\$\{\s*([A-Za-z_$][\w$]*)\s*\(\s*\)\s*\}/g)) {
    const body = helpers.get(call[1]);
    if (body !== undefined && body.includes(BRAND_CALL)) return true;
  }
  return false;
}

/**
 * Panel documents that never reach the brand tokens.
 *
 * Reported as `file#n` so a two-document file names WHICH document is bare --
 * "automationPanel.ts" alone would send a reader to the branded one first.
 */
function panelsMissingBrand(sources: { name: string; text: string }[]): string[] {
  const bare: string[] = [];
  for (const { name, text } of sources) {
    const helpers = localFunctions(text);
    documents(text).forEach((document, index) => {
      if (!documentIsBranded(document, helpers)) bare.push(`${name}#${index + 1}`);
    });
  }
  return bare;
}

// -----------------------------------------------------------------------------
// The gate can fail, and can pass for the right reason
// -----------------------------------------------------------------------------

test("the detector rejects a bare panel document", () => {
  // The scratch panel D5 asks for, kept as a FIXTURE rather than run once by
  // hand and deleted. A one-off check proves the gate worked on the day
  // somebody tried it; this proves it on every run, including after the
  // refactor that quietly breaks the regex.
  const bare = `
    this.panel.webview.html = \`<!DOCTYPE html>
    <html lang="en">
    <head><style nonce="\${nonce}">body { padding: 0; }</style></head>
    <body></body>
    </html>\`;
  `;
  assert.deepEqual(panelsMissingBrand([{ name: "scratchPanel.ts", text: bare }]), [
    "scratchPanel.ts#1",
  ]);
});

test("the detector accepts a document that reaches the tokens through a local helper", () => {
  // The other direction, and the reason the detector resolves one level at
  // all: both two-document panels share a local panelChrome(). A gate that
  // could not see through it would be telling them to duplicate the chrome.
  const indirect = `
    this.panel.webview.html = \`<!DOCTYPE html>
    <html lang="en">
    <head><style nonce="\${nonce}">\${panelChrome()}</style></head>
    <body></body>
    </html>\`;

function panelChrome(): string {
  return \`\${brandStyleBlock()}
  body { padding: 0; }\`;
}
`;
  assert.deepEqual(panelsMissingBrand([{ name: "indirectPanel.ts", text: indirect }]), []);
});

test("the detector is not fooled by an import alone", () => {
  // The weaker check D5 rejects, shown rejecting. An import is not a call.
  const importOnly = `
import { brandStyleBlock } from "./brandTokens.js";
    this.panel.webview.html = \`<!DOCTYPE html>
    <html lang="en"><body></body></html>\`;
  `;
  assert.deepEqual(panelsMissingBrand([{ name: "importOnlyPanel.ts", text: importOnly }]), [
    "importOnlyPanel.ts#1",
  ]);
});

test("the detector is not fooled by a sibling document carrying the block", () => {
  // The per-FILE check, shown failing to catch what the per-DOCUMENT one does.
  // This is the case automationPanel.ts and runPanel.ts make possible.
  const twoDocs = `
    a.webview.html = \`<!DOCTYPE html><html><head><style>\${brandStyleBlock()}</style></head><body></body></html>\`;
    b.webview.html = \`<!DOCTYPE html><html><head><style>body{}</style></head><body></body></html>\`;
  `;
  assert.deepEqual(panelsMissingBrand([{ name: "twoDocPanel.ts", text: twoDocs }]), [
    "twoDocPanel.ts#2",
  ]);
});

// -----------------------------------------------------------------------------
// The sweep
// -----------------------------------------------------------------------------

test("the sweep sees all seven panel files and nine documents", () => {
  // The reachable positive for the sweep below. Every assertion after this is
  // "nothing was missing"; without a count, a sweep that found no files at all
  // -- a renamed directory, a changed suffix -- would satisfy them all while
  // examining nothing.
  const names = panelNames();
  assert.deepEqual(names, [
    "addClusterPanel.ts",
    "automationPanel.ts",
    "conceptPanel.ts",
    "connectionPanel.ts",
    "constructPanel.ts",
    "deploymentPanel.ts",
    "runPanel.ts",
  ]);
  const total = names.reduce(
    (sum, name) => sum + documents(read(path.join(PANEL_DIR, name))).length,
    0,
  );
  assert.equal(total, 9, "seven files, nine panel classes, nine documents");
});

test("every panel document inlines the brand tokens", () => {
  const sources = panelNames().map((name) => ({
    name,
    text: read(path.join(PANEL_DIR, name)),
  }));
  assert.deepEqual(
    panelsMissingBrand(sources),
    [],
    "inline ${brandStyleBlock()} in this document's <style> block -- an unbranded panel does not look broken, it looks like a different product",
  );
});

test("the *Screens.ts fragment modules are exempt because they build no document", () => {
  // EXEMPT BY CONSTRUCTION, not by name. These three modules produce HTML
  // FRAGMENTS that are interpolated into a panel's document, so they inherit
  // that document's <style> block and have nowhere of their own to put one.
  // Asserting that they emit no document is what keeps the exemption honest:
  // if one of them ever grows a `<!DOCTYPE html>`, it has become a surface
  // with its own chrome, and this test fails and says so rather than silently
  // letting an unbranded page through under a filename the sweep skips.
  const screens = fs
    .readdirSync(PANEL_DIR)
    .filter((name) => name.endsWith("Screens.ts"))
    .sort();
  assert.deepEqual(screens, [
    "constructScreens.ts",
    "deploymentScreens.ts",
    "installScreens.ts",
  ]);
  for (const name of screens) {
    assert.deepEqual(
      documents(read(path.join(PANEL_DIR, name))),
      [],
      `${name} builds a full document -- it is a surface now, so it needs its own brandStyleBlock() and a place in the sweep`,
    );
  }
});

// -----------------------------------------------------------------------------
// The source check corresponds to bytes on the page
// -----------------------------------------------------------------------------

const CONSTRUCT: CatalogConstruct = {
  name: "spaceParticipants",
  kind: "query",
  namespace: "cognition",
  origin: "core",
  originPath: "dsl/cognition/queries.memql",
  description: "Get space participants",
  runnable: false,
  args: [],
  boundConcept: "v1:cognition:participant",
  sourceHash: "",
  source: "",
};

const DEPS = {
  viewSourceFromCluster: () => Promise.resolve(),
  browseRowsInPortal: () => Promise.resolve(),
};

const CONTEXT = { subscriptions: [] as { dispose(): unknown }[] } as unknown as ExtensionContext;

test("a rendered panel really carries the palette, in both themes", () => {
  // What ties every source assertion above to something an operator can see.
  // "The source says brandStyleBlock()" and "the page carries the palette" are
  // different claims, and only the second is the one that matters -- so at
  // least one panel is driven end to end and its bytes are inspected.
  resetRecorded();
  writtenSettings.clear();
  setActiveColorTheme(ColorThemeKind.Dark);

  ConstructPanel.open(CONTEXT, CONSTRUCT, DEPS, "local");
  const panel = recorded.webviews.at(-1);
  assert.ok(panel !== undefined, "the panel was created");

  // Both palettes ship in every document -- the CSS carries light and dark and
  // the stamped attribute picks one -- so both accents must be present.
  assert.ok(panel.html.includes(DARK.accent), "the dark accent is in the inlined stylesheet");
  assert.ok(panel.html.includes(LIGHT.accent), "the light accent is in the inlined stylesheet");
  assert.ok(
    panel.html.includes("--memql-bg"),
    "the token names are inlined, not just the hexes",
  );
  panel.close();
});
