// The closure rule, and the single-construct slice.
//
// The cases here are the ones where the training bundle and the RUN bundle
// disagree, because that is where a reader's intuition comes from the wrong
// file. run/bundle.ts includes a dependency when it has unsaved edits; this
// includes one when the cluster does not have it. A saved, never-promoted
// dependency is in one and out of the other, and it is the single most common
// shape in a workspace somebody is training constructs in.
//
// The slice cases are about a different failure: a demote takes a source and
// withdraws EVERY demotable construct it finds, so a slice that carries one
// construct too many silently withdraws something nobody asked about.
//
// Refs: #3763 #3745

import test from "node:test";
import assert from "node:assert/strict";

import {
  assembleClosure,
  assembleConstruct,
  constructWindow,
  type TrainingWorkspace,
} from "../src/training/closure.js";
import type { TrainingConstruct, TrainingState } from "../src/state/training.js";

// -----------------------------------------------------------------------------
// Harness
// -----------------------------------------------------------------------------

interface FileFixture {
  text: string;
  imports?: string[];
  /** undefined models the server declining to answer -- NOT a file with nothing in it. */
  constructs?: TrainingConstruct[] | undefined;
  /** Present but empty: a file that genuinely declares nothing. */
  declaresNothing?: boolean;
}

function construct(name: string, state: TrainingState, line = 0): TrainingConstruct {
  return {
    kind: "query",
    name,
    signatureRange: { start: { line, character: 0 }, end: { line, character: 10 } },
    state,
  };
}

function workspace(files: Record<string, FileFixture>): TrainingWorkspace {
  return {
    // The dotted path IS the path in this fixture -- resolution is the adapter's
    // job and has its own tests; what matters here is which files the walk
    // reaches and what it decides about them.
    resolveImport: (dotted) => (files[dotted] === undefined ? undefined : dotted),
    read: (p) => (files[p] === undefined ? undefined : { text: files[p]!.text }),
    imports: (p) => files[p]?.imports ?? [],
    trainingStates: (p) => {
      const file = files[p];
      if (file === undefined) return undefined;
      if (file.declaresNothing === true) return [];
      return file.constructs;
    },
  };
}

function included(bundle: { members: { path: string; included: boolean }[] }): string[] {
  return bundle.members.filter((m) => m.included).map((m) => m.path);
}

// -----------------------------------------------------------------------------
// The closure rule
// -----------------------------------------------------------------------------

test("an untrained dependency joins the bundle even though it has no unsaved edits", async () => {
  // THE CASE THE RUN BUNDLE GETS WRONG BY DESIGN. Nothing here is dirty --
  // TrainingWorkspace.read does not even carry the flag -- and the dependency
  // still travels, because "the cluster does not have it" is the question being
  // asked and "the developer has not touched it" is not.
  const ws = workspace({
    "/w/queries.memql": {
      text: "query q { }\n",
      imports: ["/w/specs.memql"],
      constructs: [construct("q", "untrained")],
    },
    "/w/specs.memql": { text: "spec s { }\n", constructs: [construct("s", "untrained")] },
  });

  const bundle = await assembleClosure("/w/queries.memql", "query q { }\n", ws);

  assert.deepEqual(included(bundle).sort(), ["/w/queries.memql", "/w/specs.memql"]);
  assert.ok(bundle.sources.includes("spec s"), "the dependency's source is in the bundle");
});

test("a dependency the cluster already has is left out", async () => {
  const ws = workspace({
    "/w/queries.memql": {
      text: "query q { }\n",
      imports: ["/w/traits.memql"],
      constructs: [construct("q", "untrained")],
    },
    "/w/traits.memql": { text: "trait t { }\n", constructs: [construct("t", "trained")] },
  });

  const bundle = await assembleClosure("/w/queries.memql", "query q { }\n", ws);

  assert.deepEqual(included(bundle), ["/w/queries.memql"]);
  const trait = bundle.members.find((m) => m.path === "/w/traits.memql");
  assert.equal(trait?.reason, "onCluster");
});

