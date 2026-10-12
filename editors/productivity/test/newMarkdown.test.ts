import { test } from "node:test";
import assert from "node:assert/strict";
import * as vscode from "vscode";
import { createMarkdown, markdownNameProblem, newMarkdown } from "../src/newMarkdown.js";
import { UserInputError } from "../src/problems.js";

const lease = { domain: "client.example", name: "Client", generation: 1 };

// A cluster that does what the Materializer does for a blank file: the
// composition is ready on the first look and its file already has a Files entry.
function cluster(rows: { artifact?: Record<string, unknown>; composition?: Record<string, unknown> } = {}) {
  const calls: { name: string; query: string }[] = [];
  const api = {
    uploadFile: async () => { throw new Error("New Markdown must not upload"); },
    execute: async (actual: unknown, name: string, query: string) => {
      assert.equal(actual, lease, "every call is made under the lease the person chose");
      calls.push({ name, query });
      if (name === "composeMaterialize") return [{ compositionId: "c1" }];
      if (name === "compositionById") return [{ status: "ready", outputFileId: "f1", ...(rows.composition ?? { name: "Plan" }) }];
      if (name === "libraryArtifactForFile") return [rows.artifact ?? { id: "a1", title: "Plan.md" }];
      return [];
    },
  };
  return { api: api as never, calls, named: (name: string) => calls.filter(call => call.name === name) };
}

test("New Markdown creates through the Materializer, once, with a clean name", async () => {
  const { api, calls, named } = cluster();
  const [first, second] = await Promise.all([
    createMarkdown(api, lease as never, "Plan.md", "req-1"),
    createMarkdown(api, lease as never, "Plan.md", "req-1"),
  ]);
  assert.equal(String(first), "memql-file://client.example/artifacts/a1/Plan.md");
  assert.equal(String(second), String(first));
  const starts = named("composeMaterialize");
  assert.equal(starts.length, 1, "a double Enter started two compositions");
  assert.match(starts[0].query, /requestId:\s*"req-1"/);
  assert.match(starts[0].query, /format:\s*"markdown"/);
  assert.match(starts[0].query, /name:\s*"Plan"[,)]/);
  assert.doesNotMatch(starts[0].query, /statement/, "a new document is blank: nothing is asked of the writer");
  // Creating is the whole job: no upload and no description hand-off to the Library.
  assert.deepEqual([...new Set(calls.map(call => call.name))].sort(), ["composeMaterialize", "compositionById", "libraryArtifactForFile"]);
});

test("the extension is dropped from the name the cluster is asked for", async () => {
  for (const [typed, sent] of [["Plan.md", "Plan"], ["Plan", "Plan"], ["notes.MARKDOWN", "notes"], ["  Plan.md  ", "Plan"], ["Plan .md", "Plan"], ["Q3.report.md", "Q3.report"]]) {
    const { api, named } = cluster();
    await createMarkdown(api, lease as never, typed, `req-${sent}-${typed.length}`);
    assert.match(named("composeMaterialize")[0].query, new RegExp(`name:\\s*"${sent.replace(/\./g, "\\.")}"[,)]`), `"${typed}" should be asked for as "${sent}"`);
  }
});

test("the editor link names the file as the cluster filed it", async () => {
  const cases = [
    { label: "a title that already names the file", typed: "Notes.md", artifact: { id: "a1", title: "Weekly-notes.md" }, linked: "Weekly-notes.md" },
    { label: "a title without the extension", typed: "Plan.md", artifact: { id: "a1", title: "Plan" }, linked: "Plan.md" },
    { label: "a name the cluster tidied", typed: ".hidden.md", artifact: { id: "a1", title: "hidden.md" }, linked: "hidden.md" },
    { label: "no title: the composition's name", typed: "Typed.md", artifact: { id: "a1" }, composition: { name: "Draft" }, linked: "Draft.md" },
    { label: "no title and no composition name: the name asked for", typed: "Typed.md", artifact: { id: "a1" }, composition: {}, linked: "Typed.md" },
  ];
  for (const { label, typed, artifact, composition, linked } of cases) {
    const uri = await createMarkdown(cluster({ artifact, composition }).api, lease as never, typed, `req-link-${linked}`);
    assert.equal(String(uri), `memql-file://client.example/artifacts/a1/${linked}`, label);
  }
});

test("a request id is free again once its creation has failed", async () => {
  const starts: string[] = [];
  let refuse = true;
  const api = { execute: async (_lease: unknown, name: string, query: string) => {
    if (name === "composeMaterialize") {
      starts.push(query);
      if (refuse) { refuse = false; throw new Error("refused"); }
      return [{ compositionId: "c1" }];
    }
    if (name === "compositionById") return [{ status: "ready", outputFileId: "f1", name: "Plan" }];
    return [{ id: "a1", title: "Plan.md" }];
  } } as never;
  await assert.rejects(createMarkdown(api, lease as never, "Plan.md", "req-again"), /refused/);
  assert.equal(String(await createMarkdown(api, lease as never, "Plan.md", "req-again")), "memql-file://client.example/artifacts/a1/Plan.md");
  assert.equal(starts.length, 2);
});

