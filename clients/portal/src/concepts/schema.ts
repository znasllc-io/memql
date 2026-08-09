// Concept schema inspection: what fields a concept declares, of what types,
// carrying which annotations.
//
// ===========================================================================
// WHERE THE SCHEMA COMES FROM, and why NOT from DslSpecMsg
// ===========================================================================
//
// memql#3316 asks whether `DslSpecMsg` should back this view. It should not,
// and the reason is worth writing down so nobody re-opens it from the message
// name alone. DslSpecMsg exports `component/language/dslspec.Build()`: the DSL
// AUTHORING SURFACE -- which constructs exist (`concept`, `query`, `shape`),
// which annotations are legal on each, the keyword / operator / field-type
// tables, and the legal-next completion rules. It is the grammar a Monaco
// editor is generated from. It says nothing whatsoever about any individual
// concept: ask it about any particular one and it does not know the name.
// Wiring it in here would put a language reference on a screen whose question
// is "what fields does THIS concept have".
//
// The two sources that DO answer that question, both concept-agnostic:
//
//   1. THE DECLARED SCHEMA. Every memQL row carries its concept's JSON Schema
//      document on the `schema` row intrinsic (MemoryNode.schema on the wire;
//      `browseConceptPage` preserves it because it returns rawNodes()). That
//      document is generated from the `concept` declaration itself, so its
//      `properties` ARE the declared fields, its `required` IS the @required
//      set, and its `x-*` keywords ARE the field annotations (`x-pii` from
//      @pii, `x-secret` from @secret, and so on -- the same keywords
//      component/database/memory-nodes/concept.go reads for the PII scrub).
//      This is the authoritative answer and it needs no new wire message.
//
//   2. THE OBSERVED SHAPE. When no loaded row carries a schema document --
//      an empty concept, or a query path that projected the intrinsic away --
//      the fields are derived from the payloads actually present: name, the
//      JSON types seen, and how many of the loaded rows carry it. Clearly
//      LABELLED as observed, because "every row I looked at has this field"
//      is a weaker claim than "the concept declares it" and an operator must
//      not have to guess which one they are reading.
//
// Both paths are field-driven. Neither enumerates a concept id, and
// portal_render_path_test.go (repo root) fails the build if one appears.

import type { Row } from "@znasllc-io/memql-sdk-core/client";

export type SchemaSource = "declared" | "observed" | "none";

export interface FieldDescriptor {
  name: string;
  // Declared: the JSON Schema `type` (plus `[]` for arrays, and the item type
  // when the schema names one). Observed: the JSON types actually seen,
  // joined -- "string | null" is a real and useful answer.
  type: string;
  required: boolean;
  description: string;
  // Declared: the schema's `x-*` custom keywords, minus the `x-` prefix, for
  // every one whose value is truthy -- so @pii shows as "pii". Observed:
  // empty; an observed field has no annotations to read.
  annotations: string[];
  // Observed only: how many of the inspected rows carried this field. Zero
  // for a declared field, which is never counted -- the declaration is the
  // fact, and a count next to it would read as "declared but missing".
  presentIn: number;
}

export interface ConceptSchemaView {
  source: SchemaSource;
  fields: FieldDescriptor[];
  // The raw JSON Schema document, when one was found. Rendered verbatim
  // underneath the field table -- the table is the readable summary, and an
  // operator chasing a validation error needs the document itself.
  document: Record<string, unknown> | null;
  // How many rows the observed shape was derived from. Zero for a declared
  // schema, and the honest denominator for `presentIn`.
  sampleSize: number;
}

export const EMPTY_SCHEMA_VIEW: ConceptSchemaView = {
  source: "none",
  fields: [],
  document: null,
  sampleSize: 0,
};

