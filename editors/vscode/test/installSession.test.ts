// The callable run seam: install, uninstall, and the uninstall preview.
//
// Until now the orchestration lived inside cli.ts, reachable only by spawning
// `npm run install-cli`. That is why the "+" button reported a command for the
// operator to copy instead of running one -- there was no function to call.
//
// The property these cases exist to protect is that there is ONE run path. The
// CLI and the webview must not be able to diverge into "it worked from the
// terminal but not from the editor", so cli.ts is reduced to argv parsing and
// presentation over exactly these functions, and the existing CLI tests
// continue to pass unchanged as the evidence that the extraction preserved
// behaviour.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs/promises";
import { mkdtempSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";

import { loadGraph, type Graph } from "../src/install/graph.js";
import type { RunScript } from "../src/install/runner.js";
import {
  previewUninstall,
  runInstall,
  runUninstall,
  type SessionOptions,
} from "../src/install/session.js";
import type { ExecEvent } from "../src/install/executor.js";

/** loadGraph parses a document, so fixtures are written as one. */
function graph(doc: unknown): Graph {
  return loadGraph(JSON.stringify(doc), "test-fixture");
}

// A two-step install graph: one tool, then something that depends on it.
const INSTALL_GRAPH: Graph = graph({
  name: "test-install",
  kind: "install",
  steps: [
    {
      id: "binary",
      description: "place a tool",
      script: "install.binary",
      elevation: "none",
      receipt: "binary",
      preExistingPath: "none",
      verify: { kind: "resultTrue", field: "result.installed" },
    },
    {
      id: "cluster",
      description: "create the cluster",
      script: "k3d.up",
      dependsOn: ["binary"],
      elevation: "none",
      receipt: "stack",
      preExistingPath: "none",
      verify: { kind: "resultTrue", field: "result.installed" },
    },
  ],
});

const UNINSTALL_GRAPH: Graph = graph({
  name: "test-uninstall",
  kind: "uninstall",
  steps: [
    {
      id: "removeCluster",
      description: "remove the cluster",
      script: "install.removeArtifact",
      reverses: "cluster",
      elevation: "none",
      verify: { kind: "resultTrue", field: "result.removed" },
    },
    {
      id: "removeBinary",
      description: "remove the tool",
      script: "install.removeArtifact",
      reverses: "binary",
      dependsOn: ["removeCluster"],
      elevation: "none",
      verify: { kind: "resultTrue", field: "result.removed" },
    },
  ],
});

/** A runner that succeeds, recording what it was asked to do. */
function okRunner(seen: string[] = []): { run: RunScript; seen: string[] } {
  const run: RunScript = async ({ params }) => {
    seen.push(JSON.stringify(params));
    return {
      argv: [],
      exitCode: 0,
      signal: null,
      stdout: "",
      stderr: "",
      envelope: {
        ok: true,
        capability: "t",
        changed: true,
        // Satisfies both graphs' verify predicates, and carries the artifact
        // fields the receipt reads back for a removal.
        result: { installed: true, removed: true, path: "/tmp/k3d", cluster: "memql" },
        error: null,
      },
    };
  };
  return { run, seen };
}

/** One receipt entry in the shape receipt.ts reads back. */
function entry(
  stepId: string,
  receipt: string,
  result: Record<string, unknown>,
  preExisting: boolean,
): unknown {
  return {
    stepId,
    script: "install.test",
    receipt,
    preExisting,
    params: {},
    result,
    changed: true,
    recordedAt: "2026-08-10T00:00:00Z",
  };
}

async function tempReceipt(entries: unknown[]): Promise<string> {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "memql-session-"));
  const file = path.join(dir, "install-receipt.json");
  await fs.writeFile(file, JSON.stringify({ version: 1, entries }, null, 2), "utf8");
  return file;
}

function options(over: Partial<SessionOptions> = {}): SessionOptions {
  // A REAL receipt path per call. An install records every executed step as it
  // finishes, so a path that cannot be written turns each of these into a test
  // of mkdir rather than of the session.
  const dir = mkdtempSync(path.join(os.tmpdir(), "memql-session-opts-"));
  return {
    root: "/nonexistent",
    receiptFile: path.join(dir, "install-receipt.json"),
    skip: new Set<string>(),
    provider: "anthropic",
    stepParams: {},
    ...over,
  };
}

// -----------------------------------------------------------------------------
// running
// -----------------------------------------------------------------------------

test("runInstall walks the graph and reports every step", async () => {
  const { run } = okRunner();
  const report = await runInstall(options(), { graph: INSTALL_GRAPH, run });

  assert.equal(report.ok, true);
  assert.deepEqual(
    report.outcomes.map((o) => `${o.id}:${o.status}`),
    ["binary:ok", "cluster:ok"],
  );
});

test("runInstall emits the events a progress view needs, without knowing about one", async () => {
  const { run } = okRunner();
  const events: ExecEvent[] = [];
  await runInstall(options(), { graph: INSTALL_GRAPH, run, onEvent: (e) => void events.push(e) });

  // The union is the executor's own, forwarded rather than re-invented: it
  // already carries status, exit code, envelope and the verify verdict per
  // step, and a parallel event type would be a second thing to keep in sync.
  const started = events.filter((e) => e.type === "stepStarted").map((e) => e.step.id);
  const finished = events.filter((e) => e.type === "stepFinished").map((e) => e.step.id);
  assert.deepEqual(started, ["binary", "cluster"]);
  assert.deepEqual(finished, ["binary", "cluster"]);
});

test("run-time params reach the steps that declare them", async () => {
  const { run, seen } = okRunner();
  await runInstall(options({ toolDir: "/opt/memql/bin" }), { graph: INSTALL_GRAPH, run });
  assert.ok(
    seen.some((p) => p.includes("/opt/memql/bin")),
    `the tool directory never reached install.binary: ${seen.join(" | ")}`,
  );
});

