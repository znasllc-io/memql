// The email editor's host (src/emailEditor.ts) against a fake panel, a fake document and fake ports, as
// markdown-refresh.test.ts drives the Markdown editor. The page itself is emailView.ts's (email-view.test.ts);
// this file holds what the host does with the page's messages and what it sends back.
import { test, type TestContext } from "node:test";
import assert from "node:assert/strict";
import * as vscode from "vscode";
import { buildEmailRender } from "@znasllc-io/memql-sdk-core/client";
import { EmailEditor } from "../src/emailEditor.js";
import type { Documents, OpenDocument } from "../src/documents.js";
import { EMAIL_COPY, fill } from "../src/emailCopy.js";
import { editEmail, minimalEdit, readEmail, writeEmail, type EmailDocument, type EmailEdit } from "../src/emailDocument.js";
import type { ConnectionLease, EditorConnectionAPI } from "../../vscode/src/connection/api.js";

const C = EMAIL_COPY.editor, H = EMAIL_COPY.host;
const VIEW = "memql.productivity.email";
const FILE = "memql-file://client.example/artifacts/art-1/Welcome.email.json";
const LOCAL = "file:///work/Welcome.email.json";
const lease: ConnectionLease = Object.freeze({ domain: "client.example", name: "Client cluster", generation: 1 });
const theme = { page: "#eef0f3", canvas: "#ffffff", primary: "#2f5d8a", secondary: "#6f93b8", accent: "#d9822b", tint: "#f5f4f0", text: "#2b2b2b", muted: "#8a8a8a", headingFont: "Georgia", bodyFont: "Helvetica" };
const welcome = { subject: "Welcome", theme, blocks: [
  { id: "headline", type: "heading", text: "Hi {{displayName}}" },
  { id: "intro", type: "text", text: "Your {{fields.plan}} plan is ready." },
  { id: "banner", type: "image", src: "", alt: "", width: 600 },
  { id: "cta", type: "button", label: "Start", href: "https://acme.example/start" },
  { id: "foot", type: "footer", links: [{ label: "Unsubscribe", href: "{{unsubscribeUrl}}" }], lines: ["Acme"] },
] } as unknown as EmailDocument;
const welcomeText = writeEmail(welcome);
const custom = { subject: "Thanks for subscribing", textBody: "Thanks for joining us.", htmlBody: "<p>Thanks for joining us.</p>" } as EmailDocument;
const rendered = '<html><body><p data-block="headline">Hi</p></body></html>';
const ENGINE = "MemQL engine failed to execute query. Details: ";
const PNG = new Uint8Array([137, 80, 78, 71, 13, 10, 26, 10, 0, 0, 0, 13, 73, 72, 68, 82, 0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0]);

type Rows = Record<string, unknown>[];
type Answer = (call: string) => Rows | Promise<Rows>;
interface Options {
  uri?: string;
  text?: string;
  eol?: number;
  connected?: boolean;
  opened?: Partial<OpenDocument>;
  answers?: Record<string, Answer>;
  state?: Record<string, unknown>;
}

function deferred<T>() {
  let resolve!: (value: T) => void, reject!: (error: unknown) => void;
  const promise = new Promise<T>((done, fail) => { resolve = done; reject = fail; });
  return { promise, resolve, reject };
}

function fileDocument(overrides: Partial<OpenDocument> = {}): OpenDocument {
  return {
    resource: { domain: "client.example", kind: "artifacts", id: "art-1", name: "Welcome.email.json" }, lease,
    content: new TextEncoder().encode(welcomeText), mime: "application/json", sourceId: "file-1", kind: "file", version: 1, revision: "file:1",
    accountIds: ["client-org"], ...overrides,
  } as OpenDocument;
}

function templateDocument(accountId: string): OpenDocument {
  return {
    resource: { domain: "client.example", kind: "templates", id: "t9", name: "Welcome.email.json" }, lease,
    content: new TextEncoder().encode(welcomeText), mime: "application/json", sourceId: "t9", kind: "campaign_template", version: 0,
    revision: "2026-10-12T09:00:00Z", template: { name: "Welcome", accountId, status: "ready" },
  } as OpenDocument;
}

const organizations: Rows = [{ id: "client-org", name: "Client", status: "active" }, { id: "other-org", name: "Other", status: "active" }];
const defaults = (): Record<string, Answer> => ({
  clientAccountsAll: () => organizations,
  compositionForOutputFile: () => [],
  templateForSource: () => [],
  emailLayouts: () => [],
  emailRender: () => [{ subject: "Welcome", html: rendered, text: "Hi", issues: [] }],
});

/**
 * A clock the test moves by hand, so "after a pause" is exact and the suite never sleeps: setTimeout and clearTimeout are
 * replaced for the test (and put back after it). A test that opens two editors moves one clock.
 */
const clocks = new WeakMap<TestContext, (ms: number) => void>();
function clock(t: TestContext): (ms: number) => void {
  const existing = clocks.get(t);
  if (existing) return existing;
  let now = 0, next = 1;
  const timers = new Map<number, { at: number; run: () => void }>();
  t.mock.method(globalThis, "setTimeout", ((run: () => void, ms = 0) => { const id = next++; timers.set(id, { at: now + ms, run }); return id; }) as never);
  t.mock.method(globalThis, "clearTimeout", ((id: number) => { timers.delete(id); }) as never);
  const advance = (ms: number) => {
    now += ms;
    for (;;) {
      let due: [number, { at: number; run: () => void }] | undefined;
      for (const entry of timers) if (entry[1].at <= now && (!due || entry[1].at < due[1].at)) due = entry;
      if (!due) return;
      timers.delete(due[0]);
      due[1].run();
    }
  };
  clocks.set(t, advance);
  return advance;
}

