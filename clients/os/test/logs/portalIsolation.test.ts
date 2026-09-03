import { existsSync, readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

// THE PORTAL IS NOT INSTRUMENTED (spec L14): not captured, not a facet. Its
// deprecation is the reason, and nothing in the logs epic names it. This
// walks the portal's source for an import of the capture module and expects
// none -- with the OS's own importers as the REACHABLE POSITIVE, so an empty
// offender list is evidence about the tree rather than about the regex.

const OS_SRC = join(__dirname, "..", "..", "src");
const PORTAL_SRC = join(__dirname, "..", "..", "..", "portal", "src");

const IMPORTS_CAPTURE = /from\s+["'][^"']*logs\/capture["']/;

function walk(dir: string): string[] {
  const out: string[] = [];
  for (const name of readdirSync(dir)) {
    const path = join(dir, name);
    if (statSync(path).isDirectory()) out.push(...walk(path));
    else if (/\.(ts|tsx)$/.test(name)) out.push(path);
  }
  return out;
}

function importers(root: string): string[] {
  return walk(root)
    .filter((path) => IMPORTS_CAPTURE.test(readFileSync(path, "utf8")))
    .map((path) => path.slice(root.length + 1))
    .sort();
}

describe("the portal never imports the capture module", () => {
  it("the OS's own importers are exactly the seams the design names", () => {
    expect(importers(OS_SRC)).toEqual([
      "chrome/CaptureContext.tsx",
      "chrome/Shell.tsx",
      "chrome/WindowErrorBoundary.tsx",
      "live/connection.tsx",
    ]);
  });

  it("the portal's source has no importer, and the walk examined it", () => {
    expect(existsSync(PORTAL_SRC)).toBe(true);
    expect(walk(PORTAL_SRC).length).toBeGreaterThan(0);
    expect(importers(PORTAL_SRC)).toEqual([]);
  });
});
