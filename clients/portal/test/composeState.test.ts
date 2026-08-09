// The composer's pure halves: the draft reducer and the saved-view round trip.
//
// Kept apart from compose.test.tsx on purpose. What is asserted here are
// INVARIANTS -- "an AI-applied arrangement is the same kind of value as a
// seeded one", "an empty binding list survives storage" -- and an invariant
// asserted through render() and waitFor() is asserted through three layers
// that can each fail for unrelated reasons.

import { describe, expect, it } from "vitest";
import {
  profileConcept,
  proposeArrangement,
  type Arrangement,
  type ConceptLike,
  type RowLike,
} from "@znasllc-io/memql-view-kit";

import {
  composerReducer,
  draftArrangements,
  draftFromSavedView,
  emptyDraft,
  isSavable,
} from "../src/compose/composerState";
import {
  parseSavedView,
  parseSavedViews,
  savedViewArgs,
  type SavedView,
} from "../src/compose/savedViews";
import { readConceptSelection, composeNewPath } from "../src/compose/urls";

const CONCEPT: ConceptLike = { id: "v9:madeup:reading", entity: "reading" };
const ROWS: RowLike[] = [
  { id: "r1", label: "north", zone: "intake", degrees: 41.2, takenAt: "2026-08-01T06:00:00Z" },
  { id: "r2", label: "south", zone: "return", degrees: 39.8, takenAt: "2026-08-01T07:00:00Z" },
];
const PROFILE = profileConcept(CONCEPT, ROWS);

describe("the composer draft", () => {
  it("seeds a section with the deterministic arrangement", () => {
    const draft = composerReducer(emptyDraft([CONCEPT.id]), {
      kind: "seeded",
      conceptId: CONCEPT.id,
      profile: PROFILE,
    });
    expect(draft.arrangements[CONCEPT.id]!).toEqual(proposeArrangement(PROFILE));
    // Seeding is not an edit: a freshly opened composer has nothing to warn
    // about on the way out.
    expect(draft.dirty).toBe(false);
  });

  it("seeding twice does not undo an edit", () => {
    let draft = composerReducer(emptyDraft([CONCEPT.id]), {
      kind: "seeded",
      conceptId: CONCEPT.id,
      profile: PROFILE,
    });
    draft = composerReducer(draft, { kind: "elementRemoved", conceptId: CONCEPT.id, at: 0 });
    const edited = draft.arrangements[CONCEPT.id]!;
    draft = composerReducer(draft, { kind: "seeded", conceptId: CONCEPT.id, profile: PROFILE });
    expect(draft.arrangements[CONCEPT.id]!).toEqual(edited);
  });

  it("an applied proposal and a seeded arrangement are the same kind of value", () => {
    // The load-bearing invariant of the whole AI story: nothing downstream can
    // tell which producer ran, because there is nothing to tell them apart.
    const seeded = composerReducer(emptyDraft([CONCEPT.id]), {
      kind: "seeded",
      conceptId: CONCEPT.id,
      profile: PROFILE,
    });
    const proposed: Arrangement = {
      conceptId: CONCEPT.id,
      elements: [{ element: "table", band: "roll" }],
    };
    const applied = composerReducer(emptyDraft([CONCEPT.id]), {
      kind: "applied",
      conceptId: CONCEPT.id,
      arrangement: proposed,
    });
    expect(Object.keys(seeded.arrangements[CONCEPT.id]!)).toEqual(
      Object.keys(applied.arrangements[CONCEPT.id]!),
    );
    expect(applied.dirty).toBe(true);
  });

  it("adds, removes and moves elements, and refuses nonsense", () => {
    let draft = composerReducer(emptyDraft([CONCEPT.id]), {
      kind: "seeded",
      conceptId: CONCEPT.id,
      profile: PROFILE,
    });
    const before = draft.arrangements[CONCEPT.id]!.elements.length;

    draft = composerReducer(draft, {
      kind: "elementAdded",
      conceptId: CONCEPT.id,
      element: "rowList",
    });
    expect(draft.arrangements[CONCEPT.id]!.elements.length).toBe(before + 1);

    // The same element in the same band twice is one click meaning "yes".
    draft = composerReducer(draft, {
      kind: "elementAdded",
      conceptId: CONCEPT.id,
      element: "rowList",
    });
    expect(draft.arrangements[CONCEPT.id]!.elements.length).toBe(before + 1);

    // An element this build does not carry changes nothing.
    draft = composerReducer(draft, {
      kind: "elementAdded",
      conceptId: CONCEPT.id,
      element: "sankeyDiagram",
    });
    expect(draft.arrangements[CONCEPT.id]!.elements.length).toBe(before + 1);

    const first = draft.arrangements[CONCEPT.id]!.elements[0]!.element;
    draft = composerReducer(draft, { kind: "elementMoved", conceptId: CONCEPT.id, at: 0, by: 1 });
    expect(draft.arrangements[CONCEPT.id]!.elements[1]!.element).toBe(first);

    // Off the end is a no-op, not a crash or a lost element.
    const held = draft.arrangements[CONCEPT.id]!;
    draft = composerReducer(draft, { kind: "elementMoved", conceptId: CONCEPT.id, at: 0, by: -1 });
    expect(draft.arrangements[CONCEPT.id]!).toEqual(held);
  });

  it("binding a slot to an empty list declines it; clearing hands it back", () => {
    let draft = composerReducer(emptyDraft([CONCEPT.id]), {
      kind: "seeded",
      conceptId: CONCEPT.id,
      profile: PROFILE,
    });
    draft = composerReducer(draft, {
      kind: "slotBound",
      conceptId: CONCEPT.id,
      at: 0,
      slot: "metric",
      fields: [],
    });
    expect(draft.arrangements[CONCEPT.id]!.elements[0]!.bindings).toEqual({ metric: [] });

    draft = composerReducer(draft, {
      kind: "slotCleared",
      conceptId: CONCEPT.id,
      at: 0,
      slot: "metric",
    });
    expect(draft.arrangements[CONCEPT.id]!.elements[0]!.bindings).toBeUndefined();
  });

  it("is savable only with a name and at least one element", () => {
    let draft = emptyDraft([CONCEPT.id]);
    expect(isSavable(draft)).toBe(false);
    draft = composerReducer(draft, { kind: "named", name: "Readings" });
    expect(isSavable(draft)).toBe(false);
    draft = composerReducer(draft, { kind: "seeded", conceptId: CONCEPT.id, profile: PROFILE });
    expect(isSavable(draft)).toBe(true);
  });

  it("saves sections in the selection's order, skipping ones that never loaded", () => {
    let draft = emptyDraft(["v9:madeup:a", CONCEPT.id, "v9:madeup:c"]);
    draft = composerReducer(draft, { kind: "seeded", conceptId: CONCEPT.id, profile: PROFILE });
    expect(draftArrangements(draft).map((a) => a.conceptId)).toEqual([CONCEPT.id]);
  });
});

