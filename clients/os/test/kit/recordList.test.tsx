import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

const CSS = readFileSync(join(__dirname, "..", "..", "src", "styles", "index.css"), "utf8");

// Shared record lists separate their rows with spacing, never a line.
describe("record list rows", () => {
  it("rows draw no border between them", () => {
    const rules = CSS.replace(/\/\*[\s\S]*?\*\//g, "").split("}");
    const offenders = rules.filter((rule) => {
      const [selector = "", body = ""] = rule.split("{");
      return /\.os-record-row|\.os-record-list\s*>\s*li|\.os-livelist-row|\.os-record-entry/.test(selector)
        && /border-(top|bottom)\s*:\s*[^;]*\b(solid|var\(--os-line\))/.test(body);
    });
    expect(offenders.map((rule) => rule.trim())).toEqual([]);
  });
});
