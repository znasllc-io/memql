// Taking a local cluster off the machine (memql#3476): the list the operator
// approves, the run that follows, and what the editor owes afterwards.
//
// THE PROPERTIES, one section each:
//
//  1. THE PREVIEW IS THE CONFIRMATION. There is no yes/no box behind it, so the
//     list has to be complete and honest by itself: both kinds in ONE list (no
//     disclosure hiding what stays), every row saying what it will ask of the
//     operator, and nothing in it that the receipt does not record.
//  2. IT RUNS AGAINST A DEAD CLUSTER. An uninstall reverses a receipt. Nothing
//     in this file dials anything, and that is the point -- gating the removal
//     on reachability would strand precisely the machine that needs cleaning.
//  3. A FAILURE NAMES ITS STEP. "The uninstall failed" does not say which
//     artifact is still there; the failing step's id and description do.
//  4. THE FOLLOW-UP IS THREE THINGS. The registry entry, the presence memo and
//     the tree -- and the last two happen whether or not the first one could.
//
// The panel that draws all this imports `vscode` and so cannot be driven here
// (cmd/memql-lsp/vscodeimportrule_test.go). What it renders CAN be: the panel
// calls `renderRemovalPreview(removalPreviewItems(preview))` and adds no
// judgement of its own, so the composition below is the screen.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs/promises";
import { mkdtempSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";

import { renderRemovalPreview, renderToHtml } from "@znasllc-io/memql-view-kit";

import { completeLocalUninstall } from "../src/clusters/registry.js";
import type { ExecEvent } from "../src/install/executor.js";
import { loadGraph, type Graph } from "../src/install/graph.js";
import { removalPreviewItems } from "../src/install/removalPreview.js";
import type { RunScript } from "../src/install/runner.js";
import { previewUninstall, runUninstall, type SessionOptions } from "../src/install/session.js";
import { failureGuidance } from "../src/state/installProgress.js";
import { removalRows, sharedToolRows } from "../src/install/removalPreview.js";
import { uninstallPreviewScreen, uninstalledScreen } from "../src/webview/addClusterScreens.js";
import { UninstallRunState } from "../src/state/uninstallRun.js";

function graph(doc: unknown): Graph {
  return loadGraph(JSON.stringify(doc), "test-fixture");
}

/**
 * A four-step uninstall graph carrying BOTH privileged elevations.
 *
 * Shaped on scripts/install/graph/uninstall.json: the cluster first, then the
 * things that depended on it, with the hosts block declaring `sudo` and the CA
 * declaring `user-trust` -- the two of the shipped graph's seven steps that stop
 * and ask for something outside MemQL's own footprint.
 */
const UNINSTALL_GRAPH: Graph = graph({
  name: "test-uninstall",
  kind: "uninstall",
  steps: [
    {
      id: "removeCluster",
      label: "Working",
      description: "Delete the local k3d cluster.",
      script: "install.removeArtifact",
      reverses: "clusterUp",
      elevation: "none",
      retained: false,
      retainedReason: "",
      shared: false,
      sharedReason: "",
      verify: { kind: "resultTrue", field: "result.removed" },
    },
    {
      id: "removeHostsBlock",
      label: "Working",
      description: "Delete the installer's marked block from the system hosts file.",
      script: "install.removeArtifact",
      reverses: "hostsBlock",
      dependsOn: ["removeCluster"],
      elevation: "sudo",
      retained: false,
      retainedReason: "",
      shared: false,
      sharedReason: "",
      verify: { kind: "resultTrue", field: "result.removed" },
    },
    {
      id: "removeLocalCA",
      label: "Working",
      description: "Uninstall the local mkcert CA from the system trust stores.",
      script: "install.removeArtifact",
      reverses: "localCA",
      dependsOn: ["removeCluster"],
      elevation: "user-trust",
      retained: false,
      retainedReason: "",
      shared: false,
      sharedReason: "",
      verify: { kind: "resultTrue", field: "result.removed" },
    },
    {
      id: "removeToolK3d",
      label: "Working",
      description: "Remove the installed k3d binary.",
      script: "install.removeArtifact",
      reverses: "toolK3d",
      dependsOn: ["removeCluster"],
      elevation: "none",
      retained: false,
      retainedReason: "",
      shared: false,
      sharedReason: "",
      verify: { kind: "resultTrue", field: "result.removed" },
    },
  ],
});

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
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "memql-uninstall-"));
  const file = path.join(dir, "install-receipt.json");
  await fs.writeFile(file, JSON.stringify({ version: 1, entries }, null, 2), "utf8");
  return file;
}

