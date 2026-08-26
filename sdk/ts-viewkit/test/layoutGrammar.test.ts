// The layout and role dimensions, and schema-first profiling (epic memql#4661,
// tasks memql#4663 / memql#4662).
//
// TWO PROPERTIES CARRY THIS FILE.
//
// The first is BACKWARD COMPATIBILITY, and it is not a courtesy. Every stored
// `v1:portalviews:view` row predates layouts, so every one of them names no
// layout and no roles. If "absent" ever came to mean anything other than
// "stack, standard", the release that changed it would silently re-lay-out
// every view every person has -- with no migration, no error, and nothing in
// the row to say what it used to look like.
//
// The second is REPAIR-NOT-TRUST, extended. sanitizeArrangement was already
// the one gate between an untrusted arrangement (a model's reply, a row from
// another release) and a render. Layouts add failure modes that are not
// element failures -- a focus with nothing that can be a hero, a split with no
// detail pane -- and each of them has to end in a rendered page rather than in
// an exception or a blank.

import test from "node:test";
import assert from "node:assert/strict";

import {
  arrangementLayout,
  arrangementProblems,
  entryRole,
  proposeArrangement,
  readArrangement,
  sanitizeArrangement,
  type Arrangement,
} from "../src/arrangement.js";
import { profileConcept } from "../src/fitness.js";
import type { ConceptLike, RowLike } from "../src/types.js";

const SENSOR: ConceptLike = { id: "v9:madeup:sensorReading", entity: "sensorReading" };

const SENSOR_ROWS: readonly RowLike[] = [
  { id: "r1", label: "north inlet", zone: "intake", degrees: 41.2, takenAt: "2026-08-01T06:00:00Z", faulty: false },
  { id: "r2", label: "south inlet", zone: "intake", degrees: 39.8, takenAt: "2026-08-01T07:00:00Z", faulty: false },
  { id: "r3", label: "return line", zone: "return", degrees: 55.4, takenAt: "2026-08-01T08:00:00Z", faulty: true },
  { id: "r4", label: "bypass", zone: "bypass", degrees: 48.0, takenAt: "2026-08-01T09:00:00Z", faulty: false },
];

const profile = () => profileConcept(SENSOR, SENSOR_ROWS);

// ---------------------------------------------------------------------------
// Absent means stack, absent means standard
// ---------------------------------------------------------------------------

test("an arrangement stored before layouts existed renders exactly as it did", () => {
  const preEpic: Arrangement = {
    conceptId: SENSOR.id,
    elements: [
      { element: "statTile", band: "reading" },
      { element: "chart.proportion", band: "shape" },
      { element: "table", band: "roll" },
    ],
  };

  const clean = sanitizeArrangement(preEpic, profile());

  // Byte-identical on the dimensions that existed before: same entries, same
  // order, same bands, and NO layout key introduced. A sanitize that stamped
  // `layout: "stack"` onto the value would be a different row the next time
  // the composer saved it.
  assert.deepEqual(clean.elements, preEpic.elements);
  assert.equal(clean.layout, undefined);
  assert.equal(arrangementLayout(clean), "stack");
  for (const entry of clean.elements) {
    assert.equal(entryRole(entry), "standard");
  }
  assert.deepEqual(arrangementProblems(preEpic, profile()), []);
});

test("the deterministic proposal still names no layout", () => {
  // Inferring one from the band mix would make the one predictable answer in
  // the module a design opinion, and would re-lay-out a view the day the
  // heuristic changed.
  assert.equal(proposeArrangement(profile()).layout, undefined);
});

// ---------------------------------------------------------------------------
// The repairs, one round trip each
// ---------------------------------------------------------------------------

test("an unknown layout falls back to stack and says so", () => {
  const stored = {
    conceptId: SENSOR.id,
    layout: "carousel",
    elements: [{ element: "table", band: "roll" }],
  } as unknown as Arrangement;

  const clean = sanitizeArrangement(stored, profile());
  assert.equal(arrangementLayout(clean), "stack");
  assert.equal(clean.layout, undefined);
  // The entries are untouched: an unknown layout is not a reason to drop
  // somebody's elements.
  assert.deepEqual(clean.elements, stored.elements);

  const faults = arrangementProblems(stored, profile()).map((p) => p.fault);
  assert.ok(faults.includes("unknown-layout"), "the repair has to be reported, not just applied");
});