function isPlainObject(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

// findSchemaDocument returns the first non-empty `schema` intrinsic across the
// rows. First rather than merged: every row of a concept carries the same
// document, so a merge would only paper over an inconsistency worth seeing.
export function findSchemaDocument(
  rows: readonly Row[],
): Record<string, unknown> | null {
  for (const row of rows) {
    const schema = row["schema"];
    if (isPlainObject(schema) && Object.keys(schema).length > 0) return schema;
  }
  return null;
}

// jsonSchemaType renders a property's declared type for a human. Arrays get
// their item type when one is declared, because "[]" alone tells the reader
// nothing they did not already know from the field name.
function jsonSchemaType(property: Record<string, unknown>): string {
  const raw = property["type"];
  const base = Array.isArray(raw)
    ? raw.filter((t): t is string => typeof t === "string").join(" | ")
    : typeof raw === "string"
      ? raw
      : "";
  if (base === "array") {
    const items = property["items"];
    if (isPlainObject(items)) {
      const itemType = jsonSchemaType(items);
      if (itemType) return `${itemType}[]`;
    }
    return "array";
  }
  // A property declared only by `enum` or `$ref` has no `type`; naming the
  // construct beats an empty cell.
  if (base === "" && Array.isArray(property["enum"])) return "enum";
  if (base === "" && typeof property["$ref"] === "string") return "ref";
  return base || "any";
}

// customAnnotations lifts the `x-*` keywords off a property. Generic by
// construction: a new annotation added to the DSL surfaces here the day it is
// emitted, with no list to update.
function customAnnotations(property: Record<string, unknown>): string[] {
  const out: string[] = [];
  for (const [key, value] of Object.entries(property)) {
    if (!key.startsWith("x-")) continue;
    if (value === false || value === null || value === undefined || value === "") continue;
    out.push(value === true ? key.slice(2) : `${key.slice(2)}=${String(value)}`);
  }
  return out.sort();
}

function describeDeclared(document: Record<string, unknown>): FieldDescriptor[] {
  const properties = document["properties"];
  if (!isPlainObject(properties)) return [];
  const requiredRaw = document["required"];
  const required = new Set(
    Array.isArray(requiredRaw)
      ? requiredRaw.filter((name): name is string => typeof name === "string")
      : [],
  );
  return Object.entries(properties)
    .map(([name, value]) => {
      const property = isPlainObject(value) ? value : {};
      const description = property["description"];
      return {
        name,
        type: jsonSchemaType(property),
        required: required.has(name),
        description: typeof description === "string" ? description : "",
        annotations: customAnnotations(property),
        presentIn: 0,
      };
    })
    .sort(compareFields);
}

// compareFields puts required fields first, then alphabetical. A schema table
// is read to answer "what must I supply", and burying the required fields
// among forty optional ones makes that a scan instead of a glance.
function compareFields(a: FieldDescriptor, b: FieldDescriptor): number {
  if (a.required !== b.required) return a.required ? -1 : 1;
  return a.name.localeCompare(b.name);
}

function jsonTypeOf(value: unknown): string {
  if (value === null) return "null";
  if (Array.isArray(value)) return "array";
  return typeof value;
}

function describeObserved(rows: readonly Row[]): FieldDescriptor[] {
  const types = new Map<string, Set<string>>();
  const counts = new Map<string, number>();
  let payloadRows = 0;

  for (const row of rows) {
    const payload = row["payload"];
    if (!isPlainObject(payload)) continue;
    payloadRows++;
    for (const [key, value] of Object.entries(payload)) {
      const seen = types.get(key) ?? new Set<string>();
      seen.add(jsonTypeOf(value));
      types.set(key, seen);
      counts.set(key, (counts.get(key) ?? 0) + 1);
    }
  }

  return [...types.entries()]
    .map(([name, seen]) => {
      const presentIn = counts.get(name) ?? 0;
      return {
        name,
        type: [...seen].sort().join(" | "),
        // "Every row I looked at has it" is the strongest claim an observation
        // supports, and it is NOT the same as @required -- which is why the
        // view labels the whole table "observed".
        required: payloadRows > 0 && presentIn === payloadRows,
        description: "",
        annotations: [],
        presentIn,
      };
    })
    .sort(compareFields);
}

// describeConceptSchema is the whole surface: hand it the rows the browser has
// loaded and it returns the best available description of the concept's
// fields, saying which kind it is.
export function describeConceptSchema(rows: readonly Row[]): ConceptSchemaView {
  const document = findSchemaDocument(rows);
  if (document) {
    const fields = describeDeclared(document);
    if (fields.length > 0) {
      return { source: "declared", fields, document, sampleSize: 0 };
    }
    // A schema document with no `properties` (a concept whose payload is
    // unconstrained) still earns the document view, but the field table has
    // to come from observation or the pane says nothing at all.
    const observed = describeObserved(rows);
    return {
      source: observed.length > 0 ? "observed" : "none",
      fields: observed,
      document,
      sampleSize: rows.length,
    };
  }

  const fields = describeObserved(rows);
  if (fields.length === 0) return EMPTY_SCHEMA_VIEW;
  return { source: "observed", fields, document: null, sampleSize: rows.length };
}
