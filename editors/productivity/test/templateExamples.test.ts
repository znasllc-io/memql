import { test } from "node:test";
import assert from "node:assert/strict";
import * as vscode from "vscode";
import { TemplateExamples } from "../src/templateExamples.js";
import type { EditorConnectionAPI } from "../../vscode/src/connection/api.js";

// What VS Code hands a progress task: a token the person can stop, which tells
// whoever is listening.
function progressToken() {
  const listeners = new Set<() => void>();
  return {
    isCancellationRequested: false,
    onCancellationRequested(listener: () => void) { listeners.add(listener); return { dispose: () => { listeners.delete(listener); } }; },
    cancel() { this.isCancellationRequested = true; for (const listener of [...listeners]) listener(); },
    get listening() { return listeners.size; },
  };
}

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
      case "libraryArtifactForFile": return [{id:"artifact",title:"Welcome.email.json"}];
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
  let checkName: ((text: string) => string | undefined) | undefined;
  vscode.window.showInputBox = async (options?: { title?: string; validateInput?: (text: string) => string | undefined }) => {
    if (options?.title?.endsWith("· Name")) checkName = options.validateInput;
    return "Welcome";
  };
  const token = progressToken();
  vscode.window.withProgress = (async (_options: unknown, action: (progress: unknown, cancel: unknown) => Promise<unknown>) =>
    action({report() {}}, token)) as typeof vscode.window.withProgress;
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
  assert.match(materialize,/requestId:\s*"[0-9a-f-]{36}"/,"a repeat of this request must be recognisable to the cluster");
  // The name becomes the draft's file name, so it is checked with the rule and words every creation flow uses
  // (the SDK's fileNameProblem): a name is not a place to file into, and the draft is a JSON file.
  assert.equal(checkName?.("Autumn welcome"),undefined);
  assert.equal(checkName?.("Autumn welcome.json"),undefined);
  for (const [text, problem] of [
    ["", "Enter a file name."], ["  ", "Enter a file name."],
    ["Autumn/Winter welcome", "Enter a file name, without a folder path."], ["Autumn\\Winter welcome", "Enter a file name, without a folder path."],
    ["x".repeat(151), "Use a name up to 150 characters."], [".json", "Enter a name before the extension."],
  ]) assert.equal(checkName?.(text),problem,`"${text}"`);
  assert.equal(token.listening,0,"nothing keeps listening for a stop once the draft is open");
  // Asking for another draft is another request, not a repeat of the first.
  await flow.start();
  const requestIds = calls.filter(call => call.name === "composeMaterialize").map(call => /requestId:\s*"([^"]*)"/.exec(call.query)?.[1]);
  assert.equal(requestIds.length,2);
  assert.ok(requestIds[0] && requestIds[1] && requestIds[0] !== requestIds[1],"a second draft reused the first one's request id");
});

// Opens the saved composition (Open Last Email Composition), with `composition`
// answering each look at it. The same follow runs after a fresh start.
function resuming(options: { composition: (look: number) => Record<string, unknown>[]; artifact?: Record<string, unknown>[]; token?: ReturnType<typeof progressToken> }) {
  const lease = { domain: "client.example", name: "Client", generation: 1 };
  const token = options.token ?? progressToken();
  const calls: { name: string; query: string }[] = [];
  const reports: string[] = [];
  const notifications: Record<string, unknown>[] = [];
  const opened: { command: string; uri: string; viewType: unknown }[] = [];
  const looks = () => calls.filter(call => call.name === "compositionById").length;
  const api = {
    connect: async (domain: string) => { assert.equal(domain, "client.example"); return lease; },
    execute: async (actual: unknown, name: string, query: string) => {
      assert.equal(actual, lease, "the composition is followed under the connection it started on");
      calls.push({ name, query });
      if (name === "compositionById") {
        assert.ok(looks() <= 3, "the draft is still being followed after it was stopped");
        return options.composition(looks());
      }
      if (name === "libraryArtifactForFile") return options.artifact ?? [{ id: "artifact", title: "Welcome.email.json" }];
      return [];
    },
  };
  vscode.window.withProgress = (async (settings: Record<string, unknown>, action: (progress: unknown, cancel: unknown) => Promise<unknown>) => {
    notifications.push(settings);
    return action({ report: (value: { message?: string }) => reports.push(value.message ?? "") }, token);
  }) as typeof vscode.window.withProgress;
  vscode.commands.executeCommand = (async (command: string, uri: unknown, viewType: unknown) => { opened.push({ command, uri: String(uri), viewType }); return undefined; }) as typeof vscode.commands.executeCommand;
  const context = { workspaceState: { get: () => ({ domain: "client.example", compositionId: "composition" }), update: async () => {} } };
  const flow = new TemplateExamples(api as unknown as EditorConnectionAPI, context as unknown as vscode.ExtensionContext);
  return { flow, token, calls, reports, notifications, opened };
}
const cancels = (calls: { name: string; query: string }[]) => calls.filter(call => call.name === "composeCancel").map(call => /compositionId:\s*"([^"]*)"/.exec(call.query)?.[1]);

