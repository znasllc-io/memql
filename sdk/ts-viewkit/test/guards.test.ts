// The guards that keep the element library generic.
//
// Everything else in this suite checks that an element does the right thing.
// These check that it CANNOT do the wrong thing: a concept id literal in a
// renderer, or a branch on which concept is being rendered, is the failure
// mode the whole display-card + fitness design exists to prevent, and it is
// the kind of mistake that looks reasonable in a diff ("just special-case
// this one concept") and is very expensive later.
//
// The scan reads source, not behaviour, because behaviour only covers the
// branches a fixture happens to reach.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";

import { SRC_DIR, sourceFiles, stripComments } from "./support/source.js";

test("the comment stripper keeps strings and drops comments (guards the guard)", () => {
  const sample = `const a = "v1:x:y"; // v1:comment:only\n/* v1:block:only */ const b = 'z';`;
  const stripped = stripComments(sample);
  assert.match(stripped, /"v1:x:y"/, "a string literal must survive");
  assert.ok(!stripped.includes("v1:comment:only"), "a line comment must be dropped");
  assert.ok(!stripped.includes("v1:block:only"), "a block comment must be dropped");
});

test("no source file contains a concept id literal", () => {
  const offenders: string[] = [];
  for (const { file, source } of sourceFiles()) {
    for (const m of stripComments(source).matchAll(/v\d+:[a-zA-Z]+:[a-zA-Z]/g)) {
      offenders.push(`${file}: ${m[0]}...`);
    }
  }
  assert.deepEqual(
    offenders,
    [],
    `a concept id appears in view-kit source: ${offenders.join(", ")}. Elements ` +
      `render whatever rows they are handed, through @displayCard slots and the ` +
      `fitness bindings; a concept id in a renderer means one concept renders ` +
      `differently from the rest, and the next concept declared gets nothing. ` +
      `If a concept needs special treatment, express it as a REQUIREMENT the ` +
      `concept's fields can satisfy.`,
  );
});

test("no source file branches on which concept it is rendering", () => {
  // The subtler form of the same mistake: no literal, but a comparison on the
  // concept's identity. `concept.entity` is fine as DISPLAY (the empty state
  // says "No rows for node"); comparing it is not.
  const patterns: readonly RegExp[] = [
    /concept\.(id|entity)\s*(===|!==|==|!=)/g,
    /(===|!==|==|!=)\s*concept\.(id|entity)/g,
    /switch\s*\(\s*concept\.(id|entity)/g,
    /concept\.(id|entity)\s*\.(startsWith|endsWith|includes|match)\s*\(/g,
  ];
  const offenders: string[] = [];
  for (const { file, source } of sourceFiles()) {
    const code = stripComments(source);
    for (const pattern of patterns) {
      for (const m of code.matchAll(pattern)) offenders.push(`${file}: ${m[0].trim()}`);
    }
  }
  assert.deepEqual(
    offenders,
    [],
    `view-kit source compares a concept's identity: ${offenders.join(", ")}. ` +
      `An element must not know which concept it is looking at.`,
  );
});

test("the guard is actually scanning the element sources", () => {
  // A scan over an empty file list passes vacuously. Pin the shape of what is
  // being scanned so a moved directory fails loudly instead of silently.
  const files = sourceFiles().map((f) => f.file);
  assert.ok(files.length >= 12, `only ${files.length} source files found in ${SRC_DIR}`);
  for (const expected of ["calendar.ts", "checklist.ts", "chart.ts", "table.ts", "map.ts"]) {
    assert.ok(files.includes(expected), `${expected} is not being scanned`);
  }
});

test("the package stays dependency-free", () => {
  // The charts are hand-rolled SVG for this reason; the moment a charting
  // library lands here, every consumer inherits it -- including a VS Code
  // webview with a strict CSP.
  const pkg = JSON.parse(
    fs.readFileSync(path.resolve(SRC_DIR, "../package.json"), "utf8"),
  ) as { dependencies?: Record<string, string>; peerDependencies?: Record<string, string> };
  assert.deepEqual(pkg.dependencies ?? {}, {});
  assert.deepEqual(pkg.peerDependencies ?? {}, {});
});

test("no element source touches the DOM", () => {
  // view-kit renders to a VNode tree; a `document` reference would break the
  // VS Code webview path and the node --test suite at once.
  const offenders: string[] = [];
  for (const { file, source } of sourceFiles()) {
    const code = stripComments(source);
    if (/\b(document|window|HTMLElement|localStorage)\b/.test(code)) offenders.push(file);
  }
  assert.deepEqual(offenders, [], `these files reference the DOM: ${offenders.join(", ")}`);
});
