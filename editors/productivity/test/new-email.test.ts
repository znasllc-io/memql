import { test } from "node:test";
import assert from "node:assert/strict";
import * as vscode from "vscode";
import { NewEmail } from "../src/newEmail.js";
import { EMAIL_COPY } from "../src/emailCopy.js";
import type { ConnectionLease, EditorConnectionAPI } from "../../vscode/src/connection/api.js";
import type { DocumentDictation } from "../src/dictation.js";
const link = "memql-file://client.example/new-email/client-org/New%20email";
const lease: ConnectionLease = { domain: "client.example", name: "Client", generation: 1 };
const draftKey = "memql.newEmail.draft:" + link, pendingKey = "memql.newEmail.pending:" + link;
const article = { id: "builtin:article", name: "Article", kind: "builtin", document: { subject: "Hello", blocks: [] } };
const write = { type: "write", layoutId: article.id, name: "Autumn welcome.email.json", description: "A short welcome." };
type Answer = (call: string) => Promise<Record<string, unknown>[]> | Record<string, unknown>[];
async function fixture(options: { state?: Map<string, any>; connected?: boolean; page?: NewEmail; answers?: Record<string, Answer> } = {}) {
  const state = options.state ?? new Map<string, any>();
  const messages: any[] = [], calls: { name: string; call: string }[] = [], opened: any[][] = [], uploads: any[][] = [];
  let current = options.connected === false ? undefined : lease, disposed = false, connects = 0;
  const memento = { get: (key: string) => state.get(key), update: async (key: string, value: unknown) => { if (value === undefined) state.delete(key); else state.set(key, structuredClone(value)); } };
  const answers: Record<string, Answer> = {
    clientAccountsAll: () => [{ id: "client-org", name: "Client", status: "active" }, { id: "other-org", name: "Other", status: "active" }],
    emailLayouts: () => [article], emailRender: () => [{ html: "<p>Hello</p>" }],
    composeMaterialize: () => [{ compositionId: "c1" }], compositionById: () => [{ status: "ready", outputFileId: "f1", name: "Autumn welcome" }],
    libraryArtifactForFile: () => [{ id: "a1", title: "Autumn welcome.email.json" }], composeCancel: () => [{ cancelled: true }], ...options.answers,
  };
  let receive!: (message: any) => Promise<void>, closed!: () => void;
  const panel = { viewColumn: 1, dispose: () => { if (!disposed) { disposed = true; closed(); } }, onDidDispose: (fn: () => void) => { closed = fn; return { dispose() {} }; },
    webview: { html: "", options: {}, asWebviewUri: (uri: vscode.Uri) => uri, postMessage: async (message: any) => { messages.push(structuredClone(message)); return true; }, onDidReceiveMessage: (fn: typeof receive) => { receive = fn; return { dispose() {} }; } } } as unknown as vscode.WebviewPanel;
  Object.assign(vscode.commands, { executeCommand: async (...args: any[]) => { opened.push(args); } });
  const api = { current: () => current, connect: async () => { connects++; current = lease; return lease; }, onDidChange: () => ({ dispose() {} }),
    execute: async (_lease: ConnectionLease, name: string, call: string) => { calls.push({ name, call }); assert.equal(_lease, lease); return (answers[name] ?? (() => []))(call); },
    uploadFile: async (...args: any[]) => { uploads.push(args); return { fileId: `upload-${uploads.length}` }; },
  } as unknown as EditorConnectionAPI;
  const page = options.page ?? new NewEmail({ extensionUri: vscode.Uri.file("/extension"), workspaceState: memento, globalState: memento, subscriptions: [] } as unknown as vscode.ExtensionContext, api, { available: async () => false, cancel() {} } as unknown as DocumentDictation);
  await page.resolveCustomEditor(page.openCustomDocument(vscode.Uri.parse(link)), panel);
  await receive({ type: "ready" });
  return { receive, messages, calls, state, opened, uploads, page, dispose: () => panel.dispose(), answers, get disposed() { return disposed; }, get connects() { return connects; } };
}

