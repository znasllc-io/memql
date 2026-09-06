import type { Concept, ConceptField, Row } from "@znasllc-io/memql-sdk-core/client";

// What a concept DECLARES, against what its rows actually CARRY.
//
// ===========================================================================
// WHY THE COMPARISON IS THE FEATURE
// ===========================================================================
// A field list on its own is the DSL file read back, and an author can
// already read the DSL file. What an operator cannot get anywhere else is
// the join: which declared fields nothing has ever written, and which keys
// rows carry that the concept does not describe.
//
// Both halves are real failures with no other symptom.
//
// A DECLARED FIELD WITH NO WRITER reads exactly like a field whose value
// happens to be empty -- the surface shows nothing either way -- so a
// mutation that quietly stopped setting it, or never set it, looks like
// data that is merely absent. It is found by diffing what is declared
// against what arrives, which is what this does.
//
// AN UNDECLARED KEY is the other direction: a writer putting something in
// the payload the concept never promised. It is invisible to the DSL and
// invisible to any shaped read, because a shape can only project declared
// fields.
//
// ===========================================================================
// THE HONESTY CONSTRAINT
// ===========================================================================
// "Observed" is a claim about the rows this browser has LOADED, which is a
// bounded page, not the concept. So every observed statement here carries
// its sample size, and `presentIn` is a count rather than a boolean. A
// field missing from 200 loaded rows is evidence; it is not proof that no
// row anywhere carries it, and the surface must not say it is.
//
// The engine publishes the declared shape directly now (`concept.fields`,
// epic memql#4661), so the declared half is no longer profiled off a JSON
// Schema riding on a row -- which is what the portal had to do. EMPTY
// `fields` IS A REAL ANSWER AND IT IS NOT "no fields": it means this
// server does not publish a shape. Rendering that as a concept with no
// fields is the mistake the SDK's own comment warns about, so it gets its
// own state here rather than collapsing into an empty list.

/** Where a field's evidence comes from. */
export type FieldStanding =
  /** Declared, and at least one loaded row carries it. */
  | "declared-and-present"
  /** Declared, and no loaded row carries it. */
  | "declared-not-seen"
  /** Not declared, but loaded rows carry it. */
  | "undeclared";

export interface SchemaField {
  name: string;
  standing: FieldStanding;
  /** The DSL's own word for the type. Empty for an undeclared field. */
  kind: string;
  required: boolean;
  enumValues: string[];
  description: string;
  /** The JS types seen across loaded rows. Empty when none carried it. */
  observedTypes: string[];
  /** How many loaded rows carried this key. */
  presentIn: number;
}

export interface SchemaReading {
  /** True when the server published no declared shape at all. */
  shapeUnpublished: boolean;
  /** How many rows the observed half was derived from. */
  sampleSize: number;
  fields: SchemaField[];
}

function payloadOf(row: Row): Record<string, unknown> {
  const payload = row["payload"];
  if (payload !== null && typeof payload === "object" && !Array.isArray(payload)) {
    return payload as Record<string, unknown>;
  }
  // A flattened row: the shape-projected form, where payload keys sit at
  // the top level. Both arrive here depending on the read, and profiling
  // the wrong one reports every field as unseen.
  return row;
}

function jsTypeOf(value: unknown): string {
  if (value === null) return "null";
  if (Array.isArray(value)) return "array";
  return typeof value;
}

/**
 * Join the declared shape against the loaded rows.
 *
 * Order is declared-first (required before optional, then alphabetical),
 * then the undeclared keys. Undeclared last because they are the
 * exception; putting them among the declared ones would make a reader
 * check every row's standing to find the concept's actual shape.
 */
export function readSchema(concept: Concept, rows: readonly Row[]): SchemaReading {
  const declared: ConceptField[] = concept.fields ?? [];

  const seenTypes = new Map<string, Set<string>>();
  const seenCount = new Map<string, number>();
  for (const row of rows) {
    const payload = payloadOf(row);
    for (const [key, value] of Object.entries(payload)) {
      let types = seenTypes.get(key);
      if (!types) {
        types = new Set<string>();
        seenTypes.set(key, types);
      }
      types.add(jsTypeOf(value));
      seenCount.set(key, (seenCount.get(key) ?? 0) + 1);
    }
  }

  const declaredNames = new Set(declared.map((f) => f.name));

  const declaredFields: SchemaField[] = declared.map((field) => {
    const count = seenCount.get(field.name) ?? 0;
    return {
      name: field.name,
      standing: count > 0 ? "declared-and-present" : "declared-not-seen",
      kind: field.kind,
      required: field.required,
      enumValues: field.enumValues,
      description: field.description,
      observedTypes: [...(seenTypes.get(field.name) ?? [])].sort(),
      presentIn: count,
    };
  });
  declaredFields.sort((a, b) => {
    if (a.required !== b.required) return a.required ? -1 : 1;
    return a.name.localeCompare(b.name);
  });

  const undeclared: SchemaField[] = [...seenTypes.keys()]
    .filter((key) => !declaredNames.has(key))
    .sort()
    .map((key) => ({
      name: key,
      standing: "undeclared" as const,
      kind: "",
      required: false,
      enumValues: [],
      description: "",
      observedTypes: [...(seenTypes.get(key) ?? [])].sort(),
      presentIn: seenCount.get(key) ?? 0,
    }));

  return {
    shapeUnpublished: declared.length === 0,
    sampleSize: rows.length,
    fields: [...declaredFields, ...undeclared],
  };
}

/**
 * The sentence for a field's standing, scoped to the sample it rests on.
 *
 * Scoped because an unscoped "no rows carry this" is a claim about the
 * concept that a page of rows cannot support.
 */
export function standingSentence(field: SchemaField, sampleSize: number): string {
  if (field.standing === "declared-and-present") {
    return `In ${field.presentIn} of the ${sampleSize} rows loaded.`;
  }
  if (field.standing === "declared-not-seen") {
    if (sampleSize === 0) return "Declared. No rows loaded yet to compare against.";
    return `Declared, but none of the ${sampleSize} rows loaded carries it.`;
  }
  return `Not declared by the concept. Carried by ${field.presentIn} of the ${sampleSize} rows loaded.`;
}