test("a seeded dependency is left out -- it is on the cluster and cannot be promoted", async () => {
  // The distinction that matters: `seeded` is on the cluster but was never
  // promoted, so including it would ask the engine to let a promoted construct
  // shadow a core name, which it refuses. "On the cluster" is the test, not "was
  // promoted".
  const ws = workspace({
    "/w/queries.memql": {
      text: "query q { }\n",
      imports: ["/core/traits.memql"],
      constructs: [construct("q", "untrained")],
    },
    "/core/traits.memql": { text: "trait core { }\n", constructs: [construct("core", "seeded")] },
  });

  const bundle = await assembleClosure("/w/queries.memql", "query q { }\n", ws);
  assert.deepEqual(included(bundle), ["/w/queries.memql"]);
});

test("a drifted dependency joins -- the cluster has a different version of it", async () => {
  const ws = workspace({
    "/w/queries.memql": {
      text: "query q { }\n",
      imports: ["/w/specs.memql"],
      constructs: [construct("q", "trained")],
    },
    "/w/specs.memql": { text: "spec s { }\n", constructs: [construct("s", "drifted")] },
  });

  const bundle = await assembleClosure("/w/queries.memql", "query q { }\n", ws);
  assert.deepEqual(included(bundle).sort(), ["/w/queries.memql", "/w/specs.memql"]);
});

test("a dependency the server cannot classify is left out AND reported", async () => {
  // Both directions of this choice fail loudly, so it is the report that makes
  // it defensible: the developer is told an assumption was made on their behalf
  // rather than finding a file quietly missing from what they approved.
  const ws = workspace({
    "/w/queries.memql": {
      text: "query q { }\n",
      imports: ["/w/mystery.memql"],
      constructs: [construct("q", "untrained")],
    },
    "/w/mystery.memql": { text: "query m { }\n", constructs: undefined },
  });

  const bundle = await assembleClosure("/w/queries.memql", "query q { }\n", ws);

  assert.deepEqual(included(bundle), ["/w/queries.memql"]);
  const mystery = bundle.members.find((m) => m.path === "/w/mystery.memql");
  assert.equal(mystery?.reason, "unclassified");
  assert.deepEqual(mystery?.constructs, []);
});

test("a file that declares nothing is not the same as one the server could not describe", async () => {
  const ws = workspace({
    "/w/queries.memql": {
      text: "query q { }\n",
      imports: ["/w/empty.memql"],
      constructs: [construct("q", "untrained")],
    },
    "/w/empty.memql": { text: "// nothing here\n", declaresNothing: true },
  });

  const bundle = await assembleClosure("/w/queries.memql", "query q { }\n", ws);
  const empty = bundle.members.find((m) => m.path === "/w/empty.memql");
  // `onCluster`, not `unclassified`: the server answered, and its answer was
  // that there is nothing in the file the cluster could be missing.
  assert.equal(empty?.reason, "onCluster");
  assert.equal(empty?.included, false);
});

test("the walk goes THROUGH a file the cluster has to reach one it does not", async () => {
  // Stopping at an on-cluster file would leave the untrained construct behind it
  // out of the promote -- the half-landing the closure exists to prevent.
  const ws = workspace({
    "/w/a.memql": {
      text: "query a { }\n",
      imports: ["/w/b.memql"],
      constructs: [construct("a", "untrained")],
    },
    "/w/b.memql": {
      text: "trait b { }\n",
      imports: ["/w/c.memql"],
      constructs: [construct("b", "trained")],
    },
    "/w/c.memql": { text: "spec c { }\n", constructs: [construct("c", "untrained")] },
  });

  const bundle = await assembleClosure("/w/a.memql", "query a { }\n", ws);
  assert.deepEqual(included(bundle).sort(), ["/w/a.memql", "/w/c.memql"]);
});

test("the active file is last in bundle order and its offsets map back", async () => {
  const ws = workspace({
    "/w/a.memql": {
      text: "query a { }\n",
      imports: ["/w/dep.memql"],
      constructs: [construct("a", "untrained")],
    },
    "/w/dep.memql": {
      text: "spec d1 { }\nspec d2 { }\n",
      constructs: [construct("d1", "untrained")],
    },
  });

  const bundle = await assembleClosure("/w/a.memql", "query a { }\n", ws);

  assert.equal(bundle.files[bundle.files.length - 1]?.path, "/w/a.memql");
  assert.equal(bundle.files[0]?.startLine, 0);
  // Two lines of dependency, so the active file starts at bundle line 2.
  assert.equal(bundle.files[1]?.startLine, 2);
});

