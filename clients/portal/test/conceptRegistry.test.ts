// Registry search and grouping (src/concepts/registry.ts), and schema
// description (src/concepts/schema.ts).
//
// Both are pure, so the semantics are pinned here rather than by clicking
// around a running cluster. The fixtures use real concept ids deliberately --
// test fixtures are exactly where real ids belong, and the guard that forbids
// them (portal_render_path_test.go) polices src/, not test/.

import { describe, expect, it } from "vitest";
import type { Concept, Row } from "@znasllc-io/memql-sdk-core/client";

import {
  domainSummaries,
  filterConcepts,
  groupByDomain,
  searchTerms,
} from "../src/concepts/registry";
import { describeConceptSchema, findSchemaDocument } from "../src/concepts/schema";

function concept(partial: Partial<Concept> & Pick<Concept, "id">): Concept {
  const [version = "", domain = "", entity = ""] = partial.id.split(":");
  return {
    version,
    domain,
    entity,
    description: "",
    type: "concept",
    ...partial,
  };
}

const REGISTRY: Concept[] = [
  concept({ id: "v1:identity:user", description: "The person; cluster-wide role" }),
  concept({ id: "v1:identity:authSession", description: "Per-token session record" }),
  concept({ id: "v1:cognition:space", description: "A conversation space" }),
  concept({ id: "v1:cluster:node", description: "A registered node in the cluster" }),
  concept({ id: "v1:planner:plan", description: "A user-visible unit of work" }),
];

describe("registry search", () => {
  it("matches on the id", () => {
    expect(filterConcepts(REGISTRY, { query: "authsession", domain: "" }).map((c) => c.id)).toEqual(
      ["v1:identity:authSession"],
    );
  });

  it("matches on the entity and on the domain", () => {
    expect(filterConcepts(REGISTRY, { query: "plan", domain: "" }).map((c) => c.id)).toEqual([
      "v1:planner:plan",
    ]);
    expect(filterConcepts(REGISTRY, { query: "cognition", domain: "" }).map((c) => c.id)).toEqual([
      "v1:cognition:space",
    ]);
  });

  it("matches on the description -- the thing an operator remembers when they forget the id", () => {
    expect(
      filterConcepts(REGISTRY, { query: "conversation", domain: "" }).map((c) => c.id),
    ).toEqual(["v1:cognition:space"]);
  });

  it("requires EVERY term to match, so two words narrow rather than widen", () => {
    // An OR would return the whole identity domain for this input, which is
    // not what someone typing two words means.
    expect(
      filterConcepts(REGISTRY, { query: "identity session", domain: "" }).map((c) => c.id),
    ).toEqual(["v1:identity:authSession"]);
    expect(filterConcepts(REGISTRY, { query: "identity nope", domain: "" })).toEqual([]);
  });

  it("is case- and whitespace-insensitive", () => {
    expect(searchTerms("  Identity   SESSION ")).toEqual(["identity", "session"]);
    expect(
      filterConcepts(REGISTRY, { query: "  IDENTITY   session ", domain: "" }),
    ).toHaveLength(1);
  });

  it("filters by exact domain, and combines with the query", () => {
    expect(filterConcepts(REGISTRY, { query: "", domain: "identity" })).toHaveLength(2);
    expect(
      filterConcepts(REGISTRY, { query: "user", domain: "identity" }).map((c) => c.id),
    ).toEqual(["v1:identity:user"]);
    // The domain is exact, not a substring: "ident" must not select
    // "identity", or the control stops meaning what its label says.
    expect(filterConcepts(REGISTRY, { query: "", domain: "ident" })).toEqual([]);
  });

  it("preserves the incoming order rather than re-ranking under the cursor", () => {
    expect(filterConcepts(REGISTRY, { query: "v1", domain: "" }).map((c) => c.id)).toEqual(
      REGISTRY.map((c) => c.id),
    );
  });

  it("counts domains off the unfiltered registry and sorts them", () => {
    expect(domainSummaries(REGISTRY)).toEqual([
      { domain: "cluster", count: 1 },
      { domain: "cognition", count: 1 },
      { domain: "identity", count: 2 },
      { domain: "planner", count: 1 },
    ]);
  });

  it("buckets a domainless concept under a visible label rather than dropping it", () => {
    const odd = concept({ id: "loose", domain: "", entity: "loose" });
    expect(domainSummaries([odd])).toEqual([{ domain: "(none)", count: 1 }]);
    expect(groupByDomain([odd])[0]?.domain).toBe("(none)");
  });

  it("groups by domain, sorted, with concepts in their incoming order", () => {
    const groups = groupByDomain(REGISTRY);
    expect(groups.map((g) => g.domain)).toEqual([
      "cluster",
      "cognition",
      "identity",
      "planner",
    ]);
    expect(groups.find((g) => g.domain === "identity")?.concepts.map((c) => c.id)).toEqual([
      "v1:identity:user",
      "v1:identity:authSession",
    ]);
  });
});

