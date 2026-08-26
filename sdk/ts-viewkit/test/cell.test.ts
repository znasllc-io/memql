// Element personality: how one value reads (epic memql#4661, task memql#4665).
//
// THE THREE RULES THIS FILE PINS, each of which replaced something worse:
//
//   a boolean never renders as the word "true" -- the value is not the label,
//   the FIELD NAME is, and the value only decides assertion or negation;
//
//   a timestamp never renders as a raw ISO string as its primary text, and its
//   exact instant is never hover-only, because `title` does not exist on a
//   touch device;
//
//   nothing renders BLANK. A blank cell is ambiguous between "no value" and
//   "the renderer gave up", and only one of those is ever true.

import test from "node:test";
import assert from "node:assert/strict";

import {
  cellAttrs,
  cellContent,
  cellKind,
  displayColumns,
  DISPLAY_COLUMN_CAP,
} from "../src/cell.js";
import { formatRelative } from "../src/format.js";
import { profileConcept, type ConceptProfile } from "../src/fitness.js";
import { renderToHtml } from "../src/vnode.js";
import type { ConceptLike, RowLike } from "../src/types.js";

const SHIPMENT: ConceptLike = {
  id: "v9:madeup:shipment",
  entity: "shipment",
  displayCard: { primary: "reference", secondary: "status", status: "status" },
  fields: [
    { name: "reference", kind: "string", required: true },
    { name: "status", kind: "enum", required: true, enumValues: ["booked", "in_transit", "delivered"] },
    { name: "weightKg", kind: "number", required: true },
    { name: "insured", kind: "boolean" },
    { name: "dispatchedAt", kind: "datetime" },
    { name: "carrierId", kind: "string" },
    { name: "notes", kind: "string" },
    { name: "manifest", kind: "object" },
    { name: "labels", kind: "array" },
  ],
  relationships: [
    { type: "references", as: "carrier", field: "carrierId", target: "v9:madeup:carrier" },
  ],
};

const ROW: RowLike = {
  id: "shipment:s1",
  reference: "AB-1",
  status: "in_transit",
  weightKg: 1247932,
  insured: false,
  dispatchedAt: "2026-08-24T06:00:00Z",
  carrierId: "carrier:c1",
  notes: "Left the depot late.",
};

function profile(rows: readonly RowLike[] = [ROW]): ConceptProfile {
  return profileConcept(SHIPMENT, rows);
}

function field(p: ConceptProfile, name: string) {
  const f = p.fields.find((x) => x.field === name);
  assert.ok(f, `no profile for ${name}`);
  return f!;
}

function html(p: ConceptProfile, name: string, row: RowLike = ROW): string {
  const f = field(p, name);
  return renderToHtml(cellContent(row, f, cellKind(f)));
}

// ---------------------------------------------------------------------------
// Which fields a table shows
// ---------------------------------------------------------------------------

test("columns lead with the display card, then the required fields", () => {
  // The order is an argument: whatever the concept calls itself belongs at
  // the left edge (that is what a display card IS), and a field the schema
  // insists every row carries is part of the thing while an optional one is
  // elaboration. The second distinction is the one row sampling could not see
  // at all.
  const names = displayColumns(profile()).map((f) => f.field);
  assert.equal(names[0], "reference");
  assert.equal(names[1], "status");
  assert.equal(names[2], "weightKg", "the remaining required field comes next");
  assert.ok(names.indexOf("insured") > names.indexOf("weightKg"));
});

test("columns skip row plumbing and structured fields", () => {
  const names = displayColumns(profile()).map((f) => f.field);
  // An element that silently picked `concept` as a column would be worse
  // than one that showed a column fewer.
  for (const plumbing of ["id", "concept", "type", "schema"]) {
    assert.ok(!names.includes(plumbing), `${plumbing} should not be a column`);
  }
  // A nested block in a table column is either "[object Object]" or a wall of
  // JSON; the honest place to read one is the row detail.
  assert.ok(!names.includes("manifest"));
  assert.ok(!names.includes("labels"));
});