/**
 * The machine the tests describe: a cluster and a k3d binary the installer
 * created, a mkcert CA it merely FOUND -- and no hosts block at all, because
 * that step never ran. Three artifacts, three different verdicts.
 */
function machineReceipt(): Promise<string> {
  return tempReceipt([
    entry("clusterUp", "stack", { cluster: "memql-local" }, false),
    entry("localCA", "mkcertCA", { caroot: "/home/dev/.local/share/mkcert" }, true),
    entry("toolK3d", "binary", { path: "/home/dev/.memql/bin/k3d" }, false),
  ]);
}

function options(over: Partial<SessionOptions> = {}): SessionOptions {
  const dir = mkdtempSync(path.join(os.tmpdir(), "memql-uninstall-opts-"));
  return {
    root: "/nonexistent",
    receiptFile: path.join(dir, "install-receipt.json"),
    skip: new Set<string>(),
    stepParams: {},
    ...over,
  };
}

/**
 * A runner that removes everything it is asked to, unless told to refuse.
 *
 * `refuse` maps an artifact kind to an exit code, which is how a single step is
 * made to fail among siblings that succeed: the runner sees the params, not the
 * step id, and `kind` is the param the receipt puts there.
 */
function remover(refuse: Record<string, number> = {}): { run: RunScript; seen: Record<string, string>[] } {
  const seen: Record<string, string>[] = [];
  const run: RunScript = async ({ params }) => {
    seen.push(params);
    const code = refuse[params.kind ?? ""] ?? 0;
    return {
      argv: [],
      exitCode: code,
      signal: null,
      stdout: "",
      stderr: code === 0 ? "" : "the script said no",
      envelope: {
        ok: code === 0,
        capability: "install.removeArtifact",
        changed: code === 0,
        result: { removed: code === 0, kind: params.kind ?? "" },
        error: code === 0 ? null : { code, message: "the script said no" },
      },
    };
  };
  return { run, seen };
}

// -----------------------------------------------------------------------------
// 1 -- the list the operator confirms
// -----------------------------------------------------------------------------

async function previewHtml(): Promise<string> {
  const { run } = remover();
  const preview = await previewUninstall(options({ receiptFile: await machineReceipt() }), {
    graph: UNINSTALL_GRAPH,
    run,
  });
  return renderToHtml(renderRemovalPreview(removalPreviewItems(preview)));
}

/** The `<li>` for each row, as the browser would see them. */
function rows(html: string): string[] {
  return html
    .split("<li ")
    .slice(1)
    .map((chunk) => chunk.slice(0, chunk.indexOf("</li>")));
}

test("both kinds render in ONE list -- what stays is not hidden behind a disclosure", async () => {
  // The preview IS the confirmation, so the operator is deciding about the
  // whole set. A preserved artifact they expected to see removed is exactly the
  // surprise worth stopping on, and it cannot be a surprise if it is one click
  // away behind a summary.
  const html = await previewHtml();

  assert.equal(html.split("<ul").length - 1, 1, "the two kinds must share one list");
  assert.ok(!html.includes("<details"), "nothing on this list may be hidden behind a disclosure");

  const kinds = rows(html).map((row) => (row.includes('data-kind="preserved"') ? "preserved" : "removed"));
  assert.deepEqual(kinds, ["removed", "removed", "preserved"]);
});

test("the privileged steps are marked, and the unprivileged ones say so too", async () => {
  const html = await previewHtml();

  // The CA is the one the operator most needs to see coming: it is preserved
  // here, and it STILL carries what it would ask for.
  const ca = rows(html).find((row) => row.includes("removeLocalCA"));
  assert.ok(ca !== undefined);
  assert.match(ca, /data-elevation="user-trust"/);

  for (const row of rows(html)) {
    assert.match(
      row,
      /data-elevation="(none|sudo|user-trust)"/,
      `a row with no elevation attribute renders no marker, which reads as "this asks for nothing": ${row}`,
    );
  }
});

