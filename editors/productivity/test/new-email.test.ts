import { test } from "node:test";
import assert from "node:assert/strict";
import * as vscode from "vscode";
import { NewEmail } from "../src/newEmail.js";
import { EMAIL_COPY } from "../src/emailCopy.js";
import type { ConnectionLease, EditorConnectionAPI } from "../../vscode/src/connection/api.js";
import type { DocumentDictation } from "../src/dictation.js";
const link = "memql-file://client.example/new-email/client-org/New%20email";
const lease: ConnectionLease = { domain: "client.example", name: "Client", generation: 1 };
const draftKey = "memql.newEmail.draft:" + link;
const SVG = '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><rect width="10" height="10"/></svg>';
const row = (id: string, name: string, category: string, kind = "builtin") => ({ id, kind, name, category, description: `${name} layout`, wireframe: SVG, document: { subject: "Hello", blocks: [] } });
const article = row("builtin:article", "Article", "Articles");
const welcome = row("builtin:welcome", "Welcome note", "Welcome");
const write = { type: "write", layoutId: article.id, name: "Autumn welcome", description: "A short welcome." };
const refusal = "MemQL engine failed to execute query. Details: Add a brand kit before writing an email.";
type Answer = (call: string) => Promise<Record<string, unknown>[]> | Record<string, unknown>[];
async function fixture(options: { state?: Map<string, any>; connected?: boolean; page?: NewEmail; answers?: Record<string, Answer> } = {}) {
  const state = options.state ?? new Map<string, any>();
  const messages: any[] = [], calls: { name: string; call: string }[] = [], opened: any[][] = [], uploads: any[][] = [];
  let current = options.connected === false ? undefined : lease, disposed = false, connects = 0;
  const memento = { get: (key: string) => state.get(key), update: async (key: string, value: unknown) => { if (value === undefined) state.delete(key); else state.set(key, structuredClone(value)); } };
  const answers: Record<string, Answer> = {
    clientAccountsAll: () => [{ id: "client-org", name: "Client", status: "active" }, { id: "other-org", name: "Other", status: "active" }],
    emailLayouts: () => [article, welcome, row("org:mine", "Mine", "Saved", "organization")], emailRender: () => { throw new Error("emailRender must not be called"); },
    campaignCreateTemplateFromLayout: () => [{ templateId: "t-1", revision: 1, status: "draft", saved: true, issues: [] }],
    campaignComposeEmail: () => [{ templateId: "t-2", goalId: "g-1", revision: 1, state: "writing" }], ...options.answers,
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

test("start creates the template from the layout and opens it", async () => {
  const f = await fixture(); await Promise.all([f.receive({ ...write, type: "start" }), f.receive({ ...write, type: "start" })]);
  const calls = f.calls.filter(c => c.name === "campaignCreateTemplateFromLayout"); assert.equal(calls.length, 1);
  assert.equal(calls[0].call, 'builtin campaignCreateTemplateFromLayout(accountId: "client-org", name: "Autumn welcome", layoutId: "builtin:article")');
  assert.equal(f.calls.some(c => c.name === "campaignComposeEmail"), false);
  assert.deepEqual(f.opened.map(([command, uri, view]) => [command, uri.toString(), view]), [["vscode.openWith", "memql-file://client.example/templates/t-1/Autumn%20welcome.email.json", "memql.productivity.email"]]);
  assert.equal(f.disposed, true); assert.equal(f.state.has(draftKey), false);
});
test("write starts a composition with files and the reference picture", async () => {
  const state = new Map([[draftKey, { organizationId: "client-org", references: [{ fileId: "logo", label: "logo.png", kind: "picture" }, { fileId: "brief", label: "brief.md", kind: "file" }], reference: { fileId: "mock", label: "mock.png", kind: "picture" } }]]);
  const f = await fixture({ state }); await Promise.all([f.receive(write), f.receive(write)]);
  const calls = f.calls.filter(c => c.name === "campaignComposeEmail"); assert.equal(calls.length, 1);
  assert.equal(calls[0].call, 'builtin campaignComposeEmail(accountId: "client-org", name: "Autumn welcome", layoutId: "builtin:article", description: "A short welcome.", fileIds: ["logo", "brief"], referenceFileId: "mock")');
  assert.deepEqual(f.opened.map(([command, uri, view]) => [command, uri.toString(), view]), [["vscode.openWith", "memql-file://client.example/templates/t-2/Autumn%20welcome.email.json", "memql.productivity.email"]]);
  assert.equal(f.disposed, true); assert.equal(f.state.has(draftKey), false);
});
test("write without a picture sends no referenceFileId", async () => {
  const f = await fixture(); await f.receive(write);
  assert.doesNotMatch(f.calls.find(c => c.name === "campaignComposeEmail")!.call, /referenceFileId/);
});
test("a composition that could not start still opens the template", async () => {
  const f = await fixture({ answers: { campaignComposeEmail: () => [{ templateId: "t-3", revision: 1, state: "failed", error: "No model" }] } });
  await f.receive(write); assert.equal(f.opened.length, 1); assert.match(f.opened[0][1].toString(), /templates\/t-3\//); assert.equal(f.disposed, true);
});
test("a refused write shows the server's sentence and keeps the page and the draft", async () => {
  const f = await fixture({ answers: { campaignComposeEmail: () => { throw new Error(refusal); } } });
  await f.receive(write); const last = f.messages.at(-1);
  assert.equal(last.type, "failed"); assert.equal(last.problem.message, "Add a brand kit before writing an email.");
  assert.equal(f.opened.length, 0); assert.equal(f.disposed, false); assert.equal(f.state.get(draftKey).description, write.description);
  await f.receive(write); assert.equal(f.calls.filter(c => c.name === "campaignComposeEmail").length, 2, "the person can try again"); f.dispose();
});
test("setup renders no email and lists the built-in layouts with their wireframes", async () => {
  const f = await fixture(); assert.equal(f.calls.some(c => c.name === "emailRender" || c.name === "emailLayoutById"), false);
  const setup = f.messages.find(m => m.type === "setup" && m.connected);
  assert.deepEqual(setup.layouts, [article, welcome].map(({ id, name, category, description, wireframe }) => ({ id, name, category, description, wireframe })));
  assert.equal(f.messages.some(m => m.type === "thumbnail"), false); f.dispose();
});
test("a layout the list does not hold is refused and nothing is created", async () => {
  const f = await fixture(); await f.receive({ ...write, layoutId: "org:mine" });
  assert.equal(f.calls.some(c => c.name === "campaignComposeEmail"), false); assert.equal(f.messages.at(-1).type, "failed");
});
test("write needs a description; start does not", async () => {
  const f = await fixture(); await f.receive({ ...write, description: "  " });
  assert.equal(f.calls.some(c => c.name === "campaignComposeEmail"), false); assert.equal(f.messages.at(-1).problem.message, EMAIL_COPY.host.descriptionRequired);
  await f.receive({ ...write, type: "start", description: "" }); assert.equal(f.opened.length, 1);
});
test("the finished-email picture takes one PNG, JPEG or GIF, separate from the files", async () => {
  const f = await fixture(); let name = "mock.png";
  Object.assign(vscode.window, { showQuickPick: async () => EMAIL_COPY.host.fromComputer, showOpenDialog: async () => [vscode.Uri.file(`/tmp/${name}`)] });
  Object.assign(vscode.workspace.fs, { stat: async () => ({ size: 1 }), readFile: async () => new Uint8Array([1]) });
  name = "mock.md"; await f.receive({ type: "attachReference" }); assert.equal(f.uploads.length, 0); assert.equal(f.messages.at(-1).problem.message, EMAIL_COPY.host.pictureType);
  name = "mock.png"; await f.receive({ type: "attachReference" }); assert.equal(f.uploads.length, 1);
  assert.deepEqual(f.messages.at(-1), { type: "reference", item: { label: "mock.png" } }); assert.equal(f.state.get(draftKey).reference.fileId, "upload-1");
  assert.equal(f.state.get(draftKey).references.length, 0);
  await f.receive({ type: "clearReference" }); assert.deepEqual(f.messages.at(-1), { type: "reference", item: null }); assert.equal(f.state.get(draftKey).reference, undefined); f.dispose();
});
test("the page connects only when asked and the command opens the remembered organization", async () => {
  const f = await fixture({ connected: false }); assert.equal(f.connects, 0); assert.equal(f.calls.length, 0); assert.equal(f.messages.at(-1).connected, false);
  await assert.rejects(f.page.open(), { message: EMAIL_COPY.host.connectFirst }); await f.receive({ type: "connect" }); assert.equal(f.connects, 1);
  f.state.set("memql.newEmail.organization:client.example", "other-org"); await f.page.open(); assert.equal(f.opened.at(-1)![1].toString(), "memql-file://client.example/new-email/other-org/New%20email"); f.dispose();
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
