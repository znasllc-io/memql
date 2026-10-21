import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";
import { describe, expect, it } from "vitest";

// ONE CREATION PATH (design: docs/superpowers/specs/2026-10-11-materializer-front-door-design.md,
// section 5). In the OS a new file comes from the Materializer: the SDK's startMaterialize
// records what was asked for, what it was made from and which run made it. Uploading bytes the
// person already has is the one other way in, and only the modules in ALLOWED do it.
//
// Three rules, and only the third has an allowlist, so an entry can only ever say "this module
// uploads":
//   1. No file under src names a construct that makes a file or a document (RAW_CONSTRUCTS).
//   2. No file builds a File out of nothing (FABRICATED_FILE).
//   3. No file outside ALLOWED uploads.
// Every rule reads every file whole, and no part of a file is exempt.
//
// Outside the invariant on purpose: appendDocumentVersion adds a version to a document that
// already exists, and libraryForkDocumentVersion creates an independent document branch from a
// snapshot of one. Neither makes a file from nothing, and edits stay on the Library revision
// workflow.
//
// REACHABLE POSITIVES, as in test/files/onePath.test.ts: an empty offender list says something
// about the tree only if the scan reaches the tree and the patterns match real code. So the
// allowlisted modules must themselves be found and must still match, and the patterns are
// shown to catch a planted violation.

const ROOT = join(__dirname, "..", "src");

// Rule 1. The SDK's helpers call composeMaterialize and are its one caller; createLibraryFile,
// createArtifact and createGeneratedOutput are raw writes that nothing in a client has any
// business with (the server's upload routes call createLibraryFile). Each is matched in the three
// ways it can be reached, and a call is matched when it is made optionally (`?.(`) too:
//   query.composeMaterialize({ ... })         a call
//   executeNamed("composeMaterialize", ...)   the quoted name
//   buildComposeMaterialize({ ... })          the generated call builder
const RAW_CONSTRUCTS = ["composeMaterialize", "createLibraryFile", "createArtifact", "createGeneratedOutput"];
const builder = (name: string) => `build${name.charAt(0).toUpperCase()}${name.slice(1)}`;
// An opening parenthesis, directly or after `?.`.
const CALLED = "\\s*(?:\\?\\.\\s*)?\\(";
const RAW_CONSTRUCT = new RegExp(
  RAW_CONSTRUCTS.flatMap((name) => [`\\b${name}${CALLED}`, `["'\`]${name}["'\`]`, `\\b${builder(name)}${CALLED}`]).join("|"),
);

