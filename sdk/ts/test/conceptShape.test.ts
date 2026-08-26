// conceptsFromWire -- the declared shape (epic memql#4661, task memql#4662).
//
// The distinction this file exists to protect is `undefined` vs `[]`. A client
// deciding whether to fall back to profiling loaded rows asks "did the server
// publish a shape", and that question has three answers on the wire: a list, an
// empty list, and nothing at all. Collapsing the last two -- which is what
// defaulting to `[]` in the decoder does -- turns "this server is too old to
// say" into "this concept has no fields", and the client then renders a real
// concept as blank instead of falling back.

import test from "node:test";
import assert from "node:assert/strict";

import { conceptsFromWire } from "../src/client/types.js";

test("conceptsFromWire carries fields with the authoring kind, requiredness and enum members", () => {
  const [concept] = conceptsFromWire([
    {
      id: "v1:test:widget",
      entity: "widget",
      fields: [
        { name: "name", kind: "string", required: true, description: "What it is called." },
        { name: "status", kind: "enum", enumValues: ["active", "archived"] },
        { name: "createdAt", kind: "datetime" },
      ],
    },
  ]);

  assert.ok(concept?.fields, "fields should be present");
  assert.equal(concept.fields?.length, 3);

  const [name, status, createdAt] = concept.fields!;
  assert.equal(name?.name, "name");
  assert.equal(name?.kind, "string");
  assert.equal(name?.required, true);
  assert.equal(name?.description, "What it is called.");

  assert.equal(status?.kind, "enum");
  assert.deepEqual(status?.enumValues, ["active", "archived"]);
  // Requiredness absent on the wire is FALSE, not undefined: an optional
  // field is the common case and the wire omits the default.
  assert.equal(status?.required, false);

  assert.equal(createdAt?.kind, "datetime");
  // A non-enum field carries no members rather than undefined, so a caller
  // can map over it without a guard.
  assert.deepEqual(createdAt?.enumValues, []);
});

test("conceptsFromWire keeps both relationship axes apart", () => {
  const [concept] = conceptsFromWire([
    {
      id: "v1:test:widget",
      relationships: [
        {
          type: "references",
          as: "ownedBy",
          field: "ownerUserId",
          target: "v1:identity:user",
          direction: "outgoing",
        },
        // Pre-memql#3652: labelled by nothing. `as` must stay empty --
        // backfilling it from `type` would hand a UI the engine's structural
        // word as if a person had chosen it as a label.
        { type: "parent", field: "parentId", target: "v1:test:widget", direction: "outgoing" },
      ],
    },
  ]);

  assert.equal(concept?.relationships?.length, 2);
  assert.equal(concept?.relationships?.[0]?.as, "ownedBy");
  assert.equal(concept?.relationships?.[0]?.type, "references");
  assert.equal(concept?.relationships?.[1]?.as, "");
  assert.equal(concept?.relationships?.[1]?.type, "parent");
});

test("conceptsFromWire says nothing rather than nothing-there when the server predates the fields", () => {
  const [concept] = conceptsFromWire([{ id: "v1:test:widget", entity: "widget" }]);

  // undefined, NOT []. This is the whole point of the file: a caller reads
  // `concept.fields === undefined` as "no shape published, profile the rows",
  // and `[]` as "the server says this concept has no fields".
  assert.equal(concept?.fields, undefined);
  assert.equal(concept?.relationships, undefined);
});

test("conceptsFromWire treats an empty published list the same as an absent one", () => {
  // A server that sent `fields: []` published no shape either -- the engine
  // emits nothing for a concept whose definition schema did not parse. Both
  // arrive as undefined so a client has ONE fallback condition to write
  // rather than two.
  const [concept] = conceptsFromWire([{ id: "v1:test:widget", fields: [], relationships: [] }]);
  assert.equal(concept?.fields, undefined);
  assert.equal(concept?.relationships, undefined);
});
