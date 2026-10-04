import { test } from "node:test";
import assert from "node:assert/strict";
import { PDFEditor, type PDFBase } from "../src/pdfEditor.js";
import { stored, writes, Uri } from "./support/vscode.js";

test("PDF hot-exit recovery keeps the original save base and refuses a newer remote revision", async () => {
  stored.clear(); writes.length = 0;
  const uri = Uri.parse("memql-file://client.example/artifacts/pdf/file.pdf");
  const destination = Uri.parse("file:///recovery/document");
  stored.set(uri.toString(), new TextEncoder().encode("original PDF"));
  const base: PDFBase = { version: 3, revision: "file:3", sourceId: "file" };
  let latest = 3;
  let recovered: PDFBase | undefined;
  const access = {
    base: async () => base,
    recover: async (_uri: unknown, from: PDFBase) => { recovered = from; if (from.version !== latest) throw new Error("remote changed"); },
    refresh: async () => {}, release: () => {},
  };
  const first = new PDFEditor({ subscriptions: [] } as never, access);
  const document = await first.openCustomDocument(uri as never, {});
  document.update(new TextEncoder().encode("edited PDF"));
  const backup = await first.backupCustomDocument(document, { destination } as never);
  document.dispose();
  const second = new PDFEditor({ subscriptions: [] } as never, access);
  const restored = await second.openCustomDocument(uri as never, { backupId: backup.id });
  assert.equal(new TextDecoder().decode(restored.bytes), "edited PDF");
  latest = 4; writes.length = 0;
  await assert.rejects(second.saveCustomDocument(restored), /remote changed/);
  assert.deepEqual(recovered, base);
  assert.equal(writes.length, 0);
  assert.equal(new TextDecoder().decode(restored.bytes), "edited PDF");
  latest = 3;
  await second.saveCustomDocument(restored);
  assert.equal(new TextDecoder().decode(stored.get(uri.toString())), "edited PDF");
  await backup.delete();
  assert.equal(stored.has(destination.toString()), false);
  assert.equal(stored.has(`${destination}.memql-base.json`), false);
});
