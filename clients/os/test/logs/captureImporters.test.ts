import { readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

// The capture module has exactly four importers, and they are the seams the
// design names (spec L14).
//
// THIS FILE WAS portalIsolation.test.ts, and its other half is gone with its
// subject. It used to walk the PORTAL's source for an import of this module
// and expect none -- the portal was deliberately not instrumented, because it
// was deprecated -- with the OS's own importers standing as the reachable
// positive, so that an empty offender list was evidence about the tree rather
// than about the regex.
//
// Epic memql#4984 deleted the portal, and the second case did exactly what it
// was built to do: it asserted `existsSync(PORTAL_SRC)` before trusting its own
// empty result, so it FAILED rather than passing vacuously over a directory
// that was no longer there. That is the whole reason it was written that way,
// and it is worth recording that the design paid out.
//
// What survives is the positive: the four seams, pinned exactly. A fifth
// importer is a decision about what the shell records, and it should be one
// somebody makes rather than one that arrives.

const OS_SRC = join(__dirname, "..", "..", "src");

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

describe("the capture module's importers", () => {
  it("are exactly the seams the design names", () => {
    expect(importers(OS_SRC)).toEqual([
      "chrome/CaptureContext.tsx",
      "chrome/Shell.tsx",
      "chrome/WindowErrorBoundary.tsx",
      "live/connection.tsx",
    ]);
  });

  it("were found by a walk that examined the tree", () => {
    // The anti-vacuous floor the deleted portal case used to provide from the
    // other direction: an empty or broken walk would make the assertion above
    // pass having read nothing.
    expect(walk(OS_SRC).length).toBeGreaterThan(100);
  });
});
