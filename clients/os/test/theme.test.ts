import { beforeEach, describe, expect, it } from "vitest";

import { applyTheme, readStoredTheme, setTheme } from "../src/app/theme";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const tokens = readFileSync(
  join(dirname(fileURLToPath(import.meta.url)), "../src/styles/tokens.css"),
  "utf8",
);

const TOKEN_PACK = [
  "--os-font",
  "--os-font-mono",
  "--os-radius",
  "--os-duration-fast",
  "--os-duration-med",
  "--os-ease",
  "--os-blur",
  "--os-ground",
  "--os-ink",
  "--os-muted",
  "--os-accent",
  "--os-glass",
  "--os-glass-solid",
  "--os-line",
  "--os-shadow",
];

describe("theme", () => {
  beforeEach(() => {
    document.documentElement.removeAttribute("data-theme");
    localStorage.clear();
  });

  it("pins light and dark on the document root", () => {
    setTheme("light");
    expect(document.documentElement.getAttribute("data-theme")).toBe("light");
    setTheme("dark");
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
  });

  it("removes the attribute for system so prefers-color-scheme can win", () => {
    setTheme("dark");
    setTheme("system");
    expect(document.documentElement.hasAttribute("data-theme")).toBe(false);
    expect(readStoredTheme()).toBe("system");
  });

  it("can apply a stored choice without writing again", () => {
    localStorage.setItem("memql-os-theme", "dark");
    applyTheme(readStoredTheme());
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
  });

  it("honors prefers-reduced-motion by zeroing motion tokens", () => {
    expect(tokens).toMatch(/prefers-reduced-motion:\s*reduce/);
    expect(tokens).toMatch(/--os-duration-fast:\s*0ms/);
    expect(tokens).toMatch(/--os-duration-med:\s*0ms/);
  });

  it("hosts the same --os-* pack on window, widget and sheet roots", () => {
    // Spec G: a later per-window theme mix must be a CSS-only change, so
    // every one of these roots re-inherits the full pack.
    expect(tokens).toMatch(/\[data-os-window\]/);
    expect(tokens).toMatch(/\[data-os-widget\]/);
    expect(tokens).toMatch(/\[data-os-sheet\]/);
    for (const name of TOKEN_PACK) {
      expect(tokens).toContain(name);
    }
  });
});
