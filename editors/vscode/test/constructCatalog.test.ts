// The Constructs catalog: grouping, the two seams, and the states (memql#3752).
//
// TWO SEAMS ARE THE POINT OF THIS FILE. `ListConstructs` delivers two closed
// vocabularies the ENGINE owns as bare strings -- the kind and the argument
// type -- and the extension's run path types both more narrowly. Getting
// either wrong is invisible until someone tries to run something:
//
//   the kind: the catalog says `mutation`, the run path says `mutate`;
//   the arg type: the catalog says `string`, the form binds a six-value union.
//
// The third thing asserted here is the set of states, because every one of
// them exists to avoid the same wrong answer -- an empty list, which reads as
// "this cluster has no constructs".

import test from "node:test";
import assert from "node:assert/strict";

import type { Construct, ConstructArg } from "@znasllc-io/memql-sdk-core/constructs";

import {
  PROMOTED_NAMESPACE,
  catalogFrom,
  classifyCatalogFailure,
  groupByKind,
  toCatalogConstruct,
  toRunnableArg,
} from "../src/state/constructCatalog.js";
import { signatureLine } from "../src/constructs/signature.js";

function arg(over: Partial<ConstructArg> = {}): ConstructArg {
  return {
    name: "spaceId",
    type: "string",
    required: true,
    enum: [],
    description: "",
    autoInjected: false,
    ...over,
  };
}

function construct(over: Partial<Construct> = {}): Construct {
  return {
    name: "spaceParticipants",
    kind: "query",
    namespace: "cognition",
    origin: "core",
    originPath: "dsl/cognition/queries.memql",
    description: "Get space participants",
    runnable: true,
    args: [],
    boundConcept: "participant",
    sourceHash: "abc123",
    source: "",
    ...over,
  };
}

// -----------------------------------------------------------------------------
// seam 1: the kind
// -----------------------------------------------------------------------------

test("a mutation is runnable as `mutate`, which is what the run path calls it", () => {
  // The catalog reports the kind; the run path knows it by the keyword it is
  // AUTHORED under, and only this one differs. Getting it wrong means every
  // mutation in the tree silently loses its run affordance.
  const c = toCatalogConstruct(construct({ kind: "mutation", name: "createSpace" }));
  assert.equal(c.kind, "mutation");
  assert.equal(c.runnableKind, "mutate");
});

test("the other four runnable kinds map to themselves", () => {
  for (const kind of ["query", "logic", "tool", "automation"] as const) {
    assert.equal(toCatalogConstruct(construct({ kind })).runnableKind, kind);
  }
});

test("a view-only kind carries no runnable kind at all", () => {
  // Not a false flag or a disabled marker: absent. The tree and the page both
  // render the ABSENCE as the statement.
  const c = toCatalogConstruct(construct({ kind: "concept", runnable: false }));
  assert.equal(c.runnableKind, undefined);
  assert.equal(c.runnable, false);
});

test("runnable is the ENGINE's verdict, and a kind this client cannot map is not offered", () => {
  // Both conditions, not either. A construct the engine calls runnable whose
  // kind this client has never heard of has no target to build, so it must not
  // be offered -- but `runnable` itself is reported faithfully, because that is
  // the engine's answer and not this client's.
  const c = toCatalogConstruct(construct({ kind: "ritual", runnable: true }));
  assert.equal(c.runnable, true);
  assert.equal(c.runnableKind, undefined);
});

test("a kind the engine says is NOT runnable is never offered, whatever its name", () => {
  const c = toCatalogConstruct(construct({ kind: "query", runnable: false }));
  assert.equal(c.runnableKind, undefined);
});

// -----------------------------------------------------------------------------
// seam 2: the argument type
// -----------------------------------------------------------------------------

test("the six form types pass through", () => {
  for (const type of ["string", "number", "boolean", "object", "array", "any"]) {
    assert.equal(toRunnableArg(arg({ type })).type, type);
  }
});

test("a type this client has never heard of narrows to `any`, not to `string`", () => {
  // The rule constructs/runnable.ts already states for the LSP path: an arg
  // the editor cannot type still gets a JSON entry box instead of blocking the
  // run. Defaulting to `string` would render a text box for a value the engine
  // will refuse -- confidently wrong rather than cautious.
  assert.equal(toRunnableArg(arg({ type: "duration" })).type, "any");
  assert.equal(toRunnableArg(arg({ type: "" })).type, "any");
});

test("the optional arg fields are omitted rather than emptied", () => {
  // `RunnableArg` marks them optional, and the form checks presence -- an
  // empty string or an empty array is not the same as "not declared".
  const plain = toRunnableArg(arg());
  assert.equal(plain.enum, undefined);
  assert.equal(plain.description, undefined);
  assert.equal(plain.autoInjected, undefined);

  const full = toRunnableArg(
    arg({ enum: ["a", "b"], description: "which one", autoInjected: true }),
  );
  assert.deepEqual(full.enum, ["a", "b"]);
  assert.equal(full.description, "which one");
  assert.equal(full.autoInjected, true);
});

// -----------------------------------------------------------------------------
// grouping
// -----------------------------------------------------------------------------

test("groups are ordered runnable-first, and counted", () => {
  const groups = groupByKind(
    [
      construct({ kind: "concept", name: "v1:cognition:space", runnable: false }),
      construct({ kind: "query", name: "a" }),
      construct({ kind: "query", name: "b" }),
      construct({ kind: "mutation", name: "c" }),
    ].map(toCatalogConstruct),
  );
  assert.deepEqual(groups.map((g) => g.kind), ["query", "mutation", "concept"]);
  assert.deepEqual(groups.map((g) => g.count), [2, 1, 1]);
  assert.deepEqual(groups.map((g) => g.label), ["queries", "mutations", "concepts"]);
  assert.deepEqual(groups.map((g) => g.runnable), [true, true, false]);
});

