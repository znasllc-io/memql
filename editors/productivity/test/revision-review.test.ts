import { test } from "node:test";
import assert from "node:assert/strict";
import * as vscode from "vscode";
import { assertRevisionBase, RevisionReview } from "../src/revisionReview.js";
import type { Documents, OpenDocument } from "../src/documents.js";

test("applying a reviewed draft requires the exact source bytes and saved revision", () => {
  const base = { resource: { id: "one" }, version: 3, revision: "file:3" } as OpenDocument;
  const proposal = { artifactId: "one", version: 3, revision: "file:3", content: "Original" };
  assert.doesNotThrow(() => assertRevisionBase(base, "Original", proposal));
  for (const change of [{ artifactId: "two" }, { version: 4 }, { revision: "file:4" }, { content: "Changed" }]) {
    assert.throws(() => assertRevisionBase(base, "Original", { ...proposal, ...change }), /document changed/);
  }
  assert.throws(() => assertRevisionBase(base, "Local changes", proposal), /document changed/);
});

test("reopening recovers the latest owned review instead of a stale browser receipt", async () => {
  const noop = () => ({ dispose() {} });
  Object.assign(vscode.workspace, { registerTextDocumentContentProvider: noop, onDidCloseTextDocument: noop });
  const uri = vscode.Uri.parse("memql-file://cluster/artifacts/doc/Guide.md");
  const document = { uri, isDirty: false, getText: () => "Existing text." } as vscode.TextDocument;
  const base = { resource: { id: "doc" }, revision: "file:1", version: 1, content: new TextEncoder().encode(document.getText()) } as OpenDocument;
  const proposal = { artifactId: "doc", revision: "file:1", version: 1, commentIds: ["extension"], instruction: "", revisedContent: "Existing text.\n\nNew section." };
  for (const initial of [undefined, { requestId: "old", fingerprint: "old" }]) {
    const saved = new Map<string, unknown>(initial ? [[`memql.documentRevision:${uri}`, initial]] : []);
    const context = { subscriptions: [], workspaceState: { get: (key: string) => saved.get(key), update: async (key: string, value: unknown) => { saved.set(key, value); } } } as unknown as vscode.ExtensionContext;
    const submitted: string[] = [];
    const files = {
      review: async () => ({ requestId: "current" }),
      revision: async (_base: unknown, requestId: string) => requestId === "old" ? { status: "succeeded" } : { status: "waiting", approvalId: "approval", proposal },
      requestRevision: async (_base: unknown, _comments: string[], _instruction: string, requestId: string) => { submitted.push(requestId); },
    } as unknown as Documents;
    const review = new RevisionReview(context, files, async () => base);
    assert.equal((await review.status(document))?.approvalId, "approval");
    assert.equal((await review.status(document))?.proposal, proposal);
    await review.prepare(document, ["extension"], "");
    assert.deepEqual(submitted, ["current"], "a recovered pending proposal reuses its durable request");
  }
});

test("an uncertain submission retains its request identifier during recovery", async () => {
  const noop = () => ({ dispose() {} });
  Object.assign(vscode.workspace, { registerTextDocumentContentProvider: noop, onDidCloseTextDocument: noop });
  const uri = vscode.Uri.parse("memql-file://cluster/artifacts/doc/Guide.md");
  const document = { uri } as vscode.TextDocument;
  const pending = { requestId: "uncertain", fingerprint: "pending" };
  const context = { subscriptions: [], workspaceState: { get: () => pending, update: async () => { assert.fail("must not overwrite an uncertain submission"); } } } as unknown as vscode.ExtensionContext;
  const files = { review: async () => ({ requestId: "old" }), revision: async () => { throw new Error("Receipt unavailable"); } } as unknown as Documents;
  const review = new RevisionReview(context, files, async () => ({} as OpenDocument));
  await assert.rejects(review.status(document), /Receipt unavailable/);
});
