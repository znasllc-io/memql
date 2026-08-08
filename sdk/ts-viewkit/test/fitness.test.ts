// The element-fitness contract (memql#3317).
//
// These tests are the contract. The prose lives in fitness.ts and
// docs/public/concepts/view-elements.md; if the three disagree, this file is
// what #3319's predefined views and #3320's composer actually get, so every
// clause of the rule gets an assertion here rather than only a sentence
// there.

import test from "node:test";
import assert from "node:assert/strict";

import {
  boundField,
  boundFields,
  explainFit,
  fitElement,
  fitElements,
  profileConcept,
  renderElement,
  type ElementSpec,
} from "../src/fitness.js";
import { h, text, renderToHtml } from "../src/vnode.js";

const BARE = { id: "x", entity: "widget" };

const NOOP = (): ReturnType<typeof h> => h("div", { class: "vk-empty" }, [text("x")]);

function spec(partial: Partial<ElementSpec> & Pick<ElementSpec, "id" | "requires">): ElementSpec {
  return {
    title: partial.id,
    summary: "",
    render: NOOP,
    ...partial,
  } as ElementSpec;
}

// ---------------------------------------------------------------------------
// profileConcept -- kinds are observed from values, not declared
// ---------------------------------------------------------------------------

test("field kinds are derived from row values", () => {
  const profile = profileConcept(BARE, [
    { id: "a", name: "Ada", count: 3, ok: true, at: "2026-08-08T10:00:00Z", tags: ["x"], meta: {} },
  ]);
  const kinds = Object.fromEntries(profile.fields.map((f) => [f.field, f.kind]));
  assert.deepEqual(kinds, {
    id: "text",
    name: "text",
    count: "number",
    ok: "boolean",
    at: "datetime",
    tags: "list",
    meta: "object",
  });
});

test("a date-shaped string is a datetime, a name is not", () => {
  const profile = profileConcept(BARE, [{ id: "a", when: "2026-08-08", who: "Ada" }]);
  const kinds = Object.fromEntries(profile.fields.map((f) => [f.field, f.kind]));
  assert.equal(kinds["when"], "datetime");
  assert.equal(kinds["who"], "text");
});

test("a field with mixed kinds takes the most specific one", () => {
  // A nullable datetime arrives as "" in some rows. It is still a datetime.
  const profile = profileConcept(BARE, [
    { id: "a", at: "" },
    { id: "b", at: "2026-08-08T10:00:00Z" },
  ]);
  assert.equal(profile.fields.find((f) => f.field === "at")?.kind, "datetime");
});

test("the profile counts presence and distinct values", () => {
  const profile = profileConcept(BARE, [
    { id: "a", status: "open" },
    { id: "b", status: "open" },
    { id: "c" },
  ]);
  const status = profile.fields.find((f) => f.field === "status");
  assert.equal(status?.present, 2);
  assert.equal(status?.distinct, 1);
});

test("the profile carries the resolved display card, declared or inferred", () => {
  const declared = profileConcept(
    { id: "x", entity: "widget", displayCard: { primary: "code" } },
    [{ id: "a", code: "A1", name: "Ada" }],
  );
  assert.equal(declared.card.primary, "code");
  assert.deepEqual(declared.fields.find((f) => f.field === "code")?.slots, ["primary"]);

  // No declaration: the fallback contract's inference, unchanged.
  const inferred = profileConcept(BARE, [{ id: "a", name: "Ada" }]);
  assert.equal(inferred.card.primary, "name");
});

// ---------------------------------------------------------------------------
// Resolution order
// ---------------------------------------------------------------------------

const ROWS = [
  { id: "a", name: "Ada", status: "open", note: "hello", at: "2026-08-08T10:00:00Z" },
];
const CARDED = {
  id: "x",
  entity: "widget",
  displayCard: { primary: "name", status: "status" },
};