test("a kind the vocabulary does not know still renders, at the end", () => {
  // Dropping it would make this view quietly disagree with the cluster it is
  // describing -- and a client is expected to outlive several engine releases.
  const groups = groupByKind(
    [construct({ kind: "ritual", runnable: false }), construct({ kind: "query" })].map(
      toCatalogConstruct,
    ),
  );
  assert.deepEqual(groups.map((g) => g.kind), ["query", "ritual"]);
  // With no label of its own it renders under its raw name rather than blank.
  assert.equal(groups[1].label, "ritual");
});

test("namespaces are grouped and sorted, and so are the constructs in them", () => {
  const groups = groupByKind(
    [
      construct({ name: "z", namespace: "identity" }),
      construct({ name: "b", namespace: "cognition" }),
      construct({ name: "a", namespace: "cognition" }),
    ].map(toCatalogConstruct),
  );
  assert.deepEqual(groups[0].namespaces.map((n) => n.namespace), ["cognition", "identity"]);
  assert.deepEqual(groups[0].namespaces[0].constructs.map((c) => c.name), ["a", "b"]);
});

test("a promoted construct has no namespace, and groups under a named heading", () => {
  // It was never authored into a domain. An empty heading would hide the one
  // group a developer most needs to notice.
  const groups = groupByKind(
    [construct({ name: "trained", namespace: "", origin: "promoted", originPath: "" })].map(
      toCatalogConstruct,
    ),
  );
  assert.equal(groups[0].namespaces[0].namespace, PROMOTED_NAMESPACE);
});

test("all three origins survive to the model", () => {
  for (const origin of ["core", "bundle", "promoted"] as const) {
    assert.equal(toCatalogConstruct(construct({ origin })).origin, origin);
  }
});

test("catalogFrom reports the total alongside the groups", () => {
  const state = catalogFrom([construct({ name: "a" }), construct({ name: "b", kind: "concept" })]);
  assert.equal(state.kind, "loaded");
  assert.ok(state.kind === "loaded");
  assert.equal(state.total, 2);
  assert.equal(state.groups.length, 2);
});

test("a connected cluster with no constructs is a fact, not a state to explain", () => {
  const state = catalogFrom([]);
  assert.ok(state.kind === "loaded");
  assert.deepEqual(state.groups, []);
  assert.equal(state.total, 0);
});

// -----------------------------------------------------------------------------
// the states -- every one of them exists to avoid an empty list
// -----------------------------------------------------------------------------

test("a cluster predating the message is a stated version mismatch", () => {
  const state = classifyCatalogFailure(new Error("listConstructs: unexpected reply envelope"));
  assert.equal(state.kind, "versionMismatch");
  assert.ok(state.kind === "versionMismatch");
  assert.match(state.message, /ListConstructs/);
});

test("any other failure keeps the engine's own words", () => {
  const state = classifyCatalogFailure(new Error("listConstructs: PERMISSION_DENIED: nope"));
  assert.equal(state.kind, "failed");
  assert.ok(state.kind === "failed");
  // Verbatim: the operator may need to match it against a log line.
  assert.match(state.message, /PERMISSION_DENIED: nope/);
});

test("a non-Error rejection still produces a message rather than [object Object]", () => {
  const state = classifyCatalogFailure("the socket closed");
  assert.ok(state.kind === "failed");
  assert.equal(state.message, "the socket closed");
});

// -----------------------------------------------------------------------------
// jump-to-source
// -----------------------------------------------------------------------------

const SOURCE = [
  "use cognition.concepts.{ participant }",
  "",
  "@description(\"Get space participants\")",
  "query participant spaceParticipants {",
  "  filter spaceId==args.spaceId",
  "}",
  "",
  "query participant spaceParticipantsByRole {",
  "}",
  "",
  "mutate space createSpace {",
  "}",
  "",
  "concept space {",
  "}",
].join("\n");

test("the signature line is found through the binding identifier", () => {
  // `query participant spaceParticipants` -- three identifiers, because
  // query/mutate/shape/spec/seed carry a signature binding.
  assert.equal(signatureLine(SOURCE, "query", "spaceParticipants"), 3);
});

test("a longer name that starts the same is not mistaken for it", () => {
  // Without the word boundary, `spaceParticipants` matches
  // `spaceParticipantsByRole` first and lands three lines late.
  assert.equal(signatureLine(SOURCE, "query", "spaceParticipantsByRole"), 7);
});

test("a mutation is searched under the keyword it is AUTHORED with", () => {
  // The catalog says `mutation`; the file says `mutate`. Searching for the
  // catalog spelling finds nothing.
  assert.equal(signatureLine(SOURCE, "mutation", "createSpace"), 10);
});

test("a concept is searched under its bare name, not its canonical id", () => {
  assert.equal(signatureLine(SOURCE, "concept", "v1:cognition:space"), 13);
});

test("a miss is -1, so the caller opens the file at the top rather than guessing", () => {
  // Landing on the wrong line is worse than landing on the first one: a
  // developer reading a signature that is not the one they asked for has no
  // cue that anything went wrong.
  assert.equal(signatureLine(SOURCE, "query", "notInThisFile"), -1);
  assert.equal(signatureLine(SOURCE, "query", ""), -1);
  assert.equal(signatureLine("", "query", "spaceParticipants"), -1);
});

test("a reference inside a body is not mistaken for a declaration", () => {
  const body = ["logic caller {", "  return query participant spaceParticipants()", "}"].join("\n");
  // The keyword has to OPEN the line. Otherwise the first mention of a
  // construct anywhere in the file wins.
  assert.equal(signatureLine(body, "query", "spaceParticipants"), -1);
});
