// A capability script saying where it has got to: `cap_progress` on the shell
// side, a `stepPhase` event on this one.
//
// The line is a statement about the step, not an account of what it did, so
// the runner takes it out of the output entirely: it must never reach the log
// pane, the Output channel, a failure's saved output or the run record. These
// tests pin both halves -- the parse, and the absence.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";

import { loadGraph, type Graph } from "../src/install/graph.js";
import { executeGraph, type ExecEvent } from "../src/install/executor.js";
import {
  PROGRESS_PREFIX,
  parseProgressLine,
  runCapabilityScript,
  type ProgressLine,
} from "../src/install/runner.js";

async function script(body: string): Promise<string> {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "memql-progress-"));
  const file = path.join(dir, "step.sh");
  await fs.writeFile(file, `#!/usr/bin/env bash\n${body}\n`, { encoding: "utf8", mode: 0o755 });
  return file;
}

const ENVELOPE = `echo '{"ok":true,"capability":"t","changed":false,"result":{"supported":true},"error":null}'`;

// -----------------------------------------------------------------------------
// the parse
// -----------------------------------------------------------------------------

test("the prefix is the one capability.sh writes", () => {
  assert.equal(PROGRESS_PREFIX, "::memql-progress::");
});

test("a counted line and an uncounted one", () => {
  assert.deepEqual(parseProgressLine("::memql-progress:: 5/9 Starting services"), {
    label: "Starting services",
    done: 5,
    total: 9,
  });
  assert.deepEqual(parseProgressLine("::memql-progress:: - Installing ArgoCD"), { label: "Installing ArgoCD" });
});

test("every other line is not progress", () => {
  for (const line of [
    "INFO:  Waiting up to 900s for the MemQL workloads to become Available...",
    "  ::memql-progress:: - indented is not the prefix",
    "memql-progress:: - no leading colons",
    "",
  ]) {
    assert.equal(parseProgressLine(line), null, line);
  }
});

test("a line with no label is not progress, so a broken call stays visible in the log", () => {
  assert.equal(parseProgressLine("::memql-progress::"), null);
  assert.equal(parseProgressLine("::memql-progress:: -"), null);
  assert.equal(parseProgressLine("::memql-progress:: 3/9"), null, "a count is not a label");
});

test("a malformed count leaves the whole remainder as the label", () => {
  assert.deepEqual(parseProgressLine("::memql-progress:: three/9 Building images"), {
    label: "three/9 Building images",
  });
  assert.deepEqual(parseProgressLine("::memql-progress:: 3/0 Building images"), { label: "Building images" });
});

test("a count past its total is clamped to the total", () => {
  assert.deepEqual(parseProgressLine("::memql-progress:: 11/9 Importing images"), {
    label: "Importing images",
    done: 9,
    total: 9,
  });
});

test("a carriage return from a CRLF writer is not part of the label", () => {
  assert.deepEqual(parseProgressLine("::memql-progress:: - Seeding secrets\r"), { label: "Seeding secrets" });
});

// -----------------------------------------------------------------------------
// the runner keeps them out of the output
// -----------------------------------------------------------------------------

test("the runner hands progress lines to onProgress and nowhere else", async () => {
  const file = await script(
    [
      `echo "INFO:  creating" >&2`,
      `echo "::memql-progress:: - Creating the cluster" >&2`,
      `echo "::memql-progress:: 2/5 Starting services" >&2`,
      `echo "INFO:  done" >&2`,
      // Unterminated, and progress: it must still be read as progress.
      `printf '%s' "::memql-progress:: 5/5 Starting services" >&2`,
      ENVELOPE,
    ].join("\n"),
  );
  const logged: string[] = [];
  const progress: ProgressLine[] = [];
  const out = await runCapabilityScript({
    scriptPath: file,
    params: {},
    onLog: (line) => logged.push(line),
    onProgress: (p) => progress.push(p),
  });
  assert.deepEqual(logged, ["INFO:  creating", "INFO:  done"]);
  assert.deepEqual(progress, [
    { label: "Creating the cluster" },
    { label: "Starting services", done: 2, total: 5 },
    { label: "Starting services", done: 5, total: 5 },
  ]);
  assert.equal(out.stderr, "INFO:  creating\nINFO:  done\n", "the outcome's output carries no progress line");
  assert.doesNotMatch(out.stderr, /memql-progress/);
});

