// The theme override attribute. The palette itself is CSS and is not
// asserted here; what IS asserted is the three-state resolution the CSS
// depends on -- specifically that "system" REMOVES the attribute rather than
// writing a value, because the token layer falls through to
// prefers-color-scheme only when the attribute is absent.

import { beforeEach, describe, expect, it } from "vitest";

import { applyStoredTheme, applyTheme, readStoredTheme, setTheme } from "../src/app/theme";

describe("theme", () => {
  beforeEach(() => {
    localStorage.clear();
    document.documentElement.removeAttribute("data-theme");
  });

  it("writes an explicit choice as data-theme", () => {
    applyTheme("dark");
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
    applyTheme("light");
    expect(document.documentElement.getAttribute("data-theme")).toBe("light");
  });

  it("removes the attribute for 'system' so prefers-color-scheme applies", () => {
    applyTheme("dark");
    applyTheme("system");
    expect(document.documentElement.hasAttribute("data-theme")).toBe(false);
  });

  it("defaults to 'system' when nothing is stored", () => {
    expect(readStoredTheme()).toBe("system");
  });

  it("defaults to 'system' when the stored value is not a theme", () => {
    localStorage.setItem("memql-portal-theme", "chartreuse");
    expect(readStoredTheme()).toBe("system");
  });

  it("round-trips a stored choice and re-applies it on load", () => {
    setTheme("dark");
    document.documentElement.removeAttribute("data-theme");
    expect(applyStoredTheme()).toBe("dark");
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
  });
});
