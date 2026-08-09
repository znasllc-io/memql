// Bundle assembly.
//
// Two acceptance criteria live here. "A buffer edit is reflected in the run
// without saving or redeploying" is the active file always going into the
// bundle whole, current text and all. And the dirty-transitive-dependency rule
// is what makes that true for an edit split across two files -- an edited
// shape in one buffer and the query using it in another is the ordinary way a
// change looks, and a bundle carrying only the query would validate the new
// query against the OLD shape and report a compile error the developer already
// fixed.
//
// The offset table is the other half. It is what the diagnostic mapper stands
// on, and an off-by-one there puts every error on the wrong line of the wrong
// file, which reads as a compiler bug rather than as a mapping bug.

import test from "node:test";
import assert from "node:assert/strict";

import {
  assembleBundle,
  bundleFileAt,
  parseUseImports,
  type WorkspaceSources,
} from "../src/run/bundle.js";

// A tiny in-memory workspace. `resolveImport` mirrors the real layout
// (dsl/<ns>/<file>.memql) closely enough to exercise the walk.
function workspace(files: Record<string, { text: string; dirty: boolean }>): WorkspaceSources {
  return {
    resolveImport: (dotted) => `/ws/dsl/${dotted.split(".").join("/")}.memql`,
    read: (path) => files[path],
  };
}

// -----------------------------------------------------------------------------
// parseUseImports
// -----------------------------------------------------------------------------

test("parseUseImports -- captures the dotted path of a brace import", () => {
  const source = [
    "use cognition.concepts.{ participant, space }",
    "use common.traits.{ isActiveRecord }",
    "",
    "query participant q { }",
  ].join("\n");
  assert.deepEqual(parseUseImports(source), ["cognition.concepts", "common.traits"]);
});

test("parseUseImports -- a multi-line brace list still yields its path", () => {
  const source = ["use cognition.shapes.{", "  participantFull,", "}", ""].join("\n");
  assert.deepEqual(parseUseImports(source), ["cognition.shapes"]);
});

test("parseUseImports -- deduplicates", () => {
  const source = "use a.b.{ x }\nuse a.b.{ y }\n";
  assert.deepEqual(parseUseImports(source), ["a.b"]);
});

test("parseUseImports -- the word `use` elsewhere on a line is not an import", () => {
  // Anchored to line start with only leading whitespace, and requiring the
  // `.{` the loader demands. A description mentioning the word must not pull
  // an unrelated file into the bundle.
  const source = [
    '@description("use this to find a space")',
    "  // we use cognition.concepts.{ x } here",
    "query q { }",
  ].join("\n");
  // The comment line DOES match -- the scan is textual, and matching a
  // commented-out import is the safe direction: the worst case is a file that
  // did not need to be in the bundle being compiled and reported on, whereas
  // missing a real import silently runs the deployed version of an edit.
  assert.deepEqual(parseUseImports(source), []);
});

test("parseUseImports -- no imports yields an empty list", () => {
  assert.deepEqual(parseUseImports("query q { }\n"), []);
});

// -----------------------------------------------------------------------------
// assembleBundle
// -----------------------------------------------------------------------------

test("assembleBundle -- the active file alone when nothing else is dirty", () => {
  const bundle = assembleBundle("/ws/q.memql", "query q { }\n", workspace({}));
  assert.equal(bundle.sources, "query q { }\n");
  assert.equal(bundle.files.length, 1);
  assert.equal(bundle.files[0]?.path, "/ws/q.memql");
  assert.equal(bundle.files[0]?.startLine, 0);
});

test("assembleBundle -- the active file is included even when it is SAVED", () => {
  // A saved-but-not-deployed buffer still has to be injected, or the run
  // silently executes whatever the cluster already has. The active file is
  // never subject to the dirty test.
  const bundle = assembleBundle("/ws/q.memql", "query q { }\n", workspace({
    "/ws/q.memql": { text: "stale on disk", dirty: false },
  }));
  assert.equal(bundle.sources, "query q { }\n");
  // And its CURRENT text wins over whatever `read` would have returned.
  assert.equal(bundle.files[0]?.lines[0], "query q { }");
});

test("assembleBundle -- a DIRTY imported file joins the bundle", () => {
  const bundle = assembleBundle(
    "/ws/q.memql",
    "use cognition.shapes.{ full }\nquery q { }\n",
    workspace({
      "/ws/dsl/cognition/shapes.memql": { text: "shape s full { }\n", dirty: true },
    }),
  );
  assert.equal(bundle.files.length, 2);
  // Dependencies first, ACTIVE LAST -- the mapper's fallback file relies on
  // that last position.
  assert.equal(bundle.files[0]?.path, "/ws/dsl/cognition/shapes.memql");
  assert.equal(bundle.files[1]?.path, "/ws/q.memql");
  // The active file goes in WHOLE, `use` line included -- the bundle is
  // source the engine compiles, so stripping the import would leave the
  // dependency's constructs unreferenced.
  assert.equal(
    bundle.sources,
    "shape s full { }\nuse cognition.shapes.{ full }\nquery q { }\n",
  );
});

