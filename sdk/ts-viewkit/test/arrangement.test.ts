// The arrangement contract (memql#3320).
//
// The claim this file has to make true is narrow and load-bearing: a view can
// be composed for a concept nobody wrote code for, WITHOUT a model. Every
// test here runs with no network, no provider and no configuration; the AI
// half of the composer is exercised only through readArrangement, which is
// deliberately a pure function over an untrusted object so that "the model
// said something odd" is a unit test rather than an integration one.

import test from "node:test";
import assert from "node:assert/strict";

import {
  ARRANGEMENT_PROPOSAL_SCHEMA,
  arrangementProblems,
  arrangementRequest,
  elementCandidates,
  elementOptions,
  explainArrangement,
  proposeArrangement,
  readArrangement,
  sanitizeArrangement,
  type Arrangement,
} from "../src/arrangement.js";
import { VIEW_KIT_ELEMENTS, elementById } from "../src/elements.js";
import {
  BAND_ROLES,
  elementBand,
  profileConcept,
  renderElement,
} from "../src/fitness.js";
import type { ConceptLike, RowLike } from "../src/types.js";
import { renderToHtml } from "../src/vnode.js";

// ---------------------------------------------------------------------------
// A concept this repository has never seen
// ---------------------------------------------------------------------------
//
// THE POINT OF THE WHOLE EPIC, asserted rather than asserted-in-prose. This
// concept is not in dsl/, no element mentions it, no registry lists it, and
// nothing in view-kit or the portal was changed to accommodate it. If a
// composed view falls out of it, then "a concept declared tomorrow is
// composable tomorrow" is a property of the code and not a hope.
const INVENTED: ConceptLike = { id: "v9:madeup:sensorReading", entity: "sensorReading" };

const INVENTED_ROWS: readonly RowLike[] = [
  { id: "r1", label: "north inlet", zone: "intake", degrees: 41.2, takenAt: "2026-08-01T06:00:00Z", faulty: false },
  { id: "r2", label: "south inlet", zone: "intake", degrees: 39.8, takenAt: "2026-08-01T07:00:00Z", faulty: false },
  { id: "r3", label: "return line", zone: "return", degrees: 55.4, takenAt: "2026-08-01T08:00:00Z", faulty: true },
  { id: "r4", label: "bypass", zone: "bypass", degrees: 48.0, takenAt: "2026-08-01T09:00:00Z", faulty: false },
];

function inventedProfile() {
  return profileConcept(INVENTED, INVENTED_ROWS);
}

test("a concept nothing in this repo declares yields a usable view, with no code change", () => {
  const profile = inventedProfile();
  const arrangement = proposeArrangement(profile);

  // Something in every band: a reading, a shape and a roll. That is the whole
  // grammar the five hand-designed views follow, produced here from the
  // concept's shape alone.
  assert.deepEqual(
    arrangement.elements.map((e) => e.band),
    ["reading", "shape", "roll"],
  );
  assert.equal(arrangement.conceptId, INVENTED.id);

  // And it RENDERS -- not "would fit", actually produces markup for every
  // band. A composed view that fits and draws nothing is not a view.
  for (const entry of arrangement.elements) {
    const element = elementById(entry.element);
    assert.ok(element, `${entry.element} is not in the library`);
    const tree = renderElement(element!, INVENTED_ROWS, INVENTED, elementOptions(entry));
    assert.ok(tree !== undefined, `${entry.element} reported fit and rendered nothing`);
    const html = renderToHtml(tree!);
    assert.ok(html.length > 0, `${entry.element} rendered an empty string`);
  }

  // The roll band shows the rows themselves, and the invented concept's own
  // values are in the output -- so the view is about THIS data, not a shell.
  const roll = arrangement.elements.find((e) => e.band === "roll")!;
  const html = renderToHtml(
    renderElement(elementById(roll.element)!, INVENTED_ROWS, INVENTED, elementOptions(roll))!,
  );
  assert.match(html, /north inlet/);
});

