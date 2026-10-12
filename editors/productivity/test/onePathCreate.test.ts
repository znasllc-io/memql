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
// Three rules, and only the third has an allowlist, so an entry can only ever say "this module
// uploads":
//   1. No file under src names a construct that makes a file or a document (RAW_CONSTRUCTS).
//   2. No file builds a File out of nothing (FABRICATED_FILE).
//   3. No file outside ALLOWED uploads.
// Every rule reads every file whole, and no part of a file is exempt. A command that uploads has a
// module of its own (insertImage.ts) so that the file registering it, extension.ts, is scanned
// like any other.
//
// Outside the invariant on purpose: libraryForkDocumentVersion (documents.ts) creates an
// independent document branch from a snapshot of a document that already exists, history
// preserved. It copies what the person already has rather than making a file from nothing, and
// edits stay on the Library revision workflow.
//
// REACHABLE POSITIVES: an empty offender list says something about the tree only if the scan
// reaches the tree and the patterns match real code. So the allowlisted modules must themselves
// be found and must still match, and the patterns are shown to catch a planted violation.

const ROOT = join(__dirname, "..", "src");

// Rule 1. The SDK's helpers call composeMaterialize and are its one caller; createLibraryFile,
// createArtifact and createGeneratedOutput are raw writes that nothing in a client has any
// business with. Each is matched in the three ways it can be reached, and a call is matched when it
// is made optionally (`?.(`) too:
//   query.composeMaterialize({ ... })               a call
//   api.execute(lease, "composeMaterialize", ...)   the quoted name (how email drafts used to start)
//   buildComposeMaterialize({ ... })                the generated call builder
const RAW_CONSTRUCTS = ["composeMaterialize", "createLibraryFile", "createArtifact", "createGeneratedOutput"];
const builder = (name: string) => `build${name.charAt(0).toUpperCase()}${name.slice(1)}`;
// An opening parenthesis, directly or after `?.`.
const CALLED = "\\s*(?:\\?\\.\\s*)?\\(";
const RAW_CONSTRUCT = new RegExp(
  RAW_CONSTRUCTS.flatMap(name => [`\\b${name}${CALLED}`, `["'\`]${name}["'\`]`, `\\b${builder(name)}${CALLED}`]).join("|"),
);

