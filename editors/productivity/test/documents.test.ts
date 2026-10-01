import { test } from "node:test";
import assert from "node:assert/strict";
import { PDFDocument } from "pdf-lib";
import { changePDF } from "../src/pdf.js";
import { Documents, isZip, resourceFrom } from "../src/documents.js";
import { newTemplate, readTemplate } from "../src/templates.js";
import type { EditorConnectionAPI } from "../../vscode/src/connection/api.js";

test("ZIP detection handles misleading names and never extracts content", () => {
  assert.ok(isZip("backup.ZIP"));
  assert.ok(isZip("renamed.pdf", "application/zip"));
  assert.ok(isZip("renamed.txt", "", new Uint8Array([80, 75, 3, 4])));
  assert.ok(!isZip("document.pdf", "application/pdf", new Uint8Array([37, 80, 68, 70])));
});
test("only single, well-formed MemQL resources can be opened", () => {
  assert.equal(resourceFrom("memql-file://client.example/artifacts/abc/report.pdf").id, "abc");
  for (const value of ["https://client.example/artifacts/abc/report.pdf", "memql-file://user@client.example/artifacts/abc/a", "memql-file://client.example/artifacts/abc/a?token=secret", "memql-file://client.example/artifacts/abc/a%2Fb", "memql-file://client.example/artifacts/a%0A/a"]) assert.throws(() => resourceFrom(value));
});
test("email template files validate independently of a cluster", () => {
  assert.equal(readTemplate(newTemplate()).subject, "Thanks for subscribing");
  assert.throws(() => readTemplate('{"subject":"hello\\nworld","textBody":"x","htmlBody":""}'));
  assert.throws(() => readTemplate('{"subject":"x","textBody":"x","htmlBody":"","accountId":"another-client"}'));
});
test("PDF edits produce readable PDFs and leave the source untouched", async () => {
  const pdf = await PDFDocument.create(); pdf.addPage([400, 600]);
  const original = await pdf.save();
  const rotated = await changePDF(original, { kind: "rotate", page: 0 });
  assert.equal((await PDFDocument.load(rotated)).getPage(0).getRotation().angle, 90);
  assert.equal((await PDFDocument.load(original)).getPage(0).getRotation().angle, 0);
  const annotated = await changePDF(rotated, { kind: "text", page: 0, text: "Review", x: 36, y: 36 });
  assert.equal((await PDFDocument.load(annotated)).getPageCount(), 1);
  await assert.rejects(changePDF(original, { kind: "rotate", page: 2 }), /existing page/);
});
test("a file save uses its opened version and preserves edits on conflict", async () => {
  let requestedVersion = 0;
  const api = { uploadVersion: async (_lease: unknown, _id: unknown, _name: unknown, _mime: unknown, _content: unknown, version: number) => {
    requestedVersion = version; throw new Error("conflict");
  } } as unknown as EditorConnectionAPI;
  const doc = { resource: { domain: "client.example", kind: "artifacts" as const, id: "abc", name: "a.txt" },
    lease: { domain: "client.example", name: "client", generation: 1 }, content: new TextEncoder().encode("old"), mime: "text/plain", sourceId: "file", kind: "file", version: 3 };
  await assert.rejects(new Documents(api).save(doc, new TextEncoder().encode("new")), /conflict/);
  assert.equal(requestedVersion, 3); assert.equal(doc.version, 3);
  assert.equal(new TextDecoder().decode(doc.content), "old");
});
