import { test } from "node:test";
import assert from "node:assert/strict";
import * as vscode from "vscode";
import { TemplateExamples } from "../src/templateExamples.js";
import type { EditorConnectionAPI } from "../../vscode/src/connection/api.js";

test("example-led creation distinguishes inspiration from included images and keeps it in the recipe", async () => {
  const calls: {name: string; query: string}[] = [];
  const lease = { domain: "client.example", name: "Client", generation: 1 };
  const api = { current: () => lease, execute: async (_lease: unknown, name: string, query: string) => {
    calls.push({name, query});
    switch (name) {
      case "clientAccountsAll": return [{id:"client-org",name:"Client",status:"active"}];
      case "libraryFilesForOwner": return [{id:"layout",name:"example.png"},{id:"logo",name:"logo.png"}];
      case "composeMaterialize": return [{compositionId:"composition"}];
      case "compositionById": return [{status:"ready",outputFileId:"file",name:"Welcome"}];
      case "libraryArtifactBySourceConceptRef": return [{id:"artifact",title:"Welcome.email.json"}];
      default: return [];
    }
  } };
  vscode.window.showQuickPick = (async (items: any[], options: {title: string}) => {
    if (options.title.endsWith("· Organization")) return items[0];
    if (options.title.endsWith("· Examples")) return items[1];
    if (options.title.endsWith("· References")) return items;
    if (options.title.endsWith("· Images to include")) return items.filter(item => item.label === "logo.png");
    return "Save as recipe and create draft";
  }) as typeof vscode.window.showQuickPick;
  vscode.window.showInputBox = async () => "Welcome";
  vscode.window.withProgress = (async (_options: unknown, action: (progress: unknown, cancel: unknown) => Promise<unknown>) =>
    action({report() {}}, {isCancellationRequested:false})) as typeof vscode.window.withProgress;
  const opened: string[] = [];
  vscode.commands.executeCommand = async (_command: string, uri: unknown) => { opened.push(String(uri)); return undefined as never; };
  const context = {workspaceState:{update:async () => {}}};
  const flow = new TemplateExamples(api as unknown as EditorConnectionAPI, context as unknown as vscode.ExtensionContext);
  await Promise.all([flow.start(),flow.start()]);
  assert.equal(calls.filter(call => call.name === "composeMaterialize").length,1,"double click started two AI jobs");
  const materialize = calls.find(call => call.name === "composeMaterialize")!.query;
  const recipe = calls.find(call => call.name === "createComposeRecipe")!.query;
  for (const query of [materialize,recipe]) {
    assert.match(query,/accountIds:\s*\["client-org"\]/);
    const logo = query.match(/\{[^{}]*(?:ref|selector): "logo"[^{}]*\}/)?.[0] || "";
    const layout = query.match(/\{[^{}]*(?:ref|selector): "layout"[^{}]*\}/)?.[0] || "";
    assert.match(logo,/includeImages: true/);
    assert.doesNotMatch(layout,/includeImages: true/);
  }
  assert.equal(opened.length,1);
  assert.match(opened[0],/^memql-file:\/\/client.example\/artifacts\/artifact\//);
  assert.ok(!calls.some(call => /send|publish|campaignSaveTemplate/i.test(call.name)),"creation must leave review and publication to the human");
});
