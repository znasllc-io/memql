// Composer v2's pure halves (epic memql#4661, task memql#4670).
//
// The reducer transitions the two-pane composer added, plus the round trip
// that decides whether a layout and a role survive being saved. Kept apart
// from a rendered test for the reason composeState.test.ts states: these are
// INVARIANTS, and an invariant asserted through render() and waitFor() is
// asserted through three layers that can each fail for unrelated reasons.

import { describe, expect, it } from "vitest";
import {
  arrangementLayout,
  entryRole,
  type Arrangement,
} from "@znasllc-io/memql-view-kit";

import {
  composerReducer,
  draftFromSuggestion,
  emptyDraft,
} from "../src/compose/composerState";
import { parseSavedView, savedViewArgs } from "../src/compose/savedViews";

const CONCEPT = "v9:madeup:sensorReading";

function draftWith(elements: Arrangement["elements"], layout?: Arrangement["layout"]) {
  const base = emptyDraft([CONCEPT]);
  return {
    ...base,
    arrangements: {
      [CONCEPT]: {
        conceptId: CONCEPT,
        ...(layout === undefined ? {} : { layout }),
        elements,
      },
    },
  };
}

const THREE: Arrangement["elements"] = [
  { element: "statTile", band: "reading" },
  { element: "chart.proportion", band: "shape" },
  { element: "table", band: "roll" },
];

describe("reordering", () => {
  it("moves an entry to an absolute position, which is what a DROP produces", () => {
    const next = composerReducer(draftWith(THREE), {
      kind: "elementReordered",
      conceptId: CONCEPT,
      from: 0,
      to: 2,
    });
    expect(next.arrangements[CONCEPT]!.elements.map((e) => e.element)).toEqual([
      "chart.proportion",
      "table",
      "statTile",
    ]);
    expect(next.dirty).toBe(true);
  });

  it("keeps the one-place move the KEYBOARD path needs", () => {
    // The up/down BUTTONS went when the composer became two panes; what they
    // dispatched did not. A drag is not operable from a keyboard, so removing
    // this transition with the buttons would have removed reordering from
    // everybody not using a mouse.
    const next = composerReducer(draftWith(THREE), {
      kind: "elementMoved",
      conceptId: CONCEPT,
      at: 2,
      by: -1,
    });
    expect(next.arrangements[CONCEPT]!.elements.map((e) => e.element)).toEqual([
      "statTile",
      "table",
      "chart.proportion",
    ]);
  });

  it("refuses a drop that would go off either end, and changes nothing", () => {
    const before = draftWith(THREE);
    for (const [from, to] of [
      [0, -1],
      [0, 3],
      [-1, 0],
      [5, 0],
      [1, 1],
    ]) {
      const next = composerReducer(before, {
        kind: "elementReordered",
        conceptId: CONCEPT,
        from: from!,
        to: to!,
      });
      expect(next).toBe(before);
    }
  });
});

describe("the layout picker", () => {
  it("writes a chosen layout", () => {
    const next = composerReducer(draftWith(THREE), {
      kind: "layoutChosen",
      conceptId: CONCEPT,
      layout: "dashboard",
    });
    expect(arrangementLayout(next.arrangements[CONCEPT]!)).toBe("dashboard");
  });

  it("REMOVES the key when stack is chosen, rather than writing it", () => {
    // Absent means stack, and every row stored before layouts existed is
    // absent. Writing "stack" would make "I tried a dashboard and went back"
    // produce a row that is not equal to the one somebody started with -- and
    // would erode the absent-means-stack rule one save at a time.
    const dashboard = draftWith(THREE, "dashboard");
    const next = composerReducer(dashboard, {
      kind: "layoutChosen",
      conceptId: CONCEPT,
      layout: "stack",
    });
    expect(next.arrangements[CONCEPT]!.layout).toBeUndefined();
    expect(arrangementLayout(next.arrangements[CONCEPT]!)).toBe("stack");
  });
});