/** One email editor panel over a fake document, connected (unless asked not to be) to a fake cluster. */
async function open(t: TestContext, options: Options = {}) {
  const advance = clock(t);
  const uri = vscode.Uri.parse(options.uri ?? FILE);
  let text = options.text ?? welcomeText;
  const log: string[] = [];
  const changeListeners = new Set<(event: { document: unknown }) => void>();
  const saveListeners = new Set<(document: unknown) => void>();
  const document = {
    uri, fileName: uri.path, version: 1, isDirty: false, isClosed: false, eol: options.eol ?? vscode.EndOfLine.LF, languageId: "json",
    getText: () => text,
    // An offset as its own position, so a span reads back exactly.
    positionAt: (offset: number) => new vscode.Position(0, offset),
    save: async () => { log.push("save"); document.isDirty = false; for (const listener of [...saveListeners]) listener(document); return true; },
  };
  const change = (next: string) => {
    text = next; document.version++; document.isDirty = true;
    for (const listener of [...changeListeners]) listener({ document });
  };
  const applied: { start: number; end: number; text: string }[] = [];
  let refuseApply = false;
  Object.assign(vscode.workspace, {
    onDidChangeTextDocument: (listener: (event: { document: unknown }) => void) => { changeListeners.add(listener); return { dispose: () => changeListeners.delete(listener) }; },
    onDidSaveTextDocument: (listener: (document: unknown) => void) => { saveListeners.add(listener); return { dispose: () => saveListeners.delete(listener) }; },
    openTextDocument: async () => document,
    applyEdit: async (edit: vscode.WorkspaceEdit) => {
      const replacements = (edit as unknown as { replacements: { uri: vscode.Uri; range: vscode.Range; text: string }[] }).replacements;
      assert.equal(replacements.length, 1, "an edit is one replacement");
      const [only] = replacements;
      assert.equal(only.uri.toString(), uri.toString());
      // As in VS Code, the edit lands on a later turn (the change is told before applyEdit answers).
      await new Promise(resolve => setImmediate(resolve));
      if (refuseApply) return false;
      const start = only.range.start.character, end = only.range.end.character;
      applied.push({ start, end, text: only.text });
      change(text.slice(0, start) + only.text + text.slice(end));
      return true;
    },
  });
  const pictures = new Map<string, Uint8Array>();
  Object.assign(vscode.workspace.fs, {
    stat: async (file: vscode.Uri) => { log.push(`stat ${file}`); return { type: 1, ctime: 0, mtime: 0, size: pictures.get(file.toString())?.length ?? 0 }; },
    readFile: async (file: vscode.Uri) => { log.push(`read ${file}`); return pictures.get(file.toString()) ?? new Uint8Array(); },
  });
  const commands: { name: string; args: unknown[] }[] = [];
  Object.assign(vscode.commands, { executeCommand: async (name: string, ...args: unknown[]) => { commands.push({ name, args }); log.push(name === "vscode.openWith" ? "open" : name); return undefined; } });
  const quickPicks: { items: any[]; options: any }[] = [];
  const dialogs: any[] = [], toasts: string[] = [], shownDocuments: unknown[] = [], closed: unknown[] = [];
  let pick: (items: any[]) => any = () => undefined;
  let chosenFiles: vscode.Uri[] | undefined;
  const localTab = { input: new vscode.TabInputCustom(uri, VIEW) };
  const tabs = [{ tabs: [{ input: new vscode.TabInputText(uri) }, localTab, { input: new vscode.TabInputCustom(vscode.Uri.parse("file:///work/Other.email.json"), VIEW) }] }];
  Object.assign(vscode.window, {
    showQuickPick: async (items: any[], options: any) => { quickPicks.push({ items, options }); return pick(items); },
    showOpenDialog: async (options: unknown) => { dialogs.push(options); return chosenFiles; },
    showInformationMessage: async (message: string) => { toasts.push(message); return undefined; },
    showErrorMessage: async (message: string) => { toasts.push(message); return undefined; },
    showTextDocument: async (shown: unknown) => { shownDocuments.push(shown); return undefined; },
    visibleTextEditors: [],
    tabGroups: { all: tabs, close: async (closing: unknown) => { closed.push(closing); log.push("close"); return true; } },
  });

  let current: ConnectionLease | undefined = options.connected === false ? undefined : lease;
  const connectionListeners = new Set<() => void>();
  const calls: { name: string; call: string }[] = [], connects: string[] = [];
  const answers: Record<string, Answer> = { ...defaults(), ...options.answers };
  const api = {
    version: 1,
    current: () => current,
    onDidChange: (listener: () => void) => { connectionListeners.add(listener); return { dispose: () => connectionListeners.delete(listener) }; },
    connect: async (domain: string) => { connects.push(domain); current = lease; for (const listener of [...connectionListeners]) listener(); return lease; },
    execute: async (actual: ConnectionLease, name: string, call: string) => {
      assert.equal(actual, current, "a call is made on the current connection");
      calls.push({ name, call });
      log.push(name);
      const answer = answers[name];
      if (!answer) throw new Error(`unexpected call ${name}`);
      return answer(call);
    },
  } as unknown as EditorConnectionAPI;
  const uploads: { lease: unknown; name: string; mime: string; text: string; bytes: Uint8Array }[] = [];
  const files = {
    createFile: async (actual: ConnectionLease, name: string, mime: string, bytes: Uint8Array) => {
      uploads.push({ lease: actual, name, mime, text: new TextDecoder().decode(bytes), bytes });
      log.push("upload");
      return `memql-file://${actual.domain}/artifacts/${name.endsWith(".email.json") ? "new-1" : "up-1"}/${encodeURIComponent(name)}`;
    },
  } as unknown as Documents;
  const opened = options.uri?.startsWith("file:") ? undefined : (options.opened as OpenDocument | undefined) ?? fileDocument();
  let loads = 0;
  const load = async () => { loads++; if (!opened) throw new Error("a local email is not loaded"); return opened; };
  const state = new Map<string, unknown>(Object.entries(options.state ?? {}));
  const stateWrites: { key: string; value: unknown }[] = [];
  const context = {
    extensionUri: vscode.Uri.parse("file:///extension"), subscriptions: [],
    workspaceState: {
      get: (key: string) => state.get(key),
      update: async (key: string, value: unknown) => { stateWrites.push({ key, value }); if (value === undefined) state.delete(key); else state.set(key, value); },
    },
  } as unknown as vscode.ExtensionContext;
  const editor = new EmailEditor(context, api, files, load);

  const panels: ReturnType<typeof fakePanel>[] = [];
  function fakePanel() {
    const messages: any[] = [];
    let receive!: (message: unknown) => Promise<void> | void, dispose!: () => void;
    const panel = {
      visible: true, active: true, viewColumn: 2, disposed: false,
      onDidChangeViewState: () => ({ dispose() {} }),
      onDidDispose: (listener: () => void) => { dispose = listener; return { dispose() {} }; },
      dispose: () => { if (!panel.disposed) { panel.disposed = true; dispose(); } },
      reveal: () => {},
      webview: {
        options: {}, html: "", cspSource: "vscode-webview:",
        asWebviewUri: (value: vscode.Uri) => value,
        postMessage: async (message: unknown) => { messages.push(JSON.parse(JSON.stringify(message))); log.push(`post ${(message as { type: string }).type}`); return true; },
        onDidReceiveMessage: (listener: typeof receive) => { receive = listener; return { dispose() {} }; },
      },
    };
    return { panel, messages, send: async (message: unknown) => { await receive(message); } };
  }
  const settle = async () => { for (let i = 0; i < 40; i++) await new Promise(resolve => setImmediate(resolve)); };
  async function attach() {
    const view = fakePanel();
    panels.push(view);
    await editor.resolveCustomTextEditor(document as unknown as vscode.TextDocument, view.panel as unknown as vscode.WebviewPanel);
    await view.send({ type: "ready" });
    await settle();
    return view;
  }
  const view = await attach();
  const of = (type: string) => view.messages.filter(message => message.type === type);
  return {
    editor, document, view, panel: view.panel, uri, calls, connects, uploads, applied, commands, quickPicks, dialogs, toasts, closed, localTab, state, stateWrites, log, pictures,
    text: () => text, change, settle, attach, of,
    send: async (message: unknown) => { await view.send(message); await settle(); },
    tick: async (ms: number) => { advance(ms); await settle(); },
    renders: () => calls.filter(call => call.name === "emailRender"),
    bars: () => of("bar").map(bar => ({ state: bar.state, acts: bar.acts })),
    lastBar: () => of("bar").at(-1),
    problems: () => of("problem").map(message => message.problem.message),
    loads: () => loads,
    setConnected: (connected: boolean | string) => {
      current = connected === true ? lease : typeof connected === "string" ? Object.freeze({ domain: connected, name: "Other cluster", generation: 2 }) : undefined;
      for (const listener of [...connectionListeners]) listener();
    },
    answer: (name: string, answer: Answer) => { answers[name] = answer; },
    pickWith: (choose: (items: any[]) => any) => { pick = choose; },
    chooseFiles: (chosen: vscode.Uri[] | undefined) => { chosenFiles = chosen; },
    refuseApply: () => { refuseApply = true; },
    edited: (edit: EmailEdit) => writeEmail(editEmail(readEmail(text), edit)),
  };
}