test("a display-card slot beats a name-family match", () => {
  const element = spec({
    id: "e",
    requires: [
      { slot: "label", description: "the label", kinds: ["text"], prefer: ["primary"], preferNames: ["note"] },
    ],
  });
  const fit = fitElement(element, profileConcept(CARDED, ROWS));
  assert.equal(boundField(fit, "label"), "name");
});

test("a name-family match beats the generic scan", () => {
  const element = spec({
    id: "e",
    requires: [{ slot: "label", description: "the label", kinds: ["text"], preferNames: ["note"] }],
  });
  const fit = fitElement(element, profileConcept(BARE, ROWS));
  assert.equal(boundField(fit, "label"), "note");
});

test("an explicit override beats everything", () => {
  const element = spec({
    id: "e",
    requires: [
      { slot: "label", description: "the label", kinds: ["text"], prefer: ["primary"], preferNames: ["note"] },
    ],
  });
  const fit = fitElement(element, profileConcept(CARDED, ROWS), {
    bindings: { label: "status" },
  });
  assert.equal(boundField(fit, "label"), "status");
});

test("the generic scan skips row plumbing", () => {
  const element = spec({
    id: "e",
    requires: [{ slot: "label", description: "the label", kinds: ["text"] }],
  });
  // `id` and `concept` come first in the row, and neither may be picked up by
  // an automatic scan.
  const fit = fitElement(
    element,
    profileConcept(BARE, [{ id: "a", concept: "x", name: "Ada" }]),
  );
  assert.equal(boundField(fit, "label"), "name");
});

test("a display card may still name row plumbing, and that decision is honoured", () => {
  const element = spec({
    id: "e",
    requires: [
      { slot: "label", description: "the label", kinds: ["text"], prefer: ["primary"] },
    ],
  });
  const fit = fitElement(
    element,
    profileConcept({ id: "x", entity: "w", displayCard: { primary: "id" } }, [{ id: "a", name: "Ada" }]),
  );
  assert.equal(boundField(fit, "label"), "id");
});

test("a field binds to at most one slot per element", () => {
  const element = spec({
    id: "e",
    requires: [
      { slot: "start", description: "the start", kinds: ["datetime"] },
      { slot: "end", description: "the end", kinds: ["datetime"], min: 0 },
    ],
  });
  const fit = fitElement(element, profileConcept(BARE, ROWS));
  assert.equal(boundField(fit, "start"), "at");
  assert.equal(boundField(fit, "end"), undefined, "the one datetime cannot fill both");
});

test("an explicitOnly slot refuses a type-compatible field it was not pointed at", () => {
  const element = spec({
    id: "e",
    requires: [
      {
        slot: "allDay",
        description: "whether it is all day",
        kinds: ["boolean"],
        min: 0,
        explicitOnly: true,
        preferNames: ["allDay"],
      },
    ],
  });
  const fit = fitElement(element, profileConcept(BARE, [{ id: "a", deleted: false }]));
  assert.equal(boundField(fit, "allDay"), undefined);
  assert.match(fit.unmet[0].reason, /none named allDay/);
});

test("a categorical slot prefers the lowest-cardinality candidate", () => {
  const element = spec({
    id: "e",
    requires: [
      {
        slot: "group",
        description: "the grouping",
        kinds: ["text"],
        distinctMax: 12,
        preferFewestDistinct: true,
      },
    ],
  });
  const rows = [
    { id: "1", title: "one", tier: "gold" },
    { id: "2", title: "two", tier: "gold" },
    { id: "3", title: "three", tier: "silver" },
  ];
  assert.equal(boundField(fitElement(element, profileConcept(BARE, rows)), "group"), "tier");
});

test("distinctMax rules out free text", () => {
  const element = spec({
    id: "e",
    requires: [
      { slot: "group", description: "the grouping", kinds: ["text"], distinctMax: 2 },
    ],
  });
  const rows = [
    { id: "1", note: "a" },
    { id: "2", note: "b" },
    { id: "3", note: "c" },
  ];
  assert.equal(fitElement(element, profileConcept(BARE, rows)).verdict, "unfit");
});

