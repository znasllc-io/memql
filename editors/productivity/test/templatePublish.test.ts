import { test } from "node:test";
import assert from "node:assert/strict";
import * as vscode from "vscode";
import { TemplatePublisher } from "../src/templatePublish.js";
import { newTemplate } from "../src/templates.js";
import type { OpenDocument } from "../src/documents.js";
import type { EditorConnectionAPI } from "../../vscode/src/connection/api.js";

function fixture() {
  const state = new Map<string, unknown>();
  const calls: {name: string; query: string}[] = [];
  const lease = { domain: "client.example", name: "Client", generation: 1 };
  const document = { uri: vscode.Uri.parse("file:///reference.email.json"), version: 1, isDirty: false,
    getText: () => newTemplate(), save: async () => true };
  const api = { current: () => lease, execute: async (_lease: unknown, name: string, query: string) => {
    calls.push({name,query});
    return name === "clientAccountsAll" ? [{id:"client-org",name:"Client"}] : [{saved:true,revision:"2026-10-01T12:00:00Z"}];
  } };
  const context = { workspaceState: { get: (key: string) => state.get(key), update: async (key: string, value: unknown) => { state.set(key,value); } } };
  let picks = 0;
  vscode.window.showQuickPick = (async (items: unknown[]) => {
    picks++;
    return typeof items[0] === "string" ? "Publish template for campaigns" : items[0];
  }) as typeof vscode.window.showQuickPick;
  vscode.window.showInputBox = async () => "Welcome";
  const publisher = new TemplatePublisher(api as unknown as EditorConnectionAPI, context as unknown as vscode.ExtensionContext,
    async () => { throw new Error("unexpected file read"); });
  return {publisher,document:document as unknown as vscode.TextDocument,mutable:document,api,state,calls,lease,context,picks:()=>picks};
}

test("publication captures the chosen client and saves before approving the exact revision", async () => {
  const f = fixture();
  await Promise.all([f.publisher.publish(f.document),f.publisher.publish(f.document)]);
  assert.equal(f.picks(),2,"a double click must reuse the same review");
  assert.deepEqual(f.calls.map(c=>c.name),["clientAccountsAll","campaignSaveTemplate","campaignSaveTemplate"]);
  assert.match(f.calls[1].query,/accountId:\s*"client-org"/);
  assert.match(f.calls[1].query,/action:\s*"save"/);
  assert.match(f.calls[2].query,/expectedRevision:\s*"2026-10-01T12:00:00Z"/);
  assert.match(f.calls[2].query,/action:\s*"publish"/);
});

test("a lost creation response retries the stored template id without creating a second draft", async () => {
  const f = fixture();
  const execute = f.api.execute;
  let loseResponse = true;
  f.api.execute = async (...args) => {
    const result = await execute(...args);
    if (args[1] === "campaignSaveTemplate" && loseResponse) { loseResponse=false; throw new Error("connection lost"); }
    return result;
  };
  await assert.rejects(f.publisher.publish(f.document),/connection lost/);
  const first = f.calls.find(c=>c.name==="campaignSaveTemplate")!.query;
  await f.publisher.publish(f.document);
  const saves = f.calls.filter(c=>c.name==="campaignSaveTemplate");
  assert.equal(saves[1].query,first);
  assert.equal(f.calls.filter(c=>c.name==="clientAccountsAll").length,1);
});

test("an edit during the approval choice cancels publication", async () => {
  const f = fixture();
  vscode.window.showQuickPick = (async (items: unknown[]) => {
    if (typeof items[0] === "string") { f.mutable.version++; return "Publish template for campaigns"; }
    return items[0];
  }) as typeof vscode.window.showQuickPick;
  await assert.rejects(f.publisher.publish(f.document),/changed during review/);
  assert.equal(f.calls.filter(c=>c.name==="campaignSaveTemplate").length,0);
});

test("an existing client template publishes using its opened revision without another organization picker", async () => {
  const f = fixture();
  f.mutable.uri = vscode.Uri.parse("memql-file://client.example/templates/template/Welcome.email.json");
  const source = { sourceId:"template",lease:f.lease,revision:"2026-09-01T12:00:00Z",
    template:{name:"Welcome",accountId:"original-client",status:"draft"} } as OpenDocument;
  const publisher = new TemplatePublisher(f.api as unknown as EditorConnectionAPI,f.context as unknown as vscode.ExtensionContext,async()=>source);
  await publisher.publish(f.document);
  assert.equal(f.calls.length,1);
  assert.match(f.calls[0].query,/accountId:\s*"original-client"/);
  assert.match(f.calls[0].query,/expectedRevision:\s*"2026-09-01T12:00:00Z"/);
  assert.equal(source.template?.status,"ready");
});
