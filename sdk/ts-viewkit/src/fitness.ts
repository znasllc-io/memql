// Element fitness -- which UI elements can render a given concept, and with
// which of its fields.
//
// This is the contract the view system is built on. A VIEW is one or more
// ELEMENTS (a table, a calendar, a chart) bound to a concept's rows.
// Something has to decide which elements are even offerable for a concept,
// and that decision must be DATA, not a switch statement:
//
//   * predefined views need to pick an element for a concept nobody wrote
//     code for;
//   * user-composed views need to show a person the choices AND explain in
//     words why the calendar is greyed out for this concept;
//   * a concept declared tomorrow must work without a renderer change -- the
//     same rule that makes @displayCard worth having (see displayCard.ts).
//
// So each element publishes an ElementSpec: a list of REQUIREMENTS naming
// what it needs from a concept ("a datetime field to plot events on"), and
// fitElement() answers, for a concrete concept + row sample, whether the
// element fits, WHICH concrete field satisfied each requirement, and what is
// missing when it does not.
//
// WHY THE ROWS AND NOT A SCHEMA
//
// The wire's ConceptInfo (component/grpc/memql.proto) carries id, entity,
// description and the display card -- it does NOT carry per-field types.
// There is therefore no schema to match against at runtime, and inventing a
// second, client-side copy of the DSL's type table would be a second source
// of truth that drifts. Fitness is instead computed from a SAMPLE OF ROWS:
// what the rows actually carry is, for rendering purposes, exactly the
// question being asked. profileConcept() turns rows into a ConceptProfile;
// everything downstream reads the profile, never the rows.
//
// This composes with the display-card fallback contract rather than
// duplicating it: a profile's `card` is resolveDisplayCard()'s answer, and a
// requirement expresses "prefer whatever the concept declared as its status
// slot" as `prefer: ["status"]` rather than re-deriving the slot itself.
//
// HOW A MATCH IS EXPRESSED
//
//   verdict "full"    every requirement bound, including the optional ones.
//   verdict "partial" every REQUIRED requirement bound, some optional one
//                     not -- usable but degraded, and `reasons` says how.
//   verdict "unfit"   a required requirement could not be bound (or the row
//                     sample is smaller than the element's minimum).
//
// `bindings` is the answer to "the calendar needs to know WHICH date field to
// plot": slot name -> the concrete field names chosen, in order. A slot binds
// a LIST because some slots are inherently plural (a table's columns); the
// single-value case is a one-element list, read via boundField().
//
// See docs/public/concepts/view-elements.md.

import { resolveDisplayCard } from "./displayCard.js";
import type { ConceptLike, DisplayCardHints, RowLike } from "./types.js";
import type { VNode } from "./vnode.js";

// ---------------------------------------------------------------------------
// Field kinds
// ---------------------------------------------------------------------------

// The kinds a renderer can meaningfully distinguish. Deliberately coarse:
// this is "what shape of thing is in this cell", not the DSL's type table.
// A DSL `enum(...)` is a `text` field whose `distinct` count happens to be
// small -- see FieldProfile.distinct, which is what a grouped board actually
// needs to know.
export type FieldKind = "text" | "number" | "boolean" | "datetime" | "list" | "object";

export const FIELD_KINDS: readonly FieldKind[] = [
  "text",
  "number",
  "boolean",
  "datetime",
  "list",
  "object",
];

// Kind precedence, used only to break a tie when a field carries different
// kinds in different rows (a nullable datetime that is "" in one row). The
// more specific kind wins: a field that parses as a timestamp anywhere is
// more useful as a timestamp than as text.
const KIND_PRECEDENCE: readonly FieldKind[] = [
  "datetime",
  "number",
  "boolean",
  "text",
  "list",
  "object",
];

// An ISO-8601 date or instant. Deliberately strict: `Date.parse` alone
// accepts "Sofia" in some engines and every free-text field would become a
// datetime candidate.
const ISO_DATE = /^\d{4}-\d{2}-\d{2}([T ]\d{2}:\d{2}(:\d{2}(\.\d+)?)?(Z|[+-]\d{2}:?\d{2})?)?$/;

export function isIsoDateString(value: string): boolean {
  return ISO_DATE.test(value) && !Number.isNaN(Date.parse(value));
}