test("an import cycle does not duplicate the active file", async () => {
  const ws = workspace({
    "/w/a.memql": {
      text: "query a { }\n",
      imports: ["/w/b.memql"],
      constructs: [construct("a", "untrained")],
    },
    "/w/b.memql": {
      text: "spec b { }\n",
      imports: ["/w/a.memql"],
      constructs: [construct("b", "untrained")],
    },
  });

  const bundle = await assembleClosure("/w/a.memql", "query a { }\n", ws);
  assert.deepEqual(
    bundle.files.map((f) => f.path),
    ["/w/b.memql", "/w/a.memql"],
  );
});

test("the active file travels whatever its state, including trained", async () => {
  const ws = workspace({
    "/w/a.memql": { text: "query a { }\n", constructs: [construct("a", "trained")] },
  });
  const bundle = await assembleClosure("/w/a.memql", "query a { }\n", ws);
  assert.deepEqual(included(bundle), ["/w/a.memql"]);
  assert.equal(bundle.members[0]?.reason, "active");
});

// -----------------------------------------------------------------------------
// The single-construct slice
// -----------------------------------------------------------------------------

const TWO_CONSTRUCTS = [
  "use cognition.concepts.{ space }",
  "",
  "@description(\"first\")",
  "query first {",
  "  filter a==1",
  "}",
  "",
  "@description(\"second\")",
  "query second {",
  "  filter b==2",
  "}",
  "",
].join("\n");

function twoConstructs(): TrainingConstruct[] {
  return [construct("first", "trained", 3), construct("second", "trained", 8)];
}

test("the demote slice declares the target and NOTHING else", async () => {
  const bundle = assembleConstruct("/w/a.memql", TWO_CONSTRUCTS, twoConstructs(), "first");
  assert.notEqual(bundle, undefined);
  assert.ok(bundle!.sources.includes("query first"), "the target survives");
  assert.equal(
    bundle!.sources.includes("query second"),
    false,
    "the neighbour must not be in a source that demotes everything it finds",
  );
  assert.ok(bundle!.sources.includes("filter a==1"), "the target's body survives");
});

test("the demote slice preserves the file's line numbering", async () => {
  const bundle = assembleConstruct("/w/a.memql", TWO_CONSTRUCTS, twoConstructs(), "second");
  assert.notEqual(bundle, undefined);
  const lines = bundle!.sources.split("\n");
  // Line 8 (0-based) is where `query second` sits in the original file; blanking
  // rather than cutting is what keeps it there, so any position the engine
  // reports against this source is a position in the file.
  assert.equal(lines[8], "query second {");
  assert.equal(lines[3], "", "the neighbour's signature line is blank, not removed");
});

test("the slice stops before the NEXT construct's annotations", async () => {
  // Left in, they would be an unclassifiable region in the source the engine
  // splits. Harmless in practice -- the demote filters to demotable kinds -- but
  // the window is meant to be the construct, and this is the edge it is drawn at.
  const window = constructWindow(TWO_CONSTRUCTS, twoConstructs(), "first");
  assert.deepEqual(window, { start: 3, end: 6 });
});

test("the trailing trim cannot eat the construct's own body", async () => {
  // A body ends in `}`, which is none of the three things the walk-back trims,
  // so the trim stops there however much blank space follows.
  const source = ["query only {", "  filter a==1", "}", "", "", ""].join("\n");
  const window = constructWindow(source, [construct("only", "trained", 0)], "only");
  assert.deepEqual(window, { start: 0, end: 3 });
});

test("a construct the file no longer declares yields no slice", async () => {
  assert.equal(
    assembleConstruct("/w/a.memql", TWO_CONSTRUCTS, twoConstructs(), "renamed"),
    undefined,
  );
});

test("the slice carries the construct's kind and state for the confirmation", async () => {
  const bundle = assembleConstruct("/w/a.memql", TWO_CONSTRUCTS, twoConstructs(), "first");
  assert.deepEqual(bundle!.members[0]?.constructs, [
    { kind: "query", name: "first", state: "trained" },
  ]);
});