test("resuming opens the finished draft in the email editor", async () => {
  const { flow, token, notifications, opened } = resuming({ composition: () => [{ status: "ready", outputFileId: "file", name: "Welcome" }] });
  await flow.resume();
  assert.deepEqual(opened, [{ command: "vscode.openWith", uri: "memql-file://client.example/artifacts/artifact/Welcome.email.json", viewType: "memql.productivity.email" }]);
  assert.equal(notifications[0].cancellable, true, "the person can stop a draft from its notification");
  assert.equal(token.listening, 0, "nothing keeps listening for a stop once the draft is open");
});

test("the draft is opened under the name it was filed as", async () => {
  const cases = [
    { filed: { id: "artifact", title: "Welcome.email.json" }, linked: "Welcome.email.json" },
    { filed: { id: "artifact", title: "Welcome" }, linked: "Welcome.email.json" },
    { filed: { id: "artifact" }, linked: "Spring.email.json" },
  ];
  for (const { filed, linked } of cases) {
    const { flow, opened } = resuming({ composition: () => [{ status: "ready", outputFileId: "file", name: "Spring" }], artifact: [filed] });
    await flow.resume();
    assert.equal(opened[0].uri, `memql-file://client.example/artifacts/artifact/${linked}`);
  }
  const { flow, opened } = resuming({ composition: () => [{ status: "ready", outputFileId: "file" }], artifact: [{ id: "artifact" }] });
  await flow.resume();
  assert.equal(opened[0].uri, "memql-file://client.example/artifacts/artifact/template.email.json");
});

test("stopping the notification stops the composition and opens nothing", async () => {
  const token = progressToken();
  const { flow, calls, reports, opened } = resuming({ token, composition: look => { if (look === 1) token.cancel(); return [{ status: "composing" }]; } });
  const started = Date.now();
  await flow.resume();
  assert.deepEqual(cancels(calls), ["composition"]);
  assert.deepEqual(opened, []);
  assert.deepEqual(reports, ["Studying references and composing the draft"]);
  assert.equal(token.listening, 0);
  assert.ok(Date.now() - started < 1500, "a stop waited for the next look at the composition instead of ending the wait");
});

test("a stop that arrived before the first look still stops the composition", async () => {
  const token = progressToken();
  token.isCancellationRequested = true;
  const { flow, calls, opened } = resuming({ token, composition: () => [{ status: "composing" }] });
  await flow.resume();
  assert.deepEqual(calls.map(call => call.name), ["composeCancel"]);
  assert.deepEqual(opened, []);
});

test("a composition stopped from somewhere else ends quietly", async () => {
  const { flow, calls, opened } = resuming({ composition: () => [{ status: "cancelled" }] });
  await flow.resume();
  assert.deepEqual(cancels(calls), []);
  assert.deepEqual(opened, []);
});

test("a failed composition is reported with its reason and opens nothing", async () => {
  const { flow, opened } = resuming({ composition: () => [{ status: "failed", failureReason: "The example could not be read" }] });
  await assert.rejects(flow.resume(), /The example could not be read/);
  assert.deepEqual(opened, []);
});