const stateKey = (uri: string) => `memql.emailView:${uri}`;

// ---------------------------------------------------------------------------
// The preview
// ---------------------------------------------------------------------------

test("the preview renders through emailRender with the organization and samples, once per pause", async t => {
  const f = await open(t, { state: { [stateKey(FILE)]: { width: "desktop", samples: { "fields.plan": "Gold", displayName: "Ada", "fields.gone": "Not in the email" } } } });
  // The organization the file is labelled with is known before the first render, and the page is told it.
  assert.deepEqual(f.of("context").at(-1), { type: "context", connected: true, organization: { id: "client-org", name: "Client" }, choices: [] });
  assert.deepEqual(f.renders().map(call => call.call), [buildEmailRender({ document: welcome as unknown as Record<string, unknown>, accountId: "client-org", sample: { displayName: "Ada", fields: { plan: "Gold" } } })],
    "the first render carries the organization and the stored sample values of the tags the email uses");
  const first = f.of("render");
  assert.equal(first.length, 1);
  assert.deepEqual({ ...first[0], seq: 0 }, { type: "render", seq: 0, html: rendered, text: "Hi", subject: "Welcome", issues: [] });

  f.calls.length = 0;
  for (const subject of ["W", "We", "Welcome back"]) { f.change(f.edited({ kind: "subject", value: subject })); await f.tick(100); }
  await f.tick(199);
  assert.equal(f.renders().length, 0, "nothing renders while the changes keep coming");
  await f.tick(1);
  assert.deepEqual(f.renders().map(call => call.call), [buildEmailRender({ document: readEmail(f.text()) as unknown as Record<string, unknown>, accountId: "client-org", sample: { displayName: "Ada", fields: { plan: "Gold" } } })],
    "three quick changes make one render, of the last document");
  const renders = f.of("render");
  assert.equal(renders.length, 2);
  assert.ok(renders[1].seq > renders[0].seq, "a later render has a later seq");

  // A sample typed in Preview with is a change too: one render after the pause, with the new value.
  f.calls.length = 0;
  await f.send({ type: "viewState", state: { width: "desktop", samples: { "fields.plan": "Silver", displayName: "Ada" } } });
  assert.equal(f.renders().length, 0);
  await f.tick(300);
  assert.equal(f.renders().length, 1);
  assert.match(f.renders()[0].call, /sample: \{displayName: "Ada", fields: \{plan: "Silver"\}\}/);
  // Changing only the width or the selection is no reason to render again.
  f.calls.length = 0;
  await f.send({ type: "viewState", state: { width: "mobile", selected: "cta", samples: { "fields.plan": "Silver", displayName: "Ada" } } });
  await f.tick(300);
  assert.equal(f.renders().length, 0);
});

test("a stale render result is dropped", async t => {
  const f = await open(t);
  assert.equal(f.of("render").length, 1);
  const answers: ReturnType<typeof deferred<Rows>>[] = [];
  f.answer("emailRender", () => { const answer = deferred<Rows>(); answers.push(answer); return answer.promise; });
  f.change(f.edited({ kind: "subject", value: "Second" }));
  await f.tick(300);
  assert.equal(answers.length, 1, "the second render is asked for");
  f.change(f.edited({ kind: "subject", value: "Third" }));
  await f.tick(300);
  assert.equal(answers.length, 1, "one render at a time: the third waits for the second");
  answers[0].resolve([{ subject: "Second", html: "<p>second</p>", text: "second", issues: [] }]);
  await f.settle();
  assert.equal(answers.length, 2, "the third is asked for once the second has answered");
  answers[1].resolve([{ subject: "Third", html: "<p>third</p>", text: "third", issues: [] }]);
  await f.settle();
  const renders = f.of("render");
  assert.deepEqual(renders.map(render => render.subject), ["Welcome", "Third"], "the second's answer is older than the newest request and is never shown");
  assert.ok(renders[1].seq > renders[0].seq);
  // A failure that is out of date is dropped the same way.
  f.change(f.edited({ kind: "subject", value: "Fourth" }));
  await f.tick(300);
  f.change(f.edited({ kind: "subject", value: "Fifth" }));
  await f.tick(300);
  answers[2].reject(new Error(`emailRender: ${ENGINE}Previews are busy right now. Try again in a moment.`));
  await f.settle();
  answers[3].resolve([{ subject: "Fifth", html: "<p>fifth</p>", text: "fifth", issues: [] }]);
  await f.settle();
  assert.equal(f.of("renderFailed").length, 0);
  assert.equal(f.of("render").at(-1).subject, "Fifth");
});

test("a render the cluster refuses is shown in its words and counts as an issue", async t => {
  const f = await open(t, { answers: { emailRender: () => { throw new Error(`emailRender: ${ENGINE}Previews are busy right now. Try again in a moment.`); } } });
  const failed = f.of("renderFailed");
  assert.equal(failed.length, 1);
  assert.equal(failed[0].problem.message, "Previews are busy right now. Try again in a moment.", "the cluster's sentence, without the call's name or the engine's frame");
  assert.deepEqual(f.lastBar(), { type: "bar", state: C.issuesOne, acts: ["saveLayout"], layoutName: "Welcome" }, "a refused render offers no Publish");
  // The next render that works clears it.
  f.answer("emailRender", () => [{ subject: "Welcome", html: rendered, text: "Hi", issues: [] }]);
  f.change(f.edited({ kind: "subject", value: "Again" }));
  await f.tick(300);
  assert.equal(f.lastBar().state, C.unsaved);
  assert.deepEqual(f.lastBar().acts, ["saveLayout", "publish"]);
});

