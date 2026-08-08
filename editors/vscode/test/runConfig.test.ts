// Run configurations as plain editable text.
//
// The requirement is not "run configurations persist" -- it is that they
// persist AS TEXT a developer can open, diff, review and commit, and that an
// agent can author or edit. That makes the file untrusted input from a
// repository, which is why every read validates instead of casting, and why a
// single malformed entry must not make a repo's whole run set disappear.
//
// It also makes the write path dangerous in a way an internal state blob is
// not: the developer may be editing the file right now, so a save has to be
// read-modify-write, and a file that does not parse has to be refused rather
// than overwritten.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";

import {
  emptyRunConfigFile,
  parseRunConfigFile,
  readRunConfigs,
  removeRunConfig,
  serializeRunConfigFile,
  upsertRunConfig,
  writeRunConfigs,
  type RunConfig,
} from "../src/run/runConfig.js";

function config(overrides: Partial<RunConfig> = {}): RunConfig {
  return {
    name: "participants in s1",
    kind: "query",
    construct: "spaceParticipants",
    args: { spaceId: "s1" },
    ...overrides,
  };
}

async function tempDir(): Promise<string> {
  return fs.mkdtemp(path.join(os.tmpdir(), "memql-runs-"));
}

// -----------------------------------------------------------------------------
// Parsing
// -----------------------------------------------------------------------------

test("parseRunConfigFile -- reads a well-formed file", () => {
  const result = parseRunConfigFile(
    JSON.stringify({
      version: 1,
      runs: [{ name: "a", kind: "query", construct: "q", file: "dsl/x/queries.memql", args: { id: "1" } }],
    }),
  );
  assert.ok(result.ok);
  assert.equal(result.file.runs.length, 1);
  assert.equal(result.file.runs[0]?.file, "dsl/x/queries.memql");
  assert.deepEqual(result.file.runs[0]?.args, { id: "1" });
  assert.deepEqual(result.dropped, []);
});

test("parseRunConfigFile -- an empty file is an empty set, not an error", () => {
  const result = parseRunConfigFile("");
  assert.ok(result.ok);
  assert.deepEqual(result.file.runs, []);
});

test("parseRunConfigFile -- broken JSON is an ERROR, not an empty list", () => {
  // Rendering an empty list would look identical to "you have no run
  // configurations", and the developer would re-create the ones they already
  // have on top of a file that is about to be refused for writing.
  const result = parseRunConfigFile("{ nope");
  assert.equal(result.ok, false);
});

test("parseRunConfigFile -- a non-object top level is an error", () => {
  assert.equal(parseRunConfigFile("[]").ok, false);
  assert.equal(parseRunConfigFile('"runs"').ok, false);
});

test("parseRunConfigFile -- a non-array runs is an error", () => {
  assert.equal(parseRunConfigFile('{"runs": {}}').ok, false);
});

test("parseRunConfigFile -- ONE malformed entry is dropped, the rest survive", () => {
  const result = parseRunConfigFile(
    JSON.stringify({
      version: 1,
      runs: [
        { name: "good", kind: "query", construct: "q", args: {} },
        { name: "", kind: "query", construct: "q", args: {} },
        { name: "no kind", construct: "q", args: {} },
        { name: "bad kind", kind: "concept", construct: "q", args: {} },
        { name: "args not an object", kind: "query", construct: "q", args: [1, 2] },
      ],
    }),
  );
  assert.ok(result.ok);
  assert.deepEqual(result.file.runs.map((r) => r.name), ["good"]);
  assert.equal(result.dropped.length, 4);
});

test("parseRunConfigFile -- a duplicate name is dropped and named", () => {
  // Every lookup resolves the FIRST match, so the loser would be visible in
  // the tree and unreachable by name.
  const result = parseRunConfigFile(
    JSON.stringify({
      runs: [
        { name: "a", kind: "query", construct: "q1", args: {} },
        { name: "a", kind: "query", construct: "q2", args: {} },
      ],
    }),
  );
  assert.ok(result.ok);
  assert.equal(result.file.runs.length, 1);
  assert.equal(result.file.runs[0]?.construct, "q1");
  assert.match(result.dropped[0] ?? "", /duplicate name "a"/);
});