test("a focus with no hero promotes the best-fitting candidate rather than degrading", () => {
  const stored: Arrangement = {
    conceptId: SENSOR.id,
    layout: "focus",
    elements: [
      { element: "chart.line", band: "shape" },
      { element: "table", band: "roll" },
    ],
  };

  const clean = sanitizeArrangement(stored, profile());
  // The layout somebody asked for SURVIVES: it was one annotation short of
  // working, and dropping to stack would discard a deliberate choice.
  assert.equal(arrangementLayout(clean), "focus");
  const heroes = clean.elements.filter((e) => entryRole(e) === "hero");
  assert.equal(heroes.length, 1, "exactly one entry is promoted");
  // The promotion uses the library's own ranking, so it agrees with what the
  // deterministic proposal would have chosen and is not a second opinion.
  assert.equal(heroes[0]?.element, "chart.line");
  // And the STORED row is not rewritten.
  assert.equal(stored.elements[0]?.role, undefined);
});

test("a focus with nothing that can carry it falls back to stack", () => {
  // `detail` is the one element in the library that declares it cannot be a
  // hero: it renders whatever the page's hero is pointing AT, so promoting it
  // inverts the layout.
  const stored: Arrangement = {
    conceptId: SENSOR.id,
    layout: "focus",
    elements: [{ element: "detail", band: "roll" }],
  };

  const clean = sanitizeArrangement(stored, profile());
  assert.equal(arrangementLayout(clean), "stack");
  assert.deepEqual(clean.elements, stored.elements);

  const problems = arrangementProblems(stored, profile());
  assert.ok(problems.some((p) => p.fault === "layout-unsatisfiable"));
  // Whole-arrangement problems are reported at -1, not blamed on an entry
  // that did not cause them.
  assert.equal(problems.find((p) => p.fault === "layout-unsatisfiable")?.at, -1);
});

test("a split with no detail pane falls back to stack", () => {
  const stored: Arrangement = {
    conceptId: SENSOR.id,
    layout: "split",
    elements: [
      { element: "statTile", band: "reading" },
      { element: "table", band: "roll" },
    ],
  };

  const clean = sanitizeArrangement(stored, profile());
  assert.equal(arrangementLayout(clean), "stack");
  assert.ok(
    arrangementProblems(stored, profile()).some((p) => p.fault === "layout-unsatisfiable"),
  );
});

test("a split with a list and a detail pane is honoured", () => {
  const stored: Arrangement = {
    conceptId: SENSOR.id,
    layout: "split",
    elements: [
      { element: "table", band: "roll" },
      { element: "detail", band: "roll" },
    ],
  };
  assert.equal(arrangementLayout(sanitizeArrangement(stored, profile())), "split");
});

test("a role the element cannot express is ignored, and the element survives", () => {
  const stored: Arrangement = {
    conceptId: SENSOR.id,
    layout: "split",
    elements: [
      { element: "table", band: "roll" },
      { element: "detail", band: "roll", role: "hero" },
    ],
  };

  const clean = sanitizeArrangement(stored, profile());
  // IGNORED, not removed. Somebody chose that element; only its emphasis was
  // not honourable.
  assert.equal(clean.elements.length, 2);
  const detail = clean.elements.find((e) => e.element === "detail");
  assert.equal(detail?.role, undefined);
  assert.equal(entryRole(detail!), "standard");

  assert.ok(
    arrangementProblems(stored, profile()).some((p) => p.fault === "role-unexpressible"),
  );
});

test("a scene or widget naming a module this build lacks is dropped, not rendered empty", () => {
  const stored: Arrangement = {
    conceptId: SENSOR.id,
    elements: [
      { element: "scene", band: "shape", options: { sceneId: "goalMap" } },
      { element: "table", band: "roll" },
    ],
  };

  // view-kit itself registers NO scenes -- it cannot, it must not import
  // three.js -- so with no registry passed the entry is unknown.
  const bare = sanitizeArrangement(stored, profile());
  assert.deepEqual(bare.elements.map((e) => e.element), ["table"]);
  assert.ok(arrangementProblems(stored, profile()).some((p) => p.fault === "unknown-module"));

  // A host that HAS the scene keeps it.
  const hosted = sanitizeArrangement(stored, profile(), { scenes: ["goalMap"] });
  assert.deepEqual(hosted.elements.map((e) => e.element), ["scene", "table"]);
});

test("the deterministic proposal never reaches for a scene or a widget", () => {
  // Both fit every concept trivially (they require nothing of it). Without
  // `placedOnly` a scene would auto-propose itself into every view in the
  // product as a black rectangle nobody asked for.
  const proposed = proposeArrangement(profile()).elements.map((e) => e.element);
  assert.ok(!proposed.includes("scene"));
  assert.ok(!proposed.includes("widget"));
});