test("nothing absent from the receipt appears in the list", async () => {
  // The graph has four steps and this machine has three artifacts. An
  // itemization built from the GRAPH would offer to edit a hosts file this
  // install never touched -- and the operator would approve it.
  const html = await previewHtml();

  assert.equal(rows(html).length, 3);
  assert.ok(!html.includes("removeHostsBlock"), "a step with no receipt entry has no artifact to list");
  assert.ok(!html.toLowerCase().includes("hosts"), "nothing on this list may name an artifact the receipt does not record");
});

test("an empty receipt says so rather than rendering a blank panel", async () => {
  const { run } = remover();
  const preview = await previewUninstall(options({ receiptFile: await tempReceipt([]) }), {
    graph: UNINSTALL_GRAPH,
    run,
  });
  const html = renderToHtml(renderRemovalPreview(removalPreviewItems(preview)));

  assert.match(html, /remove nothing/);
  assert.ok(!html.includes("<li "), "an empty receipt has no rows");
});

// -----------------------------------------------------------------------------
// 1b -- THE CLUSTER NOTHING RECORDED (memql#5118, D8)
// -----------------------------------------------------------------------------
//
// `present-unreceipted` is a live k3d cluster named `memql` that this installer
// did not create, and D8 gives it two acts: adopt, or delete. The delete had no
// path -- both session entry points require a receipt, and by definition there
// is none -- so the card was offered and the screen behind it read "no receipt
// at ~/.memql/install-receipt.json". These four cases are the act, and the
// refusal it must not have weakened.

test("with no receipt and a SEEN cluster, the preview plans exactly that one removal", async () => {
  const { run } = remover();
  const preview = await previewUninstall(
    // A receiptFile that does not exist, which is the whole premise.
    options({ unreceiptedCluster: "memql" }),
    { graph: UNINSTALL_GRAPH, run },
  );

  // ONE ARTIFACT, AND IT IS THE ONE THAT WAS OBSERVED. Every other step finds
  // no entry and skips satisfied -- the same answer it gives for an install
  // that stopped before it, and the reason a synthesized receipt cannot become
  // a licence to remove a hosts block nothing has evidence for.
  const planned = preview.steps.filter((step) => step.action === "run");
  assert.deepEqual(planned.map((step) => step.id), ["removeCluster"]);
  assert.equal(planned[0]?.params["cluster"], "memql");
  assert.equal(planned[0]?.params["kind"], "stack");

  // PRE-EXISTING, WHICH IS THE LITERAL TRUTH AND THE SAFETY. It is what makes
  // remove-artifact.sh refuse until the box is ticked and the phrase typed, so
  // the one destructive act stays destructive-on-purpose rather than becoming
  // an ordinary removal.
  assert.equal(planned[0]?.params["pre-existing"], "true");
  assert.deepEqual(preview.removals, []);
  assert.deepEqual(preview.preserved.map((step) => step.id), ["removeCluster"]);
});

test("with no receipt and NO seen cluster, it still refuses -- the general rule is untouched", async () => {
  // THE NEGATIVE CONTROL. `requireReceipt` refuses because an uninstall
  // reverses what an install RECORDED; the new path is a named observation, not
  // a fallback. If this stops throwing, the wizard has started guessing at
  // hosts files and trust stores again.
  const { run } = remover();
  await assert.rejects(
    () => previewUninstall(options(), { graph: UNINSTALL_GRAPH, run }),
    /no receipt at/,
  );
});

test("a REAL receipt outranks the observation", async () => {
  // The two can only disagree if a caller set the field in a state where the
  // verdict says it should not have, and the record describes more of the
  // machine than a listing can. `machineReceipt` has three artifacts.
  const { run } = remover();
  const preview = await previewUninstall(
    options({ receiptFile: await machineReceipt(), unreceiptedCluster: "memql" }),
    { graph: UNINSTALL_GRAPH, run },
  );
  assert.equal(preview.steps.filter((step) => step.action === "run").length, 3);
});