test("Write it creates once with the chosen layout, organization and references", async () => {
  const state = new Map([[draftKey, { organizationId: "client-org", references: [{ fileId: "logo", label: "logo.png", kind: "picture", include: true }, { fileId: "brief", label: "brief.md", kind: "file", include: false }] }]]);
  const f = await fixture({ state }); await Promise.all([f.receive(write), f.receive(write)]);
  const starts = f.calls.filter(c => c.name === "composeMaterialize"); assert.equal(starts.length, 1);
  for (const pattern of [/layoutId: "builtin:article"/, /accountIds: \["client-org"\]/, /name: "Autumn welcome"/, /statement: "A short welcome."/, /outputKind: "email_template"/, /format: "json"/, /requestId: "/, /includeImages: true[^}]*ref: "logo"/]) assert.match(starts[0].call, pattern);
  assert.doesNotMatch(starts[0].call.match(/\{[^{}]*ref: "brief"[^{}]*\}/)![0], /includeImages/);
  assert.deepEqual(f.opened.map(([command, uri, view]) => [command, uri.toString(), view]), [["vscode.openWith", "memql-file://client.example/artifacts/a1/Autumn%20welcome.email.json", "memql.productivity.email"]]);
  assert.equal(f.disposed, true); assert.equal(f.state.has(pendingKey), false); assert.equal(f.state.has(draftKey), false);
});
test("Start from the layout sends neither description nor references", async () => {
  const f = await fixture(); await f.receive({ ...write, type: "start" }); assert.doesNotMatch(f.calls.find(c => c.name === "composeMaterialize")!.call, /statement:|sources:/);
});
test("a confirmed failed composition keeps the draft and retry gets a fresh request", async () => {
  const f = await fixture({ answers: { compositionById: () => [{ status: "failed", errorMessage: "Could not write." }] } });
  await f.receive(write); await f.receive(write); const starts = f.calls.filter(c => c.name === "composeMaterialize");
  assert.notEqual(starts[0].call.match(/requestId: "([^"]+)"/)![1], starts[1].call.match(/requestId: "([^"]+)"/)![1]);
  assert.equal(f.state.get(draftKey).description, write.description); assert.equal(f.state.has(pendingKey), false); assert.equal(f.messages.at(-1).type, "failed"); f.dispose();
});
test("a lost creation response survives reload and resends the identical request", async () => {
  const f = await fixture({ answers: { composeMaterialize: () => { throw new Error("Connection lost"); } } }); await f.receive(write);
  const original = f.calls.find(c => c.name === "composeMaterialize")!.call; assert.ok(f.state.get(pendingKey)); f.dispose();
  const reopened = await fixture({ state: f.state }); assert.equal(reopened.calls.find(c => c.name === "composeMaterialize")!.call, original); assert.equal(reopened.opened.length, 1); assert.equal(reopened.disposed, true);
});
test("closing stops following without cancelling; reopening follows and opens once", async () => {
  let resolve!: (rows: Record<string, unknown>[]) => void; const reading = new Promise<Record<string, unknown>[]>(done => { resolve = done; });
  const f = await fixture({ answers: { compositionById: () => reading } }); const run = f.receive(write);
  while (!f.calls.some(c => c.name === "compositionById")) await new Promise(done => setTimeout(done, 5));
  f.dispose(); resolve([{ status: "ready", outputFileId: "f1" }]); await run;
  assert.equal(f.calls.some(c => c.name === "composeCancel"), false); assert.equal(f.opened.length, 0);
  const reopened = await fixture({ state: f.state }); assert.equal(reopened.calls.some(c => c.name === "composeMaterialize"), false); assert.equal(reopened.opened.length, 1);
});
test("Cancel cancels the composition and keeps the draft", async () => {
  const f = await fixture({ answers: { compositionById: () => [{ status: "composing" }] } }); const run = f.receive(write);
  while (!f.calls.some(c => c.name === "composeMaterialize")) await new Promise(done => setTimeout(done, 1));
  await f.receive({ type: "cancel" }); await run; assert.equal(f.calls.filter(c => c.name === "composeCancel").length, 1);
  assert.equal(f.messages.at(-1).type, "idle"); assert.equal(f.state.get(draftKey).name, write.name); f.dispose();
});
test("the page connects only when asked and the command opens the remembered organization", async () => {
  const f = await fixture({ connected: false }); assert.equal(f.connects, 0); assert.equal(f.calls.length, 0); assert.equal(f.messages.at(-1).connected, false);
  await assert.rejects(f.page.open(), { message: EMAIL_COPY.host.connectFirst }); await f.receive({ type: "connect" }); assert.equal(f.connects, 1);
  f.state.set("memql.newEmail.organization:client.example", "other-org"); await f.page.open(); assert.equal(f.opened.at(-1)![1].toString(), "memql-file://client.example/new-email/other-org/New%20email"); f.dispose();
});
test("organization changes render in the new context; omitted documents read by bare row id", async () => {
  const f = await fixture({ answers: { emailLayouts: () => [{ ...article, id: "org:opaque", layoutId: "bare-row", kind: "organization", document: undefined, documentOmitted: true }], emailLayoutById: () => [{ document: article.document }] } });
  assert.match(f.calls.find(c => c.name === "emailLayoutById")!.call, /layoutId: "bare-row"/);
  await f.receive({ type: "organization", id: "other-org" }); assert.equal(f.calls.filter(c => c.name === "emailRender").length, 2); assert.match(f.calls.filter(c => c.name === "emailRender").at(-1)!.call, /accountId: "other-org"/);
  await f.receive({ ...write, layoutId: "org:opaque" }); assert.match(f.calls.find(c => c.name === "composeMaterialize")!.call, /layoutId: "org:opaque"/);
});
test("a refused thumbnail gets an empty image and remains selectable", async () => {
  const f = await fixture({ answers: { emailRender: () => { throw new Error("Unavailable image"); } } }); assert.deepEqual(f.messages.find(m => m.type === "thumbnail"), { type: "thumbnail", id: article.id, html: "" }); await f.receive({ ...write, type: "start" }); assert.equal(f.opened.length, 1);
});
test("reference uploads validate the entire selection before uploading anything", async () => {
  const f = await fixture(); let names = Array.from({ length: 65 }, (_, i) => `file-${i}.md`), size = 1;
  Object.assign(vscode.window, { showQuickPick: async () => EMAIL_COPY.host.fromComputer, showOpenDialog: async () => names.map(name => vscode.Uri.file(`/tmp/${name}`)) });
  Object.assign(vscode.workspace.fs, { stat: async () => ({ size }), readFile: async () => new Uint8Array([1]) });
  await f.receive({ type: "attach" }); assert.equal(f.uploads.length, 0); assert.equal(f.messages.at(-1).problem.message, EMAIL_COPY.host.tooMany);
  names = ["good.md", "bad.exe"]; await f.receive({ type: "attach" }); assert.equal(f.uploads.length, 0); assert.equal(f.messages.at(-1).problem.message, EMAIL_COPY.host.unsupported);
  names = ["brief.md"]; size = 17 * 1024 * 1024; await f.receive({ type: "attach" }); assert.equal(f.uploads.length, 0); assert.equal(f.messages.at(-1).problem.message, EMAIL_COPY.host.tooLarge);
  size = 1; await f.receive({ type: "attach" }); assert.equal(f.uploads.length, 1); assert.equal(f.state.get(draftKey).references[0].fileId, "upload-1"); f.dispose();
});


test("replacing a page during creation shares its in-flight start", async () => {
  let resolve!: (rows: Record<string, unknown>[]) => void;
  const start = new Promise<Record<string, unknown>[]>(done => { resolve = done; });
  const f = await fixture({ answers: { composeMaterialize: () => start } });
  const run = f.receive(write);
  while (!f.calls.some(call => call.name === "composeMaterialize")) await new Promise(done => setTimeout(done, 1));
  const replacing = fixture({ state: f.state, page: f.page });
  await new Promise(done => setTimeout(done, 5));
  assert.equal(f.calls.filter(call => call.name === "composeMaterialize").length, 1);
  resolve([{ compositionId: "c1" }]); await run;
  const replacement = await replacing;
  assert.equal(replacement.opened.length, 1); assert.equal(replacement.disposed, true);
  assert.equal(f.calls.filter(call => call.name === "composeMaterialize").length, 1);
});


test("host readiness waits past the initial loading render", async () => {
  const f = await fixture({ connected: false });
  await f.receive({ type: "shown", state: EMAIL_COPY.newEmail.loading, loading: true });
  let completed = false;
  const ready = f.page.whenShown(vscode.Uri.parse(link)).then(value => { completed = true; return value; });
  await Promise.resolve();
  await Promise.resolve();
  assert.equal(completed, false, "the loading render is not the host's settled state");
  await f.receive({ type: "shown", state: "Not connected to client.example", loading: false });
  assert.deepEqual(await ready, { state: "Not connected to client.example" });
  f.dispose();
});