// ---------------------------------------------------------------------------
// Required entries: the page has a job
// ---------------------------------------------------------------------------

test("a required entry a version dropped is re-inserted", () => {
  const dropped: Arrangement = {
    conceptId: SENSOR.id,
    elements: [{ element: "statTile", band: "reading" }],
  };

  const clean = sanitizeArrangement(dropped, profile(), {
    required: [{ element: "table", band: "roll" }],
  });
  assert.ok(clean.elements.some((e) => e.element === "table"));
  // The stored value is untouched, like every other repair here.
  assert.equal(dropped.elements.length, 1);
});

test("a required entry already present is not duplicated, however it was edited", () => {
  // Matching is loose ON PURPOSE: a manifest requires "the machines table",
  // not "the machines table in the roll band captioned Machines". A person
  // who moved it or re-captioned it has not removed it, and a second copy
  // would be worse than the drop this guards against.
  const edited: Arrangement = {
    conceptId: SENSOR.id,
    elements: [{ element: "table", band: "roll", title: "Readings, newest first" }],
  };

  const clean = sanitizeArrangement(edited, profile(), {
    required: [{ element: "table", band: "roll" }],
  });
  assert.equal(clean.elements.filter((e) => e.element === "table").length, 1);
  assert.equal(clean.elements[0]?.title, "Readings, newest first");
});

test("a required entry that cannot render is not force-fed onto the page", () => {
  // The manifest says the page needs this element. It does not overrule the
  // fitness contract: inserting an unfit element would put view-kit's
  // "cannot render, here is why" sentence on a page as though somebody had
  // chosen it.
  const empty = profileConcept({ id: "v9:madeup:nothing", entity: "nothing" }, []);
  const clean = sanitizeArrangement(
    { conceptId: "v9:madeup:nothing", elements: [{ element: "statTile", band: "reading" }] },
    empty,
    { required: [{ element: "calendar", band: "shape" }] },
  );
  assert.ok(!clean.elements.some((e) => e.element === "calendar"));
});

// ---------------------------------------------------------------------------
// Reading an untrusted reply
// ---------------------------------------------------------------------------

test("readArrangement parses layout and role, and reports a layout it does not know", () => {
  const proposal = readArrangement(
    {
      reasoning: "Lead with the trend.",
      layout: "focus",
      elements: [
        { element: "chart.line", band: "shape", role: "hero" },
        { element: "table", band: "roll", role: "supporting" },
      ],
    },
    profile(),
  );

  assert.equal(arrangementLayout(proposal.arrangement), "focus");
  assert.equal(proposal.arrangement.elements[0]?.role, "hero");
  assert.equal(proposal.arrangement.elements[1]?.role, "supporting");

  const nonsense = readArrangement(
    { layout: "carousel", elements: [{ element: "table", band: "roll", role: "protagonist" }] },
    profile(),
  );
  assert.equal(arrangementLayout(nonsense.arrangement), "stack");
  // An unrecognised ROLE is dropped rather than corrected to "standard" --
  // the same rendered value, but the problem list keeps the difference
  // between "the model said standard" and "the model said something we do
  // not have", which is what makes a bad prompt visible.
  assert.equal(nonsense.arrangement.elements[0]?.role, undefined);
  assert.ok(nonsense.problems.some((p) => p.fault === "unknown-layout"));
});

// ---------------------------------------------------------------------------
// Schema-first profiling
// ---------------------------------------------------------------------------

const DECLARED: ConceptLike = {
  id: "v9:madeup:shipment",
  entity: "shipment",
  fields: [
    { name: "reference", kind: "string", required: true, description: "The carrier's own number." },
    { name: "status", kind: "enum", enumValues: ["booked", "in_transit", "delivered", "lost"] },
    { name: "weightKg", kind: "number" },
    { name: "dispatchedAt", kind: "datetime" },
    { name: "insured", kind: "boolean" },
    { name: "carrierId", kind: "string" },
  ],
  relationships: [
    { type: "references", as: "carrier", field: "carrierId", target: "v9:madeup:carrier" },
  ],
};

test("a declared field with no loaded value is profiled, not invisible", () => {
  // The failure this ends: compose a view over page one of a walk and half
  // the concept is missing, because nothing on that page happened to carry
  // the field.
  const p = profileConcept(DECLARED, [{ id: "s1", reference: "AB-1" }]);
  const names = p.fields.map((f) => f.field);
  for (const declared of ["reference", "status", "weightKg", "dispatchedAt", "insured", "carrierId"]) {
    assert.ok(names.includes(declared), `${declared} should be profiled from the declaration`);
  }
});

