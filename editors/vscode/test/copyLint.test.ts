// The extension's user-facing copy, held to the voice the design record sets
// (docs/internal/design/2026-09-28-vscode-extension-ux.md, "Audience and
// voice").
//
// THREE SOURCES, because copy reaches a person three ways:
//   1. package.json: every string it contributes (command titles, welcome
//      views, view names, setting descriptions and the rest), the Restricted
//      Mode line and the extension's own name and description.
//   2. Every gallery scenario's visible text, in both themes: the pages as the
//      editor draws them, with tags, scripts and styles stripped.
//   3. The string literals a source file hands straight to a toast (directly
//      or through offerDetails), an input box's prompt or title, or a progress
//      notification's title.
//
// WHAT FAILS: an issue reference, " -- " as punctuation, "portal" or
// "console" (MemQL OS is MemQL OS), internal vocabulary a reader cannot act on
// (refresh_token, client_id, SecretStorage, receipt, envelope, stderr, exit
// code, install graph, capability), a command title carrying its own "MemQL: "
// prefix (the category says it), and a lede or notice line over 25 words.
//
// A LINE is a welcome view's line, the Restricted Mode line, a toast, and on a
// page a notice, a lede, an empty state or the action bar's confirmation.
// Setting descriptions and the extension's description are held to the
// vocabulary only: they are reference text, not a line on a screen.
//
// An exemption goes in ALLOWED with its reason, and one that matches nothing
// fails the last case here, so the list cannot outlive the copy it excused.
//
// The rules and the scanning (what is copy, how it is read from a package.json,
// a page and a call) live in copyScan.ts, which the Productivity Tools lint
// (editors/productivity/test/copyLint.test.ts) shares. This file keeps what is
// the MemQL extension's own: the gallery, its exemptions and its cases.

import test from "node:test";
import assert from "node:assert/strict";
import * as path from "node:path";

import { GALLERY_THEMES } from "../gallery/harness.js";
import { SCENARIOS } from "../gallery/index.js";
import {
  callArguments,
  literalFragments,
  literalText,
  manifestCopy,
  pageLines,
  problems,
  propertyText,
  sourceCopy,
  visibleText,
  type Allowed,
  type Copy,
} from "./copyScan.js";

// dist-test/test/<name>.js -> the package root.
const PKG = path.resolve(__dirname, "..", "..");
const SRC = path.join(PKG, "src");

const ALLOWED: readonly Allowed[] = [
  {
    where: "gallery author-language-",
    rule: "-- as punctuation",
    contains: "-- do not edit",
    reason: "The grammar's own header comment, shown verbatim as the cluster serves it. It is data, not the extension's words.",
  },
];

// ---------------------------------------------------------------------------
// 2. the gallery (sources 1 and 3 are read by copyScan.ts)
// ---------------------------------------------------------------------------

/** The classes whose paragraph is a line: a notice, a lede, an empty state, a confirmation. */
const LINE_CLASS = /\b(mq-notice-line|mq-notice-next|mq-empty-line|mq-actbar-confirm|ac-line|ac-empty|[\w-]+-lede)\b/;

function galleryCopy(): Copy[] {
  const out: Copy[] = [];
  for (const scenario of SCENARIOS) {
    for (const theme of GALLERY_THEMES) {
      const html = scenario.render(theme);
      const where = `gallery ${scenario.id} (${theme})`;
      for (const text of visibleText(html)) out.push({ where, text });
      for (const text of pageLines(html, LINE_CLASS)) out.push({ where, text, line: true });
    }
  }
  return out;
}

// ---------------------------------------------------------------------------
// the check
// ---------------------------------------------------------------------------

const used = new Set<Allowed>();

function assertClean(copy: readonly Copy[], source: string): void {
  assert.ok(copy.length > 0, `no copy was read from ${source}, so this case checks nothing`);
  const found = problems(copy, ALLOWED, used);
  assert.deepEqual(found, [], `${source} carries copy the design record rules out:\n  ${found.join("\n  ")}`);
}

test("package.json copy is plain", () => {
  assertClean(manifestCopy(PKG), "package.json");
});

test("every gallery page's visible text is plain, in both themes", () => {
  assertClean(galleryCopy(), "the gallery");
});

test("toasts, input boxes and progress titles are plain", () => {
  assertClean(sourceCopy(SRC, PKG), "src");
});

test("the source scan reads the calls it exists for", () => {
  // The positive control: a scanner that matched nothing would pass forever.
  const sample = [
    'void window.showWarningMessage("Couldn\'t reach " + name + ".", "Retry");',
    "window.showInputBox({ title: `Sign in to ${cluster}`, prompt: 'Paste the code', value: x });",
    "window.withProgress({ location: 1, title: 'Signing in' }, async () => {});",
  ].join("\n");
  const warn = callArguments(sample, sample.indexOf("("));
  assert.deepEqual(warn.map(literalText), [undefined, "Retry"], "a concatenation with a variable is not a literal");
  assert.equal(literalText(`"Couldn't reach " + "it."`), "Couldn't reach it.");
  assert.deepEqual(literalFragments(`err?.message ?? "Couldn't connect." + f("not this")`), ["Couldn't connect."]);
  const box = callArguments(sample, sample.indexOf("showInputBox(") + "showInputBox".length)[0] ?? "";
  assert.equal(propertyText(box, "title"), "Sign in to X");
  assert.equal(propertyText(box, "prompt"), "Paste the code");
  const progress = callArguments(sample, sample.indexOf("withProgress(") + "withProgress".length)[0] ?? "";
  assert.equal(propertyText(progress, "title"), "Signing in");
  assert.ok(sourceCopy(SRC, PKG).length > 20, "the scan found the extension's toasts");
  assert.ok(
    sourceCopy(SRC, PKG).some((c) => c.text === "MemQL: Couldn't save the cluster." && c.line === true),
    "the scan reads a toast offerDetails shows",
  );
});

test("the manifest scan reads every kind of copy, and no identifier", () => {
  // The positive control for section 1: a walker that skipped a key would pass forever.
  const copy = manifestCopy(PKG);
  const texts = new Set(copy.map((c) => c.text));
  for (const expected of ["Clusters", "MemQL Dark", "Refresh Clusters", "No clusters yet."]) {
    assert.ok(texts.has(expected), `the scan did not read "${expected}"`);
  }
  assert.ok(
    copy.some((c) => c.title === true && c.text === "Refresh Clusters"),
    "a command title is read as a title",
  );
  for (const notCopy of ["isWorkspaceTrusted", "memqlClusters", "verbose", "keyword.control.memql", "navigation@0"]) {
    assert.ok(!texts.has(notCopy), `"${notCopy}" is not copy`);
  }
});

test("every exemption still matches the copy it excuses", () => {
  problems([...manifestCopy(PKG), ...galleryCopy(), ...sourceCopy(SRC, PKG)], ALLOWED, used);
  const stale = ALLOWED.filter((a) => !used.has(a));
  assert.deepEqual(stale, [], "an exemption matches nothing; delete it");
});