test("nothing in the arrangement layer knows the concept's name", () => {
  // The negative half of the test above: the same call against a concept with
  // completely different fields must produce a different, equally usable
  // arrangement. If any of this were concept-specific, one of the two would
  // degrade.
  const other: ConceptLike = { id: "v9:madeup:ticket", entity: "ticket" };
  const rows: readonly RowLike[] = [
    { id: "t1", title: "printer jam", state: "open" },
    { id: "t2", title: "vpn down", state: "closed" },
  ];
  const arrangement = proposeArrangement(profileConcept(other, rows));
  assert.ok(arrangement.elements.length >= 2);
  assert.ok(arrangement.elements.some((e) => e.band === "roll"));
  for (const entry of arrangement.elements) {
    assert.ok(renderElement(elementById(entry.element)!, rows, other, elementOptions(entry)));
  }
});

// ---------------------------------------------------------------------------
// Candidacy is deterministic and explainable
// ---------------------------------------------------------------------------

test("every element in the library is judged, including the ones that do not fit", () => {
  const candidates = elementCandidates(inventedProfile());
  assert.equal(candidates.length, VIEW_KIT_ELEMENTS.length);
  for (const candidate of candidates) {
    assert.ok(candidate.explanation.length > 0, `${candidate.element.id} has no explanation`);
  }
  // The map requires coordinates the invented concept does not carry, so it is
  // offered as unusable WITH A REASON rather than dropped from the picker.
  const map = candidates.find((c) => c.element.id === "map")!;
  assert.equal(map.usable, false);
  assert.match(map.explanation, /cannot render sensorReading/);
});

test("candidacy is a pure function of the profile", () => {
  const a = elementCandidates(inventedProfile()).map((c) => `${c.element.id}:${c.fit.verdict}`);
  const b = elementCandidates(inventedProfile()).map((c) => `${c.element.id}:${c.fit.verdict}`);
  assert.deepEqual(a, b);
});

test("candidates are ranked fitting-first", () => {
  const candidates = elementCandidates(inventedProfile());
  const firstUnusable = candidates.findIndex((c) => !c.usable);
  if (firstUnusable !== -1) {
    for (const candidate of candidates.slice(firstUnusable)) {
      assert.equal(candidate.usable, false, "a fitting element sorted below an unfit one");
    }
  }
});

test("every element declares a band, explicitly or by the documented default", () => {
  for (const element of VIEW_KIT_ELEMENTS) {
    assert.ok(BAND_ROLES.includes(elementBand(element)), `${element.id} has no valid band`);
  }
});

// ---------------------------------------------------------------------------
// The empty and the awkward cases
// ---------------------------------------------------------------------------

test("a concept with no rows still yields an arrangement", () => {
  // Almost every element is below its minimum row count here, so almost
  // nothing fits -- the stat strip is the exception, since "0 rows" is a
  // reading. The ROLL band fills anyway from the universal fallback:
  // rendering it produces view-kit's own explanation of why it is empty,
  // which is a better empty state than a page with no elements, and the
  // arrangement is still savable and still correct the day rows arrive.
  const empty: ConceptLike = { id: "v9:madeup:nothing", entity: "nothing" };
  const arrangement = proposeArrangement(profileConcept(empty, []));
  assert.deepEqual(
    arrangement.elements.map((e) => `${e.band}/${e.element}`),
    ["reading/statTile", "roll/rowList"],
  );
});

test("a one-field concept still gets a roll band", () => {
  const thin: ConceptLike = { id: "v9:madeup:thin", entity: "thin" };
  const rows = [{ id: "a" }, { id: "b" }];
  const arrangement = proposeArrangement(profileConcept(thin, rows));
  assert.ok(arrangement.elements.some((e) => e.band === "roll"));
});

// ---------------------------------------------------------------------------
// Editing: an arrangement is plain data
// ---------------------------------------------------------------------------

test("an arrangement survives a JSON round trip unchanged", () => {
  // This is what makes a saved view a concept row rather than a serialization
  // problem: the value the composer holds IS the value that is stored.
  const arrangement = proposeArrangement(inventedProfile());
  assert.deepEqual(JSON.parse(JSON.stringify(arrangement)), arrangement);
});

