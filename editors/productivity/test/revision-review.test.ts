import { test } from "node:test";
import assert from "node:assert/strict";
import * as vscode from "vscode";
import { assertRevisionBase, RevisionReview } from "../src/revisionReview.js";
import type { Documents, OpenDocument } from "../src/documents.js";

test("an interrupted item revision resumes its own immutable request", async () => {
  const noop=()=>({dispose(){}});
  Object.assign(vscode.workspace,{registerTextDocumentContentProvider:noop,onDidCloseTextDocument:noop});
  const uri=vscode.Uri.parse('memql-file://cluster/artifacts/doc/Guide.md');
  const amendment={requestId:'parent',approvalId:'approval',itemId:'item',instruction:'Use the research'};
  const submitted:unknown[][]=[];
  const files={revision:async()=>({proposal:{amendment}}),modifyRevision:async(...args:unknown[])=>{submitted.push(args);},requestRevision:async()=>{assert.fail('must resume modification, not generate the full proposal');}} as unknown as Documents;
  const context={subscriptions:[],workspaceState:{get:()=>({requestId:'retry-same-id'})}} as unknown as vscode.ExtensionContext;
  const base={} as OpenDocument;
  await new RevisionReview(context,files,async()=>base).resumePreparation({uri} as vscode.TextDocument);
  assert.deepEqual(submitted,[[base,'parent','approval','item','Use the research','retry-same-id']]);
});

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

test("an open editor discovers a newer review after its initial recovery", async () => {
  const noop = () => ({ dispose() {} });
  Object.assign(vscode.workspace, { registerTextDocumentContentProvider: noop, onDidCloseTextDocument: noop });
  const uri = vscode.Uri.parse("memql-file://cluster/artifacts/doc/Guide.md");
  const document = { uri } as vscode.TextDocument;
  const key = `memql.documentRevision:${uri}`;
  const saved = new Map<string, unknown>([[key, { requestId: "stopped", fingerprint: "old" }]]);
  const context = { subscriptions: [], workspaceState: { get: (key: string) => saved.get(key), update: async (key: string, value: unknown) => { saved.set(key, value); } } } as unknown as vscode.ExtensionContext;
  const proposal = { artifactId: "doc", revision: "file:1", version: 1, commentIds: ["extension"], instruction: "" };
  let latest = "stopped";
  const files = {
    review: async () => ({ requestId: latest }),
    revision: async (_base: unknown, requestId: string) => requestId === "stopped"
      ? { status: "waiting", cancelRequested: true, decision: "approved", proposal }
      : { status: "succeeded", result: { applied: true }, proposal },
  } as unknown as Documents;
  const review = new RevisionReview(context, files, async () => ({} as OpenDocument));
  assert.equal((await review.status(document))?.cancelRequested, true);
  latest = "replacement";
  assert.deepEqual((await review.status(document))?.result, { applied: true });
  assert.equal((saved.get(key) as {requestId:string}).requestId, "replacement");
});

test("history compares retained versions, not unsaved text or an unaccepted proposal",async()=>{
 let provider:any;const noop=()=>({dispose(){}});
 Object.assign(vscode.workspace,{registerTextDocumentContentProvider:(_scheme:string,value:any)=>{provider=value;return noop();},onDidCloseTextDocument:noop});
 const calls:any[]=[];Object.assign(vscode.commands,{executeCommand:async(...args:any[])=>{calls.push(args);}});
 const reads:number[]=[];const files={version:async(_base:unknown,version:number)=>{reads.push(version);return {content:version===2?"Previous saved":"Applied subset"};}} as unknown as Documents;
 const review=new RevisionReview({subscriptions:[]} as unknown as vscode.ExtensionContext,files,async()=>({} as OpenDocument));
 await review.compareVersion({getText:()=>"Unsaved content"} as vscode.TextDocument,3);
 assert.deepEqual(reads,[2,3]);assert.equal(calls[0][0],"vscode.diff");
 assert.equal(provider.provideTextDocumentContent(calls[0][1]),"Previous saved");assert.equal(provider.provideTextDocumentContent(calls[0][2]),"Applied subset");
 await assert.rejects(review.compareVersion({} as vscode.TextDocument,0),/previous snapshot/);
});


test("a deliberate proposal after abandonment gets a new request identity",async()=>{
 const noop=()=>({dispose(){}});
 Object.assign(vscode.workspace,{registerTextDocumentContentProvider:noop,onDidCloseTextDocument:noop});
 const uri=vscode.Uri.parse("memql-file://cluster/artifacts/doc/Guide.md");
 const document={uri,isDirty:false,getText:()=>"Original"} as vscode.TextDocument;
 const base={resource:{id:"doc"},revision:"file:1",version:1,content:new TextEncoder().encode("Original")} as OpenDocument;
 const key=`memql.documentRevision:${uri}`;
 const saved=new Map<string,unknown>([[key,{requestId:"abandoned",fingerprint:JSON.stringify(["file:1",1,["note"],""])}]]);
 const submitted:string[]=[];
 const context={subscriptions:[],workspaceState:{get:(k:string)=>saved.get(k),update:async(k:string,v:unknown)=>{saved.set(k,v);}}} as unknown as vscode.ExtensionContext;
 const files={review:async()=>({requestId:"abandoned"}),revision:async(_base:unknown,id:string)=>({status:id==="abandoned"?"abandoned":"running"}),requestRevision:async(_base:unknown,_ids:unknown,_text:unknown,id:string)=>{submitted.push(id);}} as unknown as Documents;
 await new RevisionReview(context,files,async()=>base).prepare(document,["note"],"");
 assert.equal(submitted.length,1);assert.notEqual(submitted[0],"abandoned");
 assert.equal((saved.get(key) as {requestId:string}).requestId,submitted[0]);
});