test("different request ids are different documents", async () => {
  const { api, named } = cluster();
  await Promise.all([createMarkdown(api, lease as never, "Plan.md", "req-a"), createMarkdown(api, lease as never, "Plan.md", "req-b")]);
  assert.deepEqual(named("composeMaterialize").map(call => /requestId:\s*"([^"]*)"/.exec(call.query)?.[1]).sort(), ["req-a", "req-b"]);
});

test("a name that is only an extension is refused before anything is sent", async () => {
  const { api, calls } = cluster();
  for (const typed of [".md", ".markdown", " .MD "]) {
    await assert.rejects(createMarkdown(api, lease as never, typed, `req-${typed.length}`), error => error instanceof UserInputError && /file name/.test(error.message));
  }
  assert.equal(calls.length, 0);
});

// ---------------------------------------------------------------------------
// The name the person types
// ---------------------------------------------------------------------------

test("a name is checked while it is typed, an extension on its own included", () => {
  for (const name of ["Untitled.md", "Plan", "Q3.report.md", "notes.markdown", "a", "Plan .md", "  Plan  "]) {
    assert.equal(markdownNameProblem(name), undefined, `"${name}" should be accepted`);
  }
  for (const name of ["", "   "]) {
    assert.equal(markdownNameProblem(name), "Enter a file name.", `"${name}" has no name in it`);
  }
  for (const name of ["a/b.md", "a\\b.md", "tab\there.md"]) {
    assert.equal(markdownNameProblem(name), "Enter a file name, without a folder path.", `"${name}" should be refused as a path`);
  }
  assert.equal(markdownNameProblem("x".repeat(151)), "Use a name up to 150 characters.");
  // Nothing before the extension: the cluster adds ".md" itself, so this would file a document with no name.
  for (const name of [".md", ".markdown", " .MD ", ".Markdown"]) {
    assert.equal(markdownNameProblem(name), "Enter a name before the extension.", `"${name}" should be refused inline`);
  }
});

// ---------------------------------------------------------------------------
// The command: name, progress, stop, open
// ---------------------------------------------------------------------------

// What VS Code hands a progress task: a token the person can stop, which tells whoever is listening.
function progressToken() {
  const listeners = new Set<() => void>();
  return {
    isCancellationRequested: false,
    onCancellationRequested(listener: () => void) { listeners.add(listener); return { dispose: () => { listeners.delete(listener); } }; },
    cancel() { this.isCancellationRequested = true; for (const listener of [...listeners]) listener(); },
    get listening() { return listeners.size; },
  };
}

// A cluster at which the person is connected, the prompt answered with `typed`, and the notification recorded.
function command(options: { typed?: string | undefined; token?: ReturnType<typeof progressToken>; answer?: (name: string, query: string, looks: number) => Record<string, unknown>[] | undefined } = {}) {
  const token = options.token ?? progressToken();
  const calls: { name: string; query: string }[] = [];
  const notifications: Record<string, unknown>[] = [];
  const reports: (string | undefined)[] = [];
  const prompts: Record<string, unknown>[] = [];
  const opened: string[] = [];
  const api = {
    current: () => lease,
    execute: async (actual: unknown, name: string, query: string) => {
      assert.equal(actual, lease, "every call is made under the lease the person chose");
      calls.push({ name, query });
      const looks = calls.filter(call => call.name === name).length;
      // A follow that is not ended by a stop would poll for as long as the composition stays unfinished: end it
      // with a message instead of hanging the run.
      assert.ok(name !== "compositionById" || looks <= 3, "the document is still being followed after it was stopped");
      const custom = options.answer?.(name, query, looks);
      if (custom) return custom;
      if (name === "composeMaterialize") return [{ compositionId: "c1" }];
      if (name === "compositionById") return [{ status: "ready", outputFileId: "f1", name: "Untitled" }];
      if (name === "libraryArtifactForFile") return [{ id: "a1", title: "Untitled.md" }];
      return [];
    },
  };
  vscode.window.showInputBox = (async (prompt?: Record<string, unknown>) => { prompts.push(prompt ?? {}); return "typed" in options ? options.typed : "Untitled.md"; }) as typeof vscode.window.showInputBox;
  vscode.window.withProgress = (async (settings: Record<string, unknown>, action: (progress: unknown, cancel: unknown) => Promise<unknown>) => {
    notifications.push(settings);
    return action({ report(value: { message?: string }) { reports.push(value.message); } }, token);
  }) as typeof vscode.window.withProgress;
  const run = () => newMarkdown(api as never, async uri => { opened.push(String(uri)); });
  return { run, token, calls, notifications, reports, prompts, opened, named: (name: string) => calls.filter(call => call.name === name) };
}

