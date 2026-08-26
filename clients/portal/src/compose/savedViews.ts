import type { CreateComposedViewArgs, Row } from "@znasllc-io/memql-sdk-core/client";
import { BAND_ROLES, type Arrangement, type ArrangedElement } from "@znasllc-io/memql-view-kit";

import { omitBlank } from "../cluster/args";
import { flattenForList } from "../viewkit/rows";

// A saved view, both directions: the row the cluster stores and the value the
// composer holds.
//
// PURE, AND THAT IS THE POINT. Nothing here touches React or the connection,
// so "what does a stored view mean" is settled by unit tests rather than by
// rendering something and looking at it. It is also the module that makes the
// storage claim honest: a `v1:portalviews:view` row's `arrangements` field IS
// view-kit's Arrangement value, so reading a saved view back is a parse and
// not a reconstruction.
//
// READING IS DEFENSIVE, WRITING IS NOT. A row arrives from a cluster that may
// be running a different release of the DSL, from a view somebody composed
// before an element existed, or -- once the row is a row like any other --
// from a hand-written mutation. So every field is read with a fallback and a
// malformed arrangement degrades to an empty one rather than throwing;
// view-kit's sanitizeArrangement then repairs it against the live rows, which
// is where the repair belongs (it needs a profile, and this module has none).
// Writing goes the other way: the composer only ever hands over values it
// built, so the write path states the shape plainly.

export interface SavedView {
  readonly id: string;
  readonly name: string;
  readonly description: string;
  // The concepts the composer selected, in section order.
  readonly conceptIds: readonly string[];
  // One arrangement per selected concept, in the same order. Not keyed by
  // concept id: the ORDER is the section order, and a map would lose it.
  readonly arrangements: readonly Arrangement[];
  readonly origin: ViewOrigin;
  readonly status: "active" | "archived";
  // What this row IS (epic memql#4661). "composed" is a view somebody built
  // and opens by name; "override" is a person's own arrangement of an
  // EXISTING page, resolved at render in place of that page's seed and
  // appearing in no list. Absent on the wire means composed, which is what
  // every row written before the field meant.
  readonly kind: "composed" | "override";
  // Which page an override belongs to. Empty on a composed view.
  readonly targetPageId: string;
  readonly updatedAt: string;
  readonly createdAt: string;
}

// Where the saved arrangement came from. Provenance only: both kinds render
// identically, and nothing branches on it.
export type ViewOrigin = "manual" | "suggested";

export const VIEW_CONCEPT_ID = "v1:portalviews:view";

// parseSavedView reads one row. Returns undefined only when the row has no id
// -- everything else has an honest default, because a view that lost its
// description is still a view and refusing to show it would be worse.
export function parseSavedView(raw: Row): SavedView | undefined {
  const row = flattenForList(raw);
  const id = str(row["id"]);
  if (id === "") return undefined;
  return {
    id,
    name: str(row["name"]) || "Untitled view",
    description: str(row["description"]),
    conceptIds: strList(row["conceptIds"]),
    arrangements: parseArrangements(row["arrangements"]),
    origin: row["origin"] === "suggested" ? "suggested" : "manual",
    status: row["status"] === "archived" ? "archived" : "active",
    kind: row["kind"] === "override" ? "override" : "composed",
    targetPageId: str(row["targetPageId"]),
    updatedAt: str(row["updatedAt"]),
    createdAt: str(row["createdAt"]),
  };
}

export function parseSavedViews(rows: readonly Row[]): SavedView[] {
  const out: SavedView[] = [];
  for (const row of rows) {
    const view = parseSavedView(row);
    if (view !== undefined) out.push(view);
  }
  return out;
}

// arrangementFor finds the section for one concept. A saved view carries its
// sections in order, so this is a lookup rather than an index -- and it
// returns undefined for a concept the view does not cover, which the composer
// treats as "seed a fresh deterministic arrangement".
export function arrangementFor(
  view: SavedView,
  conceptId: string,
): Arrangement | undefined {
  return view.arrangements.find((a) => a.conceptId === conceptId);
}

// ---------------------------------------------------------------------------
// The write side
// ---------------------------------------------------------------------------

export interface SavedViewInput {
  // Empty for a create; the caller mints the id (newShortId) exactly as every
  // other MemQL write does, so a create is idempotent under a retry.
  readonly viewId: string;
  readonly name: string;
  readonly description: string;
  readonly conceptIds: readonly string[];
  readonly arrangements: readonly Arrangement[];
  readonly origin: ViewOrigin;
}

// savedViewArgs renders the input as the mutation's named arguments.
//
// ownerUserId is CONSPICUOUSLY ABSENT and that is the whole authorization
// story: the field is @serverSet on the concept, so the engine stamps it from
// actor.userId and would reject a mutation that accepted it from here. A
// client cannot compose a view owned by somebody else because there is no
// argument through which to try.
export function savedViewArgs(input: SavedViewInput): CreateComposedViewArgs {
  return {
    viewId: input.viewId,
    name: input.name,
    description: omitBlank(input.description),
    conceptIds: [...input.conceptIds],
    // Serialized structurally, not as a JSON string. The field is []object, so
    // the arrangement stays queryable and readable in the concept browser
    // rather than being an opaque blob only this client can open.
    arrangements: input.arrangements.map(serializeArrangement),
    origin: omitBlank(input.origin),
  };
}

