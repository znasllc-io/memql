// How one VALUE reads (epic memql#4661, task memql#4665).
//
// ===========================================================================
// WHY THIS IS ONE MODULE AND NOT A RULE PER ELEMENT
// ===========================================================================
// Before this, a table cell was `scalarText(row, field)` -- one function that
// turns anything into a string -- so a timestamp read as "2026-08-01T06:00:00Z",
// a boolean read as the word "true", and an enum read as a bare lowercase
// token with no indication it was one of a closed set. Each of those is a
// small ugliness on its own and together they are why a composed view "has no
// personality": it is the same data the designed pages show, shown worse.
//
// The rules live HERE, inside view-kit, rather than in the portal, because the
// point of the element library is that every consumer improves at once. A
// humanized timestamp implemented in the portal's table would leave the VS Code
// panel, the concept browser and every composed view reading raw ISO strings.
//
// ===========================================================================
// WHAT DECIDES A RULE: THE PROFILE, NOT THE VALUE
// ===========================================================================
// Every rule below keys on the FieldProfile -- the declared kind where the
// concept published one, the observed kind otherwise -- and never on what the
// particular value happens to look like. Sniffing values means one row's cell
// is a pill and the next row's is text, which is worse than either.
//
// ===========================================================================
// NEVER BLANK
// ===========================================================================
// A cell that cannot be rendered well renders its value plainly. It does not
// render empty: an empty cell says "this row has no value here", which is a
// different and usually false statement. The one thing that renders as empty
// is an actually-absent value, and it renders as an em dash so the difference
// is visible.

import { h, text, type VNode } from "./vnode.js";
import { statusText, statusValue } from "./displayCard.js";
import {
  formatCompact,
  formatDateTime,
  formatRelative,
  isMissing,
  scalarText,
} from "./format.js";
import { NON_DISPLAY_FIELDS, type ConceptProfile, type FieldProfile } from "./fitness.js";
import type { RowLike } from "./types.js";

// How many columns a table offers before it stops offering more.
//
// A cap rather than "all the declared fields" because the schema-first profile
// made the field list COMPLETE, and complete is not the same as useful: a
// concept with forty declared fields would render a forty-column table nobody
// can read, where the row-sampled profile used to show the dozen the loaded
// rows happened to carry. Twelve is the number at which a table still fits a
// laptop without horizontal scroll at a readable size.
//
// It is a cap on the AUTOMATIC choice only. A person who names more columns in
// a binding gets them all -- naming a slot settles it, which is the fitness
// contract, and second-guessing an explicit choice is what this cap must never
// do.
export const DISPLAY_COLUMN_CAP = 12;

// displayColumns is the schema-driven answer to "which fields does a table or
// a card show", in order.
//
// THE ORDER IS AN ARGUMENT, not a preference:
//
//   1. the @displayCard slots, in slot order. Whatever the concept calls
//      itself belongs at the left edge -- that is what a display card IS.
//   2. the REQUIRED fields. A field the schema insists every row carries is a
//      field the author considered part of the thing; an optional one is
//      elaboration. This is the distinction the row-sampled profile could not
//      see at all, and it is the main thing schema-first buys a table.
//   3. everything else declared, in declaration order.
//
// Row plumbing (NON_DISPLAY_FIELDS) is skipped throughout unless a display
// card names it -- an author who declared `primary="id"` meant it.
export function displayColumns(
  profile: ConceptProfile,
  cap: number = DISPLAY_COLUMN_CAP,
): readonly FieldProfile[] {
  const byName = new Map(profile.fields.map((f) => [f.field, f]));
  const out: FieldProfile[] = [];
  const taken = new Set<string>();

  const push = (field: FieldProfile | undefined): void => {
    if (field === undefined || taken.has(field.field)) return;
    taken.add(field.field);
    out.push(field);
  };

  for (const slot of ["primary", "secondary", "tertiary", "status"] as const) {
    const named = profile.card[slot];
    if (named !== undefined) push(byName.get(named));
  }

  const rest = profile.fields.filter(
    (f) => !taken.has(f.field) && !NON_DISPLAY_FIELDS.includes(f.field) && renderable(f),
  );
  for (const field of rest) if (field.required) push(field);
  for (const field of rest) push(field);

  return out.slice(0, Math.max(cap, 0));
}