test("a hand-built arrangement is validated by the same rules as a proposed one", () => {
  const profile = inventedProfile();
  const byHand: Arrangement = {
    conceptId: INVENTED.id,
    elements: [
      { element: "table", band: "roll" },
      { element: "map", band: "roll" },
      { element: "notAnElement", band: "reading" },
      { element: "table", band: "roll", bindings: { column: ["nosuchfield"] } },
    ],
  };
  const faults = arrangementProblems(byHand, profile).map((p) => p.fault);
  assert.ok(faults.includes("unfit"), "the map over a concept with no coordinates must report unfit");
  assert.ok(faults.includes("unknown-element"));
  assert.ok(faults.includes("duplicate"));
  assert.ok(faults.includes("unknown-field"));
});

test("sanitizing drops what cannot render and keeps what merely degrades", () => {
  const profile = inventedProfile();
  const messy: Arrangement = {
    conceptId: INVENTED.id,
    elements: [
      { element: "map", band: "roll" },
      { element: "notAnElement", band: "roll" },
      // An OPTIONAL slot bound to a field the sample lacks: the strip loses
      // its measure and keeps its count, so the entry survives and the
      // problem is reported.
      { element: "statTile", band: "reading", bindings: { metric: ["nosuchfield"] } },
    ],
  };
  const clean = sanitizeArrangement(messy, profile);
  assert.deepEqual(
    clean.elements.map((e) => e.element),
    ["statTile"],
    "a degraded entry is reported, not deleted",
  );
  assert.ok(arrangementProblems(messy, profile).some((p) => p.fault === "unknown-field"));
});

test("a REQUIRED slot bound to a field that is not there leaves the element unfit", () => {
  // The fitness contract, not a rule this module adds: naming a slot settles
  // it, so an override the profile cannot honour leaves the slot unbound --
  // and a table with no columns is not a table. Dropped rather than rendered
  // blank, and the reason is reported.
  const profile = inventedProfile();
  const broken: Arrangement = {
    conceptId: INVENTED.id,
    elements: [{ element: "table", band: "roll", bindings: { column: ["nosuchfield"] } }],
  };
  const faults = arrangementProblems(broken, profile).map((p) => p.fault);
  assert.ok(faults.includes("unfit"));
  assert.ok(faults.includes("unknown-field"));
  assert.deepEqual(sanitizeArrangement(broken, profile), proposeArrangement(profile));
});

test("sanitizing a hopeless arrangement falls back to the deterministic proposal", () => {
  const profile = inventedProfile();
  const hopeless: Arrangement = {
    conceptId: INVENTED.id,
    elements: [{ element: "map", band: "roll" }],
  };
  assert.deepEqual(sanitizeArrangement(hopeless, profile), proposeArrangement(profile));
});

test("an empty binding list declines a slot rather than being ignored", () => {
  const profile = inventedProfile();
  const entry = { element: "statTile", band: "reading" as const, bindings: { metric: [] } };
  const opts = elementOptions(entry);
  assert.deepEqual(opts.bindings, { metric: [] });
  // Rendered, the strip carries the row count and no summed measure.
  const html = renderToHtml(renderElement(elementById("statTile")!, INVENTED_ROWS, INVENTED, opts)!);
  assert.ok(!html.includes("degrees"), "a declined metric slot still summed a measure");
});

test("explainArrangement speaks for every entry, in the element's own words", () => {
  const profile = inventedProfile();
  const arrangement = proposeArrangement(profile);
  const lines = explainArrangement(arrangement, profile);
  assert.equal(lines.length, arrangement.elements.length);
  for (const line of lines) assert.match(line, /sensorReading/);
});

// ---------------------------------------------------------------------------
// The AI path, read as untrusted data
// ---------------------------------------------------------------------------

test("a well-formed proposal is honoured", () => {
  const profile = inventedProfile();
  const { arrangement, reasoning, problems } = readArrangement(
    {
      reasoning: "Readings over time, split by zone.",
      elements: [
        { element: "chart.line", band: "shape" },
        { element: "table", band: "roll", title: "Every reading" },
      ],
    },
    profile,
  );
  assert.equal(reasoning, "Readings over time, split by zone.");
  assert.deepEqual(problems, []);
  assert.deepEqual(
    arrangement.elements.map((e) => `${e.band}/${e.element}`),
    ["shape/chart.line", "roll/table"],
  );
  assert.equal(arrangement.elements[1].title, "Every reading");
});

