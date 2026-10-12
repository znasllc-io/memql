import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";
import { describe, expect, it } from "vitest";

// ONE CREATION PATH (design: docs/superpowers/specs/2026-10-11-materializer-front-door-design.md,
// section 5). In the OS a new file comes from the Materializer: the SDK's startMaterialize
// records what was asked for, what it was made from and which run made it. Uploading bytes the
// person already has is the one other way in, and only the modules in ALLOWED do it.
//
// That is two rules, so the allowlist can only ever say "this module uploads":
//   1. No file under src names the Materializer's own constructs. The SDK helper is their one
//      caller and it lives outside src, so this rule has no allowlist.
//   2. No file outside ALLOWED uploads.
//
// REACHABLE POSITIVES, as in test/files/onePath.test.ts: an empty offender list says something
// about the tree only if the scan reaches the tree and the patterns match real code. So the
// allowlisted modules must themselves be found and must still match, and the patterns are
// shown to catch a planted violation.

const ROOT = join(__dirname, "..", "src");

// Rule 1: the Materializer's raw constructs, however they are reached.
//   query.composeMaterialize({ ... })         a call
//   executeNamed("composeMaterialize", ...)   the quoted name
//   buildComposeMaterialize({ ... })          the generated call builder
// createLibraryFile is the Library's raw file write: the server's upload routes call it, a client never does.
const RAW_CONSTRUCT =
  /\b(?:composeMaterialize|createLibraryFile)\s*\(|["'`](?:composeMaterialize|createLibraryFile)["'`]|\bbuild(?:ComposeMaterialize|CreateLibraryFile)\s*\(/;

// Rule 2: putting a file the person already has into the Library.
//   uploads.upload(file)    the edge upload port, UploadProvider.upload (items/upload.ts)
//   ports.uploadFile(file)  the folder-upload port (apps/files/uploadTree.ts)
//   createFile(...)         what the editors call their upload wrapper; no OS module has one
const UPLOAD = /\.upload\s*\(|\b(?:uploadFile|createFile)\s*\(/;

// Every module that uploads, and what the person handed it. None of them makes a file out of
// text: each sends a File the person picked or dropped.
const ALLOWED: Record<string, string> = {
  "apps/campaigns/AudiencesSection.tsx": "uploads the CSV of addresses the person picked, then imports it",
  "apps/files/uploadTree.ts": "uploads each file of a folder the person dropped",
  "apps/files/useUploadTasks.ts": "uploads the files the person picked or dropped in Files",
  "apps/training/useUploads.ts": "uploads the files the person dropped in Training",
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
        offenders.push(`${rel} names a Materializer construct: use startMaterialize`);
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
    for (const line of [
      "await query.composeMaterialize({ name })",
      "connection.query.createLibraryFile({})",
      'await query.executeNamed("composeMaterialize", call)',
      "const call = buildCreateLibraryFile(args)",
    ]) {
      expect(RAW_CONSTRUCT.test(line), line).toBe(true);
    }
    for (const line of [
      'await uploads.upload(new File(["\\n"], "New.md")).done',
      "await ports.uploadFile(file, folderId)",
      "const uri = await files.createFile(lease, name, mime, bytes)",
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
      "uploadFiles(files, folderId)",
      "uploadTree(walked.files, destinationFolderId, label)",
    ]) {
      expect(RAW_CONSTRUCT.test(line), line).toBe(false);
      expect(UPLOAD.test(line), line).toBe(false);
    }
  });
});