test("assembleBundle -- a CLEAN imported file stays out", () => {
  // It resolves against the live registry, which by definition has it.
  const bundle = assembleBundle(
    "/ws/q.memql",
    "use cognition.shapes.{ full }\nquery q { }\n",
    workspace({
      "/ws/dsl/cognition/shapes.memql": { text: "shape s full { }\n", dirty: false },
    }),
  );
  assert.equal(bundle.files.length, 1);
  assert.equal(bundle.files[0]?.path, "/ws/q.memql");
});

test("assembleBundle -- a dirty file reached THROUGH a clean one is still included", () => {
  // The walk traverses clean files rather than stopping at them. Stopping
  // would silently run the deployed version of an edit the developer is
  // looking at, two hops away.
  const bundle = assembleBundle(
    "/ws/q.memql",
    "use a.mid.{ x }\nquery q { }\n",
    workspace({
      "/ws/dsl/a/mid.memql": { text: "use a.leaf.{ y }\nshape mid { }\n", dirty: false },
      "/ws/dsl/a/leaf.memql": { text: "shape leaf { }\n", dirty: true },
    }),
  );
  assert.deepEqual(bundle.files.map((f) => f.path), ["/ws/dsl/a/leaf.memql", "/ws/q.memql"]);
});

test("assembleBundle -- an import cycle terminates", () => {
  const bundle = assembleBundle(
    "/ws/q.memql",
    "use a.one.{ x }\nquery q { }\n",
    workspace({
      "/ws/dsl/a/one.memql": { text: "use a.two.{ y }\nshape one { }\n", dirty: true },
      "/ws/dsl/a/two.memql": { text: "use a.one.{ x }\nshape two { }\n", dirty: true },
    }),
  );
  assert.deepEqual(bundle.files.map((f) => f.path), [
    "/ws/dsl/a/one.memql",
    "/ws/dsl/a/two.memql",
    "/ws/q.memql",
  ]);
});

test("assembleBundle -- a file importing the ACTIVE file cannot duplicate it", () => {
  // `visited` is seeded with the active path precisely so a back-reference
  // cannot enqueue it a second time and repeat every construct in it, which
  // the sandbox would then report as a redefinition.
  const bundle = assembleBundle(
    "/ws/dsl/a/q.memql",
    "use a.dep.{ x }\nquery q { }\n",
    workspace({
      "/ws/dsl/a/dep.memql": { text: "use a.q.{ q }\nshape dep { }\n", dirty: true },
    }),
  );
  assert.equal(bundle.files.filter((f) => f.path === "/ws/dsl/a/q.memql").length, 1);
});

test("assembleBundle -- an unresolvable or unreadable import is skipped", () => {
  const bundle = assembleBundle(
    "/ws/q.memql",
    "use nowhere.at.all.{ x }\nquery q { }\n",
    workspace({}),
  );
  assert.equal(bundle.files.length, 1);
});

// -----------------------------------------------------------------------------
// The offset table
// -----------------------------------------------------------------------------

test("assembleBundle -- startLine offsets are cumulative and 0-based", () => {
  const bundle = assembleBundle(
    "/ws/q.memql",
    "use a.b.{ x }\nline1\nline2\n",
    workspace({
      "/ws/dsl/a/b.memql": { text: "d1\nd2\nd3\n", dirty: true },
    }),
  );
  // [dep(3 lines), active(3 lines)] -- the active file's own `use` line is
  // one of its three.
  assert.equal(bundle.files[0]?.startLine, 0);
  assert.equal(bundle.files[0]?.lines.length, 3);
  assert.equal(bundle.files[1]?.startLine, 3);
  assert.equal(bundle.files[1]?.lines.length, 3);
});

test("assembleBundle -- a file with no trailing newline does not glue onto the next", () => {
  // Without normalisation the dependency's last line and the active file's
  // first would become one line, corrupting the source AND shifting every
  // subsequent offset by one -- so every diagnostic in the active file would
  // land one line high.
  const bundle = assembleBundle(
    "/ws/q.memql",
    "use a.b.{ x }\nquery q { }",
    workspace({ "/ws/dsl/a/b.memql": { text: "shape b { }", dirty: true } }),
  );
  assert.equal(bundle.sources, "shape b { }\nuse a.b.{ x }\nquery q { }\n");
  assert.equal(bundle.files[1]?.startLine, 1);
});

test("bundleFileAt -- resolves a bundle line to its file", () => {
  const bundle = assembleBundle(
    "/ws/q.memql",
    "use a.b.{ x }\na\nb\n",
    workspace({ "/ws/dsl/a/b.memql": { text: "c\n", dirty: true } }),
  );
  // [dep: bundle line 0] [active: bundle lines 1..3]
  assert.equal(bundleFileAt(bundle, 0)?.path, "/ws/dsl/a/b.memql");
  assert.equal(bundleFileAt(bundle, 1)?.path, "/ws/q.memql");
  assert.equal(bundleFileAt(bundle, 3)?.path, "/ws/q.memql");
  assert.equal(bundleFileAt(bundle, 4), undefined);
});

test("bundleFileAt -- a line past the end resolves to nothing, it does not clamp", () => {
  // A position past the source means the offset table and the engine disagree
  // about what was submitted. Clamping into the last file would invent a
  // location and hide the disagreement.
  const bundle = assembleBundle("/ws/q.memql", "a\n", workspace({}));
  assert.equal(bundleFileAt(bundle, 1), undefined);
  assert.equal(bundleFileAt(bundle, -1), undefined);
});
