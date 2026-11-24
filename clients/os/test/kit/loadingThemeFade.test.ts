import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

// THE LOADING SCREEN FADES FROM BLACK INTO THE THEME (Jesús, Nov 20 2026:
// "make it so that the screen starts in black and then fades into the
// color"). A page starts on the built-in dark ground; the person's theme
// arrives with their desktop about 200ms later and the boot curtain's ground
// switched at once. The curtain now eases its background into the new ground.
// Only the curtain fades: the theme marketplace's live preview writes the same
// attribute and must stay instant. Reduced motion switches at once.
const css = readFileSync(join(dirname(fileURLToPath(import.meta.url)), "../../src/styles/index.css"), "utf8")
  .replace(/\/\*[\s\S]*?\*\//g, "");

const block = (start: string): string => {
  const at = css.indexOf(start);
  expect(at, start).toBeGreaterThanOrEqual(0);
  return css.slice(at, css.indexOf("}", at));
};

describe("the loading screen's ground", () => {
  it("fades its background into the theme's ground", () => {
    expect(block(".os-boot-curtain {")).toMatch(/transition:\s*background-color\s+\d+ms/);
  });

  it("does not fade the page itself, so a theme preview stays instant", () => {
    expect(block("body {")).not.toMatch(/transition/);
  });

  it("switches at once under reduced motion", () => {
    const reduced = css.slice(css.indexOf("@media (prefers-reduced-motion: reduce) {\n  .os-boot-curtain, .os-boot-step"));
    expect(reduced.slice(0, 400)).toMatch(/\.os-boot-curtain,\s*\.os-boot-curtain \*[^{]*\{\s*transition:\s*none/);
  });
});