test("without an onProgress the lines are still kept out of the log", async () => {
  const file = await script([`echo "::memql-progress:: - Seeding secrets" >&2`, `echo "INFO:  x" >&2`, ENVELOPE].join("\n"));
  const logged: string[] = [];
  const out = await runCapabilityScript({ scriptPath: file, params: {}, onLog: (line) => logged.push(line) });
  assert.deepEqual(logged, ["INFO:  x"]);
  assert.equal(out.stderr, "INFO:  x\n");
});

test("ordinary output reaches the log and the outcome exactly as before", async () => {
  const file = await script([`echo "one" >&2`, `printf 'two' >&2`, ENVELOPE].join("\n"));
  const logged: string[] = [];
  const out = await runCapabilityScript({ scriptPath: file, params: {}, onLog: (line) => logged.push(line) });
  assert.deepEqual(logged, ["one", "two"]);
  assert.equal(out.stderr, "one\ntwo");
});

// -----------------------------------------------------------------------------
// the executor turns them into stepPhase events
// -----------------------------------------------------------------------------

function oneStepGraph(): Graph {
  return loadGraph(
    JSON.stringify({
      name: "t",
      kind: "install",
      description: "d",
      steps: [
        {
          id: "clusterUp",
          script: "k3d.up",
          label: "Creating the cluster",
          description: "d",
          readOnly: true,
          elevation: "none",
          verify: { kind: "resultTrue", field: "result.supported" },
        },
      ],
    }),
    "fixture.json",
  );
}

test("a progress line is a stepPhase event INSTEAD of a stepLog", async () => {
  const file = await script(
    [
      `echo "INFO:  hello" >&2`,
      `echo "::memql-progress:: - Installing ArgoCD" >&2`,
      `echo "::memql-progress:: 3/9 Starting services" >&2`,
      ENVELOPE,
    ].join("\n"),
  );
  const events: ExecEvent[] = [];
  const report = await executeGraph({
    graph: oneStepGraph(),
    scriptPath: () => file,
    onEvent: (event) => {
      events.push(event);
    },
  });
  assert.equal(report.ok, true);
  const shape = events
    .filter((e) => e.type === "stepLog" || e.type === "stepPhase")
    .map((e) =>
      e.type === "stepLog"
        ? `log:${e.line}`
        : `phase:${e.label}${e.done !== undefined ? ` ${e.done}/${e.total}` : ""}`,
    );
  assert.deepEqual(shape, ["log:INFO:  hello", "phase:Installing ArgoCD", "phase:Starting services 3/9"]);
  const phase = events.find((e) => e.type === "stepPhase");
  assert.equal(phase?.type === "stepPhase" ? phase.step.id : "", "clusterUp");
});

test("the plan the run announces carries each step's label", async () => {
  const events: ExecEvent[] = [];
  await executeGraph({
    graph: oneStepGraph(),
    scriptPath: () => "/bin/true",
    run: async () => ({
      argv: [],
      exitCode: 0,
      signal: null,
      stdout: "",
      stderr: "",
      envelope: { ok: true, capability: "t", changed: false, result: { supported: true }, error: null },
    }),
    onEvent: (event) => {
      events.push(event);
    },
  });
  const started = events.find((e) => e.type === "runStarted");
  assert.deepEqual(started?.type === "runStarted" ? started.steps : [], [
    { id: "clusterUp", label: "Creating the cluster", description: "d" },
  ]);
});