test("seq stays monotonic for the panel's life, across a reload of the page", async t => {
  const f = await open(t);
  await f.send({ type: "ready" });
  await f.send({ type: "ready" });
  const seqs = f.of("render").map(render => render.seq);
  assert.equal(seqs.length, 3);
  assert.deepEqual([...seqs].sort((a, b) => a - b), seqs);
  assert.equal(new Set(seqs).size, 3, "a reloaded page never sees a seq it has already been given");
  // A reloaded page is told everything again, the bar before the document, so the first thing it reports is the state.
  const ready = f.view.messages.slice(f.view.messages.findLastIndex(message => message.type === "restore" || message.type === "context"));
  assert.ok(ready.findIndex(message => message.type === "bar") >= 0, "a reloaded page gets the bar again, though it has not changed");
  assert.ok(ready.findIndex(message => message.type === "bar") < ready.findIndex(message => message.type === "document"));
});

// ---------------------------------------------------------------------------
// Edits
// ---------------------------------------------------------------------------

test("an inspector edit becomes one minimal text edit", async t => {
  const f = await open(t);
  const before = f.text();
  const expected = f.edited({ kind: "field", block: "cta", field: "label", value: "Begin" });
  await f.send({ type: "edit", op: { kind: "field", block: "cta", field: "label", value: "Begin" } });
  assert.deepEqual(f.applied, [minimalEdit(before, expected)], "one replacement of the changed span only");
  assert.ok(f.applied[0].end - f.applied[0].start <= "Start".length && f.applied[0].text.length <= "Begin".length, "the span is the label, not the document");
  assert.equal(f.text(), expected);
  // The page hears the new document; the bar comes first, so the page reports the state with it.
  const after = f.view.messages.slice(-2).map(message => message.type);
  assert.deepEqual(after, ["bar", "document"]);
  assert.deepEqual(f.of("document").at(-1), { type: "document", version: 2, email: JSON.parse(expected) });

  // Two edits sent together apply one after the other, each to the text the one before it left.
  await Promise.all([
    f.view.send({ type: "edit", op: { kind: "subject", value: "Hello there" } }),
    f.view.send({ type: "edit", op: { kind: "field", block: "headline", field: "text", value: "Hello {{displayName}}" } }),
  ]);
  await f.settle();
  assert.equal(f.applied.length, 3);
  const doc = readEmail(f.text()) as unknown as { subject: string; blocks: { id: string; text?: string }[] };
  assert.equal(doc.subject, "Hello there");
  assert.equal(doc.blocks.find(block => block.id === "headline")?.text, "Hello {{displayName}}");

  // An added block is selected once it exists.
  await f.send({ type: "edit", op: { kind: "add", after: "headline", type: "text" } });
  assert.deepEqual(f.of("select").at(-1), { type: "select", block: "text-1" });
  assert.ok(f.view.messages.findLastIndex(message => message.type === "document") < f.view.messages.findLastIndex(message => message.type === "select"), "the block is selected after the document that has it");
});

test("a CRLF email keeps its line endings and its edit stays minimal", async t => {
  const crlf = welcomeText.replace(/\n/g, "\r\n");
  const f = await open(t, { text: crlf, eol: vscode.EndOfLine.CRLF });
  await f.send({ type: "edit", op: { kind: "subject", value: "Welcome aboard" } });
  assert.equal(f.applied.length, 1);
  assert.deepEqual(f.applied[0], minimalEdit(crlf, f.edited({ kind: "subject", value: "Welcome aboard" }).replace(/\n/g, "\r\n")));
  assert.ok(f.applied[0].text.length < 20, "only the subject changed");
  assert.ok(!/(^|[^\r])\n/.test(f.text()), "every line still ends in CRLF");
  assert.equal(readEmail(f.text()).subject, "Welcome aboard");
});

test("edits are refused while the text is not valid JSON", async t => {
  const f = await open(t, { text: "{ \"subject\": " });
  assert.deepEqual(f.of("document").at(-1), { type: "document", version: 1, email: null, invalid: "This email has invalid JSON. Open Source and correct its formatting." });
  assert.equal(f.lastBar().state, C.unreadable);
  await f.send({ type: "edit", op: { kind: "subject", value: "Hello" } });
  assert.deepEqual(f.of("problem").at(-1), { type: "problem", problem: { message: H.editsNeedValidJson } });
  assert.equal(f.applied.length, 0, "nothing is written");
  assert.equal(f.renders().length, 0, "an email that can't be read is not rendered");

  // An edit the email model refuses is refused with its sentence, and nothing is written either.
  const g = await open(t);
  await g.send({ type: "edit", op: { kind: "field", block: "cta", field: "href", value: "javascript:alert(1)" } });
  assert.match(g.of("problem").at(-1).problem.message, /^Enter a web address/);
  await g.send({ type: "edit", op: { kind: "field", block: "banner", field: "src", value: "https://tracker.example/pixel.gif" } });
  assert.match(g.of("problem").at(-1).problem.message, /^A picture is artifact:<id> from Files/);
  await g.send({ type: "edit", op: { kind: "remove", block: "gone" } });
  assert.equal(g.of("problem").at(-1).problem.message, "That block is no longer in the email.");
  assert.equal(g.applied.length, 0);
  // A page message that is not an edit at all is not applied.
  await g.send({ type: "edit", op: "subject" });
  await g.send({ type: "edit" });
  assert.equal(g.applied.length, 0);
  // VS Code refusing the edit (a read-only document) is a problem, not silence.
  g.refuseApply();
  await g.send({ type: "edit", op: { kind: "subject", value: "Hello" } });
  assert.equal(g.of("problem").at(-1).problem.message, `Couldn’t ${H.operations.edit}. Try again. If this continues, choose Show details.`);
});

// ---------------------------------------------------------------------------
// Pictures
// ---------------------------------------------------------------------------

