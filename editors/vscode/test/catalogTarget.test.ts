// Running a construct that exists in no file on this machine (memql#3753).
//
// The whole issue is how little it contains: `argForm.ts`, the orchestrator's
// preflight, the write confirmation, the supersession token and the Result
// view are all reused, and only the SOURCE of the argument fields differs. So
// what is asserted here is the boundary -- which constructs get a target at
// all, what the target carries, and that a target with no file behaves.

import test from "node:test";
import assert from "node:assert/strict";

import {
  CATALOG_SCHEME,
  catalogRunTarget,
  catalogUri,
  isCatalogUri,
  offersRun,
  runUnavailableReason,
} from "../src/constructs/catalogTarget.js";
import type { CatalogConstruct } from "../src/state/constructCatalog.js";

function construct(over: Partial<CatalogConstruct> = {}): CatalogConstruct {
  return {
    name: "spaceParticipants",
    kind: "query",
    namespace: "cognition",
    origin: "core",
    originPath: "dsl/cognition/queries.memql",
    description: "",
    runnable: true,
    runnableKind: "query",
    args: [{ name: "spaceId", type: "string", required: true }],
    boundConcept: "participant",
    sourceHash: "abc",
    source: "",
    ...over,
  };
}

// -----------------------------------------------------------------------------
// which constructs get a target
// -----------------------------------------------------------------------------

test("the four arg-form kinds get a target", () => {
  for (const runnableKind of ["query", "mutate", "logic", "tool"] as const) {
    const target = catalogRunTarget(construct({ runnableKind }));
    assert.notEqual(target, undefined, runnableKind);
    assert.equal(target?.kind, runnableKind);
  }
});

test("an automation gets none, because the catalog does not carry its trigger", () => {
  // Its trigger decides the entire form -- which payload modes are offered and
  // which concept the row picker browses -- and `ListConstructs` has no such
  // field. A form missing the field that decides it would fire a real event on
  // a real cluster with a payload nobody chose.
  const c = construct({ kind: "automation", runnableKind: "automation" });
  assert.equal(catalogRunTarget(c), undefined);
  assert.equal(offersRun(c), false);
  // ...and the absence is EXPLAINED, because an operator who sees the tree
  // call it runnable and finds no button will otherwise read it as a bug.
  assert.match(runUnavailableReason(c), /trigger/);
  assert.match(runUnavailableReason(c), /\.memql file/);
});

test("a view-only construct gets none, and needs no explanation", () => {
  // The tree already says it is not runnable; the absence IS the statement,
  // and a sentence here would answer a question nobody asked.
  const c = construct({ kind: "concept", runnable: false, runnableKind: undefined });
  assert.equal(catalogRunTarget(c), undefined);
  assert.equal(runUnavailableReason(c), "");
});

// -----------------------------------------------------------------------------
// what the target carries
// -----------------------------------------------------------------------------

test("the args come straight from the catalog, in declared order", () => {
  // The point of the shape-parity gate: these are what `buildFields` binds,
  // unchanged, sourced from ListConstructs instead of the language server.
  const target = catalogRunTarget(
    construct({
      args: [
        { name: "spaceId", type: "string", required: true },
        { name: "limit", type: "number", required: false },
      ],
    }),
  );
  assert.deepEqual(target?.args.map((a) => a.name), ["spaceId", "limit"]);
  assert.equal(target?.args[0]?.required, true);
});

test("the uri names the catalog, not a file", () => {
  const target = catalogRunTarget(construct());
  assert.ok(target?.uri.startsWith(`${CATALOG_SCHEME}:`));
  assert.equal(isCatalogUri(target?.uri ?? ""), true);
  // A real file uri must never be mistaken for one.
  assert.equal(isCatalogUri("file:///home/x/dsl/cognition/queries.memql"), false);
  assert.equal(isCatalogUri(""), false);
});

test("a promoted construct with no file still gets a target", () => {
  // The acceptance item this issue exists for. There is no file anywhere on
  // the machine, so nothing about the target may depend on one.
  const target = catalogRunTarget(
    construct({ origin: "promoted", originPath: "", namespace: "", source: "logic x {}" }),
  );
  assert.notEqual(target, undefined);
  assert.equal(isCatalogUri(target?.uri ?? ""), true);
});

test("the uri is legible on its own, and survives an odd name", () => {
  // It appears in logs and in saved configurations, so it carries the kind and
  // the name rather than an opaque id.
  assert.equal(
    catalogUri(construct({ kind: "query", name: "spaceParticipants" })),
    "memql-catalog:query/spaceParticipants",
  );
  // A concept's name is its canonical id, which contains colons.
  assert.equal(
    catalogUri(construct({ kind: "concept", name: "v1:cognition:space" })),
    "memql-catalog:concept/v1%3Acognition%3Aspace",
  );
});

test("two constructs of the same name in different kinds do not collide", () => {
  assert.notEqual(
    catalogUri(construct({ kind: "query", name: "x" })),
    catalogUri(construct({ kind: "logic", name: "x" })),
  );
});
