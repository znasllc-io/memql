import { test } from "node:test";
import assert from "node:assert/strict";
import { createMarkdown } from "../src/newMarkdown.js";
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