test("pictures: choose from Files sets an artifact source, upload goes through the upload path, remove leaves the slot", async t => {
  const f = await open(t, { answers: {
    libraryFilesForOwner: () => [
      { id: "f-doc", name: "Notes.pdf", mimeType: "application/pdf" },
      { id: "f-old", name: "Old.png", mimeType: "image/png", archived: true },
      { id: "f-pic", name: "Spring_banner.png", mimeType: "image/png" },
      { id: "f-gif", name: "Wave.gif", mimeType: "image/gif" },
      { id: "f-jpg", name: "Team.jpg", mimeType: "image/jpeg" },
    ],
    libraryArtifactForFile: call => [{ id: /f-pic/.test(call) ? "art-pic" : "art-other" }],
  } });
  const banner = () => (readEmail(f.text()) as unknown as { blocks: Record<string, unknown>[] }).blocks.find(block => block.id === "banner")!;

  // Choose: the pictures in Files, by name; the chosen one becomes the block's source, and its name its description.
  f.pickWith(items => items.find(item => item.label === "Spring_banner.png"));
  await f.send({ type: "picture", block: "banner", action: "choose" });
  assert.equal(f.quickPicks.length, 1);
  assert.deepEqual(f.quickPicks[0].items.map(item => item.label), ["Spring_banner.png", "Wave.gif", "Team.jpg"], "only pictures an email can carry, and none put away");
  assert.equal(f.quickPicks[0].options.title, H.choosePicture);
  assert.equal(f.quickPicks[0].options.placeHolder, H.recentPictures);
  assert.match(f.calls.find(call => call.name === "libraryArtifactForFile")!.call, /fileId: "f-pic"/);
  assert.equal(f.applied.length, 1, "the source and the description are one edit, one undo");
  assert.equal(banner().src, "artifact:art-pic");
  assert.equal(banner().alt, "Spring banner");
  // A description the person wrote stays.
  await f.send({ type: "edit", op: { kind: "field", block: "banner", field: "alt", value: "Our spring range" } });
  f.pickWith(items => items.find(item => item.label === "Wave.gif"));
  await f.send({ type: "picture", block: "banner", action: "choose" });
  assert.equal(banner().src, "artifact:art-other");
  assert.equal(banner().alt, "Our spring range");
  // Choosing nothing changes nothing.
  const applied = f.applied.length;
  f.pickWith(() => undefined);
  await f.send({ type: "picture", block: "banner", action: "choose" });
  assert.equal(f.applied.length, applied);

  // Upload: a picture from this computer, through the upload path, under its own name.
  const photo = vscode.Uri.parse("file:///home/me/photo.png");
  f.pictures.set(photo.toString(), PNG);
  f.chooseFiles([photo]);
  await f.send({ type: "picture", block: "banner", action: "upload" });
  assert.deepEqual(f.dialogs.at(-1), { title: H.uploadPicture, openLabel: H.usePicture, canSelectMany: false, canSelectFiles: true, canSelectFolders: false, filters: { [H.pictures]: ["png", "jpg", "jpeg", "gif"] } });
  assert.equal(f.uploads.length, 1);
  assert.deepEqual({ ...f.uploads[0], bytes: undefined, text: undefined }, { lease, name: "photo.png", mime: "image/png", bytes: undefined, text: undefined });
  assert.equal(banner().src, "artifact:up-1", "the upload's own artifact id");

  // Over 1 MiB is refused before anything is read or sent.
  const large = vscode.Uri.parse("file:///home/me/large.png");
  f.pictures.set(large.toString(), new Uint8Array(1024 * 1024 + 1));
  f.chooseFiles([large]);
  f.log.length = 0;
  await f.send({ type: "picture", block: "banner", action: "upload" });
  assert.equal(f.of("problem").at(-1).problem.message, H.pictureTooLarge);
  assert.equal(f.uploads.length, 1);
  assert.ok(!f.log.some(entry => entry.startsWith("read ")), "the large picture is never read");
  // A file that is not the picture its name says is refused too.
  const fake = vscode.Uri.parse("file:///home/me/fake.png");
  f.pictures.set(fake.toString(), new TextEncoder().encode("not a picture"));
  f.chooseFiles([fake]);
  await f.send({ type: "picture", block: "banner", action: "upload" });
  assert.equal(f.of("problem").at(-1).problem.message, H.pictureType);
  assert.equal(f.uploads.length, 1);
  // Cancelling the dialog uploads nothing.
  f.chooseFiles(undefined);
  await f.send({ type: "picture", block: "banner", action: "upload" });
  assert.equal(f.uploads.length, 1);

  // Remove: the source goes, the block stays as an empty slot.
  await f.send({ type: "picture", block: "banner", action: "remove" });
  assert.equal(banner().src, "");
  assert.ok(readEmail(f.text()) && (readEmail(f.text()) as unknown as { blocks: { id: string }[] }).blocks.some(block => block.id === "banner"));

  // No pictures in Files: a toast says so and no list opens.
  const g = await open(t, { answers: { libraryFilesForOwner: () => [{ id: "f-doc", name: "Notes.pdf", mimeType: "application/pdf" }] } });
  await g.send({ type: "picture", block: "banner", action: "choose" });
  assert.deepEqual(g.toasts, [H.noPictures]);
  assert.equal(g.quickPicks.length, 0);
});

// ---------------------------------------------------------------------------
// Acts
// ---------------------------------------------------------------------------

test("publishing stays in the same editor", async t => {
  const f = await open(t, { answers: {
    campaignSaveTemplate: call => /action: "publish"/.test(call)
      ? [{ templateId: "t1", revision: "2026-10-12T10:00:01Z", status: "ready", saved: true, issues: [] }]
      : [{ templateId: "t1", revision: "2026-10-12T10:00:00Z", status: "draft", saved: true, issues: [] }],
  } });
  assert.deepEqual(f.lastBar(), { type: "bar", state: C.draft, acts: ["saveLayout", "publish"], layoutName: "Welcome" });
  f.change(f.edited({ kind: "subject", value: "Welcome to Acme" }));
  await f.tick(300);
  assert.equal(f.lastBar().state, C.unsaved);
  const bars = f.of("bar").length;
  f.log.length = 0;
  await f.send({ type: "act", act: "publish" });
  assert.deepEqual(f.bars().slice(bars), [
    { state: C.publishing, acts: [] },
    { state: fill(C.published, { organization: "Client" }), acts: ["saveLayout"] },
  ]);
  assert.deepEqual(f.log.filter(entry => !entry.startsWith("post ") && entry !== "emailRender"), ["save", "templateForSource", "campaignSaveTemplate", "campaignSaveTemplate"], "saved first, then saved to the template and published");
  assert.ok(!f.commands.some(command => command.name === "vscode.openWith"), "publishing opens nothing");
  assert.equal(f.of("problem").length, 0);
  // A second click while the first runs is the same publication.
  const before = f.calls.filter(call => call.name === "campaignSaveTemplate").length;
  f.change(f.edited({ kind: "subject", value: "Welcome again" }));
  await f.tick(300);
  await Promise.all([f.view.send({ type: "act", act: "publish" }), f.view.send({ type: "act", act: "publish" })]);
  await f.settle();
  assert.equal(f.calls.filter(call => call.name === "campaignSaveTemplate").length - before, 2, "one save and one publish");
});