test("runUninstall refuses without a receipt rather than guessing", async () => {
  const { run } = okRunner();
  await assert.rejects(
    () => runUninstall(options(), { graph: UNINSTALL_GRAPH, run }),
    /receipt/,
    "an uninstall with no record of the install would be guessing at the operator's machine",
  );
});

test("runUninstall removes what the receipt recorded", async () => {
  const receiptFile = await tempReceipt([
    entry("cluster", "stack", { cluster: "memql" }, false),
    entry("binary", "binary", { path: "/tmp/k3d" }, false),
  ]);
  const { run } = okRunner();
  const report = await runUninstall(options({ receiptFile }), { graph: UNINSTALL_GRAPH, run });

  assert.equal(report.ok, true);
  assert.deepEqual(
    report.outcomes.map((o) => o.status),
    ["ok", "ok"],
  );
});

// -----------------------------------------------------------------------------
// cancellation
// -----------------------------------------------------------------------------

test("cancelling stops the run and says so", async () => {
  const controller = new AbortController();
  const seen: string[] = [];
  const run: RunScript = async ({ params }) => {
    seen.push(JSON.stringify(params));
    // Abort while the first wave is in flight: the second must not start.
    controller.abort();
    return {
      argv: [],
      exitCode: 0,
      signal: null,
      stdout: "",
      stderr: "",
      envelope: {
        ok: true,
        capability: "t",
        changed: true,
        result: { installed: true, path: "/tmp/k3d", cluster: "memql" },
        error: null,
      },
    };
  };

  const report = await runInstall(options(), {
    graph: INSTALL_GRAPH,
    run,
    signal: controller.signal,
  });

  assert.equal(report.cancelled, true, "a cancelled run must say it was cancelled");
  assert.deepEqual(
    report.outcomes.map((o) => o.id),
    ["binary"],
    "the wave after the cancellation must not run",
  );
});

test("a cancelled run leaves a receipt that still describes what happened", async () => {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "memql-session-cancel-"));
  const receiptFile = path.join(dir, "install-receipt.json");
  const controller = new AbortController();
  const run: RunScript = async () => {
    controller.abort();
    return {
      argv: [],
      exitCode: 0,
      signal: null,
      stdout: "",
      stderr: "",
      envelope: {
        ok: true,
        capability: "t",
        changed: true,
        result: { installed: true, path: "/tmp/k3d" },
        error: null,
      },
    };
  };

  await runInstall(options({ receiptFile }), {
    graph: INSTALL_GRAPH,
    run,
    signal: controller.signal,
  });

  // This is what makes cancel safe to offer at all: the step that DID run is
  // recorded, so the uninstall can still take it back. A cancel that discarded
  // the receipt would strand whatever the run had already created.
  const raw = JSON.parse(await fs.readFile(receiptFile, "utf8")) as {
    entries: { stepId: string }[];
  };
  assert.deepEqual(
    raw.entries.map((e) => e.stepId),
    ["binary"],
  );
});

// -----------------------------------------------------------------------------
// the uninstall preview
// -----------------------------------------------------------------------------

test("previewUninstall itemizes removals without running anything", async () => {
  const receiptFile = await tempReceipt([
    entry("cluster", "stack", { cluster: "memql" }, false),
    entry("binary", "binary", { path: "/tmp/k3d" }, false),
  ]);
  let ran = false;
  const run: RunScript = async () => {
    ran = true;
    throw new Error("preview must not execute anything");
  };

  const preview = await previewUninstall(options({ receiptFile }), { graph: UNINSTALL_GRAPH, run });

  assert.equal(ran, false, "a preview that runs the graph is not a preview");
  assert.deepEqual(
    preview.removals.map((s) => s.id),
    ["removeCluster", "removeBinary"],
  );
  assert.deepEqual(preview.preserved, []);
});

test("previewUninstall reports a pre-existing artifact as PRESERVED, not removed", async () => {
  // The two-tier model (design D6). A k3d cluster the developer already had is
  // kept, and the preview has to say so BEFORE the operator confirms -- an
  // itemized list that quietly included it would be asking consent for
  // something that is not going to happen, and a list that omitted it would
  // hide that the uninstall leaves something behind.
  const receiptFile = await tempReceipt([
    entry("cluster", "stack", { cluster: "mine" }, true),
    entry("binary", "binary", { path: "/tmp/k3d" }, false),
  ]);
  const { run } = okRunner();

  const preview = await previewUninstall(options({ receiptFile }), { graph: UNINSTALL_GRAPH, run });

  assert.deepEqual(
    preview.preserved.map((s) => s.id),
    ["removeCluster"],
  );
  assert.deepEqual(
    preview.removals.map((s) => s.id),
    ["removeBinary"],
  );
});

test("previewUninstall skips a step whose install left no receipt entry", async () => {
  const receiptFile = await tempReceipt([
    entry("binary", "binary", { path: "/tmp/k3d" }, false),
  ]);
  const { run } = okRunner();

  const preview = await previewUninstall(options({ receiptFile }), { graph: UNINSTALL_GRAPH, run });

  const cluster = preview.steps.find((s) => s.id === "removeCluster");
  assert.equal(cluster?.action, "skip");
  assert.match(cluster?.reason ?? "", /nothing to remove/);
  assert.ok(
    !preview.removals.some((s) => s.id === "removeCluster"),
    "a step with nothing to remove must not appear in the list of what will go",
  );
});

test("previewUninstall refuses without a receipt", async () => {
  const { run } = okRunner();
  await assert.rejects(() => previewUninstall(options(), { graph: UNINSTALL_GRAPH, run }), /receipt/);
});
