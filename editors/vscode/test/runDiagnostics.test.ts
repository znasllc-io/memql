// Diagnostic coordinate mapping -- and the zero-means-no-position rule.
//
// The engine reports against the BUNDLE, 1-based; the developer is looking at
// FILES, and VS Code wants 0-based. Getting the arithmetic wrong puts errors
// on the wrong line of the wrong file, which reads to the developer as a
// compiler bug rather than as a mapping bug -- so it is the kind of thing they
// will not report, they will just stop trusting the panel.
//
// The zero case is called out separately in the design's error-handling table
// and in the acceptance criteria, because it is the one that looks like it
// works. `line: 0` decoded as a coordinate is a perfectly valid line index, so
// nothing throws and nothing looks broken -- the diagnostic simply lands on
// the first line of the bundle's FIRST file, which is a dirty dependency far
// more often than it is the file being edited. The engine emits no position
// rather than a wrong one (memql#2375), so this is the common case, not an
// edge.

import test from "node:test";
import assert from "node:assert/strict";

import type { AuthoringDiagnostic } from "@znasllc-io/memql-sdk-core/authoring";

import { assembleBundle, type WorkspaceSources } from "../src/run/bundle.js";
import { groupByFile, mapBundleDiagnostics } from "../src/run/diagnostics.js";

function workspace(files: Record<string, { text: string; dirty: boolean }>): WorkspaceSources {
  return {
    resolveImport: (dotted) => `/ws/dsl/${dotted.split(".").join("/")}.memql`,
    read: (path) => files[path],
  };
}

function diag(overrides: Partial<AuthoringDiagnostic> = {}): AuthoringDiagnostic {
  return {
    name: "q",
    kind: "query",
    ok: false,
    skipped: false,
    error: "unexpected token",
    line: 0,
    column: 0,
    endLine: 0,
    endColumn: 0,
    ...overrides,
  };
}

// A two-file bundle. Bundle lines, 0-based:
//
//   0  "shape dep { }"            <- /ws/dsl/a/dep.memql, buffer line 0
//   1  "use a.dep.{ x }"          <- /ws/active.memql,    buffer line 0
//   2  "query q {"                                        buffer line 1
//   3  "  filter broken =="                               buffer line 2
//   4  "}"                                                buffer line 3
//
// The engine's 1-based lines are one higher than each index above.
function twoFileBundle() {
  return assembleBundle(
    "/ws/active.memql",
    "use a.dep.{ x }\nquery q {\n  filter broken ==\n}\n",
    workspace({
      "/ws/dsl/a/dep.memql": { text: "shape dep { }\n", dirty: true },
    }),
  );
}

const BROKEN_LINE = "  filter broken ==";

// -----------------------------------------------------------------------------
// The zero rule
// -----------------------------------------------------------------------------

test("mapBundleDiagnostics -- line 0 becomes a FILE-LEVEL diagnostic on the ACTIVE file", () => {
  const bundle = twoFileBundle();
  const [mapped] = mapBundleDiagnostics([diag({ line: 0 })], bundle);
  assert.ok(mapped);
  assert.equal(mapped.fileLevel, true);
  // Not the bundle's first file -- the developer invoked the run from the
  // active buffer, and that is the Problems entry they will look at.
  assert.equal(mapped.path, "/ws/active.memql");
  assert.equal(mapped.start.line, 0);
});

test("mapBundleDiagnostics -- a file-level message says the position is missing", () => {
  // Without this the developer sees a diagnostic about a construct declared
  // on line 40 sitting at the top of the file and concludes the mapping is
  // broken.
  const [mapped] = mapBundleDiagnostics([diag({ line: 0, error: "bind failed" })], twoFileBundle());
  assert.match(mapped?.message ?? "", /no source position/);
  assert.match(mapped?.message ?? "", /bind failed/);
});

test("mapBundleDiagnostics -- line 1 is a REAL position, not the sentinel", () => {
  // The boundary that makes the zero rule non-obvious: 1-based line 1 is the
  // first line of the bundle and is perfectly valid.
  const [mapped] = mapBundleDiagnostics([diag({ line: 1, column: 1 })], twoFileBundle());
  assert.equal(mapped?.fileLevel, false);
  assert.equal(mapped?.path, "/ws/dsl/a/dep.memql");
  assert.equal(mapped?.start.line, 0);
  assert.equal(mapped?.start.character, 0);
});

test("mapBundleDiagnostics -- column 0 with a real line is start-of-line, not file-level", () => {
  // The engine can know the line and not the column; that is still a usable
  // position and must not be thrown away.
  const [mapped] = mapBundleDiagnostics([diag({ line: 5, column: 0 })], twoFileBundle());
  assert.equal(mapped?.fileLevel, false);
  assert.equal(mapped?.start.character, 0);
});