test("a refused publish reads in the cluster's words, re-reads the template and never keeps saying Published", async t => {
  let stored: Record<string, unknown> = { status: "ready", createdAt: "r1", document: welcome };
  const f = await open(t, { answers: {
    templateForSource: () => [{ id: "t1", createdAt: String(stored.createdAt) }],
    templateById: () => [stored],
  } });
  assert.equal(f.lastBar().state, fill(C.published, { organization: "Client" }));
  f.change(f.edited({ kind: "subject", value: "Welcome to Acme" }));
  await f.tick(300);
  // The save reaches the cluster, which keeps the edit as a draft, and then refuses.
  f.answer("campaignSaveTemplate", () => {
    stored = { status: "draft", createdAt: "r2", document: readEmail(f.text()) };
    throw new Error(`campaignSaveTemplate: ${ENGINE}Someone else is saving this template. Try again in a moment.`);
  });
  const reads = f.calls.filter(call => call.name === "templateById").length;
  const holdRender = deferred<Rows>();
  f.answer("emailRender", () => holdRender.promise);
  await f.send({ type: "act", act: "publish" });
  assert.equal(f.of("problem").at(-1).problem.message, "Someone else is saving this template. Try again in a moment.", "the cluster's sentence, not “Couldn’t publish”");
  assert.equal(f.lastBar().state, C.issuesOne, "until the email has been looked at again, nothing is offered that it may not be");
  assert.ok(!f.lastBar().acts.includes("publish"));
  holdRender.resolve([{ subject: "Welcome to Acme", html: rendered, text: "Hi", issues: [] }]);
  await f.settle();
  assert.ok(f.calls.filter(call => call.name === "templateById").length > reads, "the template is read again");
  assert.deepEqual(f.bars().at(-1), { state: C.draft, acts: ["saveLayout", "publish"] }, "the cluster's draft, not “Published to Client”");
  assert.ok(!f.bars().slice(f.bars().findIndex(bar => bar.state === C.publishing)).some(bar => bar.state === fill(C.published, { organization: "Client" })));

  // Issues found by the save stop the publication and stay counted until the email changes, even when a render does not show them.
  f.answer("emailRender", () => [{ subject: "Welcome to Acme", html: rendered, text: "Hi", issues: [] }]);
  f.answer("campaignSaveTemplate", () => [{ templateId: "t1", revision: "r3", status: "draft", saved: true, issues: [{ code: "image_missing", block: "banner", message: "Choose a picture for the banner." }] }]);
  await f.send({ type: "act", act: "publish" });
  assert.equal(f.of("problem").at(-1).problem.message, H.fixIssues);
  await f.settle();
  assert.equal(f.lastBar().state, C.issuesOne);
  assert.deepEqual(f.of("render").at(-1).issues, [{ code: "image_missing", block: "banner", message: "Choose a picture for the banner." }], "the save's issues are listed with the render's");
  f.change(f.edited({ kind: "subject", value: "Welcome, again" }));
  await f.tick(300);
  assert.equal(f.lastBar().state, C.unsaved, "a change is a new email to check");
  assert.deepEqual(f.of("render").at(-1).issues, []);
});

test("a template opened as a file publishes at its revision, and its next save expects the revision publishing left", async t => {
  const opened = templateDocument("client-org");
  opened.template!.status = "draft";
  const f = await open(t, { uri: "memql-file://client.example/templates/t9/Welcome.email.json", opened, answers: {
    campaignSaveTemplate: () => [{ templateId: "t9", revision: "2026-10-12T09:30:00Z", status: "ready", saved: true, issues: [] }],
  } });
  assert.equal(f.lastBar().state, C.draft);
  await f.send({ type: "act", act: "publish" });
  const call = f.calls.find(call => call.name === "campaignSaveTemplate")!.call;
  assert.match(call, /templateId: "t9"/);
  assert.match(call, /expectedRevision: "2026-10-12T09:00:00Z"/);
  assert.ok(!f.calls.some(call => call.name === "templateForSource"), "a template is its own");
  assert.equal(opened.revision, "2026-10-12T09:30:00Z", "the opened template carries the revision its next save expects");
  assert.equal(opened.template!.status, "ready");
  assert.equal(f.lastBar().state, fill(C.published, { organization: "Client" }));
});

test("an edit made while publishing stops the publication before it goes live", async t => {
  const f = await open(t, { answers: {
    campaignSaveTemplate: call => {
      // The person types while the draft is being saved.
      if (/action: "save"/.test(call)) f.change(f.edited({ kind: "subject", value: "Typed meanwhile" }));
      return [{ templateId: "t1", revision: "r1", status: /action: "publish"/.test(call) ? "ready" : "draft", saved: true, issues: [] }];
    },
  } });
  await f.send({ type: "act", act: "publish" });
  assert.equal(f.calls.filter(call => /action: "publish"/.test(call.call)).length, 0, "nothing goes live that the person has not seen");
  assert.equal(f.problems().at(-1), H.changedWhilePublishing);
});

test("only the check after a failed publish ends its count: a render already under way does not", async t => {
  let stored: Record<string, unknown> = { status: "ready", createdAt: "r1", document: welcome };
  const f = await open(t, { answers: {
    templateForSource: () => [{ id: "t1", createdAt: String(stored.createdAt) }],
    templateById: () => [stored],
  } });
  assert.equal(f.lastBar().state, fill(C.published, { organization: "Client" }));
  // Typed and typed back: unsaved, and the same as what is published.
  f.change(f.edited({ kind: "subject", value: "Welcome back" }));
  f.change(f.edited({ kind: "subject", value: "Welcome" }));
  const underWay = deferred<Rows>();
  f.answer("emailRender", () => underWay.promise);
  await f.tick(300);
  f.answer("campaignSaveTemplate", () => {
    stored = { status: "draft", createdAt: "r2", document: readEmail(f.text()) };
    throw new Error(`campaignSaveTemplate: ${ENGINE}Someone else is saving this template. Try again in a moment.`);
  });
  const reading = deferred<Rows>();
  f.answer("templateById", () => reading.promise);
  await f.send({ type: "act", act: "publish" });
  assert.equal(f.lastBar().state, C.issuesOne);
  // The render asked for before the publish answers while the template is still being read again.
  underWay.resolve([{ subject: "Welcome", html: rendered, text: "Hi", issues: [] }]);
  await f.settle();
  assert.equal(f.lastBar().state, C.issuesOne, "what is published is not known yet: the bar still counts the failure");
  assert.ok(!f.bars().slice(f.bars().findIndex(bar => bar.state === C.publishing)).some(bar => bar.state === fill(C.published, { organization: "Client" })));
  f.answer("emailRender", () => [{ subject: "Welcome", html: rendered, text: "Hi", issues: [] }]);
  reading.resolve([stored]);
  await f.settle();
  assert.deepEqual(f.bars().at(-1), { state: C.draft, acts: ["saveLayout", "publish"] });
});

test("a failure to find the organization is told, and the next change tries again", async t => {
  let failing = true;
  const f = await open(t, { answers: { clientAccountsAll: () => { if (failing) throw new Error(`clientAccountsAll: ${ENGINE}Saving is busy right now. Try again in a moment.`); return organizations; } } });
  assert.equal(f.problems().at(-1), "Saving is busy right now. Try again in a moment.");
  assert.equal(f.of("bar").length, 0, "without the organization the bar would only guess");
  assert.equal(f.renders().length, 0);
  failing = false;
  f.change(f.edited({ kind: "subject", value: "Welcome back" }));
  await f.settle();
  assert.deepEqual(f.of("context").at(-1).organization, { id: "client-org", name: "Client" });
  assert.equal(f.lastBar().state, C.unsaved);
  assert.equal(f.renders().length, 1);
});

