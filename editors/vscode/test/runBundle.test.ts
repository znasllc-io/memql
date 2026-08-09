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
  type WorkspaceSources,
} from "../src/run/bundle.js";

// A tiny in-memory workspace. `resolveImport` mirrors the real layout
// (dsl/<ns>/<file>.memql) closely enough to exercise the walk.
//
// `imports` STANDS IN FOR THE LANGUAGE SERVER. Since memql#3335 the walk asks
// `memql/imports` which modules a file declares, so a unit test running under
// bare `node --test` -- with no server to talk to -- has to supply the answer.
// The stub below is a crude line scan, which is fine and deliberately NOT
// evidence of anything: what counts as an import is decided by the compiler's
// own lexer, and THAT is proven in Go (component/memql/sense/imports_test.go,
// including the block-comment case a scan like this one gets wrong). These
// tests are about the WALK -- dirty vs clean, transitivity, cycles, offsets --
// which is the part that lives here.
const stubServerImports = (text: string): string[] => {
  const out: string[] = [];
  const re = /^[ \t]*use[ \t]+([A-Za-z_][A-Za-z0-9_.]*)\.\{/;
  for (const line of text.split("\n")) {
    const m = re.exec(line);
    if (m?.[1] !== undefined && !out.includes(m[1])) out.push(m[1]);
  }
  return out;
};

function workspace(
  files: Record<string, { text: string; dirty: boolean }>,
  imports: (path: string, text: string) => Promise<string[]> | string[] = (_p, text) =>
    stubServerImports(text),
): WorkspaceSources {
  return {
    resolveImport: (dotted) => `/ws/dsl/${dotted.split(".").join("/")}.memql`,
    read: (path) => files[path],
    imports,
  };
}

// -----------------------------------------------------------------------------
// assembleBundle
// -----------------------------------------------------------------------------

test("assembleBundle -- the active file alone when nothing else is dirty", async () => {
  const bundle = await assembleBundle("/ws/q.memql", "query q { }\n", workspace({}));
  assert.equal(bundle.sources, "query q { }\n");
  assert.equal(bundle.files.length, 1);
  assert.equal(bundle.files[0]?.path, "/ws/q.memql");
  assert.equal(bundle.files[0]?.startLine, 0);
});

test("assembleBundle -- the active file is included even when it is SAVED", async () => {
  // A saved-but-not-deployed buffer still has to be injected, or the run
  // silently executes whatever the cluster already has. The active file is
  // never subject to the dirty test.
  const bundle = await assembleBundle("/ws/q.memql", "query q { }\n", workspace({
    "/ws/q.memql": { text: "stale on disk", dirty: false },
  }));
  assert.equal(bundle.sources, "query q { }\n");
  // And its CURRENT text wins over whatever `read` would have returned.
  assert.equal(bundle.files[0]?.lines[0], "query q { }");
});

test("assembleBundle -- a DIRTY imported file joins the bundle", async () => {
  const bundle = await assembleBundle(
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

test("assembleBundle -- a CLEAN imported file stays out", async () => {
  // It resolves against the live registry, which by definition has it.
  const bundle = await assembleBundle(
    "/ws/q.memql",
    "use cognition.shapes.{ full }\nquery q { }\n",
    workspace({
      "/ws/dsl/cognition/shapes.memql": { text: "shape s full { }\n", dirty: false },
    }),
  );
  assert.equal(bundle.files.length, 1);
  assert.equal(bundle.files[0]?.path, "/ws/q.memql");
});

test("assembleBundle -- a dirty file reached THROUGH a clean one is still included", async () => {
  // The walk traverses clean files rather than stopping at them. Stopping
  // would silently run the deployed version of an edit the developer is
  // looking at, two hops away.
  const bundle = await assembleBundle(
    "/ws/q.memql",
    "use a.mid.{ x }\nquery q { }\n",
    workspace({
      "/ws/dsl/a/mid.memql": { text: "use a.leaf.{ y }\nshape mid { }\n", dirty: false },
      "/ws/dsl/a/leaf.memql": { text: "shape leaf { }\n", dirty: true },
    }),
  );
  assert.deepEqual(bundle.files.map((f) => f.path), ["/ws/dsl/a/leaf.memql", "/ws/q.memql"]);
});

test("assembleBundle -- an import cycle terminates", async () => {
  const bundle = await assembleBundle(
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

test("assembleBundle -- a file importing the ACTIVE file cannot duplicate it", async () => {
  // `visited` is seeded with the active path precisely so a back-reference
  // cannot enqueue it a second time and repeat every construct in it, which
  // the sandbox would then report as a redefinition.
  const bundle = await assembleBundle(
    "/ws/dsl/a/q.memql",
    "use a.dep.{ x }\nquery q { }\n",
    workspace({
      "/ws/dsl/a/dep.memql": { text: "use a.q.{ q }\nshape dep { }\n", dirty: true },
    }),
  );
  assert.equal(bundle.files.filter((f) => f.path === "/ws/dsl/a/q.memql").length, 1);
});

test("assembleBundle -- an unresolvable or unreadable import is skipped", async () => {
  const bundle = await assembleBundle(
    "/ws/q.memql",
    "use nowhere.at.all.{ x }\nquery q { }\n",
    workspace({}),
  );
  assert.equal(bundle.files.length, 1);
});

// -----------------------------------------------------------------------------
// The offset table
// -----------------------------------------------------------------------------

test("assembleBundle -- startLine offsets are cumulative and 0-based", async () => {
  const bundle = await assembleBundle(
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

test("assembleBundle -- a file with no trailing newline does not glue onto the next", async () => {
  // Without normalisation the dependency's last line and the active file's
  // first would become one line, corrupting the source AND shifting every
  // subsequent offset by one -- so every diagnostic in the active file would
  // land one line high.
  const bundle = await assembleBundle(
    "/ws/q.memql",
    "use a.b.{ x }\nquery q { }",
    workspace({ "/ws/dsl/a/b.memql": { text: "shape b { }", dirty: true } }),
  );
  assert.equal(bundle.sources, "shape b { }\nuse a.b.{ x }\nquery q { }\n");
  assert.equal(bundle.files[1]?.startLine, 1);
});

test("bundleFileAt -- resolves a bundle line to its file", async () => {
  const bundle = await assembleBundle(
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

test("bundleFileAt -- a line past the end resolves to nothing, it does not clamp", async () => {
  // A position past the source means the offset table and the engine disagree
  // about what was submitted. Clamping into the last file would invent a
  // location and hide the disagreement.
  const bundle = await assembleBundle("/ws/q.memql", "a\n", workspace({}));
  assert.equal(bundleFileAt(bundle, 1), undefined);
  assert.equal(bundleFileAt(bundle, -1), undefined);
});

// -----------------------------------------------------------------------------
// The import graph comes from the LANGUAGE SERVER (memql#3335)
// -----------------------------------------------------------------------------

// The load-bearing property: the walk asks the server and uses THAT, rather
// than looking at the text itself. Proven by making the two disagree in both
// directions at once -- a `use` line in the text the server does not report,
// and a module the server reports that appears nowhere in the text.
//
// A surviving regex would follow `a.inText` and miss `a.fromServer`, so this
// fails loudly if the scan is ever reintroduced.
test("assembleBundle -- follows the server's answer, not the text", async () => {
  const bundle = await assembleBundle(
    "/ws/q.memql",
    "use a.inText.{ x }\nquery q { }\n",
    workspace(
      {
        "/ws/dsl/a/inText.memql": { text: "shape inText { }\n", dirty: true },
        "/ws/dsl/a/fromServer.memql": { text: "shape fromServer { }\n", dirty: true },
      },
      () => ["a.fromServer"],
    ),
  );
  assert.deepEqual(bundle.files.map((f) => f.path), [
    "/ws/dsl/a/fromServer.memql",
    "/ws/q.memql",
  ]);
});

// THE CASE THE REGEX GOT WRONG. A `use` inside a /* ... */ block comment is
// not an import; the lexer skips the comment outright, so the real server
// reports nothing for it (proven in component/memql/sense/imports_test.go).
// The walk must then leave that dirty file out of the bundle.
test("assembleBundle -- a block-commented import pulls nothing into the bundle", async () => {
  const active = ["/*", "use a.b.{ x }", "*/", "query q { }", ""].join("\n");
  const bundle = await assembleBundle(
    "/ws/q.memql",
    active,
    // What the real server returns for this buffer: no imports.
    workspace({ "/ws/dsl/a/b.memql": { text: "shape b { }\n", dirty: true } }, () => []),
  );
  assert.deepEqual(bundle.files.map((f) => f.path), ["/ws/q.memql"]);
});

// The graceful degradation when the server does not advertise the request: the
// adapter answers with no imports, so the bundle is the active file alone. A
// dirty dependency then runs as its deployed version -- the same bounded,
// pre-existing cost a mis-read line had. What must NOT happen is a fallback
// scan, which would be a second parser that only runs when nobody is looking.
test("assembleBundle -- no import support means the active file alone, never a fallback scan", async () => {
  const bundle = await assembleBundle(
    "/ws/q.memql",
    "use a.b.{ x }\nquery q { }\n",
    workspace({ "/ws/dsl/a/b.memql": { text: "shape b { }\n", dirty: true } }, () => []),
  );
  assert.deepEqual(bundle.files.map((f) => f.path), ["/ws/q.memql"]);
});

// The seam is awaited, so a genuinely asynchronous implementation (the real
// one is a JSON-RPC round trip) walks identically to the synchronous stub the
// other tests use.
test("assembleBundle -- an async import lookup walks transitively", async () => {
  const bundle = await assembleBundle(
    "/ws/q.memql",
    "use a.mid.{ x }\nquery q { }\n",
    workspace(
      {
        "/ws/dsl/a/mid.memql": { text: "use a.leaf.{ y }\nshape mid { }\n", dirty: false },
        "/ws/dsl/a/leaf.memql": { text: "shape leaf { }\n", dirty: true },
      },
      async (_path, text) => {
        await Promise.resolve();
        return stubServerImports(text);
      },
    ),
  );
  assert.deepEqual(bundle.files.map((f) => f.path), ["/ws/dsl/a/leaf.memql", "/ws/q.memql"]);
});

// The ACTIVE file is asked about too -- it is the root of the graph, and the
// path it is asked under is its own.
test("assembleBundle -- the active file's own path and text are what it is asked about", async () => {
  const asked: Array<[string, string]> = [];
  await assembleBundle("/ws/q.memql", "query q { }\n", workspace({}, (p, text) => {
    asked.push([p, text]);
    return [];
  }));
  assert.deepEqual(asked, [["/ws/q.memql", "query q { }\n"]]);
});