// -----------------------------------------------------------------------------
// Coordinate arithmetic
// -----------------------------------------------------------------------------

test("mapBundleDiagnostics -- a bundle line inside the ACTIVE file maps to its buffer line", () => {
  // Bundle line 4 (1-based) is index 3; the active file starts at index 1, so
  // this is buffer line 2 -- the `filter broken ==` line.
  const bundle = twoFileBundle();
  const [mapped] = mapBundleDiagnostics([diag({ line: 4, column: 3 })], bundle);
  assert.equal(mapped?.path, "/ws/active.memql");
  assert.equal(mapped?.start.line, 2);
  assert.equal(mapped?.start.character, 2);
});

test("mapBundleDiagnostics -- a bundle line inside a DEPENDENCY maps to that file", () => {
  const [mapped] = mapBundleDiagnostics([diag({ line: 1, column: 1 })], twoFileBundle());
  assert.equal(mapped?.path, "/ws/dsl/a/dep.memql");
  assert.equal(mapped?.start.line, 0);
});

test("mapBundleDiagnostics -- with no end anchor the range widens to end of line", () => {
  // An empty range renders as a zero-width caret with no squiggle: the
  // diagnostic would occupy a Problems row and be invisible in the editor.
  const [mapped] = mapBundleDiagnostics([diag({ line: 4, column: 3, endLine: 0 })], twoFileBundle());
  assert.equal(mapped?.end.line, 2);
  assert.equal(mapped?.end.character, BROKEN_LINE.length);
});

test("mapBundleDiagnostics -- a real end anchor is honoured", () => {
  const [mapped] = mapBundleDiagnostics(
    [diag({ line: 4, column: 3, endLine: 4, endColumn: 9 })],
    twoFileBundle(),
  );
  assert.equal(mapped?.start.character, 2);
  assert.equal(mapped?.end.line, 2);
  assert.equal(mapped?.end.character, 8);
});

test("mapBundleDiagnostics -- an end anchor outside the file falls back to end of line", () => {
  // A range spanning a file boundary does not exist in the editor; the
  // boundary is an artefact of concatenation.
  const [mapped] = mapBundleDiagnostics(
    [diag({ line: 4, column: 3, endLine: 99, endColumn: 2 })],
    twoFileBundle(),
  );
  assert.equal(mapped?.end.line, 2);
  assert.equal(mapped?.end.character, BROKEN_LINE.length);
});

test("mapBundleDiagnostics -- a backwards end anchor widens instead of inverting", () => {
  const [mapped] = mapBundleDiagnostics(
    [diag({ line: 4, column: 10, endLine: 4, endColumn: 2 })],
    twoFileBundle(),
  );
  assert.ok((mapped?.end.character ?? 0) >= (mapped?.start.character ?? 0));
});

test("mapBundleDiagnostics -- a line past the end of the bundle degrades to file-level", () => {
  const [mapped] = mapBundleDiagnostics([diag({ line: 500, column: 1 })], twoFileBundle());
  assert.equal(mapped?.fileLevel, true);
  assert.equal(mapped?.path, "/ws/active.memql");
});

// -----------------------------------------------------------------------------
// What counts as a failure
// -----------------------------------------------------------------------------

test("mapBundleDiagnostics -- a SKIPPED construct is not a diagnostic", () => {
  // A bundle routinely carries a shape or a concept, and each reports ok=false
  // with skipped=true without failing the bundle. Rendering those would put
  // permanent noise under every file that declares one.
  const mapped = mapBundleDiagnostics(
    [diag({ ok: false, skipped: true, kind: "shape", error: "kind not compiled", line: 1 })],
    twoFileBundle(),
  );
  assert.deepEqual(mapped, []);
});

test("mapBundleDiagnostics -- a successful construct is not a diagnostic", () => {
  assert.deepEqual(mapBundleDiagnostics([diag({ ok: true, line: 1 })], twoFileBundle()), []);
});

test("mapBundleDiagnostics -- an empty error still produces a usable message", () => {
  const [mapped] = mapBundleDiagnostics([diag({ line: 4, column: 1, error: "" })], twoFileBundle());
  assert.match(mapped?.message ?? "", /failed to compile/);
});

// -----------------------------------------------------------------------------
// groupByFile
// -----------------------------------------------------------------------------

test("groupByFile -- buckets per file, preserving order", () => {
  const bundle = twoFileBundle();
  const grouped = groupByFile(
    mapBundleDiagnostics(
      [diag({ line: 1, column: 1, name: "dep" }), diag({ line: 4, column: 1, name: "q" }), diag({ line: 5, column: 1, name: "q2" })],
      bundle,
    ),
  );
  assert.equal(grouped.size, 2);
  assert.equal(grouped.get("/ws/dsl/a/dep.memql")?.length, 1);
  assert.equal(grouped.get("/ws/active.memql")?.length, 2);
});
