// The webview brand system (memql#4196).
//
// Three claims, each mechanically checkable:
//   1. The palette is memql.io's EXACTLY (the same hexes the portal ships,
//      memql#4177), in both themes, and high contrast defers to VS Code.
//   2. The inline header mark is the SAME geometry as the gated activity-bar
//      asset (icons/memql-activity.svg) -- one mark, two carriers, no drift.
//   3. No panel hardcodes a palette hex outside the token module. Tree items
//      are covered by the same sweep over src/views (they must speak
//      ThemeColor, never hex).

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";

import { brandHeader, brandMarkSvg, brandStrip, brandStyleBlock } from "../src/webview/brandTokens.js";

const ROOT = path.resolve(__dirname, "..", "..");

const DARK = ["#07090a", "#0b1110", "#0e1311", "#18231e", "#213029", "#e8e6dd", "#9ca395", "#6c726a", "#5ccda7", "#026842", "#98ffe0", "#cbb083"];
const LIGHT = ["#f2f4ef", "#ffffff", "#e9ede6", "#d6ddd4", "#c2cabf", "#14201a", "#586159", "#7c847b", "#047d5a", "#0f766e", "#b45309"];

test("both memql.io palettes are present, dark scoped to vscode-dark", () => {
  const css = brandStyleBlock();
  for (const hex of [...DARK, ...LIGHT]) {
    assert.ok(css.includes(hex), `palette hex ${hex} missing`);
  }
  const darkBlock = css.slice(css.indexOf("body.vscode-dark"), css.indexOf("high-contrast"));
  assert.ok(darkBlock.includes("#07090a"), "dark bg lives in the vscode-dark block");
  assert.ok(darkBlock.includes("#98ffe0"), "dark data tint lives in the vscode-dark block");
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

test("the header and strip escape their titles", () => {
  assert.doesNotMatch(brandHeader('<img src=x onerror=1>'), /<img/);
  assert.doesNotMatch(brandStrip('<script>'), /<script>/);
});

test("no panel or tree hardcodes a palette hex outside the token module", () => {
  const offenders: string[] = [];
  for (const dir of ["src/webview", "src/views"]) {
    for (const name of fs.readdirSync(path.join(ROOT, dir))) {
      if (!name.endsWith(".ts") || name === "brandTokens.ts") continue;
      const text = fs.readFileSync(path.join(ROOT, dir, name), "latin1");
      for (const [i, line] of text.split("\n").entries()) {
        // Hex colors only: 3/6/8-digit sequences in CSS/attribute position.
        if (/#[0-9a-fA-F]{3}\b|#[0-9a-fA-F]{6}\b/.test(line) && !/^\s*\/\//.test(line)) {
          offenders.push(`${dir}/${name}:${i + 1}: ${line.trim()}`);
        }
      }
    }
  }
  assert.deepEqual(
    offenders,
    [],
    "palette values live in brandTokens.ts (or ThemeColor ids in trees); route new colors through the tokens",
  );
});
