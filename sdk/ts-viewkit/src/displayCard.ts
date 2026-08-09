// The display-card fallback contract.
//
// A concept that declares `@displayCard(...)` tells the view system which of
// its fields to put in a row's chrome. Most concepts declare one. Some
// deliberately do not -- when every field is a foreign key, a hash, a
// timestamp or a blob, a forced primary slot says nothing the row id does not
// already say (those concepts carry a `// @no-displayCard: <reason>` marker in
// the DSL, and test/dslconformance/displaycard_inventory_test.go makes the
// choice mandatory).
//
// This file is what happens next. "Degrade to the row id" was correct but
// minimal, and -- worse -- it was EMERGENT: it was whatever `card?.primary`
// being undefined happened to produce. A view system needs the undeclared
// case to be a stated rule, so that a concept author can predict how their
// concept renders before writing a single annotation, and so that two
// consumers cannot answer the question differently.
//
// THE CONTRACT
//
//   1. A concept that declares a card is rendered through EXACTLY that card.
//      Inference never runs, never fills an omitted slot, never overrides a
//      declared one. An author who declared `primary` and no `status` chose
//      to have no status badge; silently inventing one would make the
//      annotation mean less than it says.
//
//   2. A concept that declares NO card is rendered through an INFERRED card,
//      derived only from FIELD NAMES on the rows -- never from the concept's
//      identity. The candidate lists below are the whole rule:
//
//        primary   first of PRIMARY_NAME_FIELDS any row carries as a
//                  non-empty scalar; otherwise the row id.
//        secondary first of SECONDARY_NAME_FIELDS, or nothing.
//        tertiary  never inferred. There is no honest generic answer for a
//                  third slot, and inventing one adds noise to every
//                  undeclared concept in the tree.
//        status    first of STATUS_NAME_FIELDS, or nothing.
//
//   3. Inference is resolved ONCE per row set, not per row. Deriving per row
//      would let two rows of the same concept label themselves off different
//      fields, so a list would read as a jagged mix of names and ids.
//
//   4. The candidate lists are FIELD NAMES, not concept names. Adding a
//      concept-specific entry here would be the concept-specific renderer
//      code this package exists to not have.
//
// See docs/public/concepts/display-cards.md.

import type { ConceptLike, DisplayCardHints, RowLike } from "./types.js";

// "What is this row called." Ordered: the earlier name wins when a row
// carries several.
export const PRIMARY_NAME_FIELDS = [
  "name",
  "title",
  "label",
  "displayName",
  "slug",
] as const;

// "What is this row, in a few words." Deliberately short -- a supporting slot
// that guesses wrong is worse than an absent one.
export const SECONDARY_NAME_FIELDS = ["description", "summary", "subtitle"] as const;

// "Where is this row in its lifecycle." Covers the enum spellings and the
// boolean flags; a boolean is rendered through statusText below rather than
// as the literal "true".
export const STATUS_NAME_FIELDS = [
  "status",
  "state",
  "outcome",
  "verdict",
  "health",
  "active",
  "enabled",
  "done",
] as const;

// A scalar is what fits in a single cell of row chrome. Objects and arrays are
// excluded on purpose: the loader already rejects a DECLARED slot naming one
// (component/database/memory-nodes/concept_parser.go), and the inferred path
// must not be a way around that rule.
function isScalar(v: unknown): v is string | number | boolean {
  return typeof v === "string" || typeof v === "number" || typeof v === "boolean";
}

function hasRenderableValue(rows: readonly RowLike[], field: string): boolean {
  return rows.some((row) => {
    const v = row[field];
    return isScalar(v) && String(v) !== "";
  });
}

function firstPresent(
  rows: readonly RowLike[],
  candidates: readonly string[],
): string | undefined {
  for (const field of candidates) {
    if (hasRenderableValue(rows, field)) return field;
  }
  return undefined;
}

// inferDisplayCard applies rule 2 to a row set. Exported so a consumer that
// renders its own chrome (a detail header, a breadcrumb, a tab title) resolves
// the same slots the row list does instead of re-deriving them.
export function inferDisplayCard(rows: readonly RowLike[]): DisplayCardHints {
  const card: DisplayCardHints = {};
  const primary = firstPresent(rows, PRIMARY_NAME_FIELDS);
  if (primary) card.primary = primary;
  const secondary = firstPresent(rows, SECONDARY_NAME_FIELDS);
  if (secondary) card.secondary = secondary;
  const status = firstPresent(rows, STATUS_NAME_FIELDS);
  if (status) card.status = status;
  return card;
}

// resolveDisplayCard is rules 1 + 3: the declared card wins whole, and the
// inferred one is computed once for the whole set.
export function resolveDisplayCard(
  concept: ConceptLike,
  rows: readonly RowLike[],
): DisplayCardHints {
  return concept.displayCard ?? inferDisplayCard(rows);
}

// statusFieldLabel turns a field name into badge prose. The only transform is
// dropping an `is` / `has` predicate prefix (`isError` -> `error`), because a
// badge reading "not isError" is worse than the raw boolean it replaced. No
// other rewriting: a field name is the author's word for the thing, and
// prettifying it further would start guessing at English.
export function statusFieldLabel(field: string): string {
  const m = /^(?:is|has)([A-Z])(.*)$/.exec(field);
  if (!m) return field;
  return m[1].toLowerCase() + m[2];
}

// statusText is the answer to memql#3303: a boolean status slot used to render
// the literal "true" / "false", which is the value of a field whose NAME the
// reader cannot see. A boolean is not a label -- the label is the field name,
// and the value only decides whether it is asserted or negated. So
// `active: true` reads "active" and `active: false` reads "not active".
//
// Value-agnostic and concept-agnostic: no enumeration of known statuses, no
// per-concept mapping. Non-boolean values (the enum case) are passed through
// untouched -- an enum already IS a label.
export function statusText(field: string, value: unknown): string {
  if (typeof value === "boolean") {
    const label = statusFieldLabel(field);
    return value ? label : `not ${label}`;
  }
  if (typeof value === "string" || typeof value === "number") return String(value);
  return "";
}

// statusValue is the machine-readable half, emitted as `data-status` so a host
// can colour a badge without parsing its prose. It stays the raw stringified
// value ("true" / "false" / the enum member) on purpose: prose is for people,
// data attributes are for stylesheets, and collapsing the two would make every
// host's `[data-status="..."]` rule depend on the label transform above.
export function statusValue(value: unknown): string {
  if (typeof value === "string") return value;
  if (typeof value === "number" || typeof value === "boolean") return String(value);
  return "";
}