test("a proposal naming an element that does not exist is corrected, not obeyed", () => {
  const profile = inventedProfile();
  const { arrangement, problems } = readArrangement(
    { elements: [{ element: "sankeyDiagram", band: "roll" }, { element: "table", band: "roll" }] },
    profile,
  );
  assert.deepEqual(arrangement.elements.map((e) => e.element), ["table"]);
  assert.ok(problems.some((p) => p.fault === "unknown-element"));
});

test("a proposal is never allowed to produce a view a person could not build by hand", () => {
  const profile = inventedProfile();
  const { arrangement } = readArrangement(
    { elements: [{ element: "map", band: "roll" }] },
    profile,
  );
  // The map is unfit for this concept, so the proposal is emptied and the
  // deterministic answer stands.
  assert.deepEqual(arrangement, proposeArrangement(profile));
});

test("a proposal that is not an object at all degrades to the deterministic arrangement", () => {
  const profile = inventedProfile();
  for (const junk of [null, undefined, "elements", 42, [], { elements: "table" }]) {
    const { arrangement } = readArrangement(junk, profile);
    assert.deepEqual(arrangement, proposeArrangement(profile), `junk input ${JSON.stringify(junk)}`);
  }
});

test("a proposal with a wrong band is filed under the element's declared one", () => {
  const profile = inventedProfile();
  const { arrangement } = readArrangement(
    { elements: [{ element: "statTile", band: "roll" }] },
    profile,
  );
  assert.equal(arrangement.elements[0].band, "roll", "an explicit valid band is honoured");

  const { arrangement: fixed } = readArrangement(
    { elements: [{ element: "statTile", band: "nonsense" }] },
    profile,
  );
  assert.equal(fixed.elements[0].band, "reading", "an invalid band falls back to the declaration");
});

test("a proposal's bindings are coerced, and a string is read as a one-field list", () => {
  const profile = inventedProfile();
  const { arrangement } = readArrangement(
    { elements: [{ element: "table", band: "roll", bindings: { column: "label", junk: 7 } }] },
    profile,
  );
  assert.deepEqual(arrangement.elements[0].bindings, { column: ["label"] });
});

// ---------------------------------------------------------------------------
// What the model is shown
// ---------------------------------------------------------------------------

test("the request carries field shapes and no row values", () => {
  const request = arrangementRequest(inventedProfile());
  const serialized = JSON.stringify(request);
  assert.match(serialized, /"degrees"/, "field names are the question");
  assert.ok(!serialized.includes("north inlet"), "a row value reached the model");
  assert.ok(!serialized.includes("41.2"), "a row value reached the model");
});

test("the request offers only elements that already fit", () => {
  const request = arrangementRequest(inventedProfile());
  assert.ok(request.candidates.length > 0);
  assert.ok(
    !request.candidates.some((c) => c.element === "map"),
    "an unfit element was offered to the model",
  );
  for (const candidate of request.candidates) {
    assert.ok(candidate.explanation.length > 0);
  }
});

test("the request carries the deterministic baseline, so the model improves rather than invents", () => {
  const profile = inventedProfile();
  assert.deepEqual(arrangementRequest(profile).baseline, proposeArrangement(profile));
});

test("the schema and the parser agree on the shape", () => {
  // The provider enforces ARRANGEMENT_PROPOSAL_SCHEMA; readArrangement parses
  // what comes back. Two descriptions of one shape, one file apart, so pin
  // that they name the same things.
  const props = ARRANGEMENT_PROPOSAL_SCHEMA.properties;
  assert.deepEqual(Object.keys(props).sort(), ["elements", "reasoning"]);
  const item = props.elements.items.properties;
  assert.deepEqual(Object.keys(item).sort(), ["band", "bindings", "element", "title"]);
  assert.deepEqual([...item.band.enum], [...BAND_ROLES]);
});
