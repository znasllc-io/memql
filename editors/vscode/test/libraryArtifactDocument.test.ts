// The Library artifact address and the delivery decision (memql#4748).
//
// The uri is the ONLY state a content provider gets: VS Code hands it a Uri and
// nothing else, so everything the fetch needs has to survive a round trip
// through `Uri.parse`. And the delivery decision is the part of this epic that
// must be made from METADATA -- buffering a file to discover it is binary is
// the failure mode, so these cases drive it from rows and never from bytes.

import test from "node:test";
import assert from "node:assert/strict";

import {
  ARTIFACT_BUFFER_LIMIT_BYTES,
  ARTIFACT_DOCUMENT_SCHEME,
  artifactContentUrl,
  artifactDelivery,
  artifactDocumentUri,
  artifactFileName,
  artifactProvenanceLine,
  baseMimeType,
  fetchFailedNotice,
  formatByteSize,
  isTextArtifact,
  languageIdFor,
  noContentNotice,
  notConnectedNotice,
  parseArtifactDocumentUri,
  sanitizeArtifactFileName,
  tooLargeNotice,
  type ArtifactMeta,
} from "../src/library/artifactDocument.js";

function meta(overrides: Partial<ArtifactMeta> = {}): ArtifactMeta {
  return {
    artifactId: "v1:library:artifact:abc",
    title: "Weekly review",
    kind: "file",
    format: "text",
    mimeType: "text/plain",
    fileName: "weekly-review.txt",
    ...overrides,
  };
}

// -----------------------------------------------------------------------------
// The address
// -----------------------------------------------------------------------------

test("an artifact uri round-trips its cluster, filename and id", () => {
  const uri = artifactDocumentUri({ cluster: "staging", artifactId: "v1:library:artifact:abc", fileName: "report.md" });
  assert.equal(uri, `${ARTIFACT_DOCUMENT_SCHEME}://staging/report.md?id=v1%3Alibrary%3Aartifact%3Aabc`);
  assert.deepEqual(
    parseArtifactDocumentUri({ authority: "staging", path: "/report.md", query: "id=v1%3Alibrary%3Aartifact%3Aabc" }),
    { cluster: "staging", artifactId: "v1:library:artifact:abc", fileName: "report.md" },
  );
});

test("the filename is the LAST path segment, because that is what names the tab", () => {
  // VS Code labels an editor from the last segment of its uri path and offers
  // no API to set a tab title -- so a filename anywhere else in the uri is a
  // tab called something the person did not recognise.
  const uri = artifactDocumentUri({ cluster: "c", artifactId: "abc", fileName: "notes.md" });
  assert.ok(uri.endsWith("/notes.md?id=abc"), uri);
});

test("a cluster name with a space survives the authority", () => {
  const uri = artifactDocumentUri({ cluster: "my lab", artifactId: "abc", fileName: "a.txt" });
  assert.ok(uri.startsWith(`${ARTIFACT_DOCUMENT_SCHEME}://my%20lab/`), uri);
  assert.equal(parseArtifactDocumentUri({ authority: "my%20lab", path: "/a.txt", query: "id=abc" })?.cluster, "my lab");
});

test("a filename with a percent sign is not decoded a second time", () => {
  // `Uri.path` arrives DECODED. Running decodeURIComponent over it again turns
  // `Q1 100% report.md` into an empty string, which reads as a malformed uri.
  const ref = parseArtifactDocumentUri({ authority: "c", path: "/Q1 100% report.md", query: "id=abc" });
  assert.equal(ref?.fileName, "Q1 100% report.md");
});

test("a malformed uri parses to undefined rather than a half-filled ref", () => {
  assert.equal(parseArtifactDocumentUri({ authority: "", path: "/a.txt", query: "id=abc" }), undefined);
  assert.equal(parseArtifactDocumentUri({ authority: "c", path: "/", query: "id=abc" }), undefined);
  assert.equal(parseArtifactDocumentUri({ authority: "c", path: "/a.txt", query: "" }), undefined);
});

test("the content url is the documented route, with the id escaped", () => {
  assert.equal(
    artifactContentUrl("https://api.acme.example.com", "v1:library:artifact:abc"),
    "https://api.acme.example.com/artifacts/v1%3Alibrary%3Aartifact%3Aabc/content",
  );
  // A trailing slash on the base must not double up.
  assert.equal(artifactContentUrl("https://api.a.test/", "abc"), "https://api.a.test/artifacts/abc/content");
});