test("a failure to find what is published is told, and the bar never says Published", async t => {
  const f = await open(t, { answers: { templateForSource: () => { throw new Error(`templateForSource: ${ENGINE}Previews are busy right now. Try again in a moment.`); } } });
  assert.equal(f.problems().at(-1), "Previews are busy right now. Try again in a moment.");
  assert.deepEqual(f.lastBar(), { type: "bar", state: C.draft, acts: ["saveLayout", "publish"], layoutName: "Welcome" });
  assert.equal(f.renders().length, 1, "the email is still shown");
});

test("Save to MemQL uploads a local email, labels it and reopens it in place", async t => {
  const f = await open(t, { uri: LOCAL, answers: { clientAccountsAll: () => [organizations[0]!], setArtifactAccounts: () => [{ id: "new-1" }] } });
  assert.deepEqual(f.lastBar(), { type: "bar", state: C.local, acts: ["saveToMemQL"] });
  assert.equal(f.renders().length, 1, "a local email renders like any other once its organization is known");
  f.change(f.edited({ kind: "subject", value: "Welcome to Acme" }));
  const bars = f.of("bar").length;
  f.log.length = 0;
  await f.send({ type: "act", act: "saveToMemQL" });
  assert.deepEqual(f.bars().slice(bars, bars + 1), [{ state: C.savingCopy, acts: [] }]);
  assert.deepEqual(f.log.filter(entry => !entry.startsWith("post ") && entry !== "emailRender"), ["save", "upload", "setArtifactAccounts", "open", "close"]);
  assert.deepEqual({ ...f.uploads[0], bytes: undefined }, { lease, name: "Welcome.email.json", mime: "application/json", text: f.text(), bytes: undefined }, "the saved text, as it is");
  assert.match(f.calls.find(call => call.name === "setArtifactAccounts")!.call, /artifactId: "new-1", accountIds: \["client-org"\]/);
  const opened = f.commands.find(command => command.name === "vscode.openWith")!;
  assert.equal(String(opened.args[0]), "memql-file://client.example/artifacts/new-1/Welcome.email.json");
  assert.deepEqual(opened.args.slice(1), [VIEW, { viewColumn: 2, preview: false }], "in the same column, as a kept tab");
  assert.deepEqual(f.closed, [[f.localTab]], "the local email's tab closes; its Source tab and other emails stay");
});

test("the editor connects only when asked", async t => {
  const f = await open(t, { connected: false });
  assert.deepEqual(f.connects, []);
  assert.deepEqual(f.calls, []);
  assert.equal(f.loads(), 0, "reading the file would connect");
  assert.deepEqual(f.of("context").at(-1), { type: "context", connected: false, choices: [] });
  assert.deepEqual(f.lastBar(), { type: "bar", state: fill(C.notConnectedTo, { domain: "client.example" }), acts: ["connect"] });
  await f.send({ type: "act", act: "connect" });
  assert.deepEqual(f.connects, ["client.example"]);
  assert.deepEqual(f.of("context").at(-1), { type: "context", connected: true, organization: { id: "client-org", name: "Client" }, choices: [] });
  assert.equal(f.lastBar().state, C.draft);
  assert.equal(f.renders().length, 1);
  // Losing the connection says so, and the next connection renders again.
  f.setConnected(false);
  await f.settle();
  assert.equal(f.of("context").at(-1).connected, false);
  assert.equal(f.lastBar().state, fill(C.notConnectedTo, { domain: "client.example" }));
  f.setConnected(true);
  await f.settle();
  assert.equal(f.renders().length, 2);
  assert.equal(f.lastBar().state, C.draft);
  // A connection to another cluster is not this file's.
  f.setConnected("other.example");
  await f.settle();
  assert.equal(f.of("context").at(-1).connected, false);
  assert.equal(f.lastBar().state, fill(C.notConnectedTo, { domain: "client.example" }));

  // A local email connects by choosing a cluster.
  const g = await open(t, { uri: LOCAL, connected: false });
  assert.deepEqual(g.lastBar(), { type: "bar", state: C.notConnected, acts: ["connect"] });
  await g.send({ type: "act", act: "connect" });
  assert.deepEqual(g.commands.map(command => command.name), ["memql.clusters.select"]);
  assert.deepEqual(g.connects, []);
  assert.equal(g.lastBar().state, C.notConnected, "a choice that was not made is answered with the bar as it is");
});

test("every act is answered by the bar or a problem", async t => {
  const f = await open(t);
  for (const act of ["saveToMemQL", "connect", "zap", 7]) {
    const before = f.view.messages.length;
    await f.send({ type: "act", act });
    assert.ok(f.view.messages.slice(before).some(message => message.type === "bar" || message.type === "problem"), `act ${String(act)} is answered`);
  }
  const g = await open(t, { uri: LOCAL });
  for (const act of ["publish", "saveLayout"]) {
    const before = g.view.messages.length;
    await g.send({ type: "act", act });
    assert.ok(g.view.messages.slice(before).some(message => message.type === "bar" || message.type === "problem"), `act ${act} is answered`);
  }
  // Open Source is answered by the page going: the text takes its place.
  await g.send({ type: "act", act: "openSource" });
  assert.equal(g.panel.disposed, true);
});

test("an organization the person does not list is named in words, never by its id", async t => {
  const f = await open(t, { uri: "memql-file://client.example/templates/t9/Welcome.email.json", opened: templateDocument("secret-org") });
  assert.deepEqual(f.of("context").at(-1).organization, { id: "secret-org", name: H.unlistedOrganization });
  assert.equal(f.lastBar().state, fill(C.published, { organization: H.unlistedOrganization }));
  assert.ok(!f.bars().some(bar => bar.state.includes("secret-org")));
  assert.ok(!f.of("problem").some(message => message.problem.message.includes("secret-org")));
  // A template's own organization, when it is listed, is named as it is.
  const g = await open(t, { uri: "memql-file://client.example/templates/t9/Welcome.email.json", opened: templateDocument("other-org") });
  assert.equal(g.lastBar().state, fill(C.published, { organization: "Other" }));
});