test("columns are capped, because a complete field list is not a useful one", () => {
  const wide: ConceptLike = {
    id: "v9:madeup:wide",
    entity: "wide",
    fields: Array.from({ length: 40 }, (_, i) => ({ name: `f${String(i).padStart(2, "0")}`, kind: "string" })),
  };
  const names = displayColumns(profileConcept(wide, []));
  assert.equal(names.length, DISPLAY_COLUMN_CAP);
});

// ---------------------------------------------------------------------------
// One rule per kind
// ---------------------------------------------------------------------------

test("the rule is chosen from the PROFILE, never from the value", () => {
  // Sniffing values means one row's cell is a pill and the next row's is
  // text, which is worse than either.
  const p = profile();
  assert.equal(cellKind(field(p, "status")), "enum");
  assert.equal(cellKind(field(p, "dispatchedAt")), "datetime");
  assert.equal(cellKind(field(p, "weightKg")), "number");
  assert.equal(cellKind(field(p, "insured")), "boolean");
  assert.equal(cellKind(field(p, "carrierId")), "reference");
  assert.equal(cellKind(field(p, "reference")), "text");
  assert.equal(cellKind(field(p, "id")), "id");
});

test("a datetime reads as elapsed time and carries the exact instant more than one way", () => {
  const out = html(profile(), "dispatchedAt");
  assert.match(out, /<time /);
  // The exact instant is NOT hover-only: `title` is unreachable on a touch
  // device, so it also rides `datetime` -- which is what a screen reader and
  // a copy-paste get.
  assert.match(out, /datetime="2026-08-24T06:00:00Z"/);
  assert.match(out, /title="2026-08-24 06:00 UTC"/);
  // And the visible text is the elapsed form, not the ISO string.
  assert.ok(!out.includes(">2026-08-24T06:00:00Z<"));
  assert.match(out, /ago<\/time>/);
});

test("a datetime carrying something unparseable shows what it holds", () => {
  // "the value is wrong" and "there is no value" are different problems, and
  // the first one has to stay visible.
  const out = html(profile(), "dispatchedAt", { ...ROW, dispatchedAt: "not-a-date" });
  assert.match(out, /not-a-date/);
});

test("a number is compact, mono and right-aligned, with the exact figure kept", () => {
  const out = html(profile(), "weightKg");
  // A column of "1.2M" is readable and a column of "1,247,932" is a wall.
  assert.match(out, />1\.2M</);
  assert.match(out, /title="1247932"/);
  assert.deepEqual(cellAttrs("number"), { "data-vk-cell": "number" });
  // Below a thousand, compact and exact are the same string, so nothing is
  // lost by the rule.
  assert.match(html(profile(), "weightKg", { ...ROW, weightKg: 42 }), />42</);
});

test("a boolean is a dot plus the FIELD's label, never the word true", () => {
  const out = html(profile(), "insured");
  assert.match(out, /vk-cell-dot/);
  assert.match(out, /data-value="false"/);
  assert.match(out, /not insured/);
  assert.ok(!/>true</.test(out) && !/>false</.test(out));

  const yes = html(profile(), "insured", { ...ROW, insured: true });
  assert.match(yes, /data-value="true"/);
  assert.match(yes, />insured</);
});

test("an enum is a pill carrying its raw member for the stylesheet", () => {
  const out = html(profile(), "status");
  assert.match(out, /class="vk-cell-pill"/);
  // Prose for people, data attributes for stylesheets -- the same split the
  // row status badge makes.
  assert.match(out, /data-status="in_transit"/);
});

test("an id and an unresolved reference both read as data, never as prose", () => {
  assert.match(html(profile(), "carrierId"), /class="vk-cell-data"/);
  assert.match(html(profile(), "carrierId"), /carrier:c1/);
  assert.match(html(profile(), "id"), /class="vk-cell-data"/);
});