// Rule 2. A File the person picked arrives from the platform; the only way to make one up is the
// constructor, so nothing under src may call it, by its name or through the global object
// (window.File, globalThis.File, self.File).
const FABRICATED_FILE = /\bnew\s+(?:(?:window|globalThis|self)\s*\.\s*)?File\s*\(/;

// Rule 3: putting a file the person already has into the Library.
//   api.uploadFile(lease, name, mime, bytes)   the connection's upload (editors/vscode/src/connection/api.ts)
//   files.createFile(lease, name, mime, bytes) Documents.createFile, which wraps it (documents.ts)
// each called directly or optionally (`?.(`).
const UPLOAD = /\b(?:uploadFile|createFile)\s*(?:\?\.\s*)?\(/;

// Every module that uploads, and what the person handed it. None of them makes a file out of
// text: each sends bytes the person picked.
const ALLOWED: Record<string, string> = {
  "documents.ts": "uploads of existing bytes (insert image, review attachments)",
  "emailEditor.ts": "uploads a picture the person picked, or the local email they save to MemQL",
  "insertImage.ts": "uploads a picture the person picked with the open dialog",
  "markdownEditor.ts": "uploads review attachments the person picked",
  "templateExamples.ts": "uploads reference files the person picked",
};

function walk(dir: string): string[] {
  return readdirSync(dir).flatMap(name => {
    const path = join(dir, name);
    return statSync(path).isDirectory() ? walk(path) : /\.(ts|tsx)$/.test(name) ? [path] : [];
  });
}

const FILES = walk(ROOT).map(path => relative(ROOT, path));
const source = (rel: string) => readFileSync(join(ROOT, rel), "utf8");

test("creates files only through the Materializer or the named upload modules", () => {
  const offenders: string[] = [];
  for (const rel of FILES) {
    const text = source(rel);
    if (RAW_CONSTRUCT.test(text)) {
      offenders.push(`${rel} names a raw file-creating construct: use materialize`);
    }
    if (FABRICATED_FILE.test(text)) {
      offenders.push(`${rel} builds a File: bytes the person did not hand over are not an upload, use materialize`);
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
    assert.ok(UPLOAD.test(source(rel)), `${rel} no longer uploads: remove it from ALLOWED`);
  }
});

test("New Markdown is scanned like any other module, and creates through the SDK helper", () => {
  const text = source("newMarkdown.ts");
  assert.ok(FILES.includes("newMarkdown.ts"), "newMarkdown.ts is not under src: update this guard");
  assert.ok(!("newMarkdown.ts" in ALLOWED), "New Markdown must not be exempt");
  assert.ok(!UPLOAD.test(text), "New Markdown uploads a file");
  assert.ok(!RAW_CONSTRUCT.test(text), "New Markdown names a raw file-creating construct");
  assert.ok(/\b(?:materialize|startMaterialize)\s*\(/.test(text), "New Markdown no longer creates through the SDK helper");
});

test("extension.ts is scanned whole, and registers insert-image from the module that uploads", () => {
  const text = source("extension.ts");
  assert.ok(FILES.includes("extension.ts"), "extension.ts is not under src: update this guard");
  assert.ok(!("extension.ts" in ALLOWED), "extension.ts must not be exempt");
  assert.ok(!UPLOAD.test(text), "extension.ts uploads");
  assert.ok(
    /registerCommand\("memql\.productivity\.markdown\.insertImage",\s*insertImage\(/.test(text),
    "extension.ts no longer registers the insert-image command from insertImage.ts",
  );
});

test("catches a planted violation", () => {
  for (const name of RAW_CONSTRUCTS) {
    assert.match(`await query.${name}({ name })`, RAW_CONSTRUCT, `${name} as a call`);
    assert.match(`await api.execute(lease, "${name}", call)`, RAW_CONSTRUCT, `${name} by its quoted name`);
    assert.match(`await api.execute(lease, 'x', ${builder(name)}(args))`, RAW_CONSTRUCT, `${name} through its builder`);
  }
  // An optional call is still a call.
  for (const name of RAW_CONSTRUCTS) {
    assert.match(`await query.${name}?.({ name })`, RAW_CONSTRUCT, `${name} as an optional call`);
    assert.match(`await api.execute(lease, 'x', ${builder(name)}?.(args))`, RAW_CONSTRUCT, `${name} through its builder, called optionally`);
  }
  // Email drafts before they moved onto the helper.
  assert.match('await this.api.execute(lease, "composeMaterialize", buildComposeMaterialize({ name }))', RAW_CONSTRUCT);
  assert.match('const file = new File(["\\n"], "Untitled.md")', FABRICATED_FILE);
  // The constructor reached through the global object is the same constructor.
  assert.match('const file = new window.File(["\\n"], "Untitled.md")', FABRICATED_FILE);
  assert.match("const file = new globalThis.File(parts, name)", FABRICATED_FILE);
  for (const line of [
    // New Markdown before it moved onto the helper.
    'const uri = await files.createFile(lease, "Untitled.md", "text/markdown", new TextEncoder().encode("\\n"))',
    "const uploaded = await this.api.uploadFile(lease, name, mime, bytes)",
    "const uri = await files.createFile?.(lease, name, mime, bytes)",
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
    'this.api.execute(lease, "libraryForkDocumentVersion", buildLibraryForkDocumentVersion({ artifactId }))',
    "const reader = new FileReader()",
    "const blob = new Blob([text], { type: 'text/plain' })",
  ]) {
    assert.doesNotMatch(line, RAW_CONSTRUCT, line);
    assert.doesNotMatch(line, FABRICATED_FILE, line);
    assert.doesNotMatch(line, UPLOAD, line);
  }
});
