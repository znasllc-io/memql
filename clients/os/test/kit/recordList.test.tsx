import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

const SRC = join(__dirname, "..", "..", "src");
const CSS = readFileSync(join(SRC, "styles", "index.css"), "utf8");

// DESIGN.md rule 1: a surface moves off horizontal dividers by setting
// data-os-dividers="none" on its root; its record rows then stand apart by
// spacing alone. The default keeps dividers between items until the surfaces
// have moved.
describe("record list rows without dividers", () => {
  it("a surface without dividers draws no border between its rows", () => {
    const rules = CSS.replace(/\/\*[\s\S]*?\*\//g, "").split("}");
    const scoped = rules
      .map((rule) => rule.split("{"))
      .filter(([selector = ""]) => selector.includes('[data-os-dividers="none"]'));
    const cleared = scoped.filter(([selector = "", body = ""]) =>
      /\.os-record-row|\.os-record-list\s*>\s*li|\.os-livelist-row/.test(selector)
      && /border-top\s*:\s*0\b/.test(body));
    expect(cleared.length).toBeGreaterThan(0);
    const selectors = cleared.map(([selector = ""]) => selector).join(",");
    expect(selectors).toMatch(/\.os-record-row/);
    expect(selectors).toMatch(/\.os-record-list\s*>\s*li/);
    expect(selectors).toMatch(/\.os-livelist-row/);
  });

  // The Email surfaces this work changed have moved (DESIGN.md names them).
  it.each([
    ["apps/email/templates/TemplatesSection.tsx"],
    ["apps/email/templates/TemplatePage.tsx"],
    ["apps/email/settings/SettingsSection.tsx"],
    ["apps/email/settings/brand/BrandPage.tsx"],
  ])("%s draws no dividers", (file) => {
    expect(readFileSync(join(SRC, file), "utf8")).toContain('data-os-dividers="none"');
  });
});