test("a reference renders the target's label once it resolves, and the id until then", () => {
  // NEVER BLANK is the whole contract of this cell. There are four ways a
  // lookup fails to resolve -- deleted target, unreadable target, unknown
  // relationship, read not landed -- and a cell cannot tell them apart. The
  // id is true in all four and is something a person can paste into a query;
  // a blank would say "this row has no value here", which is false in all
  // four.
  const p = profile();
  const carrierId = field(p, "carrierId");

  const unresolved = renderToHtml(cellContent(ROW, carrierId, "reference"));
  assert.match(unresolved, /carrier:c1/);
  assert.match(unresolved, /vk-cell-data/);

  // A resolver that has not got the answer yet returns undefined, which is
  // the SAME rendering -- not a spinner and not an empty cell.
  const pending = renderToHtml(cellContent(ROW, carrierId, "reference", () => undefined));
  assert.equal(pending, unresolved);

  const resolved = renderToHtml(
    cellContent(ROW, carrierId, "reference", (as, rowId) => {
      // The DOMAIN label is what a resolver is keyed on -- what the edge
      // MEANS -- not the engine's structural type.
      assert.equal(as, "carrier");
      assert.equal(rowId, "carrier:c1");
      return "Northern Freight";
    }),
  );
  assert.match(resolved, /Northern Freight/);
  assert.match(resolved, /vk-cell-ref/);
  // The id stays reachable: a resolved label is friendlier, and the id is
  // what somebody debugging needs.
  assert.match(resolved, /title="carrier:c1"/);
  assert.match(resolved, /data-vk-ref-concept="v9:madeup:carrier"/);
});

test("a resolver that returns an empty label falls back to the id rather than blanking", () => {
  // A target row whose display card resolves to nothing is a real state -- a
  // concept with no card and no name-shaped field. Rendering the empty string
  // would produce exactly the blank cell this rule exists to prevent.
  const out = renderToHtml(cellContent(ROW, field(profile(), "carrierId"), "reference", () => ""));
  assert.match(out, /carrier:c1/);
});

test("an absent value is an em dash, and it is labelled", () => {
  for (const name of ["dispatchedAt", "weightKg", "status", "insured", "carrierId"]) {
    const out = html(profile(), name, { id: "shipment:s2" });
    assert.match(out, /—/, `${name} rendered nothing for an absent value`);
    assert.match(out, /aria-label="no value"/);
  }
});

test("plain text stays a bare text node", () => {
  // The majority of every table. It needs no styling hook, and wrapping it
  // would change the markup of every cell in the product to carry an element
  // nothing selects on.
  assert.equal(html(profile(), "reference"), "AB-1");
});

// ---------------------------------------------------------------------------
// The relative clock
// ---------------------------------------------------------------------------

test("formatRelative is coarse, both-directional and pinned to a passed clock", () => {
  // `now` is a parameter rather than a call to Date.now() inside, because a
  // renderer that reads the clock is a renderer whose output cannot be
  // asserted on.
  const now = new Date("2026-08-26T12:00:00Z");
  const ago = (iso: string) => formatRelative(new Date(iso), now);

  assert.equal(ago("2026-08-26T11:59:40Z"), "just now");
  assert.equal(ago("2026-08-26T12:00:20Z"), "just now", "the near future is also just now");
  assert.equal(ago("2026-08-26T11:30:00Z"), "30 minutes ago");
  assert.equal(ago("2026-08-26T09:00:00Z"), "3 hours ago");
  assert.equal(ago("2026-08-24T12:00:00Z"), "2 days ago");
  assert.equal(ago("2026-08-25T12:00:00Z"), "1 day ago", "singular, not '1 days'");
  assert.equal(ago("2026-06-26T12:00:00Z"), "2 months ago");
  assert.equal(ago("2024-08-26T12:00:00Z"), "2 years ago");
  assert.equal(ago("2026-08-28T12:00:00Z"), "in 2 days");
});