// A field a cell can render at all. Lists and objects are excluded: a nested
// block in a table column is either "[object Object]" or a wall of JSON, and
// the honest place to read one is the row detail, which shows the whole shape.
function renderable(field: FieldProfile): boolean {
  return field.kind !== "list" && field.kind !== "object";
}

// The CELL KIND -- what rule applies. Derived from the profile once per column
// rather than per cell, since it cannot vary between rows of one field.
export type CellKind = "text" | "id" | "reference" | "number" | "boolean" | "enum" | "datetime";

export function cellKind(field: FieldProfile): CellKind {
  // A relationship's pointer field is a REFERENCE before it is a string: it
  // holds another row's id, and rendering it as prose would left-align a
  // token nobody reads as words. Until lookups resolve it (memql#4671) it
  // renders as the id in the data voice, which is never blank and never a lie.
  if (field.relationship !== undefined) return "reference";
  if (field.declaredKind === "enum" || (field.kind === "text" && field.enumValues.length > 0)) {
    return "enum";
  }
  switch (field.kind) {
    case "datetime":
      return "datetime";
    case "number":
      return "number";
    case "boolean":
      return "boolean";
    default:
      // `id` and the row intrinsics that hold ids. A table may legitimately
      // carry an id column, and when it does the value should read as data.
      return field.field === "id" || field.field.endsWith("Id") ? "id" : "text";
  }
}

// cellAttrs is the per-cell attribute set a host puts on the container (a
// `<td>`, a card line). Separate from cellContent so a table can put the
// alignment hint on the cell and the content inside it.
export function cellAttrs(kind: CellKind): Readonly<Record<string, string>> {
  // Right-aligned mono numerals: a column of numbers is compared by reading
  // down it, and that only works when the digits line up.
  return kind === "number" ? { "data-vk-cell": "number" } : {};
}

// cellContent renders one value under one rule.
// A resolver a host supplies so a REFERENCE cell can render the row it points
// at rather than the id (epic memql#4661, task memql#4671).
//
// A FUNCTION rather than a map of pre-resolved values: the host owns the
// batching, the cache and the network, and view-kit must not learn about any
// of them. It answers `undefined` for "not resolved", which is not a failure
// -- it is the state a cell is in for the first paint, and the honest
// rendering of it is the id.
export type RefResolver = (
  relationshipAs: string,
  rowId: string,
  targetField: string,
) => string | undefined;

export function cellContent(
  row: RowLike,
  field: FieldProfile,
  kind: CellKind,
  resolve?: RefResolver,
): VNode {
  const raw = row[field.field];

  // ABSENT IS ABSENT, and it looks different from empty. A blank cell is
  // ambiguous between "no value" and "the renderer gave up"; an em dash is
  // only ever the first.
  if (isMissing(raw)) {
    return h("span", { class: "vk-cell-absent", "aria-label": "no value" }, [text("—")]);
  }

  switch (kind) {
    case "datetime":
      return datetimeCell(raw);
    case "number":
      return numberCell(raw);
    case "boolean":
      return booleanCell(field.field, raw);
    case "enum":
      return enumCell(raw);
    case "reference":
      return referenceCell(field, raw, resolve);
    case "id":
      return idCell(raw);
    default:
      // A BARE TEXT NODE, not a wrapped span. Plain text is the majority of
      // every table, it needs no styling hook, and wrapping it would change
      // the markup of every cell in the product to carry an element nothing
      // selects on.
      return text(scalarText(row, field.field));
  }
}

