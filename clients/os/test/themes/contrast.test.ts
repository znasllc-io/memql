import { describe, expect, it } from "vitest";

import { BUILT_IN_PACKS } from "../../src/themes/builtins";
import type { OsThemeTokens } from "../../src/themes/pack";

// PER-PACK LIGHT AND DARK VERIFICATION, as a number.
//
// The epic asks for it, and the marketplace card SHOWS it -- two miniature
// desktops side by side. But a picture is verified by looking, and nobody
// looks at the pack they did not add. These are the pairings the whole shell
// rests on, checked for both modes of every pack that ships:
//
//   ink on plate        every word in a window
//   ink on ground       the desk, the dock, the wallpaper
//   muted on plate      captions, which are where the explanations live
//   accent-fg on accent the primary button's own label
//
// jsdom has no CSS engine and no colour maths, so this is arithmetic on the
// pack's declared values rather than anything measured in a browser -- which
// is also why it is worth having: it is the one check on a palette that a
// screenshot review cannot do by eye.

/** WCAG 2.1 relative luminance. */
function luminance(hex: string): number {
  const m = /^#([0-9a-f]{6})$/i.exec(hex.trim());
  if (!m) throw new Error(`not a hex colour: ${hex}`);
  const n = parseInt(m[1]!, 16);
  const channel = (v: number) => {
    const c = v / 255;
    return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
  };
  return (
    0.2126 * channel((n >> 16) & 255) +
    0.7152 * channel((n >> 8) & 255) +
    0.0722 * channel(n & 255)
  );
}

function contrast(a: string, b: string): number {
  const la = luminance(a);
  const lb = luminance(b);
  return (Math.max(la, lb) + 0.05) / (Math.min(la, lb) + 0.05);
}

/** WCAG AA for body text. Captions here are 12px, so this is the right bar. */
const AA = 4.5;

const PAIRS: Array<[keyof OsThemeTokens, keyof OsThemeTokens, string]> = [
  ["ink", "plate", "window text"],
  ["ink", "ground", "desk text"],
  ["muted", "plate", "captions"],
  ["muted", "ground", "captions on the desk"],
  ["accent-fg", "accent", "the primary button's label"],
];

describe("every built-in pack is legible in both modes", () => {
  for (const pack of BUILT_IN_PACKS) {
    for (const mode of ["dark", "light"] as const) {
      it(`${pack.name}, ${mode}`, () => {
        const tokens = pack.tokens[mode];
        for (const [fg, bg, what] of PAIRS) {
          const ratio = contrast(tokens[fg], tokens[bg]);
          expect(
            ratio,
            `${pack.id} ${mode}: ${what} (--os-${fg} on --os-${bg}) is ${ratio.toFixed(2)}:1, under ${AA}:1`,
          ).toBeGreaterThanOrEqual(AA);
        }
      });
    }
  }

  it("checks something -- the ratios are real and they differ", () => {
    // The reachable positive. Every assertion above passing would look
    // identical if `contrast` returned a constant, and a palette gate that
    // cannot fail is a palette gate that is not running.
    expect(contrast("#000000", "#ffffff")).toBeCloseTo(21, 1);
    expect(contrast("#777777", "#808080")).toBeLessThan(AA);
  });
});