test("Save as layout prefills the layout saved from this file and keeps its pending id under the email and organization", async t => {
  const layouts: Rows = [
    { id: "builtin:article", kind: "builtin", name: "Article" },
    { id: "org:l1", layoutId: "l1", kind: "organization", name: "Monthly letter", sourceArtifactId: "art-1", revision: "r1", description: "", documentOmitted: true },
  ];
  const f = await open(t, { answers: { emailLayouts: () => layouts } });
  assert.equal(f.lastBar().layoutName, "Monthly letter");
  assert.ok(!f.calls.some(call => call.name === "emailLayoutById"), "the name is all the bar needs");

  const pendingKey = `memql.emailLayoutPending:${JSON.stringify([FILE, "client-org"])}`;
  let pendingDuringSave: unknown;
  const g = await open(t, { answers: { campaignSaveLayout: () => { pendingDuringSave = g.state.get(pendingKey); return [{ layoutId: String(pendingDuringSave), revision: "r1", saved: true }]; } } });
  assert.equal(g.lastBar().layoutName, "Welcome", "no layout yet: the file's name");
  const bars = g.of("bar").length;
  await g.send({ type: "saveLayout", name: "Spring" });
  assert.match(String(pendingDuringSave), /^[0-9a-f-]{36}$/, "the new layout's id is kept, under the email and its organization, before the request");
  assert.match(g.calls.find(call => call.name === "campaignSaveLayout")!.call, new RegExp(`layoutId: "${String(pendingDuringSave)}"`));
  assert.equal(g.state.get(pendingKey), undefined, "and let go once the cluster has it");
  assert.deepEqual(g.bars().slice(bars), [{ state: C.savingLayout, acts: [] }, { state: C.layoutSaved, acts: ["saveLayout", "publish"] }]);
  assert.equal(g.lastBar().layoutName, "Spring");
  await g.tick(4000);
  assert.equal(g.lastBar().state, C.draft);
  // A name the cluster would refuse is refused here, with the sentence.
  await g.send({ type: "saveLayout", name: "   " });
  assert.equal(g.of("problem").at(-1).problem.message, H.layoutNameInvalid);
});

test("one act at a time: nothing is published while a layout is being saved", async t => {
  const saving = deferred<Rows>();
  const f = await open(t, { answers: {
    campaignSaveLayout: () => saving.promise,
    campaignSaveTemplate: () => [{ templateId: "t1", revision: "r1", status: "ready", saved: true, issues: [] }],
  } });
  const layout = f.view.send({ type: "saveLayout", name: "Spring" });
  await f.settle();
  assert.equal(f.lastBar().state, C.savingLayout);
  await f.send({ type: "act", act: "publish" });
  assert.ok(!f.calls.some(call => call.name === "campaignSaveTemplate"), "no publication starts while the layout is being saved");
  assert.equal(f.lastBar().state, C.savingLayout, "and the bar still says what is running");
  saving.resolve([{ layoutId: "l1", revision: "r1", saved: true }]);
  await layout;
  await f.settle();
  assert.equal(f.lastBar().state, C.layoutSaved);
});

test("a page reloaded while the organization is being found again gets its bar once it is found", async t => {
  const f = await open(t);
  assert.equal(f.lastBar().state, C.draft);
  const finding = deferred<Rows>();
  f.answer("clientAccountsAll", () => finding.promise);
  // The connection is made again (a new sign-in): what was known belongs to the old one.
  f.setConnected("client.example");
  await f.settle();
  const before = f.view.messages.length;
  await f.send({ type: "ready" });
  assert.ok(!f.view.messages.slice(before).some(message => message.type === "bar"), "no bar while it is not known what it says");
  finding.resolve(organizations);
  await f.settle();
  assert.deepEqual(f.view.messages.slice(before).filter(message => message.type === "bar").map(message => message.state), [C.draft],
    "the reloaded page gets its bar, though it says what the page said before it reloaded");
});

// ---------------------------------------------------------------------------
// The page state and the organization
// ---------------------------------------------------------------------------

test("the page state, including Preview with values, is kept per email and restored on ready", async t => {
  const f = await open(t);
  assert.equal(f.of("restore").length, 0, "nothing kept, nothing to restore");
  await f.send({ type: "viewState", state: { width: "mobile", selected: "cta", themeOpen: true, samplesOpen: true, samples: { "fields.plan": "Gold", displayName: "Ada", bad: 5, "not a tag": "x", "fields.": "y" }, extra: "x" } });
  assert.deepEqual(f.state.get(stateKey(FILE)), { width: "mobile", selected: "cta", themeOpen: true, samplesOpen: true, samples: { "fields.plan": "Gold", displayName: "Ada" } });
  // What is not a page state is not kept.
  await f.send({ type: "viewState", state: "mobile" });
  await f.send({ type: "viewState", state: { width: "huge", selected: 4 } });
  assert.deepEqual(f.state.get(stateKey(FILE)), { width: "mobile", selected: "cta", themeOpen: true, samplesOpen: true, samples: { "fields.plan": "Gold", displayName: "Ada" } });
  // Another panel on the same email (the page reopened) is given it back on ready.
  f.panel.dispose();
  const again = await f.attach();
  assert.deepEqual(again.messages.find(message => message.type === "restore"), { type: "restore", state: { width: "mobile", selected: "cta", themeOpen: true, samplesOpen: true, samples: { "fields.plan": "Gold", displayName: "Ada" } } });
  // Each email keeps its own.
  const g = await open(t, { uri: "memql-file://client.example/artifacts/art-2/Other.email.json", opened: fileDocument({ resource: { domain: "client.example", kind: "artifacts", id: "art-2", name: "Other.email.json" } }) });
  assert.equal(g.of("restore").length, 0);
});

test("an organization chosen on the page is kept: as the file's label when it has none, else with the page state", async t => {
  // Two labels and two organizations: the person chooses, and the choice is kept with the page.
  const f = await open(t, { opened: fileDocument({ accountIds: ["client-org", "other-org"] }) });
  assert.deepEqual(f.of("context").at(-1), { type: "context", connected: true, choices: [{ id: "client-org", name: "Client" }, { id: "other-org", name: "Other" }] });
  assert.deepEqual(f.lastBar(), { type: "bar", state: C.chooseOrganization, acts: [] });
  assert.equal(f.renders().length, 0, "no render without the organization");
  await f.send({ type: "organization", id: "other-org" });
  assert.equal((f.state.get(stateKey(FILE)) as { organization?: string }).organization, "other-org");
  assert.ok(!f.calls.some(call => call.name === "setArtifactAccounts"), "a file with labels keeps them");
  assert.deepEqual(f.of("context").at(-1).organization, { id: "other-org", name: "Other" });
  assert.match(f.renders().at(-1)!.call, /accountId: "other-org"/);
  // The page state written later keeps the choice.
  await f.send({ type: "viewState", state: { width: "plain" } });
  assert.deepEqual(f.state.get(stateKey(FILE)), { width: "plain", organization: "other-org" });
  // An organization that is not among the choices is not taken.
  await f.send({ type: "organization", id: "secret-org" });
  assert.equal((f.state.get(stateKey(FILE)) as { organization?: string }).organization, "other-org");

  // An unlabelled file is labelled with the choice, so it knows its organization from then on.
  const opened = fileDocument({ accountIds: [] });
  const g = await open(t, { opened, answers: { setArtifactAccounts: () => [{ id: "art-1" }] } });
  await g.send({ type: "organization", id: "client-org" });
  assert.match(g.calls.find(call => call.name === "setArtifactAccounts")!.call, /artifactId: "art-1", accountIds: \["client-org"\]/);
  assert.deepEqual(opened.accountIds, ["client-org"]);
  assert.deepEqual(g.of("context").at(-1), { type: "context", connected: true, organization: { id: "client-org", name: "Client" }, choices: [] });
  assert.equal((g.state.get(stateKey(FILE)) as { organization?: string } | undefined)?.organization, undefined);
});