test("the phrase deletes the cluster nothing recorded, and without it nothing goes", async () => {
  // END TO END on the act D9 describes: the same run, twice, differing only by
  // the param a typed phrase produces.
  const refused = remover({ stack: 3 });
  const kept = await runUninstall(options({ unreceiptedCluster: "memql" }), {
    graph: UNINSTALL_GRAPH,
    run: refused.run,
  });
  assert.equal(kept.outcomes.find((o) => o.id === "removeCluster")?.status, "preserved");
  assert.equal(kept.keptCluster, true, "no phrase, so the cluster stays and the records stay with it");

  const allowed = remover();
  const removed = await runUninstall(
    options({
      unreceiptedCluster: "memql",
      stepParams: { removeCluster: { confirm: "delete-memql-data" } },
    }),
    { graph: UNINSTALL_GRAPH, run: allowed.run },
  );
  assert.equal(removed.outcomes.find((o) => o.id === "removeCluster")?.status, "ok");
  assert.equal(removed.keptCluster, false, "the cluster is gone, so the records go too");
  // The phrase reached the script rather than being consumed by the wizard.
  const invocation = allowed.seen.find((params) => params["kind"] === "stack");
  assert.equal(invocation?.["confirm"], "delete-memql-data");
});

// -----------------------------------------------------------------------------
// 2 -- the run
// -----------------------------------------------------------------------------

test("the uninstall runs against a DEAD cluster -- nothing is dialled", async () => {
  // There is no endpoint, no probe and no connection anywhere in this test, and
  // the removal still reports every artifact gone. That is the property: an
  // uninstall reverses a receipt, so a cluster that stopped answering (the state
  // this page is most often opened in) is removable exactly like a healthy one.
  const { run, seen } = remover();
  const report = await runUninstall(options({ receiptFile: await machineReceipt() }), {
    graph: UNINSTALL_GRAPH,
    run,
  });

  assert.equal(report.ok, true);
  assert.deepEqual(
    report.outcomes.map((o) => `${o.id}:${o.status}`),
    ["removeCluster:ok", "removeHostsBlock:skipped", "removeLocalCA:ok", "removeToolK3d:ok"],
  );
  // Every flag came off the receipt, including the verdict that keeps a
  // developer's own CA: `--pre-existing=true` is an unconditional refusal
  // inside remove-artifact.sh.
  const ca = seen.find((params) => params.kind === "mkcertCA");
  assert.equal(ca?.["pre-existing"], "true");
  assert.equal(ca?.caroot, "/home/dev/.local/share/mkcert");
});

test("a failed step is reported BY NAME, and the receipt is untouched for a retry", async () => {
  const receiptFile = await tempReceipt([
    entry("clusterUp", "stack", { cluster: "memql-local" }, false),
    entry("hostsBlock", "hostsEntries", { hostsFile: "/etc/hosts" }, false),
    entry("toolK3d", "binary", { path: "/home/dev/.memql/bin/k3d" }, false),
  ]);
  const before = await fs.readFile(receiptFile, "utf8");

  // Exit 5 -- the step ran and did not succeed. Not 3, which the executor reads
  // as a refusal and records as `preserved`.
  const { run } = remover({ hostsEntries: 5 });
  const state = new UninstallRunState();
  state.begin();
  const report = await runUninstall(options({ receiptFile }), {
    graph: UNINSTALL_GRAPH,
    run,
    onEvent: (event) => state.apply(event),
  });
  state.finish(report);

  assert.equal(report.ok, false);
  assert.equal(state.phase, "failed");
  assert.equal(state.failure?.id, "removeHostsBlock");
  assert.equal(
    state.failure?.description,
    "Delete the installer's marked block from the system hosts file.",
    "the screen names the step, because 'the uninstall failed' does not say which artifact is still there",
  );
  assert.equal(state.failure?.exitCode, 5);

  // AN UNINSTALL NEVER REWRITES THE RECEIPT. That is what makes the retry
  // meaningful: the record still describes the same machine, so running it
  // again repeats exactly the list the operator already approved.
  assert.equal(await fs.readFile(receiptFile, "utf8"), before);
});

test("a step whose sibling failed still runs -- one refusal is not a stopped uninstall", async () => {
  const receiptFile = await tempReceipt([
    entry("clusterUp", "stack", { cluster: "memql-local" }, false),
    entry("hostsBlock", "hostsEntries", { hostsFile: "/etc/hosts" }, false),
    entry("toolK3d", "binary", { path: "/home/dev/.memql/bin/k3d" }, false),
  ]);
  const { run } = remover({ hostsEntries: 5 });
  const report = await runUninstall(options({ receiptFile }), { graph: UNINSTALL_GRAPH, run });

  const byId = new Map(report.outcomes.map((o) => [o.id, o.status]));
  assert.equal(byId.get("removeHostsBlock"), "failed");
  assert.equal(byId.get("removeToolK3d"), "ok", "the binary is independent of the hosts block");
});