test("autoMax caps the automatic scan but not an override", () => {
  const element = spec({
    id: "e",
    requires: [
      { slot: "y", description: "the measures", kinds: ["number"], max: 3, autoMax: 1 },
    ],
  });
  const rows = [{ id: "1", a: 1, b: 2, c: 3 }];
  assert.deepEqual(boundFields(fitElement(element, profileConcept(BARE, rows)), "y"), ["a"]);
  assert.deepEqual(
    boundFields(
      fitElement(element, profileConcept(BARE, rows), { bindings: { y: ["b", "c"] } }),
      "y",
    ),
    ["b", "c"],
  );
});

test("naming a slot settles it: an empty override binds nothing and stops the scan", () => {
  // How a predefined view asks the stat strip for a row count and no summed
  // measures. Without it, every optional numeric slot is auto-filled and a
  // view has no way to decline -- which is how "revocationEpoch total" ends up
  // on a page about people (memql#3319).
  const element = spec({
    id: "e",
    requires: [
      { slot: "metric", description: "the measures", kinds: ["number"], min: 0, max: 3 },
    ],
  });
  const rows = [{ id: "1", a: 1, b: 2 }];
  assert.deepEqual(boundFields(fitElement(element, profileConcept(BARE, rows)), "metric"), [
    "a",
    "b",
  ]);

  const declined = fitElement(element, profileConcept(BARE, rows), {
    bindings: { metric: [] },
  });
  assert.deepEqual(boundFields(declined, "metric"), []);
  // Optional, so declining it degrades rather than disqualifies.
  assert.equal(declined.verdict, "partial");
  assert.equal(declined.unmet[0].reason, "the caller bound no field to it");
});

test("an override naming a field that does not exist reports the gap", () => {
  // Rather than silently substituting whatever the generic scan liked next,
  // which a caller who named a field has no way to notice.
  const element = spec({
    id: "e",
    requires: [{ slot: "label", description: "the label", kinds: ["text"] }],
  });
  const fit = fitElement(element, profileConcept(BARE, ROWS), {
    bindings: { label: "noSuchField" },
  });
  assert.equal(fit.verdict, "unfit");
  assert.deepEqual(boundFields(fit, "label"), []);
});

test('max "all" binds every candidate, preferred ones first', () => {
  const element = spec({
    id: "e",
    requires: [
      {
        slot: "column",
        description: "the columns",
        kinds: ["text"],
        max: "all",
        prefer: ["status"],
      },
    ],
  });
  const fit = fitElement(element, profileConcept(CARDED, ROWS));
  assert.deepEqual(boundFields(fit, "column"), ["status", "name", "note"]);
});

// ---------------------------------------------------------------------------
// Verdicts
// ---------------------------------------------------------------------------

test("every requirement bound is a full fit", () => {
  const element = spec({
    id: "e",
    requires: [
      { slot: "label", description: "the label", kinds: ["text"] },
      { slot: "at", description: "the time", kinds: ["datetime"], min: 0 },
    ],
  });
  assert.equal(fitElement(element, profileConcept(BARE, ROWS)).verdict, "full");
});

test("an unbound optional requirement is a partial fit, not a rejection", () => {
  const element = spec({
    id: "e",
    requires: [
      { slot: "label", description: "the label", kinds: ["text"] },
      { slot: "n", description: "a measure", kinds: ["number"], min: 0, degraded: "no total is shown" },
    ],
  });
  const fit = fitElement(element, profileConcept(BARE, ROWS));
  assert.equal(fit.verdict, "partial");
  assert.equal(fit.unmet.length, 1);
  assert.equal(fit.unmet[0].required, false);
});

test("an unbound required requirement is unfit", () => {
  const element = spec({
    id: "e",
    requires: [{ slot: "n", description: "a measure", kinds: ["number"] }],
  });
  const fit = fitElement(element, profileConcept(BARE, ROWS));
  assert.equal(fit.verdict, "unfit");
  assert.equal(fit.unmet[0].required, true);
});

