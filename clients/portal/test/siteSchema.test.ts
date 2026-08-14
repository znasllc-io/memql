// enumValuesForField (src/concepts/schema.ts, memql#3717 ruling 5).
//
// The property under test is specifically NOT "the selector shows spa and
// static" -- a hardcoded two-option constant would pass that trivially, and
// a hardcoded constant is exactly the defect ruling 5 exists to rule out
// (a third value, "server", is a decided follow-on epic, #3718). So every
// schema fixture here carries a THIRD, non-obvious enum value and the
// assertions check that it survives extraction. A test that only checked for
// "spa"/"static" could not tell a schema-driven selector from a hardcoded one
// apart; this one can.

import { describe, expect, it } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { describeConceptSchema, enumValuesForField } from "../src/concepts/schema";

const SITE_SCHEMA = {
  type: "object",
  required: ["hostname", "kind", "bundleRef", "status"],
  properties: {
    hostname: { type: "string" },
    // Three values, not the two the current UI happens to offer today.
    kind: { type: "string", enum: ["spa", "static", "server"] },
    bundleRef: { type: "string" },
    status: { type: "string", enum: ["draft", "live", "disabled"] },
  },
};

function siteRow(schema: Record<string, unknown> | undefined): Row {
  const row: Row = {
    id: "site-1",
    concept: "v1:platform:site",
    type: "concept",
    createdAt: "2026-08-01T00:00:00Z",
    payload: { hostname: "shop.example.com", kind: "static" },
  };
  if (schema) row["schema"] = schema;
  return row;
}

describe("enumValuesForField", () => {
  it("reads the declared enum values off the row's schema intrinsic, in schema order", () => {
    expect(enumValuesForField([siteRow(SITE_SCHEMA)], "kind")).toEqual(["spa", "static", "server"]);
  });

  // THE property ruling 5 is about: a value beyond any two-option constant a
  // caller might have hardcoded shows up with no code change here.
  it("surfaces a third enum value a two-option hardcoded list would have capped at two", () => {
    const values = enumValuesForField([siteRow(SITE_SCHEMA)], "kind");
    expect(values).toContain("server");
    expect(values.length).toBe(3);
  });

  it("tracks a schema that adds a value, with no change to this function", () => {
    const grown = {
      ...SITE_SCHEMA,
      properties: {
        ...SITE_SCHEMA.properties,
        kind: { type: "string", enum: ["spa", "static", "server", "edgeFunction"] },
      },
    };
    expect(enumValuesForField([siteRow(grown)], "kind")).toEqual([
      "spa",
      "static",
      "server",
      "edgeFunction",
    ]);
  });

  it("returns empty for a field with no declared enum", () => {
    expect(enumValuesForField([siteRow(SITE_SCHEMA)], "hostname")).toEqual([]);
  });

  it("returns empty rather than crashing when no row carries a schema yet", () => {
    expect(enumValuesForField([], "kind")).toEqual([]);
    expect(enumValuesForField([siteRow(undefined)], "kind")).toEqual([]);
  });

  it("finds the schema off ANY row in the set, not only the first", () => {
    const values = enumValuesForField([siteRow(undefined), siteRow(SITE_SCHEMA)], "kind");
    expect(values).toEqual(["spa", "static", "server"]);
  });

  // Pin against the recon's claim that describeConceptSchema labels an enum
  // field's `type` as the literal "enum" (schema.ts:120 at the time of the
  // recon). Measured against the ACTUAL Go generator
  // (component/database/memory-nodes/concept_parser.go propertyToJSONSchema,
  // case "enum"): it always stamps `type: "string"` alongside `enum`, so a
  // concept-sourced schema property never has an empty `type`, and the
  // type==="enum" branch in jsonSchemaType is unreachable for real concept
  // fields. enumValues is read off `enum` directly, independent of `type`,
  // which is what makes it correct regardless of that branch's reachability.
  it("labels a concept-sourced enum field's type as declared (string), and separately exposes enumValues", () => {
    const view = describeConceptSchema([siteRow(SITE_SCHEMA)]);
    const kindField = view.fields.find((f) => f.name === "kind");
    expect(kindField?.type).toBe("string");
    expect(kindField?.enumValues).toEqual(["spa", "static", "server"]);
  });
});
