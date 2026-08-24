// The palette module: one source of hexes, three consumers (memql#4419, D3).
//
// WHY IT WAS EXTRACTED. The hexes used to live inside `brandStyleBlock()`, as
// CSS text. That was fine while CSS was the only thing that wanted them; it
// stopped being fine the moment the extension had to emit VS Code COLOR THEME
// JSON from the same values (memql#4420). A generator cannot read a template
// literal, so either the theme files would carry a second hand-typed copy of
// the palette -- which is the drift this repo's generate-then-gate pairs exist
// to prevent -- or the palette had to become data. It became data.
//
// The three consumers, all of which must agree by construction rather than by
// anyone remembering:
//   1. `brandStyleBlock()` composes its `--memql-*` custom properties from it.
//   2. `buildEditorTheme()` maps it onto VS Code workbench colors.
//   3. `scripts/generate-themes.mjs` writes (2)'s output to themes/*.json.
//
// WHAT THIS FILE PINS, AND WHY THE HEXES ARE REPEATED HERE. The literals below
// are typed out a second time on purpose. Asserting `LIGHT.bg === LIGHT.bg`
// via an import would be a tautology; the claim worth making is that the
// palette is memql.io's EXACT palette (memql#4177, the portal redesign), so
// the expected values have to come from somewhere other than the module under
// test. Changing a brand hex should require editing two files and noticing.

import test from "node:test";
import assert from "node:assert/strict";

import { DARK, LIGHT, PALETTE_KEYS, type PaletteKey } from "../src/webview/palette.js";

// memql.io's palette, transcribed from memql#4177 / brand/. The order matches
// PALETTE_KEYS so a reader can compare the two columns down the page.
const EXPECTED_LIGHT: Record<PaletteKey, string> = {
  bg: "#f2f4ef",
  surface: "#ffffff",
  raised: "#e9ede6",
  border: "#d6ddd4",
  "border-strong": "#c2cabf",
  fg: "#14201a",
  muted: "#586159",
  subtle: "#7c847b",
  accent: "#047d5a",
  "accent-deep": "#026842",
  "on-accent": "#ffffff",
  "on-accent-hover": "#ffffff",
  danger: "#b42318",
  "data-number": "#0f766e",
  "data-string": "#b45309",
};

const EXPECTED_DARK: Record<PaletteKey, string> = {
  bg: "#07090a",
  surface: "#0b1110",
  raised: "#0e1311",
  border: "#18231e",
  "border-strong": "#213029",
  fg: "#e8e6dd",
  muted: "#9ca395",
  subtle: "#6c726a",
  accent: "#5ccda7",
  "accent-deep": "#026842",
  "on-accent": "#052e21",
  "on-accent-hover": "#ffffff",
  danger: "#f97066",
  "data-number": "#98ffe0",
  "data-string": "#cbb083",
};

test("the light palette is memql.io's, value for value", () => {
  assert.deepEqual(LIGHT, EXPECTED_LIGHT);
});

test("the dark palette is memql.io's, value for value", () => {
  assert.deepEqual(DARK, EXPECTED_DARK);
});

test("both palettes carry exactly the same keys, in the same order", () => {
  // A key present in one palette and absent from the other is a token that
  // renders in one theme and inherits whatever came before it in the other --
  // which looks like a rendering bug three files away from its cause. The
  // ORDER matters too: brandStyleBlock() emits the two blocks by iterating
  // these maps, and a reviewer diffing the light block against the dark one
  // needs the lines to correspond.
  assert.deepEqual(Object.keys(LIGHT), Object.keys(DARK));
  assert.deepEqual(Object.keys(LIGHT), [...PALETTE_KEYS]);
});

test("every palette value is a six-digit hex", () => {
  // The generator writes these straight into theme JSON, where VS Code accepts
  // #RGB, #RRGGBB and #RRGGBBAA. Pinning the six-digit form keeps the CSS and
  // the theme JSON byte-identical for the same token, so a value can be
  // grepped across both and found.
  for (const key of PALETTE_KEYS) {
    for (const [name, palette] of [["light", LIGHT], ["dark", DARK]] as const) {
      assert.match(
        palette[key],
        /^#[0-9a-f]{6}$/,
        `${name}.${key} = "${palette[key]}" is not a lower-case six-digit hex`,
      );
    }
  }
});

test("PALETTE_KEYS is not empty and names no key twice", () => {
  // Guards the shape the two assertions above lean on: an empty list would
  // make the six-digit sweep pass without examining anything, and a repeated
  // key would make the deepEqual on Object.keys pass against a map that is
  // missing a token entirely.
  assert.ok(PALETTE_KEYS.length > 0, "the palette has keys");
  assert.equal(new Set(PALETTE_KEYS).size, PALETTE_KEYS.length, "no key is listed twice");
});
