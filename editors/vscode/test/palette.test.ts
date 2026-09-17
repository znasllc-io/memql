import test from "node:test";
import * as fs from "node:fs";
import * as path from "node:path";
import assert from "node:assert/strict";

import { DARK, LIGHT, PALETTE_KEYS, type PaletteKey } from "../src/webview/palette.js";

const brand = fs.readFileSync(path.resolve(__dirname, "../../../..", "brand/tokens.css"), "utf8");
const roles: Partial<Record<PaletteKey, string>> = {
  bg: "bg", surface: "surface", raised: "surface-raised", border: "border",
  "border-strong": "border-strong", fg: "fg", muted: "fg-muted", subtle: "fg-subtle",
  accent: "accent", "accent-deep": "accent-deep", "on-accent": "accent-fg",
  danger: "danger", "data-number": "data-number", "data-string": "data-string",
};
const lifted = new Set(["bg", "surface", "raised", "border", "border-strong"]);
for (const [variant, palette, column] of [["light", LIGHT, 1], ["dark", DARK, 2]] as const) {
  test(`${variant}: canonical brand roles stay in sync`, () => {
    for (const [key, role] of Object.entries(roles)) {
      if (variant === "dark" && lifted.has(key)) continue;
      const match = brand.match(new RegExp(`--memql-${role}:\\s*light-dark\\((#[a-f0-9]{6}),\\s*(#[a-f0-9]{6})\\)`));
      assert.ok(match, `${role} exists in brand/tokens.css`);
      assert.equal(palette[key as PaletteKey], match[column], `${variant}.${key}`);
    }
  });
}

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