test("parseRunConfigFile -- a missing args block reads as no arguments", () => {
  const result = parseRunConfigFile(
    JSON.stringify({ runs: [{ name: "a", kind: "query", construct: "q" }] }),
  );
  assert.ok(result.ok);
  assert.deepEqual(result.file.runs[0]?.args, {});
});

// -----------------------------------------------------------------------------
// Serialisation
// -----------------------------------------------------------------------------

test("serializeRunConfigFile -- is indented text with a trailing newline", () => {
  // A human edits this and a repository diffs it.
  const text = serializeRunConfigFile(upsertRunConfig(emptyRunConfigFile(), config()));
  assert.match(text, /\n {2}"runs": \[/);
  assert.ok(text.endsWith("\n"));
});

test("serializeRunConfigFile -- argument keys are sorted so re-saving produces no diff", () => {
  const a = serializeRunConfigFile(
    upsertRunConfig(emptyRunConfigFile(), config({ args: { b: 1, a: 2 } })),
  );
  const b = serializeRunConfigFile(
    upsertRunConfig(emptyRunConfigFile(), config({ args: { a: 2, b: 1 } })),
  );
  assert.equal(a, b);
});

test("serialize -> parse round-trips", () => {
  const file = upsertRunConfig(
    upsertRunConfig(emptyRunConfigFile(), config()),
    config({ name: "two", kind: "mutate", construct: "createSpace", args: { name: "Ops" } }),
  );
  const result = parseRunConfigFile(serializeRunConfigFile(file));
  assert.ok(result.ok);
  assert.deepEqual(result.file, file);
});

// -----------------------------------------------------------------------------
// Upsert / remove
// -----------------------------------------------------------------------------

test("upsertRunConfig -- replaces by name and does not mutate the input", () => {
  const before = upsertRunConfig(emptyRunConfigFile(), config());
  const after = upsertRunConfig(before, config({ args: { spaceId: "s2" } }));
  assert.equal(after.runs.length, 1);
  assert.deepEqual(after.runs[0]?.args, { spaceId: "s2" });
  assert.deepEqual(before.runs[0]?.args, { spaceId: "s1" });
});

test("removeRunConfig -- drops only the named entry", () => {
  const file = upsertRunConfig(
    upsertRunConfig(emptyRunConfigFile(), config({ name: "a" })),
    config({ name: "b" }),
  );
  assert.deepEqual(removeRunConfig(file, "a").runs.map((r) => r.name), ["b"]);
  assert.deepEqual(removeRunConfig(file, "missing").runs.map((r) => r.name), ["a", "b"]);
});

// -----------------------------------------------------------------------------
// File IO
// -----------------------------------------------------------------------------

test("readRunConfigs -- a missing file is an empty set, not a failure", async () => {
  // Most workspaces ship no run configurations, so this is the common state
  // and the tree must render an empty list rather than an error row.
  const dir = await tempDir();
  const result = await readRunConfigs(path.join(dir, "runs.json"));
  assert.ok(result.ok);
  assert.deepEqual(result.file.runs, []);
});

test("writeRunConfigs -- creates the directory and the file", async () => {
  const dir = await tempDir();
  const file = path.join(dir, ".memql", "runs.json");
  await writeRunConfigs(file, (current) => upsertRunConfig(current, config()));
  const result = await readRunConfigs(file);
  assert.ok(result.ok);
  assert.equal(result.file.runs[0]?.name, "participants in s1");
});

test("writeRunConfigs -- is READ-MODIFY-WRITE against the file on disk", async () => {
  // The developer may be editing this file right now, or an agent may have
  // just appended to it. Serialising a cached value would clobber that.
  const dir = await tempDir();
  const file = path.join(dir, "runs.json");
  await writeRunConfigs(file, (c) => upsertRunConfig(c, config({ name: "first" })));
  // Simulate the concurrent edit, then perform a write that knows nothing
  // about it.
  const external = parseRunConfigFile(await fs.readFile(file, "utf8"));
  assert.ok(external.ok);
  await fs.writeFile(
    file,
    serializeRunConfigFile(upsertRunConfig(external.file, config({ name: "external" }))),
    "utf8",
  );

  await writeRunConfigs(file, (c) => upsertRunConfig(c, config({ name: "second" })));

  const result = await readRunConfigs(file);
  assert.ok(result.ok);
  assert.deepEqual(result.file.runs.map((r) => r.name).sort(), ["external", "first", "second"]);
});

test("writeRunConfigs -- REFUSES to overwrite a file that does not parse", async () => {
  // The one thing worse than not saving the new run configuration is
  // destroying the ten already in the file.
  const dir = await tempDir();
  const file = path.join(dir, "runs.json");
  await fs.writeFile(file, "{ this is not json", "utf8");
  await assert.rejects(
    writeRunConfigs(file, (c) => upsertRunConfig(c, config())),
    /refusing to overwrite/,
  );
  assert.equal(await fs.readFile(file, "utf8"), "{ this is not json");
});

// -----------------------------------------------------------------------------
// The automation block (memql#3310)
// -----------------------------------------------------------------------------
//
// An automation has no declared `args`, so `args` cannot carry what its run
// needs. The block below EXTENDS the same configuration shape rather than
// introducing a parallel file, which is what keeps one Runs tree, one reader
// and one writer serving both surfaces.

test("parseRunConfigFile -- an automation entry round-trips its trigger event", () => {
  const parsed = parseRunConfigFile(
    JSON.stringify({
      version: 1,
      runs: [
        {
          name: "join a participant",
          kind: "automation",
          construct: "autoJoinSI",
          file: "dsl/cognition/automations.memql",
          args: {},
          automation: {
            payload: { id: "v1:cognition:participant:abc", payload: { role: "human" } },
            concept: "v1:cognition:participant",
            targetNodeType: "cognition",
            includeStepOutput: true,
          },
        },
      ],
    }),
  );
  assert.ok(parsed.ok);
  assert.deepEqual(parsed.file.runs[0]?.automation, {
    payload: { id: "v1:cognition:participant:abc", payload: { role: "human" } },
    concept: "v1:cognition:participant",
    targetNodeType: "cognition",
    includeStepOutput: true,
  });
  // And it survives a serialize/parse cycle unchanged, which is what makes the
  // file plain editable text rather than a format that rewrites itself.
  const round = parseRunConfigFile(serializeRunConfigFile(parsed.file));
  assert.ok(round.ok);
  assert.deepEqual(round.file.runs[0]?.automation, parsed.file.runs[0]?.automation);
});

test("parseRunConfigFile -- an automation entry with no block is valid", () => {
  // A fire-now run of a scheduled automation carries no payload at all, so the
  // block is genuinely optional rather than merely tolerated.
  const parsed = parseRunConfigFile(
    JSON.stringify({
      version: 1,
      runs: [{ name: "reap", kind: "automation", construct: "reapStale", args: {} }],
    }),
  );
  assert.ok(parsed.ok);
  assert.equal(parsed.file.runs.length, 1);
  assert.equal(parsed.file.runs[0]?.automation, undefined);
});

test("parseRunConfigFile -- a MALFORMED automation block DROPS the entry", () => {
  // Ignoring the block would run the automation with an empty event -- a
  // different run from the one the file describes, and one with real side
  // effects. Dropping it says so.
  for (const automation of [
    { payload: [1, 2] },
    { payload: "not an object" },
    { concept: 7 },
    { targetNodeType: {} },
    { includeStepOutput: "yes" },
    "not a block",
    [],
  ]) {
    const parsed = parseRunConfigFile(
      JSON.stringify({
        version: 1,
        runs: [{ name: "bad", kind: "automation", construct: "x", args: {}, automation }],
      }),
    );
    assert.ok(parsed.ok);
    assert.equal(parsed.file.runs.length, 0, `expected ${JSON.stringify(automation)} to drop the entry`);
    assert.equal(parsed.dropped.length, 1);
  }
});

test("parseRunConfigFile -- an empty payload OBJECT is kept, not normalised away", () => {
  // `{}` and an absent payload both fire with an empty event today, but they
  // are different things an author can write, and a reader that silently
  // rewrote one into the other would make the file disagree with itself on the
  // next save.
  const parsed = parseRunConfigFile(
    JSON.stringify({
      version: 1,
      runs: [
        { name: "e", kind: "automation", construct: "x", args: {}, automation: { payload: {} } },
      ],
    }),
  );
  assert.ok(parsed.ok);
  assert.deepEqual(parsed.file.runs[0]?.automation, { payload: {} });
});