test("the declaration owns the kind and the rows own the cardinality", () => {
  const p = profileConcept(DECLARED, [
    // A datetime that arrived EMPTY in every row. Sampling alone would call
    // this text and a timeline would stop offering itself.
    { id: "s1", reference: "AB-1", dispatchedAt: "", weightKg: 12, status: "booked" },
    { id: "s2", reference: "AB-2", dispatchedAt: "", weightKg: 12, status: "booked" },
  ]);
  const by = (name: string) => p.fields.find((f) => f.field === name)!;

  assert.equal(by("dispatchedAt").kind, "datetime", "the declaration wins on kind");
  assert.equal(by("weightKg").kind, "number");
  assert.equal(by("insured").kind, "boolean");
  // An enum collapses to `text` in the RENDERING kind (that was already the
  // contract -- every distinctMax requirement depends on it) while
  // declaredKind keeps the distinction losslessly.
  assert.equal(by("status").kind, "text");
  assert.equal(by("status").declaredKind, "enum");

  // Cardinality is an observation and the declaration cannot make it up.
  assert.equal(by("weightKg").present, 2);
  assert.equal(by("weightKg").distinct, 1);
  assert.equal(by("dispatchedAt").present, 0);
});

test("a declared enum reports its member count when no rows are loaded", () => {
  // Zero distinct values would tell a grouped board there is nothing to group
  // by -- which is precisely the "invisible until enough rows load" failure
  // the schema exists to end. The member list makes the count exact rather
  // than guessed.
  const p = profileConcept(DECLARED, []);
  const status = p.fields.find((f) => f.field === "status")!;
  assert.equal(status.distinct, 4);
  assert.deepEqual([...status.enumValues], ["booked", "in_transit", "delivered", "lost"]);
});

test("declared fields come first, row-only fields keep their place after them", () => {
  const p = profileConcept(DECLARED, [
    { id: "s1", reference: "AB-1", createdAt: "2026-08-01T00:00:00Z", concept: DECLARED.id },
  ]);
  const names = p.fields.map((f) => f.field);
  assert.equal(names[0], "reference", "the declaration's order leads");
  // Intrinsics are real fields of the row set -- a table may legitimately
  // want an id column -- so they are profiled, just not first.
  for (const intrinsic of ["id", "createdAt", "concept"]) {
    assert.ok(names.includes(intrinsic));
    assert.ok(names.indexOf(intrinsic) > names.indexOf("carrierId"));
  }
  assert.equal(p.fields.find((f) => f.field === "id")!.declared, false);
  assert.equal(p.fields.find((f) => f.field === "reference")!.declared, true);
  assert.equal(p.fields.find((f) => f.field === "reference")!.required, true);
});

test("a relationship's pointer field carries the edge", () => {
  // What makes a lookup column possible: the target concept is named on the
  // field, so a renderer knows which rows to resolve the value against.
  const p = profileConcept(DECLARED, [{ id: "s1", carrierId: "c1" }]);
  const carrierId = p.fields.find((f) => f.field === "carrierId")!;
  assert.equal(carrierId.relationship?.as, "carrier");
  assert.equal(carrierId.relationship?.target, "v9:madeup:carrier");
  assert.equal(p.fields.find((f) => f.field === "reference")!.relationship, undefined);
});

test("a concept whose cluster publishes no shape profiles exactly as it used to", () => {
  // The row-only path is not a legacy branch. It is the answer for a server
  // that predates the fields, for a concept whose definition schema did not
  // parse, and for row sets that are not concept rows at all.
  const rowOnly = profileConcept(SENSOR, SENSOR_ROWS);
  assert.ok(rowOnly.fields.length > 0);
  assert.equal(rowOnly.fields.find((f) => f.field === "degrees")!.kind, "number");
  assert.equal(rowOnly.fields.find((f) => f.field === "takenAt")!.kind, "datetime");
  assert.equal(rowOnly.fields.find((f) => f.field === "faulty")!.kind, "boolean");
  for (const field of rowOnly.fields) {
    assert.equal(field.declared, false);
  }
  // And an empty published list is treated identically to an absent one --
  // both mean "no shape published".
  const emptyShape = profileConcept({ ...SENSOR, fields: [] }, SENSOR_ROWS);
  assert.deepEqual(
    emptyShape.fields.map((f) => f.field),
    rowOnly.fields.map((f) => f.field),
  );
});