test("New Markdown asks for a name, creates the document in a notification the person can stop, and opens it", async () => {
  const { run, token, notifications, prompts, opened, named } = command();
  await run();
  assert.equal(prompts.length, 1);
  assert.equal(prompts[0]?.value, "Untitled.md");
  assert.equal(notifications.length, 1);
  assert.equal(notifications[0]?.cancellable, true, "a document that is slow to appear can be stopped from its notification");
  assert.equal(notifications[0]?.title, "Creating Untitled.md");
  assert.equal(named("composeMaterialize").length, 1);
  assert.deepEqual(opened, ["memql-file://client.example/artifacts/a1/Untitled.md"]);
  assert.equal(token.listening, 0, "nothing keeps listening for a stop once the document is open");
});

test("the notification says where the document is while the cluster makes it", { timeout: 20000 }, async () => {
  // The composition is still being made at the first look, and once it is ready its Files entry
  // trails it: both are said in the notification the person is watching.
  const { run, reports, opened } = command({
    answer: (name, _query, looks) => {
      if (name === "compositionById") return looks === 1 ? [{ status: "composing" }] : undefined;
      if (name === "libraryArtifactForFile") return looks === 1 ? [] : undefined;
      return undefined;
    },
  });
  await run();
  assert.deepEqual(reports, ["composing", "Saved; waiting for its Files entry"]);
  assert.deepEqual(opened, ["memql-file://client.example/artifacts/a1/Untitled.md"]);
});

test("the prompt refuses an extension on its own, before anything is created", async () => {
  const { run, prompts } = command({ typed: undefined });
  await run();
  const check = prompts[0]?.validateInput as ((text: string) => string | undefined) | undefined;
  assert.equal(check?.(".md"), "Enter a name before the extension.");
  assert.equal(check?.("Plan.md"), undefined);
  assert.equal(check?.("a/b"), "Enter a file name, without a folder path.");
});

test("a prompt left empty creates nothing", async () => {
  const { run, calls, notifications, opened } = command({ typed: undefined });
  await run();
  assert.deepEqual(calls, []);
  assert.deepEqual(notifications, []);
  assert.deepEqual(opened, []);
});

test("not being connected is said before the person is asked for anything", async () => {
  const prompts: unknown[] = [];
  vscode.window.showInputBox = (async (prompt?: unknown) => { prompts.push(prompt); return "Untitled.md"; }) as typeof vscode.window.showInputBox;
  const api = { current: () => undefined, execute: async () => { throw new Error("must not be called"); } };
  await assert.rejects(newMarkdown(api as never, async () => {}), error => error instanceof UserInputError && /Connect to your cluster/.test(error.message));
  assert.deepEqual(prompts, []);
});

test("each document asked for is its own request, so a repeat of one ask is not mistaken for the next", async () => {
  const { run, named } = command();
  await run();
  await run();
  const ids = named("composeMaterialize").map(call => /requestId:\s*"([^"]*)"/.exec(call.query)?.[1]);
  assert.equal(ids.length, 2);
  assert.ok(ids[0] && ids[1] && ids[0] !== ids[1], "the second document reused the first one's request id");
});

test("stopping the notification stops the composition, opens nothing, and is not reported as a failure", { timeout: 20000 }, async () => {
  const token = progressToken();
  const { run, named, opened } = command({ token, answer: (name, _query, looks) => name === "compositionById" ? (looks === 1 ? (token.cancel(), [{ status: "composing" }]) : [{ status: "composing" }]) : undefined });
  const started = Date.now();
  await run(); // resolves: a stop is a quiet end, not an error for the person to read
  assert.deepEqual(named("composeCancel").map(call => /compositionId:\s*"([^"]*)"/.exec(call.query)?.[1]), ["c1"]);
  assert.deepEqual(opened, []);
  assert.equal(token.listening, 0);
  assert.ok(Date.now() - started < 1500, "a stop waited for the next look at the composition instead of ending the wait");
});

test("a stop that arrived before anything was sent creates nothing", { timeout: 20000 }, async () => {
  const token = progressToken();
  token.isCancellationRequested = true;
  const { run, calls, opened } = command({ token });
  await run();
  assert.deepEqual(calls, []);
  assert.deepEqual(opened, []);
});

test("a stop that lands once the document exists does not take it away", { timeout: 20000 }, async () => {
  // The file is made: stopping can no longer undo it, so the person is given it.
  const token = progressToken();
  const { run, named, opened } = command({ token, answer: name => name === "libraryArtifactForFile" ? (token.cancel(), [{ id: "a1", title: "Untitled.md" }]) : undefined });
  await run();
  assert.equal(named("composeCancel").length, 0, "a document that exists is not cancelled");
  assert.deepEqual(opened, ["memql-file://client.example/artifacts/a1/Untitled.md"]);
});

test("a creation that fails is reported, not swallowed as a stop", async () => {
  const { run, opened } = command({ answer: name => { if (name === "composeMaterialize") throw new Error("compose is unavailable"); return undefined; } });
  await assert.rejects(run(), /compose is unavailable/);
  assert.deepEqual(opened, []);
});