test("too few rows is unfit, and says so", () => {
  const element = spec({ id: "e", requires: [], minRows: 2 });
  const fit = fitElement(element, profileConcept(BARE, [{ id: "a" }]));
  assert.equal(fit.verdict, "unfit");
  assert.match(fit.unmet[0].reason, /at least 2 rows, the set has 1/);
});

test("an element with no requirements fits anything", () => {
  const element = spec({ id: "e", requires: [] });
  const fit = fitElement(element, profileConcept(BARE, [{ id: "a" }]));
  assert.equal(fit.verdict, "full");
  assert.equal(fit.score, 1);
});

test("score weighs required slots above optional ones", () => {
  const element = spec({
    id: "e",
    requires: [
      { slot: "label", description: "the label", kinds: ["text"] },
      { slot: "n", description: "a measure", kinds: ["number"], min: 0 },
    ],
  });
  // 1 of 1.5 earned.
  assert.equal(fitElement(element, profileConcept(BARE, ROWS)).score, 0.667);
});

// ---------------------------------------------------------------------------
// Ranking + explanation
// ---------------------------------------------------------------------------

test("fitElements ranks full above partial above unfit, specific above universal", () => {
  const universal = spec({ id: "universal", requires: [] });
  const specific = spec({
    id: "specific",
    requires: [{ slot: "at", description: "the time", kinds: ["datetime"] }],
  });
  const impossible = spec({
    id: "impossible",
    requires: [{ slot: "n", description: "a measure", kinds: ["number"] }],
  });
  const ranked = fitElements([universal, impossible, specific], profileConcept(BARE, ROWS));
  assert.deepEqual(
    ranked.map((f) => f.element),
    ["specific", "universal", "impossible"],
  );
});

test("explainFit names the fields it bound", () => {
  const element = spec({
    id: "calendar",
    title: "Calendar",
    requires: [{ slot: "at", description: "the date each row sits on", kinds: ["datetime"] }],
  });
  const profile = profileConcept(BARE, ROWS);
  const words = explainFit(element, fitElement(element, profile), profile);
  assert.equal(words, "Calendar fits widget. It uses at for the date each row sits on.");
});

test("explainFit says what is lost on a partial fit", () => {
  const element = spec({
    id: "e",
    title: "Chart",
    requires: [
      { slot: "label", description: "the label", kinds: ["text"] },
      {
        slot: "n",
        description: "the measure",
        kinds: ["number"],
        min: 0,
        degraded: "bars count rows instead",
      },
    ],
  });
  const profile = profileConcept(BARE, ROWS);
  const words = explainFit(element, fitElement(element, profile), profile);
  assert.match(words, /Chart fits widget, with limits\./);
  assert.match(words, /Nothing supplies the measure .*, so bars count rows instead\./);
});

test("explainFit says why an unfit element cannot render", () => {
  const element = spec({
    id: "e",
    title: "Chart",
    requires: [{ slot: "n", description: "the measure", kinds: ["number"] }],
  });
  const profile = profileConcept(BARE, ROWS);
  const words = explainFit(element, fitElement(element, profile), profile);
  assert.match(words, /Chart cannot render widget\./);
  assert.match(words, /It needs the measure, and there is no unused a number field\./);
});

// ---------------------------------------------------------------------------
// renderElement
// ---------------------------------------------------------------------------

test("renderElement refuses to render an unfit element", () => {
  const element = spec({
    id: "e",
    requires: [{ slot: "n", description: "a measure", kinds: ["number"] }],
  });
  assert.equal(renderElement(element, ROWS, BARE), undefined);
});

test("renderElement renders a fitting one", () => {
  const element = spec({ id: "e", requires: [] });
  const node = renderElement(element, ROWS, BARE);
  assert.ok(node);
  assert.match(renderToHtml(node), /vk-empty/);
});
