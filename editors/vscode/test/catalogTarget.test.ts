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
  catalogAutomationTarget,
  catalogRunTarget,
  catalogUri,
  isAutomationRun,
  isCatalogUri,
  offersRun,
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

// An automation gets an AutomationTarget, never a RunTarget (memql#3805). The
// two are different shapes going to different commands, and the failure of
// confusing them is not an error -- it is a form with nothing in it.
test("an automation gets an automation target and not an arg-form one", () => {
  const c = construct({
    kind: "automation",
    runnableKind: "automation",
    args: [],
    trigger: { event: "node.created", concept: "v1:cognition:participant" },
  });
  assert.equal(catalogRunTarget(c), undefined, "an automation must not get an arg-form target");
  const target = catalogAutomationTarget(c);
  assert.notEqual(target, undefined);
  assert.equal(target?.name, c.name);
  assert.equal(target?.uri, catalogUri(c));
  // The trigger is what the form is built from: the concept decides which
  // rows the picker browses, the event decides the modes offered.
  assert.deepEqual(target?.trigger, { event: "node.created", concept: "v1:cognition:participant" });
  assert.equal(offersRun(c), true, "the page must now draw a Run control for an automation");
  assert.equal(isAutomationRun(c), true);
});

// A trigger the cluster did not report is manual-run, which IS a describable
// form -- so the affordance is offered with no trigger on the target, rather
// than withheld. Defaulting an empty trigger in here would instead claim the
// automation fires on nothing.
test("an automation with no reported trigger still gets a target, carrying none", () => {
  const c = construct({ kind: "automation", runnableKind: "automation", args: [] });
  const target = catalogAutomationTarget(c);
  assert.notEqual(target, undefined);
  assert.equal(target?.trigger, undefined);
  assert.equal(offersRun(c), true);
});

// The target's trigger is a COPY. The catalog entry outlives the panel, and a
// form mutating its own target must not edit the cached construct underneath
// the tree.
test("the automation target copies the trigger rather than aliasing it", () => {
  const c = construct({
    kind: "automation",
    runnableKind: "automation",
    args: [],
    trigger: { event: "node.created", concept: "v1:x:y" },
  });
  const target = catalogAutomationTarget(c);
  assert.notEqual(target?.trigger, c.trigger, "the target must not alias the catalog entry's trigger");
  assert.deepEqual(target?.trigger, c.trigger);
});

test("a view-only construct gets neither kind of target", () => {
  // The tree already says it is not runnable; the absence IS the statement.
  const c = construct({ kind: "concept", runnable: false, runnableKind: undefined });
  assert.equal(catalogRunTarget(c), undefined);
  assert.equal(catalogAutomationTarget(c), undefined);
  assert.equal(offersRun(c), false);
  assert.equal(isAutomationRun(c), false);
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
