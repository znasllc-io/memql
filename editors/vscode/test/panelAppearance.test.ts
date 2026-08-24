// Every panel wears the resolved appearance, and repaints when it changes
// (memql#4419, D2).
//
// TWO LAYERS, because neither alone is enough.
//
// The SOURCE SWEEP is the complete one. There are seven `*Panel.ts` files and
// NINE panel classes in them (automationPanel.ts and runPanel.ts each hold
// two), and driving all nine through a render test would mean constructing
// nine different dependency sets -- so the coverage would quietly become
// "whichever panels were cheap to build". A sweep asks the question of every
// panel that exists, including the one added next month.
//
// The RENDER TEST is the honest one. A sweep proves the source SAYS the right
// thing; it cannot prove the attribute reaches the page, that the resolver is
// wired to the right setting, or -- the part that makes the setting worth
// having at all -- that flipping it repaints a panel that is ALREADY OPEN. A
// setting that only took effect on the next open would satisfy every sweep in
// this file and still read as broken.
//
// The render case forces LIGHT under a DARK editor deliberately. Under a light
// editor a forced-light panel is indistinguishable from a panel that stamps
// nothing at all, so the interesting direction is the one where the setting
// and the editor disagree and the setting has to win.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";

import type { ExtensionContext } from "vscode";

import type { CatalogConstruct } from "../src/state/constructCatalog.js";
import { effectiveTheme } from "../src/webview/appearance.js";
import { ConstructPanel } from "../src/webview/constructPanel.js";
import {
  ColorThemeKind,
  fireConfigurationChange,
  recorded,
  resetRecorded,
  setActiveColorTheme,
  writtenSettings,
  type StubWebviewPanel,
} from "./support/vscodeStub.js";

// dist-test/test/<name>.js, so the package root is two levels up.
const ROOT = path.resolve(__dirname, "..", "..");
const PANEL_DIR = path.join(ROOT, "src", "webview");

/**
 * Every `*Panel.ts` under src/webview, read as text.
 *
 * READ AS "latin1", not "utf8", and that is not a style choice. runPanel.ts
 * used to carry two RAW NUL BYTES inside panelKey()'s template literal, which
 * made `file` call it "data" and made plain `grep` skip it in SILENCE -- which
 * is exactly how the design record for this epic came to say "all six webview
 * panels" when there are seven. The bytes are gone (they are written as \u0000
 * escapes now), but a decoder that cannot fail is what keeps a future stray
 * byte from turning this gate's coverage back into six of seven with nothing
 * reported. `readFileSync(path, "utf8")` would not throw either -- it would
 * substitute U+FFFD and carry on -- so the protection here is the assertion
 * below that the file COUNT is what we expect, not the encoding alone.
 */
function panelFiles(): { name: string; text: string }[] {
  return fs
    .readdirSync(PANEL_DIR)
    .filter((name) => name.endsWith("Panel.ts"))
    .sort()
    .map((name) => ({ name, text: fs.readFileSync(path.join(PANEL_DIR, name), "latin1") }));
}

test("the sweep sees every panel file, and there are seven of them", () => {
  // A COUNT, deliberately. Every other assertion in this file is a loop over
  // whatever panelFiles() returns, so a sweep that silently returned nothing
  // -- a renamed directory, a changed suffix, a decode that dropped a file --
  // would pass all of them while examining no code at all. This is the
  // reachable positive that makes the rest of the file mean something.
  const names = panelFiles().map((f) => f.name);
  assert.deepEqual(
    names,
    [
      "addClusterPanel.ts",
      "automationPanel.ts",
      "conceptPanel.ts",
      "connectionPanel.ts",
      "constructPanel.ts",
      "deploymentPanel.ts",
      "runPanel.ts",
    ],
    "ADDED OR REMOVED A PANEL? Update this list, and make sure the new panel stamps ${currentBodyThemeAttr()} on its <body> and calls onAppearanceChange(). test/brandCoverage.test.ts carries the same list for the brand block.",
  );
});

test("every <body> a panel emits carries the resolved theme attribute", () => {
  // Checked per OCCURRENCE rather than per file: automationPanel.ts and
  // runPanel.ts each host two panel classes with two separate documents, and a
  // file-level "mentions bodyThemeAttr somewhere" check would pass with one of
  // the two still bare.
  const bare: string[] = [];
  for (const { name, text } of panelFiles()) {
    for (const match of text.matchAll(/<body(.{0,24})/g)) {
      if (!match[1].startsWith("${")) {
        bare.push(`${name}: <body${match[1].split("\n")[0]}`);
      }
    }
  }
  assert.deepEqual(
    bare,
    [],
    "a bare <body> renders with the light palette whatever the setting says -- interpolate ${currentBodyThemeAttr()}",
  );
});

