import { test } from "node:test";
import assert from "node:assert/strict";
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";

// ONE CREATION PATH (design: docs/superpowers/specs/2026-10-11-materializer-front-door-design.md,
// section 5). In this extension a new file comes from the Materializer: the SDK's materialize
// (New Markdown) and startMaterialize (email drafts) record what was asked for, what it was made
// from and which run made it. Uploading bytes the person already has is the one other way in, and
// only the modules in ALLOWED do it.
//
// That is two rules, so the allowlist can only ever say "this module uploads":
//   1. No file under src names the Materializer's own constructs. The SDK helpers are their one
//      caller and they live outside src, so this rule has no allowlist.
//   2. No file outside ALLOWED uploads.
//
// REACHABLE POSITIVES: an empty offender list says something about the tree only if the scan
// reaches the tree and the patterns match real code. So the allowlisted modules must themselves
// be found and must still match, and the patterns are shown to catch a planted violation.

const ROOT = join(__dirname, "..", "src");

// Rule 1: the Materializer's raw constructs, however they are reached.
//   api.execute(lease, "composeMaterialize", ...)   the quoted name (how email drafts used to start)
//   buildComposeMaterialize({ ... })                the generated call builder
//   query.composeMaterialize({ ... })               a call
// createLibraryFile is the Library's raw file write: the server's upload routes call it, a client never does.
const RAW_CONSTRUCT =
  /\b(?:composeMaterialize|createLibraryFile)\s*\(|["'`](?:composeMaterialize|createLibraryFile)["'`]|\bbuild(?:ComposeMaterialize|CreateLibraryFile)\s*\(/;

// Rule 2: putting a file the person already has into the Library.
//   api.uploadFile(lease, name, mime, bytes)   the connection's upload (editors/vscode/src/connection/api.ts)
//   files.createFile(lease, name, mime, bytes) Documents.createFile, which wraps it (documents.ts)
const UPLOAD = /\b(?:uploadFile|createFile)\s*\(/;

// Every module that uploads, and what the person handed it. None of them makes a file out of
// text: each sends bytes the person picked. extension.ts uploads too, in one command only; it is
// not listed because the scan reads it with that command cut out (see below).
const ALLOWED: Record<string, string> = {
  "documents.ts": "uploads of existing bytes (insert image, review attachments)",
  "markdownEditor.ts": "uploads review attachments the person picked",
  "templateExamples.ts": "uploads reference files the person picked",
};

// The insert-image command sends the picture the person chose, so it is the one place extension.ts
// may upload. The command runs from its registration to the next one.
const INSERT_IMAGE = 'registerCommand("memql.productivity.markdown.insertImage"';

function splitInsertImage(extension: string): { command: string; rest: string } {
  const start = extension.indexOf(INSERT_IMAGE);
  assert.notEqual(start, -1, "extension.ts no longer registers the insert-image command: update this guard");
  const next = extension.indexOf("registerCommand(", start + INSERT_IMAGE.length);
  const end = next === -1 ? extension.length : next;
  return { command: extension.slice(start, end), rest: extension.slice(0, start) + extension.slice(end) };
}

function walk(dir: string): string[] {
  return readdirSync(dir).flatMap(name => {
    const path = join(dir, name);
    return statSync(path).isDirectory() ? walk(path) : /\.(ts|tsx)$/.test(name) ? [path] : [];
  });
}

const FILES = walk(ROOT).map(path => relative(ROOT, path));
const wholeFile = (rel: string) => readFileSync(join(ROOT, rel), "utf8");
// What the scan reads: the file, less the one command that may upload.
const scanned = (rel: string) => (rel === "extension.ts" ? splitInsertImage(wholeFile(rel)).rest : wholeFile(rel));

test("creates files only through the Materializer or the named upload modules", () => {
  const offenders: string[] = [];
  for (const rel of FILES) {
    const text = scanned(rel);
    if (RAW_CONSTRUCT.test(text)) {
      offenders.push(`${rel} names a Materializer construct: use materialize`);
    }
    if (UPLOAD.test(text) && !(rel in ALLOWED)) {
      offenders.push(`${rel} uploads a file: add it to ALLOWED if it sends bytes the person picked, else use materialize`);
    }
  }
  assert.deepEqual(offenders, []);
});

test("scans the real tree: every allowlisted module is found and still uploads", () => {
  // An entry that no longer matches would exempt whatever is added to that file next.
  for (const rel of Object.keys(ALLOWED)) {
    assert.ok(FILES.includes(rel), `${rel} is not under src: remove it from ALLOWED`);
    assert.ok(UPLOAD.test(wholeFile(rel)), `${rel} no longer uploads: remove it from ALLOWED`);
  }
});

test("New Markdown is scanned like any other module, and creates through the SDK helper", () => {
  const text = wholeFile("newMarkdown.ts");
  assert.ok(FILES.includes("newMarkdown.ts"), "newMarkdown.ts is not under src: update this guard");
  assert.ok(!("newMarkdown.ts" in ALLOWED), "New Markdown must not be exempt");
  assert.ok(!UPLOAD.test(text), "New Markdown uploads a file");
  assert.ok(!RAW_CONSTRUCT.test(text), "New Markdown names a Materializer construct");
  assert.ok(/\b(?:materialize|startMaterialize)\s*\(/.test(text), "New Markdown no longer creates through the SDK helper");
});

test("extension.ts uploads only in the insert-image command", () => {
  const { command, rest } = splitInsertImage(wholeFile("extension.ts"));
  assert.ok(UPLOAD.test(command), "the insert-image command no longer uploads: stop cutting it out of the scan");
  assert.ok(!UPLOAD.test(rest), "extension.ts uploads outside the insert-image command");
  assert.ok(!RAW_CONSTRUCT.test(rest), "extension.ts names a Materializer construct");
});

test("catches a planted violation", () => {
  for (const line of [
    'await this.api.execute(lease, "composeMaterialize", buildComposeMaterialize({ name }))', // email drafts before they moved onto the helper
    "await query.composeMaterialize({ name })",
    "await api.execute(lease, 'createLibraryFile', buildCreateLibraryFile(args))",
  ]) {
    assert.match(line, RAW_CONSTRUCT, line);
  }
  for (const line of [
    'const uri = await files.createFile(lease, "Untitled.md", "text/markdown", new TextEncoder().encode("\\n"))', // New Markdown before it moved onto the helper
    "const uploaded = await this.api.uploadFile(lease, name, mime, bytes)",
  ]) {
    assert.match(line, UPLOAD, line);
  }
});

test("leaves the Materializer's own entry points and lookalikes alone", () => {
  for (const line of [
    'materialize({ execute }, { name: stem, format: "markdown", requestId })',
    "const compositionId = await startMaterialize(this.executeAs(lease), request)",
    "await followComposition(ports, compositionId)",
    "vscode.workspace.createFileSystemWatcher('**/*.memql')",
  ]) {
    assert.doesNotMatch(line, RAW_CONSTRUCT, line);
    assert.doesNotMatch(line, UPLOAD, line);
  }
});