describe("per-entry editing", () => {
  it("sets a role and removes it again when standard is chosen", () => {
    // Same rule as the layout, one level down: standard is what absence means.
    const hero = composerReducer(draftWith(THREE), {
      kind: "roleChosen",
      conceptId: CONCEPT,
      at: 1,
      role: "hero",
    });
    expect(hero.arrangements[CONCEPT]!.elements[1]!.role).toBe("hero");

    const back = composerReducer(hero, {
      kind: "roleChosen",
      conceptId: CONCEPT,
      at: 1,
      role: "standard",
    });
    expect(back.arrangements[CONCEPT]!.elements[1]!.role).toBeUndefined();
    expect(entryRole(back.arrangements[CONCEPT]!.elements[1]!)).toBe("standard");
  });

  it("clears a caption to the element's OWN title rather than to an empty one", () => {
    const titled = composerReducer(draftWith(THREE), {
      kind: "titled",
      conceptId: CONCEPT,
      at: 2,
      title: "  Every reading  ",
    });
    expect(titled.arrangements[CONCEPT]!.elements[2]!.title).toBe("Every reading");

    const cleared = composerReducer(titled, {
      kind: "titled",
      conceptId: CONCEPT,
      at: 2,
      title: "   ",
    });
    // Undefined, not "". A caption of the empty string is a caption; absent is
    // "use the element's own title", which is what clearing a field means.
    expect(cleared.arrangements[CONCEPT]!.elements[2]!.title).toBeUndefined();
  });
});

describe("a described draft", () => {
  it("is the same KIND of value a reopened view is", () => {
    // The property that keeps the AI optional rather than structural: nothing
    // downstream can tell a described draft from a reopened one, because there
    // is nothing to tell apart.
    const draft = draftFromSuggestion({
      name: "Failing agents",
      conceptIds: [CONCEPT],
      arrangements: [{ conceptId: CONCEPT, layout: "dashboard", elements: THREE }],
    });
    expect(draft.name).toBe("Failing agents");
    expect(draft.conceptIds).toEqual([CONCEPT]);
    expect(draft.arrangements[CONCEPT]!.elements).toEqual(THREE);
    expect(draft.origin).toBe("suggested");
    expect(draft.viewId).toBe("");
  });

  it("is NOT dirty, so closing it warns about nothing", () => {
    // Nothing has been changed by hand yet. Marking it dirty would make "close
    // without saving" warn about work nobody did.
    const draft = draftFromSuggestion({
      name: "x",
      conceptIds: [CONCEPT],
      arrangements: [{ conceptId: CONCEPT, elements: THREE }],
    });
    expect(draft.dirty).toBe(false);
  });
});

describe("the round trip", () => {
  it("carries layout, role and options through a save and back", () => {
    // A field the composer writes and the serializer drops is a setting that
    // works until the view is reopened -- the worst shape a bug can take,
    // because it looks like it works.
    const args = savedViewArgs({
      viewId: "view-1",
      name: "Readings",
      description: "",
      conceptIds: [CONCEPT],
      origin: "manual",
      arrangements: [
        {
          conceptId: CONCEPT,
          layout: "focus",
          elements: [
            { element: "chart.line", band: "shape", role: "hero", title: "Trend" },
            {
              element: "table",
              band: "roll",
              options: { sortField: "takenAt", sortDir: "desc" },
              bindings: { column: ["label", "degrees"] },
            },
          ],
        },
      ],
    });

    const back = parseSavedView({
      id: "view-1",
      concept: "v1:portalviews:view",
      createdAt: "2026-08-26T00:00:00Z",
      payload: {
        name: "Readings",
        conceptIds: [CONCEPT],
        arrangements: args.arrangements,
        origin: "manual",
        status: "active",
      },
    } as never)!;

    const section = back.arrangements[0]!;
    expect(section.layout).toBe("focus");
    expect(section.elements[0]!.role).toBe("hero");
    expect(section.elements[0]!.title).toBe("Trend");
    expect(section.elements[1]!.options).toEqual({ sortField: "takenAt", sortDir: "desc" });
    expect(section.elements[1]!.bindings).toEqual({ column: ["label", "degrees"] });
  });

  it("omits the defaults, so absent keeps meaning what it always meant", () => {
    const args = savedViewArgs({
      viewId: "view-2",
      name: "Plain",
      description: "",
      conceptIds: [CONCEPT],
      origin: "manual",
      arrangements: [
        {
          conceptId: CONCEPT,
          layout: "stack",
          elements: [{ element: "table", band: "roll", role: "standard" }],
        },
      ],
    });
    const written = args.arrangements[0] as Record<string, unknown>;
    expect(written["layout"]).toBeUndefined();
    const entry = (written["elements"] as Record<string, unknown>[])[0]!;
    expect(entry["role"]).toBeUndefined();
  });
});