test("every panel subscribes to appearance changes", () => {
  // The half that makes the setting live. Without it a panel picks up the new
  // appearance only when it is next opened, which reads to an operator as "the
  // setting does nothing".
  const missing = panelFiles()
    .filter(({ text }) => !text.includes("onAppearanceChange("))
    .map(({ name }) => name);
  assert.deepEqual(
    missing,
    [],
    "call onAppearanceChange(() => this.render()) and push the disposables onto the panel's own list",
  );
});

test("no panel reads memql.appearance for itself", () => {
  // ONE resolver, one reading of it (D2). A panel that read the setting
  // directly would be a second opinion about it -- and the one most likely to
  // forget that high contrast wins, since that rule lives in appearance.ts and
  // nowhere near a panel.
  //
  // MATCHED ON THE KEY, not on the word "memql". An earlier draft also flagged
  // a bare `'memql'`, which is too broad to live in a shared tree: src/ uses
  // single-quoted strings in places, `memql` is this extension's own prefix for
  // every command, view and viewType, and a panel naming one for an unrelated
  // reason would fail this gate with a message about appearance. Reading the
  // setting REQUIRES naming its key, so the key is what to look for.
  const offenders = panelFiles()
    .filter(
      ({ text }) =>
        text.includes('getConfiguration("memql")') ||
        text.includes("getConfiguration('memql')") ||
        text.includes('"appearance"') ||
        text.includes("'appearance'"),
    )
    .map(({ name }) => name);
  assert.deepEqual(
    offenders,
    [],
    "resolve through src/webview/theme.ts (currentBodyThemeAttr / onAppearanceChange), never in a panel",
  );
});

// -----------------------------------------------------------------------------
// The manifest contribution
// -----------------------------------------------------------------------------

test("package.json contributes memql.appearance as the documented enum", () => {
  // The setting has to EXIST in the manifest, not just be readable: an
  // undeclared key gets no Settings-UI row, no enum picker and no default, so
  // the feature would be reachable only by hand-editing settings.json -- and
  // `get()` would answer undefined forever, which resolves to `system` and
  // looks exactly like a working default.
  const manifest = JSON.parse(fs.readFileSync(path.join(ROOT, "package.json"), "utf8")) as {
    contributes: { configuration: { properties: Record<string, Record<string, unknown>> } };
  };
  const setting = manifest.contributes.configuration.properties["memql.appearance"];
  assert.ok(setting !== undefined, "memql.appearance is contributed");
  assert.equal(setting.type, "string");
  assert.deepEqual(setting.enum, ["system", "light", "dark"]);
  assert.equal(setting.default, "system", "system is the default -- the panels follow the editor");
  assert.equal(
    (setting.enum as string[]).length,
    (setting.enumDescriptions as string[]).length,
    "every enum value carries its own description, or the picker shows blanks",
  );
  // The one thing the description MUST say. "System" is ambiguous in an editor
  // that itself has a "follow the OS" mode, and the answer -- it follows the
  // EDITOR, which may in turn follow the OS -- is not guessable.
  assert.match(String(setting.description), /editor/i);
});

test("the manifest's enum is exactly the resolver's vocabulary", () => {
  // Two lists that must not drift: a value the manifest offers but the
  // resolver does not know falls through to `system`, so the picker would
  // present a choice that silently does nothing.
  const manifest = JSON.parse(fs.readFileSync(path.join(ROOT, "package.json"), "utf8")) as {
    contributes: { configuration: { properties: Record<string, { enum?: string[] }> } };
  };
  const offered = manifest.contributes.configuration.properties["memql.appearance"]?.enum ?? [];
  for (const value of offered) {
    const resolved = effectiveTheme(value, ColorThemeKind.Dark);
    const forced = effectiveTheme(value, ColorThemeKind.Light);
    // `system` is the one value that is ALLOWED to follow the editor; every
    // other offered value must pin the same answer under both editor kinds.
    if (value === "system") {
      assert.notEqual(resolved, forced, "system must track the editor");
    } else {
      assert.equal(resolved, forced, `"${value}" must override the editor, or it does nothing`);
    }
  }
});

// -----------------------------------------------------------------------------
// The render test
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

// The panel only calls these from a click, and this file never clicks.
const DEPS = {
  viewSourceFromCluster: () => Promise.resolve(),
  browseRowsInPortal: () => Promise.resolve(),
};

const CONTEXT = { subscriptions: [] as { dispose(): unknown }[] } as unknown as ExtensionContext;

