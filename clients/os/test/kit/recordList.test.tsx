import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

const CSS = readFileSync(join(__dirname, "..", "..", "src", "styles", "index.css"), "utf8");

// jsdom has no layout. Guard the shared default here; browser QA checks
// spacing, hover/focus, metadata and narrow rows against the real stylesheet.
describe("record list rows without dividers", () => {
  it("does not add a divider to direct records, semantic items or live wrappers", () => {
    const rules = CSS.replace(/\/\*[\s\S]*?\*\//g, "").split("}");
    const dividers = rules
      .map((rule) => rule.split("{"))
      .filter(([selector = "", body = ""]) =>
        /\.os-record-row|\.os-record-entry|\.os-record-list\s*>\s*li|\.os-livelist-row/.test(selector)
        && /border-(?:top|bottom)\s*:\s*(?!0(?:\D|$))\d/.test(body));
    expect(dividers).toEqual([]);
  });
});
