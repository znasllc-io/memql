import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

// THE LOADING LOGO KEEPS ITS SIZE WHEN THE DESKTOP SCALE TURNS ON (Jesús,
// Nov 20 2026: on reload, signed in, "it's a certain size, and then it
// resizes again"). The shell zooms <html> by --os-scale the moment it mounts
// (system/scale.ts), which is while the boot curtain is still showing, so a
// plain 86px mark grew to 107px mid-load on a x1.25 screen. Signed out the
// shell never mounts, which is why only a signed-in reload jumped. Every
// length in the loading lockup is divided by --os-scale, so its size on
// screen is the same before and after the zoom.
const css = readFileSync(join(dirname(fileURLToPath(import.meta.url)), "../../src/styles/index.css"), "utf8")
  .replace(/\/\*[\s\S]*?\*\//g, "");

const rule = (selector: string): string => {
  const at = css.indexOf(`${selector} {`);
  expect(at, selector).toBeGreaterThanOrEqual(0);
  return css.slice(at, css.indexOf("}", at));
};

describe("the loading lockup", () => {
  it.each([
    ".os-entry-pending-content",
    ".os-entry-pending-label",
    ".os-entry-pending-mark svg",
    ".os-boot-curtain-content",
    ".os-boot-mark",
    ".os-boot-steps",
  ])("%s divides every px and rem length by --os-scale", (selector) => {
    const bare = rule(selector).match(/(?<![\w.-])\d*\.?\d+(px|rem)\b(?! \/ var\(--os-scale, 1\))/g);
    expect(bare).toBeNull();
  });
});
