// The `memql/imports` contract, from the consumer's side (memql#3335).
//
// This request exists to delete a second parser: run/bundle.ts walked the
// import graph with a line regex, because there was no import request to ask.
// A regex cannot know what the lexer knows -- a `use` line inside a block
// comment is not an import -- and the failure in the other direction is the
// invisible class this epic kept tripping over: a missed dirty dependency
// means the run executes the SAVED copy of a file the developer is editing.
//
// What is asserted here is the DECODE. Whether the server got the imports
// right is proven in Go, against the real lexer
// (component/memql/sense/imports_test.go).

import test from "node:test";
import assert from "node:assert/strict";

import { importPaths, parseImports } from "../src/constructs/imports.js";

test("parseImports -- decodes the contract shape", () => {
  const out = parseImports({
    imports: [
      { path: "cognition.concepts", names: ["participant", "space"] },
      { path: "common.traits", names: ["isActiveRecord"] },
    ],
  });
  assert.deepEqual(out, [
    { path: "cognition.concepts", names: ["participant", "space"] },
    { path: "common.traits", names: ["isActiveRecord"] },
  ]);
});

// The language server is a separate process, possibly an OLDER build. Every
// malformation degrades rather than throwing: a thrown error here would abort
// a run the developer just started.
test("parseImports -- a non-object, a missing array and a null all yield an empty list", () => {
  for (const raw of [null, undefined, 42, "imports", {}, { imports: null }, { imports: "a.b" }]) {
    assert.deepEqual(parseImports(raw), []);
  }
});

test("parseImports -- an entry with no usable path is dropped, the rest survive", () => {
  const out = parseImports({
    imports: [
      { names: ["x"] },
      { path: "", names: ["x"] },
      { path: 7, names: ["x"] },
      null,
      { path: "common.traits", names: ["isActiveRecord"] },
    ],
  });
  assert.deepEqual(out, [{ path: "common.traits", names: ["isActiveRecord"] }]);
});

// `names` is not read by the bundle walk -- a file with any unsaved edit goes
// in whole, whichever of its constructs are referenced -- so a malformed one
// must not cost the dependency.
test("parseImports -- a malformed names field keeps the import and empties the names", () => {
  const out = parseImports({
    imports: [
      { path: "a.b" },
      { path: "c.d", names: null },
      { path: "e.f", names: ["ok", 3, null] },
    ],
  });
  assert.deepEqual(out, [
    { path: "a.b", names: [] },
    { path: "c.d", names: [] },
    { path: "e.f", names: ["ok"] },
  ]);
});

// The server reports declarations as AUTHORED, duplicates included -- two
// `use` lines naming one module are two declarations and one file. The walk
// wants the file, so the collapse happens here.
test("importPaths -- deduplicates by path, in first-appearance order", () => {
  const paths = importPaths([
    { path: "cognition.concepts", names: ["participant"] },
    { path: "common.traits", names: ["isActiveRecord"] },
    { path: "cognition.concepts", names: ["space"] },
  ]);
  assert.deepEqual(paths, ["cognition.concepts", "common.traits"]);
});

test("importPaths -- an empty list stays empty", () => {
  assert.deepEqual(importPaths([]), []);
});