test("the run refuses without a receipt rather than guessing at the machine", async () => {
  const { run } = remover();
  await assert.rejects(() => runUninstall(options(), { graph: UNINSTALL_GRAPH, run }), /receipt/);
});

test("an unsupported platform refuses uninstall before any removal, and the receipt stays", async () => {
  const receiptFile = await machineReceipt();
  const before = await fs.readFile(receiptFile, "utf8");
  const inner = remover();
  const darwin =
    "unsupported platform darwin/amd64: the local cluster installer targets linux/amd64, darwin/arm64";
  const run: RunScript = async (inv) => {
    if ((inv.capability ?? inv.scriptPath).includes("detect")) {
      return {
        argv: [],
        exitCode: 3,
        signal: null,
        stdout: "",
        stderr: darwin,
        envelope: {
          ok: false,
          capability: "install.detect",
          changed: false,
          result: { os: "darwin", arch: "arm64", supported: false },
          error: { code: 3, message: darwin },
        },
      };
    }
    return inner.run(inv);
  };
  const report = await runUninstall(options({ receiptFile }), { graph: UNINSTALL_GRAPH, run });
  assert.equal(report.ok, false);
  assert.equal(report.outcomes.map((o) => o.id).join(","), "detect");
  assert.equal(inner.seen.length, 0, "removeCluster / removeHostsBlock / removeLocalCA must not run");
  assert.equal(await fs.readFile(receiptFile, "utf8"), before);
  const copy = failureGuidance(report.outcomes[0]?.exitCode ?? null, "", report.outcomes[0]?.reason ?? "");
  assert.match(copy.advice, /linux\/amd64/);
  assert.equal(copy.retryable, false);
});

// -----------------------------------------------------------------------------
// 3 -- the state the screen reads
// -----------------------------------------------------------------------------

function started(id: string, description: string): ExecEvent {
  return { type: "stepStarted", step: stepOf(id, description), params: {} };
}

function finished(id: string, description: string, status: "ok" | "failed" | "preserved", exitCode: number): ExecEvent {
  return {
    type: "stepFinished",
    step: stepOf(id, description),
    outcome: {
      id,
      script: "install.removeArtifact",
      status,
      exitCode,
      envelope: null,
      verified: status === "ok",
      preExisting: status === "preserved",
      params: {},
      reason: status === "ok" ? undefined : "the script said no",
      startedAt: "",
      finishedAt: "",
    },
  };
}

function stepOf(id: string, description: string): Graph["steps"][number] {
  return {
    id,
    script: "install.removeArtifact",
    label: "Working",
    description,
    elevation: "none",
    retained: false,
    retainedReason: "",
    shared: false,
    sharedReason: "",
    verify: { kind: "resultTrue", field: "result.removed" },
  };
}

test("the phase starts at the list and only leaves it when the operator says so", () => {
  const state = new UninstallRunState();
  assert.equal(state.phase, "preview");
  state.begin();
  assert.equal(state.phase, "running");
});

test("a failure mid-run does not settle the phase -- the executor is still working", () => {
  // Every wave that does not depend on the failure still runs, so a phase that
  // went terminal here would report a finished uninstall while steps were
  // executing.
  const state = new UninstallRunState();
  state.begin();
  state.apply(finished("removeHostsBlock", "hosts", "failed", 5));
  assert.equal(state.phase, "running");
  assert.equal(state.failure?.id, "removeHostsBlock");
});

test("the FIRST failure is the one reported", () => {
  // The later ones may be consequences of it, and the operator is being told
  // which step to act on.
  const state = new UninstallRunState();
  state.begin();
  state.apply(finished("removeLocalCA", "ca", "failed", 5));
  state.apply(finished("removeToolMkcert", "mkcert", "failed", 5));
  assert.equal(state.failure?.id, "removeLocalCA");
});

