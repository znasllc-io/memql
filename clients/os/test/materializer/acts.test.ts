import { describe, expect, it } from "vitest";

import { actsFor, type DraftState } from "../../src/apps/materializer/acts";
import { compositionFromRow, sourcesOf, type CompositionRow } from "../../src/apps/materializer/rows";
import { compositionRow } from "./harness";

// DESIGN.md rule 12 for the Materializer, as the table `actsFor` computes: the
// acts a composition offers from its state alone, three at most, the primary
// last, and an act that is not legal ABSENT. This file pins the one act that is
// not about the composition at all: the way back to the app that sent the person.

const composition = (over: Record<string, unknown> = {}): CompositionRow =>
  compositionFromRow(compositionRow({ id: "c1", name: "Notes", status: "ready", format: "markdown", ...over }));

const DRAFT: DraftState = { sourceCount: 0, hasContent: true, hasFormat: true, hasOrganization: true, submitting: false };
const ids = (acts: { id: string }[]) => acts.map((act) => act.id);

describe("the way back to the app that sent the person", () => {
  it("is not there unless a sender asked for it", () => {
    expect(ids(actsFor(composition(), DRAFT))).toEqual(["archive", "saveRecipe", "openFile"]);
  });

  it("stands in front of the primary act, as a label rather than a button", () => {
    const acts = actsFor(composition(), DRAFT, "Files");
    const back = acts.find((act) => act.id === "returnToApp");
    expect(back).toMatchObject({ label: "Back to Files", text: true });
    expect(acts[acts.length - 1]?.id).toBe("openFile");
    expect(ids(acts).indexOf("returnToApp")).toBe(ids(acts).indexOf("openFile") - 1);
  });

  it("makes room by giving up Archive, the housekeeping act, and never the primary one", () => {
    expect(ids(actsFor(composition(), DRAFT, "Files"))).toEqual(["saveRecipe", "returnToApp", "openFile"]);
  });

  it("keeps Archive when the recipe is already saved and everything fits", () => {
    expect(ids(actsFor(composition({ recipeId: "r1" }), DRAFT, "Files"))).toEqual(["archive", "returnToApp", "openFile"]);
  });

  it("keeps the deployable's next step last", () => {
    expect(ids(actsFor(composition({ deployableKind: "site" }), DRAFT, "Files"))).toEqual(["openFile", "returnToApp", "openDeployables"]);
  });

  it.each([
    ["still composing", { status: "composing", outputFileId: "" }],
    ["failed", { status: "failed", outputFileId: "", failureReason: "No budget left" }],
    ["stopped", { status: "cancelled", outputFileId: "" }],
    ["archived", { archived: true }],
  ])("is absent from a composition that is %s", (_state, over) => {
    expect(ids(actsFor(composition(over), DRAFT, "Files"))).not.toContain("returnToApp");
  });

  it("is absent from the form, where there is no file yet to leave with", () => {
    expect(ids(actsFor(null, DRAFT, "Files"))).toEqual(["materialize"]);
  });

  it("never takes the bar past three acts", () => {
    for (const over of [{}, { recipeId: "r1" }, { deployableKind: "spa" }, { outputFileId: "" }]) {
      expect(actsFor(composition(over), DRAFT, "Files").length).toBeLessThanOrEqual(3);
    }
  });
});

// SAVE AS RECIPE IS OFFERED ONLY WHERE THE RECIPE COULD RUN. A recipe keeps the
// brief and the sources it can select again -- a query, a Library file -- and
// composeRunRecipe refuses one with neither ("this recipe needs a brief or
// reference sources"). A blank file has neither, and a recipe saved from it
// would be refused the first time it ran: an act whose only outcome is a refusal
// is absent (rule 12).
describe("Save as recipe", () => {
  const row = (over: Record<string, unknown> = {}) => compositionRow({ id: "c1", name: "Notes", status: "ready", format: "markdown", ...over });
  const acts = (over: Record<string, unknown> = {}) => {
    const r = row(over);
    return ids(actsFor(compositionFromRow(r), DRAFT, undefined, sourcesOf(r)));
  };
  const query = { kind: "query", ref: "query openInvoices()", label: "Open invoices", capturedAt: "" };
  const file = { kind: "library_file", ref: "f1", label: "notes.md", capturedAt: "" };
  const picked = { kind: "concept_row", ref: "v1:x:invoice#a", label: "INV-1001", capturedAt: "" };

  it("is absent from a blank file: no brief and no sources", () => {
    expect(acts({ statement: "", sources: [] })).toEqual(["archive", "openFile"]);
  });

  it("is absent where the only sources are rows picked one by one, which a recipe does not keep", () => {
    expect(acts({ statement: "", sources: [picked] })).not.toContain("saveRecipe");
  });

  it("is offered for a brief, a query or a Library file, each enough for a recipe to run", () => {
    expect(acts({ statement: "Draft the Q3 report", sources: [] })).toContain("saveRecipe");
    expect(acts({ statement: "", sources: [query] })).toContain("saveRecipe");
    expect(acts({ statement: "", sources: [picked, file] })).toContain("saveRecipe");
  });
});
