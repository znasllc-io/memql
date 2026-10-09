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

test("a stopped review keeps checking for a replacement and refreshes its applied document", async t => {
  const noop = () => ({ dispose() {} });
  Object.assign(vscode.Uri, { joinPath: (uri: vscode.Uri, path: string) => vscode.Uri.parse(`${uri}/${path}`) });
  Object.assign(vscode.workspace, { registerTextDocumentContentProvider: noop, onDidCloseTextDocument: noop, onDidChangeTextDocument: noop, onDidSaveTextDocument: noop });
  Object.assign(vscode.window, { onDidChangeVisibleTextEditors: noop, visibleTextEditors: [] });
  const timers: { callback: () => void; delay: number }[] = [];
  t.mock.method(globalThis, "setTimeout", (callback: () => void, delay: number) => {
    timers.push({ callback, delay }); return 0;
  });
  const uri = vscode.Uri.parse("memql-file://cluster/artifacts/doc/Guide.md");
  const source = "Existing text.";
  const document = { uri, fileName: "Guide.md", version: 1, isDirty: false, getText: () => source } as vscode.TextDocument;
  const base = { resource: { id: "doc" }, content: new TextEncoder().encode(source), version: 1, revision: "file:1" } as OpenDocument;
  const saved = new Map<string, unknown>([[`memql.documentRevision:${uri}`, { fingerprint: "old", requestId: "old" }]]);
  const context = { extensionUri: vscode.Uri.parse("file:///extension"), subscriptions: [], workspaceState: { get: (key: string) => saved.get(key), update: async (key: string, value: unknown) => { saved.set(key, value); } } } as unknown as vscode.ExtensionContext;
  const proposal = { artifactId: "doc", revision: "file:1", version: 1, commentIds: ["extension"], instruction: "", content: source };
  let latest = "old", refreshes = 0;
  const files = {
    notes: async () => ({ notes: [] }),
    review: async () => ({ requestId: latest, comments: [] }),
    revision: async (_base: unknown, requestId: string) => requestId === "old"
      ? { status: "cancelled", decision: "approved", cancelRequested: true, runId: "old-run", proposal }
      : { status: "succeeded", runId: "new-run", result: { applied: true }, proposal },
  } as unknown as Documents;
  const messages: any[] = [];
  let receive!: (message: unknown) => Promise<void>, dispose!: () => void;
  const panel = { visible: true, active: true, onDidChangeViewState: noop, onDidDispose: (fn: () => void) => { dispose = fn; }, webview: {
    asWebviewUri: (value: vscode.Uri) => value,
    postMessage: async (message: unknown) => { messages.push(message); return true; },
    onDidReceiveMessage: (fn: typeof receive) => { receive = fn; return noop(); },
  } } as unknown as vscode.WebviewPanel;
  await new MarkdownEditor(context, files, async () => base, async () => { refreshes++; return true; }).resolveCustomTextEditor(document, panel);
  try {
    await receive({ type: "ready" });
    assert.equal(timers.at(-1)?.delay, 10000, "a terminal review must keep checking while visible");
    latest = "new";
    timers.at(-1)!.callback();
    await new Promise<void>(resolve => setImmediate(resolve));
    assert.equal(messages.filter(message => message.type === "revision").at(-1)?.status.runId, "new-run");
    assert.equal(refreshes, 1, "the newly applied document is loaded into the editor");
    timers.at(-1)!.callback();
    await new Promise<void>(resolve => setImmediate(resolve));
    assert.equal(refreshes, 1, "the same applied receipt must not reload the document repeatedly");
  } finally { dispose(); }
});

test("annotation deletion cancels only the confirmed review and never removes after cancellation failure",async()=>{
  const noop=()=>({dispose(){}});
  Object.assign(vscode.Uri,{joinPath:(uri:vscode.Uri,path:string)=>vscode.Uri.parse(`${uri}/${path}`)});
  Object.assign(vscode.workspace,{registerTextDocumentContentProvider:noop,onDidCloseTextDocument:noop,onDidChangeTextDocument:noop,onDidSaveTextDocument:noop});
  Object.assign(vscode.window,{onDidChangeVisibleTextEditors:noop,visibleTextEditors:[]});
  for(const scenario of ["feedback","note","cancel-failed","approved","stale","other-author"]){
    const uri=vscode.Uri.parse("memql-file://cluster/artifacts/doc/Guide.md"),source="Existing text.";
    const document={uri,fileName:"Guide.md",version:1,isDirty:false,getText:()=>source} as vscode.TextDocument;
    const base={resource:{id:"doc"},content:new TextEncoder().encode(source),version:1,revision:"file:1"} as OpenDocument;
    const saved=new Map<string,unknown>([[`memql.documentRevision:${uri}`,{fingerprint:"saved",requestId:"request"}]]);
    const context={extensionUri:vscode.Uri.parse("file:///extension"),subscriptions:[],workspaceState:{get:(key:string)=>saved.get(key),update:async(key:string,value:unknown)=>{saved.set(key,value);}}} as unknown as vscode.ExtensionContext;
    const events:string[]=[],messages:any[]=[];
    const row={id:"comment",canRemove:scenario!=="other-author",anchor:{kind:"document-end"},body:"Add details",revision:"file:1"};
    const proposal={artifactId:"doc",requestId:"request",revision:"file:1",version:1,commentIds:["comment"],instruction:"",content:source};
    const files={review:async()=>({requestId:"request",comments:[row]}),notes:async()=>({notes:[row]}),revision:async()=>({status:"waiting",runId:"run",approvalId:"approval",decision:scenario==="approved"?"approved":undefined,proposal}),
      cancelRevision:async()=>{events.push("cancel");if(scenario==="cancel-failed")throw new Error("offline");},
      removeAnnotation:async()=>{events.push("remove");},
    } as unknown as Documents;
    let receive!:(message:unknown)=>Promise<void>,dispose!:()=>void;
    const panel={visible:false,active:true,onDidChangeViewState:noop,onDidDispose:(fn:()=>void)=>{dispose=fn;},webview:{asWebviewUri:(value:vscode.Uri)=>value,postMessage:async(message:unknown)=>{messages.push(message);return true;},onDidReceiveMessage:(fn:typeof receive)=>{receive=fn;return noop();}}} as unknown as vscode.WebviewPanel;
    await new MarkdownEditor(context,files,async()=>base).resolveCustomTextEditor(document,panel);
    try {
      await receive({type:"removeAnnotation",id:"comment",purpose:scenario==="note"?"note":"feedback",runId:scenario==="stale"?"old-run":"run"});
      assert.deepEqual(events,scenario==="feedback"?["cancel","remove"]:scenario==="note"?["remove"]:scenario==="cancel-failed"?["cancel"]:[],scenario);
      assert.equal(messages.some(m=>m.type==="annotationRemoved"),["feedback","note"].includes(scenario),scenario);
      assert.equal(messages.some(m=>m.type==="annotationRemoveError"),!["feedback","note"].includes(scenario),scenario);
    } finally{dispose();}
  }
});