test("a cancelled run is STOPPED, never failed", () => {
  // ExecutionReport keeps `cancelled` separate from `ok` for this reason: the
  // operator intervened and nothing broke. Telling them a step failed would
  // send them looking for a fault that is not there.
  const state = new UninstallRunState();
  state.begin();
  state.finish({ ok: true, cancelled: true });
  assert.equal(state.phase, "stopped");

  const clean = new UninstallRunState();
  clean.begin();
  clean.finish({ ok: true });
  assert.equal(clean.phase, "removed");
});

test("a run that never started reports its sentence rather than an empty list", () => {
  // The case that matters is a missing receipt: previewUninstall and
  // runUninstall both refuse, and rendering that as "this would remove nothing"
  // would be the claim an EMPTY receipt makes. The two are opposite news.
  const state = new UninstallRunState();
  state.begin();
  state.fail("no receipt at /home/dev/.memql/install-receipt.json");
  assert.equal(state.phase, "failed");
  assert.equal(state.failure, undefined);
  assert.match(state.problem, /no receipt/);
});

test("the log accumulates per step, and a description arriving late is kept", () => {
  const state = new UninstallRunState();
  state.begin();
  state.apply(started("removeCluster", ""));
  state.apply({ type: "stepLog", step: stepOf("removeCluster", ""), line: "deleting" });
  state.apply({ type: "stepLog", step: stepOf("removeCluster", ""), line: "gone" });
  state.apply(finished("removeCluster", "Delete the local k3d cluster.", "ok", 0));

  const [row] = state.steps;
  assert.equal(row.log, "deleting\ngone");
  assert.equal(row.description, "Delete the local k3d cluster.");
  assert.equal(row.state, "done");
});

test("an unfamiliar event is ignored rather than abandoning the run", () => {
  const state = new UninstallRunState();
  state.begin();
  state.apply({ type: "waveStarted", index: 0, ids: ["removeCluster"] });
  assert.deepEqual(state.steps, []);
  assert.equal(state.phase, "running");
});

test("reset returns to the list with nothing carried over", () => {
  const state = new UninstallRunState();
  state.begin();
  state.apply(finished("removeCluster", "cluster", "failed", 5));
  state.finish({ ok: false });
  state.noteFollowUpProblem("the entry stayed");
  state.reset();

  assert.equal(state.phase, "preview");
  assert.deepEqual(state.steps, []);
  assert.equal(state.failure, undefined);
  assert.equal(state.problem, "");
  assert.equal(state.followUpProblem, "");
});

test("a preserved step is its own state, never a failure", () => {
  // It is the uninstall keeping something the operator already had, and the
  // whole two-tier model rests on the two reading differently.
  const state = new UninstallRunState();
  state.begin();
  state.apply(finished("removeLocalCA", "ca", "preserved", 3));
  state.finish({ ok: true });

  assert.equal(state.phase, "removed");
  assert.equal(state.failure, undefined);
  assert.equal(state.steps[0].state, "preserved");
});

// -----------------------------------------------------------------------------
// 4 -- what the editor owes once the machine is clean
// -----------------------------------------------------------------------------

test("the completion trio all fire: the entry goes, the memo drops, the tree repaints", async () => {
  const order: string[] = [];
  const problem = await completeLocalUninstall({
    keptCluster: false,
    clusterName: "local",
    removeEntry: async (name) => void order.push(`remove:${name}`),
    invalidatePresence: () => order.push("invalidate"),
    refreshTree: () => order.push("refresh"),
    deleteReceipt: async () => void order.push("deleteReceipt"),
  });

  assert.equal(problem, "");
  // The entry FIRST: it is the only one that can fail, so it runs before
  // anything says the surface is up to date. The RECEIPT goes with it
  // (memql#3544) -- it is the record of the install that has just been
  // reversed, and detectPresence reads it, so leaving it behind leaves the
  // wizard offering to repair and uninstall a cluster that is gone.
  assert.deepEqual(order, ["remove:local", "deleteReceipt", "invalidate", "refresh"]);
});