// -----------------------------------------------------------------------------
// Text or binary, from the row alone
// -----------------------------------------------------------------------------

test("a non-file kind is always text, because the server renders it", () => {
  // A note / generated output / memory has no stored bytes at all: the content
  // route renders it to text/markdown or text/plain on the way out. Nothing
  // about its mime type can make it binary.
  for (const kind of ["note", "generated_output", "memory"]) {
    assert.equal(isTextArtifact(meta({ kind, format: "other", mimeType: "application/octet-stream" })), true, kind);
  }
});

test("a file is text when its format or its mime type says so", () => {
  assert.equal(isTextArtifact(meta({ format: "markdown", mimeType: "" })), true);
  assert.equal(isTextArtifact(meta({ format: "text", mimeType: "" })), true);
  assert.equal(isTextArtifact(meta({ format: "conversation", mimeType: "" })), true);
  assert.equal(isTextArtifact(meta({ format: "other", mimeType: "text/csv" })), true);
  assert.equal(isTextArtifact(meta({ format: "other", mimeType: "application/json" })), true);
  assert.equal(isTextArtifact(meta({ format: "other", mimeType: "application/x-yaml" })), true);
  // Parameters do not change the type.
  assert.equal(isTextArtifact(meta({ format: "other", mimeType: "text/markdown; charset=utf-8" })), true);
});

test("a file is binary unless something says otherwise", () => {
  for (const mimeType of ["application/pdf", "image/png", "application/octet-stream", "application/zip", ""]) {
    assert.equal(isTextArtifact(meta({ format: "other", mimeType })), false, mimeType);
  }
  assert.equal(isTextArtifact(meta({ format: "pdf", mimeType: "application/pdf" })), false);
});

test("baseMimeType drops parameters and case", () => {
  assert.equal(baseMimeType("TEXT/Markdown; charset=UTF-8"), "text/markdown");
  assert.equal(baseMimeType(""), "");
});

// -----------------------------------------------------------------------------
// The size cap
// -----------------------------------------------------------------------------

test("text under the cap opens in the editor", () => {
  assert.deepEqual(artifactDelivery(meta({ sizeBytes: ARTIFACT_BUFFER_LIMIT_BYTES })), { kind: "editor" });
});

test("text PAST the cap is offered as a file, with the size named", () => {
  // The cap applies to text too: a text document is a string in the extension
  // host, another in the renderer, and a tokenized model on top of both.
  const delivery = artifactDelivery(meta({ sizeBytes: ARTIFACT_BUFFER_LIMIT_BYTES + 1 }));
  assert.equal(delivery.kind, "saveToDisk");
  assert.match(delivery.kind === "saveToDisk" ? delivery.reason : "", /8\.0 MB/);
});

test("a size nobody stated is not a size of zero", () => {
  // Only a file-backed artifact has a size to state. Treating "not stated" as 0
  // would pass the cap by accident rather than by measurement.
  const rendered = meta({ kind: "note", format: "markdown", mimeType: "", fileName: "" });
  assert.equal(rendered.sizeBytes, undefined);
  assert.deepEqual(artifactDelivery(rendered), { kind: "editor" });
});

test("binary is offered as a file whatever its size, and the reason says why", () => {
  const small = artifactDelivery(meta({ format: "pdf", mimeType: "application/pdf", sizeBytes: 12 }));
  assert.equal(small.kind, "saveToDisk");
  // Named for the reason it IS, not the reason it also happens to satisfy: a
  // size in this sentence would imply a smaller PDF would have opened.
  assert.match(small.kind === "saveToDisk" ? small.reason : "", /application\/pdf/);
  assert.doesNotMatch(small.kind === "saveToDisk" ? small.reason : "", /MB/);
});

test("byte sizes read as a person reads them", () => {
  assert.equal(formatByteSize(0), "0 B");
  assert.equal(formatByteSize(1023), "1023 B");
  assert.equal(formatByteSize(ARTIFACT_BUFFER_LIMIT_BYTES), "8.0 MB");
  assert.equal(formatByteSize(-1), "an unknown size");
});