function kindOf(value: unknown): FieldKind | undefined {
  if (value === null || value === undefined) return undefined;
  if (typeof value === "boolean") return "boolean";
  if (typeof value === "number") return Number.isFinite(value) ? "number" : undefined;
  if (typeof value === "string") {
    if (value === "") return undefined;
    return isIsoDateString(value) ? "datetime" : "text";
  }
  if (Array.isArray(value)) return "list";
  if (typeof value === "object") return "object";
  return undefined;
}

// Fields that are row plumbing rather than data. They are still PROFILED (a
// table may legitimately want an id column, a timeline may plot createdAt),
// but the automatic candidate scan skips them: an element that silently
// picked `concept` as its label would be worse than one that reported no fit.
// A display-card slot, a preferred name, or an explicit override still binds
// them -- an author who declared `primary="id"` meant it.
export const NON_DISPLAY_FIELDS: readonly string[] = [
  "id",
  "concept",
  "type",
  "schema",
  "partition",
];

// Above this many distinct values a text field is free text, not a category.
// A grouped board with forty columns is not a board.
export const CATEGORICAL_MAX_DISTINCT = 12;

// Distinct-value counting stops here. A profile is built on every render, and
// the only question asked of the count is "is this small"; walking a
// ten-thousand-row set to learn the answer is 9,988 wasted comparisons.
const DISTINCT_SCAN_CAP = 64;

export interface FieldProfile {
  readonly field: string;
  readonly kind: FieldKind;
  // Rows carrying a non-empty value for this field.
  readonly present: number;
  // Distinct stringified values observed, capped at DISTINCT_SCAN_CAP. A
  // value equal to the cap means "at least this many".
  readonly distinct: number;
  // Which @displayCard slots (declared or inferred) name this field.
  readonly slots: readonly DisplaySlotName[];
}

export type DisplaySlotName = "primary" | "secondary" | "tertiary" | "status";

const DISPLAY_SLOT_NAMES: readonly DisplaySlotName[] = [
  "primary",
  "secondary",
  "tertiary",
  "status",
];

export interface ConceptProfile {
  readonly concept: ConceptLike;
  readonly rowCount: number;
  // The display card in force for this row set -- declared verbatim, else
  // inferred. Requirements name slots through it rather than re-deriving.
  readonly card: DisplayCardHints;
  // Field order is first-seen key order across the sample: stable for a
  // stable row set, and it matches the order a table would naturally use.
  readonly fields: readonly FieldProfile[];
}

// profileConcept turns a concept plus a row sample into the inspectable
// description everything else matches against.
export function profileConcept(
  concept: ConceptLike,
  rows: readonly RowLike[],
): ConceptProfile {
  const card = resolveDisplayCard(concept, rows);
  const slotsByField = new Map<string, DisplaySlotName[]>();
  for (const slot of DISPLAY_SLOT_NAMES) {
    const field = card[slot];
    if (!field) continue;
    const list = slotsByField.get(field) ?? [];
    list.push(slot);
    slotsByField.set(field, list);
  }

  const order: string[] = [];
  const counts = new Map<string, Map<FieldKind, number>>();
  const present = new Map<string, number>();
  const values = new Map<string, Set<string>>();

  for (const row of rows) {
    for (const [field, raw] of Object.entries(row)) {
      const kind = kindOf(raw);
      if (kind === undefined) continue;
      if (!counts.has(field)) {
        order.push(field);
        counts.set(field, new Map());
        values.set(field, new Set());
      }
      const byKind = counts.get(field)!;
      byKind.set(kind, (byKind.get(kind) ?? 0) + 1);
      present.set(field, (present.get(field) ?? 0) + 1);
      const seen = values.get(field)!;
      if (seen.size < DISTINCT_SCAN_CAP && (kind !== "list" && kind !== "object")) {
        seen.add(String(raw));
      }
    }
  }

  const fields: FieldProfile[] = order.map((field) => {
    const byKind = counts.get(field)!;
    let best: FieldKind = "text";
    let bestCount = -1;
    for (const kind of KIND_PRECEDENCE) {
      const n = byKind.get(kind) ?? 0;
      if (n > bestCount) {
        best = kind;
        bestCount = n;
      }
    }
    return {
      field,
      kind: best,
      present: present.get(field) ?? 0,
      distinct: values.get(field)!.size,
      slots: slotsByField.get(field) ?? [],
    };
  });

  return { concept, rowCount: rows.length, card, fields };
}

