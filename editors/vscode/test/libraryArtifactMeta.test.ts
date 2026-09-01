// Resolving a Library artifact's metadata over the stream (memql#4748).
//
// TWO ROWS, AND THE CALLS THAT FETCH THEM ARE RENDERED MemQL TEXT. A query
// reaches the engine as a string, so a hand-built call is a parse failure
// waiting for production -- these cases assert what actually goes on the wire,
// not just what comes back.

import test from "node:test";
import assert from "node:assert/strict";

import {
  artifactMetaFrom,
  fileIdFromSourceRef,
  isArchived,
  resolveArtifactMeta,
  type RowLike,
} from "../src/library/artifactMeta.js";

/** A reader that records the calls it was asked to run and answers from a map. */
function reader(answers: Record<string, RowLike[]>) {
  const calls: Array<{ name: string; call: string }> = [];
  return {
    calls,
    executeNamed(name: string, call: string): Promise<{ rows(): RowLike[] }> {
      calls.push({ name, call });
      const rows = answers[name] ?? [];
      return Promise.resolve({ rows: () => rows });
    },
  };
}

test("a file reference yields its file id, and nothing else does", () => {
  assert.equal(fileIdFromSourceRef("v1:library:file:abc123"), "abc123");
  assert.equal(fileIdFromSourceRef("  v1:library:file:abc123  "), "abc123");
  // Prefix-matched rather than split on colons: the concept part is itself
  // colon-separated, so "the last segment" is only the id by coincidence.
  assert.equal(fileIdFromSourceRef("v1:knowledge:document:abc123"), undefined);
  assert.equal(fileIdFromSourceRef("v1:library:file:"), undefined);
  assert.equal(fileIdFromSourceRef(""), undefined);
});

test("the file row wins where it speaks, and the index row stands where it does not", () => {
  const meta = artifactMetaFrom(
    "v1:library:artifact:abc",
    { title: "Q3 plan", kind: "file", format: "document", mimeType: "application/octet-stream" },
    { name: "q3-plan.docx", mimeType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", size: 4096, format: "" },
  );
  assert.equal(meta.fileName, "q3-plan.docx");
  assert.match(meta.mimeType, /wordprocessingml/);
  // The file row states no format, so the index's copy survives.
  assert.equal(meta.format, "document");
  assert.equal(meta.sizeBytes, 4096);
});

test("a size that cannot be read is absent, not zero", () => {
  // The cap treats a stated 0 and an unstated size completely differently.
  assert.equal(artifactMetaFrom("a", { kind: "file" }, { name: "x", size: "not a number" }).sizeBytes, undefined);
  assert.equal(artifactMetaFrom("a", { kind: "file" }, { name: "x" }).sizeBytes, undefined);
  // A JSON round trip can leave a number as a string; that IS readable.
  assert.equal(artifactMetaFrom("a", { kind: "file" }, { name: "x", size: "4096" }).sizeBytes, 4096);
  assert.equal(artifactMetaFrom("a", { kind: "file" }, { name: "x", size: 0 }).sizeBytes, 0);
});

test("absent means NOT archived", () => {
  // Rows promoted before the field existed carry no key at all, which is the
  // same reading the DSL's own `archived != true` filter takes.
  assert.equal(isArchived({}), false);
  assert.equal(isArchived({ archived: false }), false);
  assert.equal(isArchived({ archived: true }), true);
});

test("a file artifact costs two reads, and both calls carry their id", async () => {
  const r = reader({
    libraryArtifactById: [
      { id: "v1:library:artifact:abc", title: "Q3 plan", kind: "file", sourceConceptRef: "v1:library:file:f1", format: "document" },
    ],
    libraryFileById: [{ name: "q3-plan.docx", mimeType: "application/pdf", size: 99 }],
  });
  const lookup = await resolveArtifactMeta(r, "v1:library:artifact:abc");
  assert.ok(lookup.found);
  assert.equal(lookup.meta.fileName, "q3-plan.docx");
  assert.equal(lookup.meta.sizeBytes, 99);

  assert.deepEqual(r.calls.map((c) => c.name), ["libraryArtifactById", "libraryFileById"]);
  // The rendered calls are what the engine parses. An id full of colons has
  // to arrive QUOTED, which is exactly what the generated builder knows and a
  // hand-built string does not.
  assert.equal(r.calls[0]!.call, 'query libraryArtifactById(artifactId: "v1:library:artifact:abc")');
  assert.equal(r.calls[1]!.call, 'query libraryFileById(fileId: "f1")');
});

test("a non-file artifact costs one read", async () => {
  const r = reader({
    libraryArtifactById: [{ title: "Weekly review", kind: "note", sourceConceptRef: "v1:notes:note:n1" }],
  });
  const lookup = await resolveArtifactMeta(r, "abc");
  assert.ok(lookup.found);
  assert.equal(lookup.meta.kind, "note");
  assert.deepEqual(r.calls.map((c) => c.name), ["libraryArtifactById"]);
});

test("no row is `not found`, which is also `not yours`", async () => {
  // libraryArtifactById filters on ownerUserId == actor.userId, so somebody
  // else's row comes back as no rows. There is no difference to report.
  const lookup = await resolveArtifactMeta(reader({}), "abc");
  assert.deepEqual(lookup, { found: false });
});

test("a file row that cannot be read leaves the index row's answer standing", async () => {
  const r = reader({
    libraryArtifactById: [
      { title: "Q3 plan", kind: "file", sourceConceptRef: "v1:library:file:f1", format: "markdown", mimeType: "text/markdown" },
    ],
    libraryFileById: [],
  });
  const lookup = await resolveArtifactMeta(r, "abc");
  assert.ok(lookup.found);
  assert.equal(lookup.meta.format, "markdown");
  assert.equal(lookup.meta.mimeType, "text/markdown");
  assert.equal(lookup.meta.fileName, "");
  assert.equal(lookup.meta.sizeBytes, undefined);
});
