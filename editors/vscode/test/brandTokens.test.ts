// The webview brand system (memql#4196, re-keyed for the appearance setting in
// memql#4419).
//
// Four claims, each mechanically checkable:
//   1. The CSS uses the shared editor palette in both themes, and high
//      contrast defers to the editor. palette.test.ts gates brand parity.
//   2. The dark palette is selected by the attribute MemQL stamps
//      (`body[data-memql-theme="dark"]`), NOT by the class VS Code stamps
//      (`body.vscode-dark`). That swap is the whole of decision D2: while the
//      selector was VS Code's class, `memql.appearance` could not exist,
//      because the editor's theme was the only input the cascade could see.
//   3. The inline header mark is the SAME geometry as the gated activity-bar
//      asset (icons/memql-activity.svg) -- one mark, two carriers, no drift.
//   4. No panel hardcodes a palette hex outside the token module. Tree items
//      are covered by the same sweep over src/views (they must speak
//      ThemeColor, never hex).

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";

import { brandMarkSvg, brandStyleBlock } from "../src/webview/brandTokens.js";

const ROOT = path.resolve(__dirname, "..", "..");

import { DARK, LIGHT } from "../src/webview/palette.js";

const DARK_SELECTOR = 'body[data-memql-theme="dark"]';

test("both MemQL editor palettes are present, dark scoped to the stamped attribute", () => {
  const css = brandStyleBlock();
  for (const hex of [...Object.values(DARK), ...Object.values(LIGHT)]) {
    assert.ok(css.includes(hex), `palette hex ${hex} missing`);
  }
  const darkBlock = css.slice(css.indexOf(DARK_SELECTOR), css.indexOf("high-contrast"));
  assert.ok(darkBlock.includes(`--memql-bg: ${DARK.bg}`), "dark bg lives in the stamped-attribute block");
  assert.ok(darkBlock.includes(`--memql-data-number: ${DARK["data-number"]}`), "dark data tint lives in the stamped-attribute block");
});

test("the dark palette is no longer selected by the editor's own theme class", () => {
  // The REGRESSION this file exists to prevent. `body.vscode-dark` is stamped
  // by VS Code from the EDITOR's theme; while it selected the dark palette,
  // `memql.appearance: light` under a dark editor was unimplementable -- the
  // cascade had no input but the editor's. Leaving the old selector in place
  // alongside the new one would be worse than either: whichever came last in
  // the block would win, so a forced-light panel under a dark editor would
  // flip back to dark and nothing would say why.
  const css = brandStyleBlock();
  assert.ok(css.includes(DARK_SELECTOR), "the dark palette selects on the stamped attribute");
  assert.ok(
    !css.includes("body.vscode-dark"),
    "body.vscode-dark must not select a palette -- the setting, not the editor, decides",
  );
});

test("high contrast defers every token to VS Code variables", () => {
  const css = brandStyleBlock();
  const hcStart = css.indexOf("body.vscode-high-contrast");
  assert.ok(hcStart > 0, "high-contrast block exists");
  const hcBlock = css.slice(hcStart, css.indexOf("}", css.indexOf("--memql-data-string", hcStart)));
  assert.doesNotMatch(hcBlock, /#[0-9a-fA-F]{3,8}\b/, "no literal color survives in high contrast");
  assert.match(hcBlock, /--memql-accent: var\(--vscode-/);
});

test("view-kit tokens ride the brand tokens", () => {
  const css = brandStyleBlock();
  assert.match(css, /--vk-fg: var\(--memql-fg\)/);
  assert.match(css, /--vk-border: var\(--memql-border\)/);
});

test("the inline mark is the activity-bar asset's geometry", () => {
  const asset = fs.readFileSync(
    path.join(ROOT, "icons", "memql-activity.svg"),
    "utf8",
  );
  const circles = (svg: string): string[] =>
    [...svg.matchAll(/<circle cx="([\d.]+)" cy="([\d.]+)" r="([\d.]+)"\s*\/>/g)]
      .map((m) => `${m[1]},${m[2]},${m[3]}`)
      .sort();
  const assetCircles = circles(asset);
  const inlineCircles = circles(brandMarkSvg(20));
  assert.equal(assetCircles.length, 9, "the mark is the 9-node graph");
  assert.deepEqual(inlineCircles, assetCircles, "one mark, two carriers, no drift");
});

// The two files permitted to name a hex, and what each is for. brandTokens.ts
// composes CSS; palette.ts holds the values it composes from (memql#4419).
// Nothing else in either directory may: a panel with its own hex is a colour
// that does not flip with the appearance setting, and a tree item with one is a
// colour that ignores the user's editor theme outright.
const TOKEN_MODULES = new Set(["brandTokens.ts", "palette.ts"]);

/** Every .ts file under a source directory, subdirectories included (src/webview/ui is the kit). */
function sourcesUnder(dir: string): string[] {
  return fs
    .readdirSync(path.join(ROOT, dir), { withFileTypes: true })
    .flatMap((entry) =>
      entry.isDirectory() ? sourcesUnder(`${dir}/${entry.name}`) : entry.name.endsWith(".ts") ? [`${dir}/${entry.name}`] : [],
    );
}

test("no panel or tree hardcodes a palette hex outside the token modules", () => {
  const offenders: string[] = [];
  const files = [...sourcesUnder("src/webview"), ...sourcesUnder("src/views")];
  assert.ok(files.includes("src/webview/ui/kit.ts"), "the sweep reaches the kit");
  for (const file of files) {
    if (path.dirname(file) === "src/webview" && TOKEN_MODULES.has(path.basename(file))) continue;
    const text = fs.readFileSync(path.join(ROOT, file), "latin1");
    for (const [i, line] of text.split("\n").entries()) {
      // Hex colors only: 3/6/8-digit sequences in CSS/attribute position.
      if (/#[0-9a-fA-F]{3}\b|#[0-9a-fA-F]{6}\b/.test(line) && !/^\s*\/\//.test(line)) {
        offenders.push(`${file}:${i + 1}: ${line.trim()}`);
      }
    }
  }
  assert.deepEqual(
    offenders,
    [],
    "palette values live in brandTokens.ts (or ThemeColor ids in trees); route new colors through the tokens",
  );
});