// ---------------------------------------------------------------------------
// Requirements
// ---------------------------------------------------------------------------

// One thing an element needs from a concept.
//
// `slot` is the element's own name for the role ("start", "value", "group"),
// NOT a field name -- the whole point is that the field is discovered.
export interface ElementRequirement {
  readonly slot: string;
  // A noun phrase completing "<element> uses this for ___". Used verbatim in
  // the human-readable explanation, so write it as prose, not as a label.
  readonly description: string;
  readonly kinds: readonly FieldKind[];
  // How many fields this slot binds. min 0 makes the slot OPTIONAL: unbound
  // degrades the element to a "partial" fit rather than ruling it out.
  // max "all" binds every remaining candidate (a table's columns).
  readonly min?: number;
  readonly max?: number | "all";
  // Cap on what the AUTOMATIC resolution (steps 2-4) may bind, when that is
  // lower than `max`. A multi-series line chart accepts several numeric
  // fields, but choosing them on its own would plot a call count and an error
  // rate against one axis; auto-binding one and leaving the rest to an
  // explicit override keeps that comparison a deliberate act.
  readonly autoMax?: number;
  // Display-card slots to try first, in order. This is how an element says
  // "whatever the concept calls its status" without knowing the concept.
  readonly prefer?: readonly DisplaySlotName[];
  // Field-name families to try next, in order. Names, never concept ids --
  // the same rule the display-card fallback lists follow.
  readonly preferNames?: readonly string[];
  // Cap on distinct values; a text field above it is free text, not a
  // category. Defaults to unlimited.
  readonly distinctMax?: number;
  // Among otherwise-equal automatic candidates, prefer the one with the
  // fewest distinct values. For grouping slots this is the difference
  // between columns-by-status and columns-by-title.
  readonly preferFewestDistinct?: boolean;
  // Bind only from an override, a display-card slot or a preferred name --
  // never from the generic scan. For a semantically loaded slot (is-this-an-
  // all-day-event, is-this-a-latitude) any type-compatible field will "fit"
  // and almost all of them are wrong; a wrong binding there is worse than a
  // reported gap, because the render is confidently incorrect.
  readonly explicitOnly?: boolean;
  // What the element loses when an optional slot goes unbound. Shown to a
  // person choosing elements, so it says the consequence, not the cause.
  readonly degraded?: string;
}

// Which QUESTION about a population an element answers. Three, because a
// person arriving at an unfamiliar row set asks the same three in the same
// order -- how many are there, how does that divide, which ones specifically
// -- and the predefined views are already built on exactly that grammar (see
// clients/portal/src/views/registry.ts and docs/public/concepts/
// view-elements.md section 7).
//
// It lives on the ELEMENT rather than in whatever is laying a page out, for
// the same reason a requirement does: an arrangement built from a list held
// by the composer would have to be edited every time an element is added,
// and the element added would be the one nobody remembered to list. Declared
// here, a new element takes its place in a composed view -- and in the
// deterministic proposal -- on the day it is written.
export type BandRole = "reading" | "shape" | "roll";

export const BAND_ROLES: readonly BandRole[] = ["reading", "shape", "roll"];

// The question each band answers, in the words a person would use. Shown as
// the band caption in a composer, so it is prose rather than a label.
export const BAND_QUESTIONS: Readonly<Record<BandRole, string>> = {
  reading: "How many are there?",
  shape: "How does that divide?",
  roll: "Which ones, specifically?",
};

export interface ElementSpec {
  readonly id: string;
  readonly title: string;
  // One sentence for a picker: what this element shows.
  readonly summary: string;
  // Which band this element belongs in. Omitted means "roll": an element
  // that has not thought about it enumerates rows, which is what all but a
  // handful of them do.
  readonly band?: BandRole;
  readonly requires: readonly ElementRequirement[];
  // Below this many rows the element has nothing to show (a detail pane needs
  // one row; a chart of one point is a stat tile). Defaults to 1.
  readonly minRows?: number;
  readonly render: ElementRenderer;
}