// The JSON Schema document an engine row carries on its `schema` intrinsic,
// with the x-* keywords the DSL emits for field annotations.
const SCHEMA_DOC = {
  type: "object",
  required: ["ownerUserId", "name"],
  properties: {
    ownerUserId: { type: "string", description: "Who owns this row" },
    name: { type: "string" },
    email: { type: "string", "x-pii": true },
    apiKey: { type: "string", "x-secret": true, "x-internal": false },
    tags: { type: "array", items: { type: "string" } },
    scores: { type: "array" },
    status: { enum: ["active", "archived"] },
    meta: { $ref: "#/definitions/meta" },
    weird: {},
  },
};

describe("schema description", () => {
  it("reads the declared schema off a row's schema intrinsic", () => {
    const rows: Row[] = [
      { id: "a", payload: { name: "Alpha" } },
      { id: "b", schema: SCHEMA_DOC, payload: { name: "Beta" } },
    ];
    expect(findSchemaDocument(rows)).toBe(SCHEMA_DOC);

    const view = describeConceptSchema(rows);
    expect(view.source).toBe("declared");
    expect(view.document).toBe(SCHEMA_DOC);

    // Required first, then alphabetical -- a schema table is read to answer
    // "what must I supply".
    expect(view.fields.map((f) => f.name)).toEqual([
      "name",
      "ownerUserId",
      "apiKey",
      "email",
      "meta",
      "scores",
      "status",
      "tags",
      "weird",
    ]);
    expect(view.fields.filter((f) => f.required).map((f) => f.name)).toEqual([
      "name",
      "ownerUserId",
    ]);
  });

  it("renders types the way a reader needs them, including arrays and enums", () => {
    const byName = new Map(
      describeConceptSchema([{ id: "a", schema: SCHEMA_DOC }]).fields.map((f) => [
        f.name,
        f,
      ]),
    );
    expect(byName.get("tags")?.type).toBe("string[]");
    expect(byName.get("scores")?.type).toBe("array");
    expect(byName.get("status")?.type).toBe("enum");
    expect(byName.get("meta")?.type).toBe("ref");
    expect(byName.get("weird")?.type).toBe("any");
  });

  it("surfaces x-* annotations generically, and drops the falsy ones", () => {
    const byName = new Map(
      describeConceptSchema([{ id: "a", schema: SCHEMA_DOC }]).fields.map((f) => [
        f.name,
        f,
      ]),
    );
    expect(byName.get("email")?.annotations).toEqual(["pii"]);
    // x-internal: false is not an annotation the field carries.
    expect(byName.get("apiKey")?.annotations).toEqual(["secret"]);
    expect(byName.get("name")?.annotations).toEqual([]);
  });

  it("carries the field description through", () => {
    const view = describeConceptSchema([{ id: "a", schema: SCHEMA_DOC }]);
    expect(view.fields.find((f) => f.name === "ownerUserId")?.description).toBe(
      "Who owns this row",
    );
  });

  it("falls back to the OBSERVED shape when no row carries a schema", () => {
    const rows: Row[] = [
      { id: "a", payload: { name: "Alpha", count: 1 } },
      { id: "b", payload: { name: "Beta", tags: ["x"], count: null } },
    ];
    const view = describeConceptSchema(rows);

    expect(view.source).toBe("observed");
    expect(view.document).toBeNull();
    expect(view.sampleSize).toBe(2);

    const byName = new Map(view.fields.map((f) => [f.name, f]));
    expect(byName.get("name")?.type).toBe("string");
    // Both types seen, joined -- "number | null" is a real and useful answer.
    expect(byName.get("count")?.type).toBe("null | number");
    expect(byName.get("tags")?.type).toBe("array");
    // "Required" observed means "every row I looked at carried it", which is
    // weaker than @required -- the pane labels the whole table accordingly.
    expect(byName.get("name")?.required).toBe(true);
    expect(byName.get("tags")?.required).toBe(false);
    expect(byName.get("tags")?.presentIn).toBe(1);
  });

  it("reports nothing rather than guessing when there are no rows at all", () => {
    const view = describeConceptSchema([]);
    expect(view.source).toBe("none");
    expect(view.fields).toEqual([]);
    expect(view.document).toBeNull();
  });

  it("ignores an empty schema object -- it is not a declaration", () => {
    expect(findSchemaDocument([{ id: "a", schema: {} }])).toBeNull();
    expect(describeConceptSchema([{ id: "a", schema: {} }]).source).toBe("none");
  });

  it("keeps the document but observes the fields when the schema declares no properties", () => {
    const doc = { type: "object" };
    const view = describeConceptSchema([{ id: "a", schema: doc, payload: { free: "form" } }]);
    expect(view.document).toBe(doc);
    expect(view.source).toBe("observed");
    expect(view.fields.map((f) => f.name)).toEqual(["free"]);
  });
});