describe("a saved view", () => {
  const STORED = {
    id: "view-1",
    name: "Readings",
    description: "Sensor readings by zone",
    conceptIds: [CONCEPT.id],
    arrangements: [
      {
        conceptId: CONCEPT.id,
        elements: [
          { element: "statTile", band: "reading", bindings: { metric: [] } },
          { element: "table", band: "roll", title: "Every reading" },
        ],
      },
    ],
    origin: "suggested",
    status: "active",
    updatedAt: "2026-08-08T10:00:00Z",
    createdAt: "2026-08-08T09:00:00Z",
  };

  it("round-trips through the wire shape without losing a declined slot", () => {
    const parsed = parseSavedView(STORED)!;
    expect(parsed.arrangements[0]!.elements[0]!.bindings).toEqual({ metric: [] });
    expect(parsed.arrangements[0]!.elements[1]!.title).toBe("Every reading");
    expect(parsed.origin).toBe("suggested");

    const args = savedViewArgs({
      viewId: parsed.id,
      name: parsed.name,
      description: parsed.description,
      conceptIds: parsed.conceptIds,
      arrangements: parsed.arrangements,
      origin: parsed.origin,
    });
    // The declined slot survives serialization. Dropping an empty list would
    // silently switch a measure back on that somebody turned off.
    expect(JSON.stringify(args)).toContain('"metric":[]');
    // ownerUserId is never sent: the field is @serverSet, and there is no
    // argument through which a client could claim somebody else's view.
    expect(Object.keys(args)).not.toContain("ownerUserId");
  });

  it("reads a nested-payload row the same as a flat one", () => {
    const { id, ...payload } = STORED;
    const nested = { id, concept: "v1:portalviews:view", payload };
    expect(parseSavedView(nested)).toEqual(parseSavedView(STORED));
  });

  it("survives a malformed arrangement rather than throwing", () => {
    const broken = { ...STORED, arrangements: "not an array", conceptIds: 7 };
    const parsed = parseSavedView(broken)!;
    expect(parsed.arrangements).toEqual([]);
    expect(parsed.conceptIds).toEqual([]);
  });

  it("drops a row with no id and keeps the rest", () => {
    expect(parseSavedViews([{ name: "orphan" }, STORED]).map((v) => v.id)).toEqual(["view-1"]);
  });

  it("reopens in the composer with its stored arrangement", () => {
    const view = parseSavedView(STORED) as SavedView;
    const draft = draftFromSavedView(view);
    expect(draft.viewId).toBe("view-1");
    expect(draft.name).toBe("Readings");
    expect(draft.dirty).toBe(false);
    expect(draft.arrangements[CONCEPT.id]!.elements).toHaveLength(2);
  });
});

describe("the composer's addresses", () => {
  it("carries a multi-concept selection as repeated parameters", () => {
    const path = composeNewPath(["v9:madeup:a", "v9:madeup:b"]);
    expect(readConceptSelection(path.split("?")[1] ?? "")).toEqual(["v9:madeup:a", "v9:madeup:b"]);
  });

  it("drops duplicates and keeps order", () => {
    expect(readConceptSelection("concept=b&concept=a&concept=b")).toEqual(["b", "a"]);
  });
});
