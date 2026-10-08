import { test } from "node:test";
import assert from "node:assert/strict";
import * as vscode from "vscode";
import { MarkdownEditor } from "../src/markdownEditor.js";
import { Documents, type OpenDocument } from "../src/documents.js";

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>(done => { resolve = done; });
  return { promise, resolve };
}

test("a new extension proposal supersedes an in-flight completed review and keeps polling", async () => {
  const noop = () => ({ dispose() {} });
  Object.assign(vscode.Uri, { joinPath: (uri: vscode.Uri, path: string) => vscode.Uri.parse(`${uri.toString()}/${path}`) });
  Object.assign(vscode.workspace, { registerTextDocumentContentProvider: noop, onDidCloseTextDocument: noop, onDidChangeTextDocument: noop, onDidSaveTextDocument: noop });
  Object.assign(vscode.window, { onDidChangeVisibleTextEditors: noop, visibleTextEditors: [] });
  const source = "# Guide\n\nExisting text.\n";
  const uri = vscode.Uri.parse("memql-file://cluster/artifacts/doc/Guide.md");
  const document = { uri, fileName: "Guide.md", version: 1, isDirty: false, getText: () => source } as vscode.TextDocument;
  const base = { resource: { id: "doc" }, content: new TextEncoder().encode(source), version: 1, revision: "file:1" } as OpenDocument;
  const saved = new Map<string, unknown>([[`memql.documentRevision:${uri}`, { fingerprint: "old", requestId: "old" }]]);
  const context = { extensionUri: vscode.Uri.parse("file:///extension"), subscriptions: [], workspaceState: { get: (key: string) => saved.get(key), update: async (key: string, value: unknown) => { saved.set(key, value); } } } as unknown as vscode.ExtensionContext;
  const oldStarted = deferred<void>(), oldResponse = deferred<Record<string, unknown>>(), newStarted = deferred<void>();
  let newReads = 0;
  const proposal = { content: source, artifactId: "doc", commentIds: ["extension"], summary: "Add next steps", revisedContent: source + "\n## Next steps\n\nTry it.\n", edits: [{ before: "Existing text.", after: "Existing text.\n\n## Next steps\n\nTry it.", commentIds: ["extension"] }] };
  const files = {
    review: async () => ({ comments: [{ id: "extension", revision: base.revision, body: "Add next steps", anchor: { kind: "document-end" } }] }),
    requestRevision: async (_base: unknown, ids: string[]) => { assert.deepEqual(ids, ["extension"]); },
    revision: async (_base: unknown, requestId: string) => {
      if (requestId === "old") { oldStarted.resolve(); return oldResponse.promise; }
      newReads++; newStarted.resolve();
      return { status: "waiting", prepared: true, runId: "new", approvalId: "new-approval", proposal };
    },
  } as unknown as Documents;
  const messages: any[] = [];
  let receive!: (message: unknown) => Promise<void>, dispose!: () => void;
  const panel = { visible: true, active: true, onDidChangeViewState: noop, onDidDispose: (fn: () => void) => { dispose = fn; }, webview: {
    asWebviewUri: (value: vscode.Uri) => value,
    postMessage: async (message: unknown) => { messages.push(message); return true; },
    onDidReceiveMessage: (fn: typeof receive) => { receive = fn; return noop(); },
  } } as unknown as vscode.WebviewPanel;
  await new MarkdownEditor(context, files, async () => base).resolveCustomTextEditor(document, panel);
  try {
    const opening = receive({ type: "ready" });
    await oldStarted.promise;
    const submitting = receive({ type: "prepareRevision", version: 1, commentIds: ["extension"], instruction: "" });
    await newStarted.promise;
    await new Promise<void>(resolve => setImmediate(resolve));
    oldResponse.resolve({ status: "succeeded", runId: "old", result: { applied: false }, proposal: { summary: "Previous feedback" } });
    await Promise.all([opening, submitting]);
    const statuses = messages.filter(message => message.type === "revision");
    assert.equal(statuses.at(-1)?.status.approvalId, "new-approval");
    assert.deepEqual(statuses.at(-1)?.status.proposal.edits, proposal.edits);
    assert.ok(!statuses.some(message => message.status.runId === "old"), "an old terminal response must not replace the new request or stop its updates");
    assert.ok(newReads >= 2, "submission must trigger another read after the old read finishes");
  } finally { dispose(); }
});
