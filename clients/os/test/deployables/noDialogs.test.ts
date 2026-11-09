import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative, sep } from "node:path";
import { describe, expect, it } from "vitest";

// NO POP-UP IN DEPLOYABLES (the owner, 2026-11-08; design record
// docs/superpowers/specs/2026-11-08-site-and-email-statistics-design.md,
// section 10). An option that showed a pop-up is a page in the app that is
// open, or a question asked where its act stands. Visit and a version's
// details became pages and Remove source asks in place, which left the
// app's modal wrapper with no caller, so it went too. This fails the build
// the moment anything under apps/deployables brings one back: the kit's
// modal imported, the old wrapper named, or a dialog drawn by hand.
//
// REACHABLE POSITIVES, as in test/onePathCreate.test.ts: an empty offender
// list says something about the tree only if the walk reaches it and the
// pattern matches real code, so the walk must find the pages it guards and
// the pattern must catch each way a dialog could come back.

const ROOT = join(__dirname, "..", "..", "src", "apps", "deployables");
const DIALOG = /kit\/Dialog|DetailDialog|<dialog|role="dialog"/;

/** Every module under apps/deployables, as a path relative to it. */
function modules(dir: string): string[] {
  return readdirSync(dir).flatMap((name) => {
    const path = join(dir, name);
    if (statSync(path).isDirectory()) return modules(path);
    return /\.tsx?$/.test(name) ? [relative(ROOT, path).split(sep).join("/")] : [];
  });
}

describe("Deployables opens no dialog", () => {
  it("nothing under apps/deployables imports the kit's modal, names the old wrapper, or draws a dialog", () => {
    const files = modules(ROOT);
    // The walk reaches the pages that used to open one.
    for (const page of ["page/DeployablePage.tsx", "page/VisitPage.tsx", "page/VersionPage.tsx", "page/Versions.tsx", "page/VisitSiteButton.tsx", "sources/RemoveSource.tsx"]) {
      expect(files).toContain(page);
    }
    expect(files).not.toContain("page/DetailDialog.tsx");
    expect(files.filter((file) => DIALOG.test(readFileSync(join(ROOT, file), "utf8")))).toEqual([]);
  });

  it("catches each way a dialog could come back", () => {
    for (const planted of [
      'import { Dialog } from "../../../kit/Dialog";',
      'import { DetailDialog } from "./DetailDialog";',
      "<dialog open>",
      '<section role="dialog" aria-label="Visit website">',
    ]) {
      expect(DIALOG.test(planted)).toBe(true);
    }
    // The word in a sentence is not a dialog: a comment saying why there is none stays legal.
    expect(DIALOG.test("Visit used to ask in a dialog over the page")).toBe(false);
  });

  it("keeps no styles for the modal that is gone", () => {
    expect(readFileSync(join(ROOT, "composition.css"), "utf8")).not.toMatch(/deployable-dialog/);
  });
});