export interface ElementOptions {
  readonly selectedRowId?: string;
  // Overrides the automatic binding for one or more slots. This is what a
  // user-composed view writes when a person says "plot dueAt, not createdAt".
  //
  // NAMING A SLOT HERE SETTLES IT. The automatic resolution (steps 2-4 of
  // fitElement) runs only for slots the caller did NOT speak about, so an
  // EMPTY list is the way to say "this slot binds nothing" -- which is how a
  // predefined view asks the stat strip for a row count and no summed
  // measures (memql#3319: `revocationEpoch total` is a true number and a
  // meaningless one). It is also why a misspelled field name reports the slot
  // unmet instead of quietly substituting whatever the scan liked next.
  readonly bindings?: Readonly<Record<string, string | readonly string[]>>;
  // Charts need a concrete light/dark answer for their series palette; every
  // other element is theme-neutral. Omitted means "follow the host", which
  // resolves through prefers-color-scheme.
  readonly theme?: "light" | "dark";
  readonly sort?: { readonly field: string; readonly direction: "asc" | "desc" };
  // Calendar month to render, "YYYY-MM". Omitted picks the month of the
  // earliest event.
  readonly month?: string;
}

export interface ElementRenderInput {
  readonly rows: readonly RowLike[];
  readonly concept: ConceptLike;
  readonly fit: ElementFit;
  readonly options?: ElementOptions;
}

export type ElementRenderer = (input: ElementRenderInput) => VNode;

// ---------------------------------------------------------------------------
// Fit
// ---------------------------------------------------------------------------

export type FitVerdict = "full" | "partial" | "unfit";

export interface UnmetRequirement {
  readonly slot: string;
  readonly required: boolean;
  // Why nothing bound, in words.
  readonly reason: string;
}

export interface ElementFit {
  readonly element: string;
  readonly verdict: FitVerdict;
  // 0..1. Ranks fitting elements; it does NOT decide fitness (the verdict
  // does). Required slots weigh 1, optional slots 0.5.
  readonly score: number;
  // slot -> the concrete fields chosen, in the order the element should use
  // them. A slot that bound nothing is absent, never an empty array.
  readonly bindings: Readonly<Record<string, readonly string[]>>;
  readonly unmet: readonly UnmetRequirement[];
}

// boundField reads a single-valued slot. Returns undefined for an unbound
// optional slot, which every renderer must handle -- a partial fit is a
// supported state, not an error.
export function boundField(fit: ElementFit, slot: string): string | undefined {
  return fit.bindings[slot]?.[0];
}

export function boundFields(fit: ElementFit, slot: string): readonly string[] {
  return fit.bindings[slot] ?? [];
}

// elementBand reads an element's declared band, applying the documented
// default. One implementation so a composer, a picker and a proposal cannot
// each decide differently what an undeclared element is.
export function elementBand(element: ElementSpec): BandRole {
  return element.band ?? "roll";
}

function asArray(value: string | readonly string[]): readonly string[] {
  return typeof value === "string" ? [value] : value;
}

function kindLabel(kinds: readonly FieldKind[]): string {
  if (kinds.length === 1) return `a ${kinds[0]} field`;
  return `a ${kinds.slice(0, -1).join(", ")} or ${kinds[kinds.length - 1]} field`;
}