test("a registry write that fails still drops the memo and repaints the tree", async () => {
  // The MACHINE changed either way. Leaving a thirty-second memo claiming a
  // healthy local cluster because a YAML write failed would compound one
  // problem with a second, unrelated lie.
  const order: string[] = [];
  const problem = await completeLocalUninstall({
    keptCluster: false,
    clusterName: "local",
    removeEntry: async () => {
      throw new Error("clusters.yaml is read-only");
    },
    invalidatePresence: () => order.push("invalidate"),
    refreshTree: () => order.push("refresh"),
    deleteReceipt: async () => void order.push("deleteReceipt"),
  });

  // The receipt is removed even though the registry write failed: they are
  // independent records of the same departed cluster, and the receipt is the
  // one that decides what the wizard offers next.
  assert.deepEqual(order, ["deleteReceipt", "invalidate", "refresh"]);
  assert.match(problem, /clusters\.yaml is read-only/);
  assert.match(problem, /off this machine/, "the uninstall SUCCEEDED -- only the bookkeeping did not");
});

// -----------------------------------------------------------------------------
// KEPT IS A RESULT, AND THE RECORDS GO WITH THE ARTIFACTS (memql#5118, D8)
// -----------------------------------------------------------------------------

test("a run that KEPT something leaves the receipt and the registry row alone", async () => {
  // THE BUG THIS CLOSES, end to end. remove-artifact.sh refuses to delete a
  // pre-existing k3d cluster; executor.ts classes that `preserved`; ok ignores
  // preservations. So the wizard deleted the receipt that described a cluster
  // still running, presence read `absent` because nothing asked k3d, Install
  // was offered again, and it adopted the same database -- after telling the
  // operator everything had been taken back.
  const order: string[] = [];
  const problem = await completeLocalUninstall({
    keptCluster: true,
    clusterName: "local",
    removeEntry: async (name) => void order.push(`remove:${name}`),
    invalidatePresence: () => order.push("invalidate"),
    refreshTree: () => order.push("refresh"),
    deleteReceipt: async () => void order.push("deleteReceipt"),
  });

  assert.equal(problem, "");
  // NEITHER RECORD IS TOUCHED. Both name a cluster that is still here.
  assert.ok(!order.includes("deleteReceipt"), `the receipt was deleted: ${order.join(", ")}`);
  assert.ok(!order.includes("remove:local"), `the registry row was dropped: ${order.join(", ")}`);
  // The memo and the tree still refresh: the machine HAS changed, and a
  // thirty-second-old verdict about it is stale either way.
  assert.deepEqual(order, ["invalidate", "refresh"]);
});

test("a run that kept a CHECKOUT but not the cluster still takes both records", async () => {
  // THE NARROW PREDICATE, ASSERTED FROM THE OTHER SIDE. Both records name a
  // CLUSTER -- clusters.yaml says what to connect to, the receipt is what
  // `detectPresence` reads -- so a preserved checkout must not keep them. It
  // reaches here as `keptCluster: false` even though the run left something on
  // the machine and the closing sentence says so; reading one field for both
  // questions strands a receipt describing a cluster that is gone, which is
  // memql#3544 re-opened and which nothing in the extension can clear.
  const order: string[] = [];
  await completeLocalUninstall({
    keptCluster: false,
    clusterName: "local",
    removeEntry: async (name) => void order.push(`remove:${name}`),
    invalidatePresence: () => order.push("invalidate"),
    refreshTree: () => order.push("refresh"),
    deleteReceipt: async () => void order.push("deleteReceipt"),
  });
  assert.deepEqual(order, ["remove:local", "deleteReceipt", "invalidate", "refresh"]);
});

test("a run that kept NOTHING takes both records, exactly as before", async () => {
  const order: string[] = [];
  await completeLocalUninstall({
    keptCluster: false,
    clusterName: "local",
    removeEntry: async (name) => void order.push(`remove:${name}`),
    invalidatePresence: () => order.push("invalidate"),
    refreshTree: () => order.push("refresh"),
    deleteReceipt: async () => void order.push("deleteReceipt"),
  });
  assert.deepEqual(order, ["remove:local", "deleteReceipt", "invalidate", "refresh"]);
});