// -----------------------------------------------------------------------------
// Naming and language
// -----------------------------------------------------------------------------

test("a file artifact opens under its file's own name", () => {
  assert.equal(artifactFileName(meta({ kind: "file", fileName: "Q3 plan.docx", title: "Ignored" })), "Q3 plan.docx");
});

test("a rendered artifact gets the extension the server will export it with", () => {
  // Mirrors ExportBody's Markdown flag: a note is always markdown, a memory
  // never is, and everything else is believed when it says markdown.
  assert.equal(artifactFileName(meta({ kind: "note", title: "Weekly review", fileName: "", format: "" })), "Weekly-review.md");
  assert.equal(artifactFileName(meta({ kind: "memory", title: "Prefers tea", fileName: "", format: "markdown" })), "Prefers-tea.txt");
  assert.equal(
    artifactFileName(meta({ kind: "generated_output", title: "Bird list", fileName: "", format: "markdown" })),
    "Bird-list.md",
  );
  assert.equal(
    artifactFileName(meta({ kind: "generated_output", title: "Bird list", fileName: "", format: "other", mimeType: "" })),
    "Bird-list.txt",
  );
});

test("a nameless artifact still gets a filename", () => {
  assert.equal(artifactFileName(meta({ kind: "file", fileName: "", title: "" })), "artifact");
  assert.equal(artifactFileName(meta({ kind: "note", fileName: "", title: "   ", format: "" })), "artifact.md");
});

test("a filename from the cluster is treated as input", () => {
  // It is a value the CLUSTER produced, and it lands in a uri path, a tab
  // title, a save dialog and a channel line.
  assert.equal(sanitizeArtifactFileName("../../etc/passwd"), "passwd");
  assert.equal(sanitizeArtifactFileName("C:\\Users\\me\\secret.txt"), "secret.txt");
  assert.equal(sanitizeArtifactFileName("  .hidden.  "), "hidden");
  assert.equal(sanitizeArtifactFileName(".."), "");
  assert.equal(sanitizeArtifactFileName(`report${String.fromCharCode(0)}.md`), "report.md");
  assert.equal(sanitizeArtifactFileName("x".repeat(400)).length, 120);
});

test("the language is set only where the metadata names one", () => {
  assert.equal(languageIdFor("markdown", ""), "markdown");
  assert.equal(languageIdFor("other", "application/json"), "json");
  assert.equal(languageIdFor("other", "text/markdown; charset=utf-8"), "markdown");
  // text/plain maps to NOTHING: plaintext is the fallback anyway, so setting it
  // could only ever discard the language the filename's extension implied.
  assert.equal(languageIdFor("other", "text/plain"), undefined);
  // No built-in language id, so no guess -- even though these are text.
  assert.equal(languageIdFor("other", "application/toml"), undefined);
  assert.equal(languageIdFor("other", "application/csv"), undefined);
  assert.equal(languageIdFor("", ""), undefined);
});

// -----------------------------------------------------------------------------
// What the reader is told
// -----------------------------------------------------------------------------

test("the provenance line names the cluster, the row and what it is", () => {
  const line = artifactProvenanceLine("staging", meta({ sizeBytes: 2048 }));
  assert.match(line, /v1:library:artifact:abc/);
  assert.match(line, /staging/);
  assert.match(line, /file/);
  assert.match(line, /2\.0 KB/);
});

test("no notice carries a raw error, and every one is comment-prefixed", () => {
  // The text is rendered INTO a document. The raw detail goes to the MemQL
  // Connection channel through the redactor -- which is what these point at.
  const notices = [
    notConnectedNotice("staging"),
    noContentNotice("staging", "report.md"),
    fetchFailedNotice("staging", "report.md"),
    tooLargeNotice("report.md", ARTIFACT_BUFFER_LIMIT_BYTES * 2),
  ];
  for (const notice of notices) {
    for (const line of notice.split("\n").filter((l) => l !== "")) {
      assert.ok(line.startsWith("//"), line);
    }
  }
  assert.match(fetchFailedNotice("staging", "report.md"), /output channel/);
  assert.match(noContentNotice("staging", "report.md"), /report\.md/);
  assert.match(tooLargeNotice("report.md", ARTIFACT_BUFFER_LIMIT_BYTES * 2), /16\.0 MB/);
});