// fitElement resolves one element against one concept profile.
//
// THE RESOLUTION ORDER, which is the part a consumer needs to be able to
// predict. For each requirement, in declaration order, over the fields not
// already bound to an earlier slot of the same element:
//
//   1. an explicit override from options.bindings
//   2. the display-card slots named in `prefer`, in order
//   3. the field names in `preferNames`, in order
//   4. every remaining kind-compatible candidate, skipping NON_DISPLAY_FIELDS,
//      ordered by fewest-distinct (when the requirement asks) and then by the
//      profile's field order -- skipped entirely for an `explicitOnly` slot
//
// STEPS 2-4 ARE FOR SLOTS THE CALLER DID NOT DECIDE. If options.bindings names
// the slot at all -- even with an empty list, even with a field that does not
// exist -- the automatic resolution is skipped for it. A caller who spoke
// about a slot and got a field they did not name would have no way to tell;
// an unbound slot with a stated reason is the honest report, and an empty list
// is how a view asks for nothing at all (see ElementOptions.bindings).
//
// Steps 1-3 bypass the NON_DISPLAY_FIELDS skip: naming a field explicitly, or
// declaring it as the display card's primary, is a decision already made.
//
// Requirements are resolved in declaration order and a field binds at most
// once per element, so an element that needs two datetimes must declare the
// more specific one first (calendar: `start` before `end`).
export function fitElement(
  element: ElementSpec,
  profile: ConceptProfile,
  options?: ElementOptions,
): ElementFit {
  const byName = new Map(profile.fields.map((f) => [f.field, f]));
  const taken = new Set<string>();
  const bindings: Record<string, readonly string[]> = {};
  const unmet: UnmetRequirement[] = [];
  let earned = 0;
  let total = 0;

  const minRows = element.minRows ?? 1;
  if (profile.rowCount < minRows) {
    return {
      element: element.id,
      verdict: "unfit",
      score: 0,
      bindings: {},
      unmet: [
        {
          slot: "*",
          required: true,
          reason: `needs at least ${minRows} row${minRows === 1 ? "" : "s"}, the set has ${profile.rowCount}`,
        },
      ],
    };
  }

  for (const req of element.requires) {
    const min = req.min ?? 1;
    const max = req.max ?? 1;
    const weight = min > 0 ? 1 : 0.5;
    total += weight;

    const accepts = (f: FieldProfile | undefined, explicit: boolean): boolean => {
      if (!f) return false;
      if (taken.has(f.field)) return false;
      if (!req.kinds.includes(f.kind)) return false;
      if (req.distinctMax !== undefined && f.distinct > req.distinctMax) return false;
      if (!explicit && NON_DISPLAY_FIELDS.includes(f.field)) return false;
      return true;
    };

    const chosen: string[] = [];
    const take = (field: string): void => {
      if (chosen.includes(field)) return;
      chosen.push(field);
      taken.add(field);
    };
    const room = (): boolean => max === "all" || chosen.length < max;
    // Steps 2-4 stop at autoMax; an explicit override (step 1) may fill max.
    const autoRoom = (): boolean =>
      room() && (req.autoMax === undefined || chosen.length < req.autoMax);

    // 1. explicit override
    const override = options?.bindings?.[req.slot];
    // Whether the caller SPOKE about this slot, which is not the same as
    // whether anything bound: `[]` and a misspelled name both settle the slot.
    const decided = override !== undefined;
    if (override !== undefined) {
      for (const field of asArray(override)) {
        if (!room()) break;
        // An override names a field the caller chose. It is honoured whenever
        // the field exists at all: refusing it on a kind mismatch would leave
        // a person who picked a column with no way to see why nothing changed.
        if (byName.has(field) && !taken.has(field)) take(field);
      }
    }

    // 2. display-card slots
    for (const slot of req.prefer ?? []) {
      if (decided || !autoRoom()) break;
      const field = profile.card[slot];
      if (field && accepts(byName.get(field), true)) take(field);
    }

    // 3. preferred field names
    for (const name of req.preferNames ?? []) {
      if (decided || !autoRoom()) break;
      if (accepts(byName.get(name), true)) take(name);
    }

    // 4. the remaining candidates
    if (!decided && autoRoom() && !req.explicitOnly) {
      const rest = profile.fields.filter((f) => accepts(f, false));
      if (req.preferFewestDistinct) {
        // Stable: sort() is stable in every engine this package supports, so
        // ties keep field order.
        rest.sort((a, b) => a.distinct - b.distinct);
      }
      for (const f of rest) {
        if (!autoRoom()) break;
        take(f.field);
      }
    }

    if (chosen.length >= Math.max(min, 1)) {
      bindings[req.slot] = chosen;
      earned += weight;
    } else if (chosen.length > 0) {
      bindings[req.slot] = chosen;
      unmet.push({
        slot: req.slot,
        required: min > 0,
        reason: `needs ${min} ${kindLabel(req.kinds)}s, found ${chosen.length}`,
      });
    } else {
      const named = (req.preferNames ?? []).slice(0, 3).join(", ");
      unmet.push({
        slot: req.slot,
        required: min > 0,
        reason: decided
          ? "the caller bound no field to it"
          : req.explicitOnly
          ? `no field the concept points at for it${named ? `, and none named ${named}` : ""}`
          : `no unused ${kindLabel(req.kinds)}${
              req.distinctMax !== undefined
                ? ` with at most ${req.distinctMax} distinct values`
                : ""
            }`,
      });
    }
  }

  const missingRequired = unmet.some((u) => u.required);
  const verdict: FitVerdict = missingRequired
    ? "unfit"
    : unmet.length > 0
      ? "partial"
      : "full";

  return {
    element: element.id,
    verdict,
    score: total === 0 ? 1 : Math.round((earned / total) * 1000) / 1000,
    bindings,
    unmet,
  };
}