// ConstructPanel is a SINGLETON: a second open() reveals the existing panel
// instead of creating one. So the panel a previous case left behind has to be
// closed before this one opens -- and closed HERE rather than at the end of
// each case, because a case that fails an assertion never reaches its own
// cleanup and would take every case after it down with it.
let live: StubWebviewPanel | undefined;

/**
 * The panel's `<body …>` tag, which is the ONLY place these cases may look.
 *
 * Not a nicety: `brandStyleBlock()` now emits a
 * `body[data-memql-theme="dark"] { … }` RULE into every panel's inline
 * stylesheet, so `html.includes("data-memql-theme")` is true of every document
 * whether or not anything was stamped. A whole-document assertion would
 * therefore pass for a panel that stamps nothing at all -- which is precisely
 * the bug this file exists to catch.
 */
function bodyTagOf(html: string): string {
  const tag = /<body[^>]*>/.exec(html);
  assert.ok(tag !== null, "the panel rendered a <body> tag");
  return tag[0];
}

/** Reset the editor state, close any panel still open, and open a fresh one. */
function openPanel(appearance: string | undefined, kind: number): StubWebviewPanel {
  live?.close();
  live = undefined;
  resetRecorded();
  writtenSettings.clear();
  if (appearance !== undefined) writtenSettings.set("memql.appearance", appearance);
  setActiveColorTheme(kind);

  ConstructPanel.open(CONTEXT, CONSTRUCT, DEPS, "local");
  const panel = recorded.webviews.at(-1);
  assert.ok(panel !== undefined, "the panel was created");
  live = panel;
  return panel;
}

test("a forced light appearance under a dark editor stamps light", () => {
  const panel = openPanel("light", ColorThemeKind.Dark);
  assert.equal(bodyTagOf(panel.html), '<body data-memql-theme="light">');
});

test("the default appearance follows the editor, in both directions", () => {
  const dark = openPanel(undefined, ColorThemeKind.Dark);
  assert.equal(bodyTagOf(dark.html), '<body data-memql-theme="dark">');

  const light = openPanel(undefined, ColorThemeKind.Light);
  assert.equal(bodyTagOf(light.html), '<body data-memql-theme="light">');
});

test("a high-contrast editor stamps nothing, whatever the setting says", () => {
  // The attribute's ABSENCE is the assertion: brandTokens.ts's high-contrast
  // rules key on VS Code's own body classes, and a data-memql-theme beside
  // them would hand the cascade a second opinion.
  const panel = openPanel("dark", ColorThemeKind.HighContrast);
  assert.equal(bodyTagOf(panel.html), "<body>", "high contrast defers to VS Code entirely");
});

test("changing memql.appearance repaints an OPEN panel", () => {
  const panel = openPanel(undefined, ColorThemeKind.Dark);
  const before = panel.renders;
  assert.equal(bodyTagOf(panel.html), '<body data-memql-theme="dark">');

  writtenSettings.set("memql.appearance", "light");
  fireConfigurationChange("memql.appearance");

  assert.ok(panel.renders > before, "the open panel re-rendered rather than waiting to be reopened");
  assert.equal(bodyTagOf(panel.html), '<body data-memql-theme="light">');
});

test("changing the editor's theme repaints an open panel on the system default", () => {
  const panel = openPanel(undefined, ColorThemeKind.Dark);
  assert.equal(bodyTagOf(panel.html), '<body data-memql-theme="dark">');

  setActiveColorTheme(ColorThemeKind.Light);

  assert.equal(bodyTagOf(panel.html), '<body data-memql-theme="light">');
});

test("an unrelated setting does not repaint the panel", () => {
  // The guard on affectsConfiguration. Without it every panel re-renders on
  // every settings.json keystroke -- and a re-render replaces the whole
  // document, which destroys focus, caret and selection (see StubWebviewPanel's
  // `renders` doc comment).
  const panel = openPanel(undefined, ColorThemeKind.Dark);
  const before = panel.renders;

  fireConfigurationChange("memql.lsp.serverPath");

  assert.equal(panel.renders, before, "only memql.appearance may trigger an appearance repaint");
});

test("a closed panel stops listening", () => {
  // The listener holds a reference to the panel; if closing the tab did not
  // remove it, every panel an operator ever opened would re-render on every
  // theme flip for the life of the window.
  const panel = openPanel(undefined, ColorThemeKind.Dark);
  panel.close();
  live = undefined;
  const after = panel.renders;

  setActiveColorTheme(ColorThemeKind.Light);
  fireConfigurationChange("memql.appearance");

  assert.equal(panel.renders, after, "a disposed panel must not be rendered into");
});