// serializeArrangement is the WRITE side of the round trip, and every field
// the grammar has must be here: a field written by the composer and dropped
// here is a setting that works until the view is saved and reopened, which is
// the worst shape a bug can take -- it looks like it works.
export function serializeArrangement(arrangement: Arrangement): Record<string, unknown> {
  const out: Record<string, unknown> = { conceptId: arrangement.conceptId };
  // Omitted rather than written as "stack", so a row stored before layouts
  // existed and a row deliberately left as a stack are the same value. The
  // absent-means-stack rule is only additive if absence survives a save.
  if (arrangement.layout !== undefined && arrangement.layout !== "stack") {
    out["layout"] = arrangement.layout;
  }
  out["elements"] = arrangement.elements.map((entry) => {
    const el: Record<string, unknown> = { element: entry.element, band: entry.band };
    if (entry.title !== undefined) el["title"] = entry.title;
    // Same reasoning as layout: standard is what absence means.
    if (entry.role !== undefined && entry.role !== "standard") el["role"] = entry.role;
    if (entry.options !== undefined && Object.keys(entry.options).length > 0) {
      el["options"] = { ...entry.options };
    }
    if (entry.bindings !== undefined) {
      el["bindings"] = Object.fromEntries(
        Object.entries(entry.bindings).map(([slot, fields]) => [slot, [...fields]]),
      );
    }
    return el;
  });
  return out;
}

// ---------------------------------------------------------------------------
// Parsing the stored arrangement
// ---------------------------------------------------------------------------

export function parseArrangements(raw: unknown): Arrangement[] {
  if (!Array.isArray(raw)) return [];
  const out: Arrangement[] = [];
  for (const item of raw) {
    if (!isRecord(item)) continue;
    const conceptId = str(item["conceptId"]);
    if (conceptId === "") continue;
    // A layout this build does not have is READ AS STORED and repaired by
    // sanitizeArrangement against the live rows, which is where repair
    // belongs -- it needs a profile and this module has none. Correcting it
    // here would hide from the composer that a stored row names something
    // this release removed.
    const layout = str(item["layout"]);
    out.push({
      conceptId,
      ...(layout === "" ? {} : { layout: layout as Arrangement["layout"] }),
      elements: parseEntries(item["elements"]),
    });
  }
  return out;
}

function parseEntries(raw: unknown): ArrangedElement[] {
  if (!Array.isArray(raw)) return [];
  const out: ArrangedElement[] = [];
  for (const item of raw) {
    if (!isRecord(item)) continue;
    const element = str(item["element"]);
    if (element === "") continue;
    // A band the row does not carry, or carries wrongly, falls back to the
    // roll: an entry with a nonsense band is still an element somebody chose,
    // and view-kit re-files it against the element's declaration anyway.
    const band = BAND_ROLES.find((b) => b === item["band"]) ?? "roll";
    const title = str(item["title"]);
    const bindings = parseBindings(item["bindings"]);
    const role = ROLES.find((r) => r === item["role"]);
    const options = parseOptions(item["options"]);
    out.push({
      element,
      band,
      ...(title === "" ? {} : { title }),
      ...(bindings === undefined ? {} : { bindings }),
      ...(role === undefined ? {} : { role }),
      ...(options === undefined ? {} : { options }),
    });
  }
  return out;
}

function parseBindings(
  raw: unknown,
): Readonly<Record<string, readonly string[]>> | undefined {
  if (!isRecord(raw)) return undefined;
  const out: Record<string, readonly string[]> = {};
  for (const [slot, value] of Object.entries(raw)) {
    // An EMPTY list survives the round trip. It is the fitness contract's way
    // of declining a slot, so dropping it would silently re-enable a measure
    // somebody deliberately turned off.
    if (Array.isArray(value)) out[slot] = value.filter((v): v is string => typeof v === "string");
    else if (typeof value === "string") out[slot] = [value];
  }
  return Object.keys(out).length === 0 ? undefined : out;
}

const ROLES = ["hero", "supporting", "standard"] as const;

// Element options are STRING-VALUED, which is what keeps a stored arrangement
// a plain JSON value that survives the round trip unchanged. A non-string is
// dropped rather than coerced: a module id or a sort direction that arrived as
// a number is a row somebody wrote by hand, and stringifying it would produce
// an id that resolves to nothing while looking deliberate.
function parseOptions(raw: unknown): Readonly<Record<string, string>> | undefined {
  if (!isRecord(raw)) return undefined;
  const out: Record<string, string> = {};
  for (const [key, value] of Object.entries(raw)) {
    if (typeof value === "string" && value !== "") out[key] = value;
  }
  return Object.keys(out).length === 0 ? undefined : out;
}

function str(value: unknown): string {
  return typeof value === "string" ? value : "";
}

function strList(value: unknown): string[] {
  if (!Array.isArray(value)) return [];
  return value.filter((v): v is string => typeof v === "string");
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}