// A datetime reads as elapsed time, with the exact instant available.
//
// THE EXACT VALUE IS NOT HOVER-ONLY. `title` is unreachable on a touch device,
// so the instant also rides `datetime` (machine-readable, and what a screen
// reader and a copy-paste get) and the row detail shows the raw value
// unchanged. "2 days ago" is the answer to the question people actually ask of
// a timestamp; the instant is the answer to the one they ask second.
function datetimeCell(raw: unknown): VNode {
  const iso = typeof raw === "string" ? raw : "";
  const ms = Date.parse(iso);
  if (Number.isNaN(ms)) {
    // A datetime field carrying something unparseable renders what it holds
    // rather than an em dash: "the value is wrong" and "there is no value"
    // are different problems and the first one needs to be visible.
    return h("span", { class: "vk-cell-data" }, [text(iso || String(raw))]);
  }
  const exact = formatDateTime(new Date(ms));
  return h(
    "time",
    { class: "vk-cell-time", datetime: iso, title: `${exact} UTC` },
    [text(formatRelative(new Date(ms)))],
  );
}

function numberCell(raw: unknown): VNode {
  const n = typeof raw === "number" ? raw : Number(raw);
  if (!Number.isFinite(n)) {
    return h("span", { class: "vk-cell-data" }, [text(String(raw))]);
  }
  // Compact above a thousand, with the exact figure on hover: a column of
  // "1.2k" is readable and a column of "1,247,932" is a wall. Below a
  // thousand compact and exact are the same string, so nothing is lost.
  return h("span", { class: "vk-cell-number", title: String(n) }, [text(formatCompact(n))]);
}

// A boolean is not a label. The label is the FIELD NAME and the value only
// decides whether it is asserted or negated -- memql#3303's rule, applied to
// every boolean rather than only to the display card's status slot.
function booleanCell(field: string, raw: unknown): VNode {
  const value = raw === true || raw === "true";
  return h("span", { class: "vk-cell-bool", "data-value": String(value) }, [
    h("span", { class: "vk-cell-dot" }, []),
    text(statusText(field, value)),
  ]);
}

// An enum is one of a closed set, and a pill is how a reader sees that at a
// glance. `data-status` carries the raw member so a host colours it without
// parsing prose -- the same split the row list's status badge makes.
function enumCell(raw: unknown): VNode {
  const value = statusValue(raw);
  return h("span", { class: "vk-cell-pill", "data-status": value }, [text(value)]);
}

// An id reads as DATA: monospace, tight, not prose. It is also the fallback
// for an unresolvable reference, which is the rule memql#4671 builds on -- a
// lookup that cannot resolve shows the id, never a blank.
function idCell(raw: unknown): VNode {
  return h("span", { class: "vk-cell-data" }, [text(statusValue(raw) || String(raw))]);
}

// A reference: the target row's own label where it resolved, the id where it
// did not.
//
// NEVER BLANK, and that is the whole contract of this cell. There are four
// ways a lookup fails to resolve -- the target row was deleted, the caller's
// row authz does not admit it, the relationship names a concept this cluster
// no longer publishes, or the read simply has not landed yet -- and a cell
// cannot tell them apart. What it CAN do is show the id, which is true in all
// four cases and is something a person can paste into a query. A blank cell
// would say "this row has no value here", which is false in all four.
//
// The resolved value is linked to the target's row detail through the same
// `data-row-id` contract every element uses, so a host that already wires row
// selection gets click-through for free.
function referenceCell(
  field: FieldProfile,
  raw: unknown,
  resolve: RefResolver | undefined,
): VNode {
  const id = statusValue(raw) || String(raw);
  const rel = field.relationship;
  const label =
    resolve === undefined || rel === undefined
      ? undefined
      : resolve(rel.as ?? rel.field, id, "");
  if (label === undefined || label === "") return idCell(raw);
  return h(
    "span",
    {
      class: "vk-cell-ref",
      "data-vk-ref-id": id,
      "data-vk-ref-concept": rel?.target ?? "",
      // The id stays reachable on hover: a resolved label is friendlier and
      // the id is what somebody debugging needs.
      title: id,
    },
    [text(label)],
  );
}
