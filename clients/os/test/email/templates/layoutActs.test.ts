import { describe, expect, it } from "vitest";

import { toKitActs } from "../../../src/apps/email/acts";
import { layoutFromRow, layoutName, layoutsInView, type LayoutRow } from "../../../src/apps/email/rows";
import { layoutActs } from "../../../src/apps/email/templates/layoutActs";
import { BUILT_IN_LAYOUTS, layoutRow } from "../harness";

// WHAT A LAYOUT'S BAR OFFERS (DESIGN.md rule 12), as a pure table, and the pure
// reading of the organizations' layout listings beneath the Layouts view.
//
//   built-in             nothing: it ships with the renderer and has no lifecycle
//   organization active  Archive (text)
//   archived             Restore (primary)

const dress = (specs: ReturnType<typeof layoutActs>) => specs.map((spec) => [spec.label, spec.tone ?? (spec.text ? "text" : "quiet")]);
const builtIn = (): LayoutRow => layoutFromRow(BUILT_IN_LAYOUTS[2]!, "self");
const own = (over: Record<string, unknown> = {}): LayoutRow => layoutFromRow(layoutRow({ layoutId: "l1", ...over }), "acme");

describe("layoutActs", () => {
  it("offers a built-in nothing", () => {
    expect(layoutActs(builtIn())).toEqual([]);
  });

  it("offers an organization's active layout Archive, quietly", () => {
    expect(dress(layoutActs(own()))).toEqual([["Archive", "text"]]);
  });

  it("offers an archived one Restore, as the act this state is for", () => {
    expect(dress(layoutActs(own({ status: "archived" })))).toEqual([["Restore", "primary"]]);
  });

  it("never draws an act disabled", () => {
    for (const layout of [builtIn(), own(), own({ status: "archived" })]) {
      expect(toKitActs(layoutActs(layout), () => {}).every((act) => act.busy === false)).toBe(true);
    }
  });
});

describe("reading the layout listings", () => {
  it("projects a listing row with the organization whose reading returned it", () => {
    expect(own({ sourceArtifactId: "art-1" })).toEqual({
      id: "org:l1",
      kind: "organization",
      name: "Acme letter",
      description: "Our letter, with the logo and the footer.",
      document: { subject: "From Acme", blocks: [] },
      documentOmitted: false,
      status: "active",
      accountId: "acme",
      layoutId: "l1",
      revision: "2026-10-03T09:00:00Z",
      sourceArtifactId: "art-1",
    });
    const article = builtIn();
    expect(article.kind).toBe("builtin");
    expect(article.layoutId).toBeUndefined();
    expect(article.revision).toBeUndefined();
    // A listing past its size leaves a document out and says so.
    expect(own({ document: undefined, documentOmitted: true })).toMatchObject({ document: {}, documentOmitted: true });
  });

  it("lists the built-ins once, then each organization's own, and archived ones only when asked", () => {
    const readings = new Map<string, LayoutRow[]>([
      ["self", [...BUILT_IN_LAYOUTS.map((row) => layoutFromRow(row, "self")), layoutFromRow(layoutRow({ layoutId: "s1", name: "Ours" }), "self")]],
      ["acme", [...BUILT_IN_LAYOUTS.map((row) => layoutFromRow(row, "acme")), layoutFromRow(layoutRow({ layoutId: "a1", name: "Theirs", status: "archived" }), "acme")]],
    ]);
    expect(layoutsInView(readings, false).map((layout) => layout.name)).toEqual(["Confirmation", "Letter", "Article", "Spotlight", "Ours"]);
    expect(layoutsInView(readings, true).map((layout) => layout.name)).toEqual(["Confirmation", "Letter", "Article", "Spotlight", "Ours", "Theirs"]);
    // A built-in, once deduplicated, belongs to no one organization.
    expect(layoutsInView(readings, false).filter((layout) => layout.kind === "builtin").every((layout) => layout.accountId === "")).toBe(true);
  });

  it("keeps a built-in from a reading that carried its document", () => {
    const omitted = { ...BUILT_IN_LAYOUTS[0]!, document: undefined, documentOmitted: true };
    const readings = new Map<string, LayoutRow[]>([
      ["self", [layoutFromRow(omitted, "self")]],
      ["acme", [layoutFromRow(BUILT_IN_LAYOUTS[0]!, "acme")]],
    ]);
    const [confirmation] = layoutsInView(readings, false);
    expect(confirmation?.documentOmitted).toBe(false);
    expect(confirmation?.document).toEqual(BUILT_IN_LAYOUTS[0]!.document);
  });

  it("names a layout by the id a template carries, and knows nothing of one it was not told", () => {
    const readings = new Map<string, LayoutRow[]>([["acme", [builtIn(), own({ name: "Acme letter" })]]]);
    expect(layoutName(readings, "builtin:article")).toBe("Article");
    expect(layoutName(readings, "org:l1")).toBe("Acme letter");
    expect(layoutName(readings, "org:gone")).toBe("");
    expect(layoutName(readings, "")).toBe("");
  });
});