// Rule 2. A File the person picked, dropped or pasted arrives from the browser; the only way to
// make one up is the constructor, so nothing under src may call it, by its name or through the
// global object (window.File, globalThis.File, self.File). Without this, an allowlisted uploader
// could send provider.upload(new File(["\n"], "Untitled.md")) and pass.
const FABRICATED_FILE = /\bnew\s+(?:(?:window|globalThis|self)\s*\.\s*)?File\s*\(/;

// Rule 3: putting a file the person already has into the Library.
//   uploads.upload(file)    the edge upload port, UploadProvider.upload (items/upload.ts)
//   ports.uploadFile(file)  the folder-upload port (apps/files/uploadTree.ts)
//   createFile(...)         what the editors call their upload wrapper; no OS module has one
// each called directly or optionally (`?.(`).
const UPLOAD = /\.upload\s*(?:\?\.\s*)?\(|\b(?:uploadFile|createFile)\s*(?:\?\.\s*)?\(/;

// Every module that uploads, and what the person handed it. None of them makes a file out of
// text: each sends a File the person picked or dropped.
const ALLOWED: Record<string, string> = {
  "apps/email/audiences/AddPeopleWizard.tsx": "uploads the CSV of addresses the person picked, then imports it",
  "apps/files/uploadTree.ts": "uploads each file of a folder the person dropped",
  "apps/files/useUploadTasks.ts": "uploads the files the person picked or dropped in Files",
  "apps/training/useUploads.ts": "uploads the files the person dropped in Training",
  "ask/conversationSession.ts": "uploads the files the person attached to an Ask message",
  "chrome/Desktop.tsx": "uploads the files the person dropped on the desk",
};

function walk(dir: string): string[] {
  return readdirSync(dir).flatMap((name) => {
    const path = join(dir, name);
    return statSync(path).isDirectory() ? walk(path) : /\.(ts|tsx)$/.test(name) ? [path] : [];
  });
}

const FILES = walk(ROOT).map((path) => relative(ROOT, path));
const source = (rel: string) => readFileSync(join(ROOT, rel), "utf8");

describe("one creation path", () => {
  it("creates files only through the Materializer or the named upload modules", () => {
    const offenders: string[] = [];
    for (const rel of FILES) {
      const text = source(rel);
      if (RAW_CONSTRUCT.test(text)) {
        offenders.push(`${rel} names a raw file-creating construct: use startMaterialize`);
      }
      if (FABRICATED_FILE.test(text)) {
        offenders.push(`${rel} builds a File: bytes the person did not hand over are not an upload, use startMaterialize`);
      }
      if (UPLOAD.test(text) && !(rel in ALLOWED)) {
        offenders.push(`${rel} uploads a file: add it to ALLOWED if it sends bytes the person picked, else use startMaterialize`);
      }
    }
    expect(offenders).toEqual([]);
  });

  it("scans the real tree: every allowlisted module is found and still uploads", () => {
    // An entry that no longer matches would exempt whatever is added to that file next.
    for (const rel of Object.keys(ALLOWED)) {
      expect(FILES.includes(rel), `${rel} is not under src: remove it from ALLOWED`).toBe(true);
      expect(UPLOAD.test(source(rel)), `${rel} no longer uploads: remove it from ALLOWED`).toBe(true);
    }
  });

  it("scans the Materializer app, which creates through the SDK helper", () => {
    // The app used to call composeMaterialize itself. That the scan reaches it, finds the helper
    // there and finds no raw construct is what makes the empty offender list above mean something.
    const creators = FILES.filter(
      (rel) => rel.startsWith("apps/materializer/") && /\bstartMaterialize\s*\(/.test(source(rel)),
    );
    expect(creators, "the Materializer app no longer creates through startMaterialize").not.toEqual([]);
  });

  it("catches a planted violation", () => {
    for (const name of RAW_CONSTRUCTS) {
      expect(RAW_CONSTRUCT.test(`await query.${name}({ name })`), `${name} as a call`).toBe(true);
      expect(RAW_CONSTRUCT.test(`await query.executeNamed("${name}", call)`), `${name} by its quoted name`).toBe(true);
      expect(RAW_CONSTRUCT.test(`const call = ${builder(name)}(args)`), `${name} through its builder`).toBe(true);
    }
    // An optional call is still a call.
    for (const name of RAW_CONSTRUCTS) {
      expect(RAW_CONSTRUCT.test(`await query.${name}?.({ name })`), `${name} as an optional call`).toBe(true);
      expect(RAW_CONSTRUCT.test(`const call = ${builder(name)}?.(args)`), `${name} through its builder, called optionally`).toBe(true);
    }
    expect(FABRICATED_FILE.test('provider.upload(new File(["\\n"], "Untitled.md"))')).toBe(true);
    expect(FABRICATED_FILE.test("const file = new  File (parts, name)")).toBe(true);
    // The constructor reached through the global object is the same constructor.
    expect(FABRICATED_FILE.test('provider.upload(new window.File(["\\n"], "Untitled.md"))')).toBe(true);
    expect(FABRICATED_FILE.test("const file = new globalThis.File(parts, name)")).toBe(true);
    for (const line of [
      'await uploads.upload(new File(["\\n"], "New.md")).done',
      "await ports.uploadFile(file, folderId)",
      "const uri = await files.createFile(lease, name, mime, bytes)",
      "await uploads.upload?.(file).done",
      "await ports.uploadFile?.(file, folderId)",
    ]) {
      expect(UPLOAD.test(line), line).toBe(true);
    }
  });

  it("leaves the Materializer's own entry points and lookalikes alone", () => {
    for (const line of [
      "const id = await startMaterialize(execute, request)",
      "const made = await materialize(ports, request)",
      "await followComposition(ports, compositionId)",
      "await query.createLibraryFolder({ folderId, name })",
      "await query.executeNamed('libraryForkDocumentVersion', buildLibraryForkDocumentVersion(args))",
      "uploadFiles(files, folderId)",
      "uploadTree(walked.files, destinationFolderId, label)",
      "const reader = new FileReader()",
      'const blob = new Blob([text], { type: "text/plain" })',
    ]) {
      expect(RAW_CONSTRUCT.test(line), line).toBe(false);
      expect(FABRICATED_FILE.test(line), line).toBe(false);
      expect(UPLOAD.test(line), line).toBe(false);
    }
  });
});