test("the preview of a cluster with no install record says what it is, and claims no record", async () => {
  // THE SCREEN IMMEDIATELY BEFORE THE ONE DESTRUCTIVE ACT. A `make up`
  // cluster has no receipt; the preview plans the one removal the observation
  // supports -- the cluster, KEPT -- and the page must neither reassure with a
  // record that does not exist nor offer an Uninstall that would do nothing.
  const preview = await previewUninstall(
    { ...options({ receiptFile: "/nonexistent/receipt.json" }), unreceiptedCluster: "memql" },
    { graph: UNINSTALL_GRAPH },
  );
  const screen = (deleteData: { on: boolean; phrase: string }) => {
    const parts = uninstallPreviewScreen({
      loading: false,
      rows: removalRows(preview),
      sharedTools: sharedToolRows(preview),
      chosen: new Set(),
      deleteData,
      clusterName: "memql",
    });
    return parts.head + parts.body + parts.actions;
  };
  const off = screen({ on: false, phrase: "" });
  assert.doesNotMatch(off, /receipt|artifact/i, "the page names an internal the reader cannot act on");
  assert.match(off, /The cluster<\/span><span class="ac-row-detail">Kept unless you delete its data/);
  assert.doesNotMatch(off, /data-act="uninstallStart"/, "Uninstall is offered for a run that would remove nothing");

  // The switch and the phrase are what make it a removal, and only both.
  assert.doesNotMatch(screen({ on: true, phrase: "delete memql" }), /data-act="uninstallStart"/);
  const confirmed = screen({ on: true, phrase: "delete memql data" });
  assert.match(confirmed, /data-act="uninstallStart"[^>]*>Uninstall and delete data</);
  // The list says what goes -- the cluster and every database in it -- ONCE:
  // under "Will be removed", not again as a sentence over the button.
  assert.match(confirmed, /Will be removed<\/h2><ul class="ac-list" role="list"><li class="ac-row"><span class="ac-row-name">The cluster<\/span><span class="ac-row-detail">memql, and every database in it/);
  assert.doesNotMatch(confirmed, /mq-actbar-confirm/, "the deletion said a second time over the bar");
});

test("the finished uninstall says whether anything was kept", () => {
  const clean = uninstalledScreen({ removed: 4, kept: 0, followUpProblem: "", logsOpen: false });
  const kept = uninstalledScreen({ removed: 3, kept: 1, followUpProblem: "", logsOpen: false });
  assert.notEqual(clean.body, kept.body);
  assert.match(clean.body, /Nothing left behind/);
  assert.match(clean.body, /4 removed</);
  // The claim that was FALSE on a machine where the cluster is still running.
  assert.doesNotMatch(kept.body, /Nothing left behind/);
  assert.match(kept.body, /Kept what was here before MemQL/);
  assert.match(kept.body, /3 removed · 1 kept/);
});

test("no registered cluster is an ordinary case, not a problem", async () => {
  // An operator can install a cluster and never add it to clusters.yaml. There
  // is then no entry to remove, and nothing to report.
  for (const clusterName of [undefined, ""]) {
    const order: string[] = [];
    const problem = await completeLocalUninstall({
      keptCluster: false,
      clusterName,
      removeEntry: async (name) => void order.push(`remove:${name}`),
      invalidatePresence: () => order.push("invalidate"),
      refreshTree: () => order.push("refresh"),
      deleteReceipt: async () => void order.push("deleteReceipt"),
    });
    assert.equal(problem, "");
    assert.deepEqual(order, ["deleteReceipt", "invalidate", "refresh"]);
  }
});

test("a receipt that cannot be removed is reported, because the wizard will keep lying", async () => {
  // The failure worth a sentence of its own. Every artifact is gone, but the
  // record of them is not -- and detectPresence reads that record, so the
  // wizard goes on withholding the Install card and offering Repair for a
  // cluster that is not there. An operator told only "uninstalled" would have
  // no way to connect the two.
  const order: string[] = [];
  const problem = await completeLocalUninstall({
    keptCluster: false,
    clusterName: "local",
    removeEntry: async (name) => void order.push(`remove:${name}`),
    invalidatePresence: () => order.push("invalidate"),
    refreshTree: () => order.push("refresh"),
    deleteReceipt: async () => {
      throw new Error("install-receipt.json is read-only");
    },
  });

  assert.deepEqual(order, ["remove:local", "invalidate", "refresh"]);
  assert.match(problem, /install-receipt\.json is read-only/);
  assert.match(problem, /go on reporting a cluster/, "it must say what the operator will SEE");
  assert.match(problem, /off this machine/, "the removal itself succeeded");
});