// fitElements ranks a whole element library against one concept, best first.
//
// Ordering: full before partial before unfit; then by score; then by
// SPECIFICITY (the count of required slots), so the universal fallbacks --
// a row list requires nothing of a concept and therefore always fits -- sort
// below an element that actually engaged with the concept's shape; then by
// the order the library declared them, so the result is deterministic.
export function fitElements(
  elements: readonly ElementSpec[],
  profile: ConceptProfile,
  options?: ElementOptions,
): readonly ElementFit[] {
  const rank: Record<FitVerdict, number> = { full: 0, partial: 1, unfit: 2 };
  const specificity = new Map(
    elements.map((e) => [e.id, e.requires.filter((r) => (r.min ?? 1) > 0).length]),
  );
  const order = new Map(elements.map((e, i) => [e.id, i]));
  return elements
    .map((element) => fitElement(element, profile, options))
    .sort((a, b) => {
      if (rank[a.verdict] !== rank[b.verdict]) return rank[a.verdict] - rank[b.verdict];
      if (a.score !== b.score) return b.score - a.score;
      const sa = specificity.get(a.element) ?? 0;
      const sb = specificity.get(b.element) ?? 0;
      if (sa !== sb) return sb - sa;
      return (order.get(a.element) ?? 0) - (order.get(b.element) ?? 0);
    });
}

// explainFit renders a fit as prose. This exists because a user-composed view
// has to answer "why can't I pick the calendar for this?" in a sentence, and
// every consumer inventing that sentence from `unmet` would produce a
// different one. The wording is built from the requirement descriptions the
// element author wrote, so it stays honest as elements change.
export function explainFit(
  element: ElementSpec,
  fit: ElementFit,
  profile: ConceptProfile,
): string {
  const entity = profile.concept.entity;
  const sentences: string[] = [];

  if (fit.verdict === "unfit") {
    const blockers = fit.unmet.filter((u) => u.required);
    sentences.push(`${element.title} cannot render ${entity}.`);
    for (const u of blockers) {
      const req = element.requires.find((r) => r.slot === u.slot);
      sentences.push(
        req
          ? `It needs ${req.description}, and there is ${u.reason}.`
          : `It ${u.reason}.`,
      );
    }
    return sentences.join(" ");
  }

  sentences.push(
    fit.verdict === "full"
      ? `${element.title} fits ${entity}.`
      : `${element.title} fits ${entity}, with limits.`,
  );
  for (const req of element.requires) {
    const fields = boundFields(fit, req.slot);
    if (fields.length === 0) continue;
    sentences.push(`It uses ${fields.join(", ")} for ${req.description}.`);
  }
  for (const u of fit.unmet) {
    const req = element.requires.find((r) => r.slot === u.slot);
    if (!req) continue;
    sentences.push(
      `Nothing supplies ${req.description} (${u.reason})` +
        (req.degraded ? `, so ${req.degraded}.` : "."),
    );
  }
  return sentences.join(" ");
}

// renderElement is the one call a view runtime makes: profile, fit, render.
// Returns undefined when the element does not fit, so a caller cannot
// accidentally render an element against a concept it reported unfit for.
export function renderElement(
  element: ElementSpec,
  rows: readonly RowLike[],
  concept: ConceptLike,
  options?: ElementOptions,
): VNode | undefined {
  const fit = fitElement(element, profileConcept(concept, rows), options);
  if (fit.verdict === "unfit") return undefined;
  return element.render({ rows, concept, fit, options });
}
