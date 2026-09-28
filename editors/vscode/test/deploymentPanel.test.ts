// The cluster page, driven end to end through the webview stub.
//
// What only the PANEL can get wrong, because it is where the reads, the run
// slot and the page's messages meet:
//
//   - a page opened (or a run's row clicked) while a run is going shows that
//     run live, with Cancel, and nothing that starts a second run;
//   - closing the tab leaves the run going;
//   - a read that fails is said, never an endless skeleton;
//   - Sign in is THE sign-in command, with the cluster named;
//   - a rollback goes to the run on screen, and a promote names its rollout;
//   - a typed confirmation that does not match changes nothing, and says so;
//   - the page re-reads when the connection changes.
//
// The stub never sends `ready`, so every render is a new document and the
// assertions read `html`, exactly as LiveView documents for a page not yet up.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";

import type { ExtensionContext } from "vscode";

import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { CODE_UNAUTHENTICATED, DeployControlError } from "@znasllc-io/memql-sdk-core/deploy";

import { roleVisibility } from "../src/deploy/actions.js";
import type { DeployControlPort } from "../src/deploy/controller.js";
import { LocalRuns } from "../src/deploy/localRun.js";
import type { PresenceResult } from "../src/clusters/presence.js";
import type { ClustersFile } from "../src/clusters/model.js";
import { loadGraph, type Graph } from "../src/install/graph.js";
import type { RunScript, ScriptOutcome } from "../src/install/runner.js";
import type { ConnectionFacts } from "../src/state/deploymentsCatalog.js";
import type { Run } from "../src/state/deployments.js";
import { createReleaseCache } from "../src/version/releaseCache.js";
import { DeploymentPanel, type DeploymentPanelDeps } from "../src/webview/deploymentPanel.js";
import { recorded, resetRecorded, type StubWebviewPanel } from "./support/vscodeStub.js";

const REPO_ROOT = path.resolve(__dirname, "..", "..", "..", "..");
const HOME = fs.mkdtempSync(path.join(os.tmpdir(), "memql-deploy-panel-"));

function context(): ExtensionContext {
  return { subscriptions: [] } as unknown as ExtensionContext;
}

function presenceOf(verdict: PresenceResult["verdict"]): () => Promise<PresenceResult> {
  return async () => ({ verdict, evidence: { receipt: true, registry: true, liveCluster: false }, endpoint: "" });
}

const LOCAL_REGISTERED: ClustersFile = {
  clusters: [{ name: "local", endpoint: "api.memql.localhost:443", domain: "memql.localhost", local: true, version: "v0.23.5" }],
  selectedCluster: "local",
};

const STAGING: ClustersFile = {
  clusters: [{ name: "staging", endpoint: "api.staging.example:443", domain: "staging.example" }],
  selectedCluster: "staging",
};

const TWO_WAVES: Graph = loadGraph(
  JSON.stringify({
    name: "test-install",
    kind: "install",
    steps: ["fetch", "apply"].map((id, n) => ({
      id,
      label: n === 0 ? "Downloading MemQL" : "Creating the cluster",
      description: id,
      script: n === 0 ? "install.binary" : "k3d.up",
      ...(n === 0 ? {} : { dependsOn: ["fetch"] }),
      elevation: "none",
      retained: false,
      retainedReason: "",
      shared: false,
      sharedReason: "",
      receipt: id,
      preExistingPath: "none",
      verify: { kind: "resultTrue", field: "result.installed" },
    })),
  }),
  "test-fixture",
);

const OK: ScriptOutcome = {
  argv: [],
  exitCode: 0,
  signal: null,
  stdout: "",
  stderr: "",
  envelope: { ok: true, capability: "t", changed: true, result: { installed: true, imageSource: "checkout" }, error: null },
};

function heldRunner() {
  const waiting = new Map<string, (outcome: ScriptOutcome) => void>();
  const started: string[] = [];
  const run: RunScript = async ({ capability }) => {
    started.push(capability ?? "");
    if (capability === "install.dockerAccess") return OK;
    return new Promise<ScriptOutcome>((resolve) => waiting.set(capability ?? "", resolve));
  };
  return {
    run,
    started,
    release: (id: string) => {
      waiting.get(id)?.(OK);
      waiting.delete(id);
    },
  };
}

async function until(check: () => boolean, what: string): Promise<void> {
  for (let i = 0; i < 400; i += 1) {
    if (check()) return;
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
  assert.fail(`timed out waiting for ${what}`);
}

interface Harness {
  deps: DeploymentPanelDeps;
  runs: LocalRuns;
  setConnection(facts: ConnectionFacts | undefined): void;
  change(): void;
  runner: ReturnType<typeof heldRunner>;
}

function harness(over: Partial<DeploymentPanelDeps> & { clusters?: ClustersFile; runList?: Run[]; presence?: PresenceResult["verdict"] } = {}): Harness {
  const runs = new LocalRuns();
  const runner = heldRunner();
  let connection: ConnectionFacts | undefined;
  const listeners: (() => void)[] = [];
  const dir = fs.mkdtempSync(path.join(HOME, "case-"));
  const deps: DeploymentPanelDeps = {
    catalog: {
      clustersPath: path.join(dir, "clusters.yaml"),
      receiptPath: path.join(dir, "install-receipt.json"),
      runsDir: path.join(dir, "runs"),
      presence: presenceOf(over.presence ?? "installed-healthy"),
      readClusters: async () => ({ ok: true as const, file: over.clusters ?? LOCAL_REGISTERED }),
      readReceiptFile: async () => null,
      listRunsIn: async () => over.runList ?? [],
    },
    installRoot: REPO_ROOT,
    receiptFile: path.join(dir, "install-receipt.json"),
    runsDir: path.join(dir, "runs"),
    refreshTree: () => undefined,
    openInstallFlow: () => undefined,
    connection: () => connection,
    runs,
    runScript: runner.run,
    graphs: { update: TWO_WAVES, changeVersion: TWO_WAVES },
    home: HOME,
    onDidChange: (listener) => {
      listeners.push(listener);
      return { dispose: () => undefined };
    },
    ...over,
  };
  return {
    deps,
    runs,
    runner,
    setConnection: (facts) => {
      connection = facts;
    },
    change: () => listeners.forEach((l) => l()),
  };
}

function page(): StubWebviewPanel {
  return recorded.webviews[recorded.webviews.length - 1]!;
}

/** The page's words, with the kit's escaping undone for reading. */
function text(): string {
  return page().html.replace(/&#39;/g, "'");
}

async function settle(): Promise<void> {
  for (let i = 0; i < 20; i += 1) await new Promise((resolve) => setImmediate(resolve));
}

function fresh(): void {
  for (const panel of recorded.webviews) if (!panel.disposed) panel.close();
  resetRecorded();
}

// -----------------------------------------------------------------------------
// reading
// -----------------------------------------------------------------------------

test("before the first read the page is the shape of a page, with no fetch copy", async () => {
  fresh();
  const h = harness();
  DeploymentPanel.show(context(), h.deps);
  assert.match(page().html, /class="mq-skeleton"/);
  assert.doesNotMatch(page().html, /Reading this machine/);
  await settle();
  assert.match(page().html, /<h1 class="mq-title">local<\/h1>/);
  page().close();
});

test("a cluster list that will not read is said, with a way to try again -- never an endless skeleton", async () => {
  fresh();
  const h = harness();
  h.deps.catalog.readClusters = async () => ({ ok: false as const, error: "bad yaml at line 3" });
  DeploymentPanel.show(context(), h.deps);
  await settle();
  assert.match(text(), /Can't read your cluster list\./);
  assert.match(text(), /bad yaml at line 3/);
  assert.doesNotMatch(page().html, /class="mq-skeleton"/);
  page().close();
});

test("a cluster that left the list says so", async () => {
  fresh();
  const h = harness();
  DeploymentPanel.show(context(), h.deps, "ghost");
  await settle();
  assert.match(text(), /This cluster is no longer in your list\./);
  page().close();
});

test("the page re-reads when the connection changes: Not signed in, then Connected", async () => {
  fresh();
  const h = harness();
  h.setConnection({ clusterName: "local", connected: false, word: "signIn" });
  DeploymentPanel.show(context(), h.deps);
  await settle();
  assert.match(text(), /Not signed in/);
  h.setConnection({ clusterName: "local", connected: true, word: "connected" });
  h.change();
  await settle();
  assert.match(text(), /mq-actbar-word">Connected</);
  page().close();
});

test("a branch install's page shows its version from what was recorded, never 'unknown'", async () => {
  fresh();
  const h = harness();
  DeploymentPanel.show(context(), h.deps);
  await settle();
  // The registry's recorded release, because the receipt names none.
  assert.match(page().html, /mq-head-meta">v0\.23\.5</);
  assert.doesNotMatch(page().html, /unknown/);
  page().close();
});

// -----------------------------------------------------------------------------
// acting
// -----------------------------------------------------------------------------

test("Sign in is THE sign-in command, with the cluster named -- no picker in front of it", async () => {
  fresh();
  const h = harness();
  h.setConnection({ clusterName: "local", connected: false, word: "signIn" });
  DeploymentPanel.show(context(), h.deps);
  await settle();
  page().send({ type: "signIn" });
  await settle();
  const at = recorded.executed.indexOf("memql.clusters.signIn");
  assert.notEqual(at, -1);
  assert.deepEqual(recorded.executedArgs[at], ["local"]);
  page().close();
});

test("an act the page did not draw is dropped", async () => {
  fresh();
  const h = harness();
  h.setConnection({ clusterName: "local", connected: true, word: "connected" });
  DeploymentPanel.show(context(), h.deps);
  await settle();
  // A connected, healthy cluster draws no Repair and no Uninstall on its bar.
  page().send({ type: "repair" });
  page().send({ type: "uninstall" });
  await settle();
  assert.equal(recorded.executed.includes("memql.clusters.repair"), false);
  assert.equal(recorded.executed.includes("memql.clusters.uninstall"), false);
  page().close();
});

test("a typed confirmation that does not match changes nothing, and says so", async () => {
  fresh();
  const h = harness({
    releases: createReleaseCache({ fetch: async () => ({ tags: ["v0.24.0", "v0.23.5"], error: "" }) }),
    confirm: async () => "v0.24",
  });
  h.setConnection({ clusterName: "local", connected: true, word: "connected" });
  DeploymentPanel.show(context(), h.deps);
  await settle();
  assert.match(page().html, /data-act="update" data-value="v0\.24\.0"/);
  page().send({ type: "update", value: "v0.24.0" });
  await settle();
  assert.match(text(), /That didn't match, so nothing changed\./);
  assert.equal(h.runs.current, undefined, "a run started on a phrase that did not match");
  page().close();
});

test("the update confirms once, typed, with a short question, then runs on the page", async () => {
  fresh();
  const asked: { title: string; prompt: string; phrase: string }[] = [];
  const h = harness({
    releases: createReleaseCache({ fetch: async () => ({ tags: ["v0.24.0", "v0.23.5"], error: "" }) }),
    confirm: async (c) => {
      asked.push(c);
      return c.phrase;
    },
  });
  h.setConnection({ clusterName: "local", connected: true, word: "connected" });
  DeploymentPanel.show(context(), h.deps);
  await settle();
  page().send({ type: "update", value: "v0.24.0" });
  await settle();
  assert.deepEqual(asked, [{ title: "Update local", prompt: "Update local from v0.23.5 to v0.24.0? Type v0.24.0 to confirm.", phrase: "v0.24.0" }]);
  await until(() => h.runner.started.includes("install.binary"), "the run");
  assert.match(page().html, /Updating MemQL/);
  h.runner.release("install.binary");
  await until(() => h.runner.started.includes("k3d.up"), "the second step");
  h.runner.release("k3d.up");
  await h.runs.current!.settled();
  page().close();
});

// -----------------------------------------------------------------------------
// a run in flight
// -----------------------------------------------------------------------------

test("opening the page while a run is going shows it live, with Cancel, and nothing that starts a second", async () => {
  fresh();
  const h = harness();
  const run = h.runs.start({ kind: "update", instance: "local", from: "v0.23.5", to: "v0.24.0" }, {
    installRoot: REPO_ROOT,
    receiptFile: h.deps.receiptFile,
    runsDir: h.deps.runsDir!,
    runScript: h.runner.run,
    graphs: { update: TWO_WAVES },
  })!;
  await until(() => h.runner.started.includes("install.binary"), "the run");

  DeploymentPanel.show(context(), h.deps);
  await settle();
  assert.match(page().html, /Updating MemQL/);
  assert.match(page().html, /data-act="cancel"/);
  for (const act of ["changeVersion", "update", "repair", "uninstall", "rebuildFromCheckout"]) {
    assert.doesNotMatch(page().html, new RegExp(`data-act="${act}"`), act);
  }
  // A change-version request posted anyway is refused by the slot.
  page().send({ type: "changeVersion" });
  await settle();
  assert.equal(h.runs.current, run);

  // Clicking the run's own row opens it live, too.
  DeploymentPanel.showRun(context(), h.deps, "local", run.recordId);
  await settle();
  assert.match(page().html, /Updating MemQL/);

  // Cancel stops after the current step, and says so until it has.
  page().send({ type: "cancel" });
  await settle();
  assert.match(page().html, /Stopping/);
  h.runner.release("install.binary");
  await run.settled();
  await settle();
  assert.match(page().html, /mq-actbar-word">Stopped</);
  page().close();
});

test("closing the tab leaves the run going; reopening shows it", async () => {
  fresh();
  const h = harness();
  h.setConnection({ clusterName: "local", connected: true, word: "connected" });
  DeploymentPanel.show(context(), h.deps);
  await settle();
  page().send({ type: "changeVersion" });
  await settle();
  // Pick a version by typing, then start.
  page().send({ type: "input", field: "version", value: "__other__" });
  page().send({ type: "input", field: "typed", value: "v0.22.0" });
  await settle();
  page().send({ type: "beginChange", value: "v0.22.0" });
  await until(() => h.runner.started.includes("install.binary"), "the run");
  const run = h.runs.current!;

  page().close();
  assert.equal(run.inFlight, true, "closing the tab stopped the run");

  DeploymentPanel.show(context(), h.deps);
  await settle();
  assert.match(page().html, /Changing version/);
  h.runner.release("install.binary");
  await until(() => h.runner.started.includes("k3d.up"), "the second step");
  h.runner.release("k3d.up");
  await run.settled();
  await settle();
  assert.match(page().html, /local is on v0\.22\.0/);
  page().close();
});

// -----------------------------------------------------------------------------
// a remote cluster
// -----------------------------------------------------------------------------

function deploymentRow(id: string, status: string, version: string, createdAt: string, previous = ""): Row {
  return {
    id: `v1:cluster:deployment:${id}`,
    concept: "v1:cluster:deployment",
    createdAt,
    payload: { deploymentId: id, status, version, previousDeploymentId: previous },
  };
}

function remoteHarness(calls: string[]): Harness {
  const port = {
    getDeploymentStatus: async () => ({ rollouts: [{ name: "bff", phase: "Paused" }] }),
    rollbackDeployment: async (id: string) => {
      calls.push(`rollback ${id}`);
      return { ok: true, message: "", auditEventId: "a1", correlationId: "", details: {} };
    },
    rolloutAction: async (rollout: string, action: string) => {
      calls.push(`${action} ${rollout}`);
      return { ok: true, message: "", auditEventId: "a2", correlationId: "", details: {} };
    },
  } as unknown as DeployControlPort;
  const h = harness({
    clusters: STAGING,
    deployPort: () => port,
    readRole: async () => roleVisibility("owner"),
    confirm: async (c) => c.phrase,
    readDeployments: () => async () => ({
      deployments: [
        deploymentRow("dep-3", "succeeded", "v0.23.5", "2026-09-27T10:00:00Z", "dep-2"),
        deploymentRow("dep-2", "failed", "v0.23.4", "2026-09-20T10:00:00Z", "dep-1"),
        deploymentRow("dep-1", "succeeded", "v0.23.3", "2026-09-10T10:00:00Z"),
      ],
      specs: [],
    }),
  });
  h.setConnection({ clusterName: "staging", connected: true, word: "connected" });
  return h;
}

test("a rollback goes to the run on screen, never to the deployment already running", async () => {
  fresh();
  const calls: string[] = [];
  const h = remoteHarness(calls);
  DeploymentPanel.showRun(context(), h.deps, "staging", "dep-1");
  await settle();
  assert.match(text(), /Roll back to v0\.23\.3/);
  // A rollback aimed at another deployment than the one on the button is dropped.
  page().send({ type: "rollback", value: "dep-3" });
  await settle();
  assert.deepEqual(calls, []);
  page().send({ type: "rollback", value: "dep-1" });
  await settle();
  assert.deepEqual(calls, ["rollback dep-1"]);
  page().close();
});

test("the running deployment's own page offers no rollback", async () => {
  fresh();
  const h = remoteHarness([]);
  DeploymentPanel.showRun(context(), h.deps, "staging", "dep-3");
  await settle();
  assert.doesNotMatch(page().html, /data-act="rollback"/);
  page().close();
});

test("a promote names the rollout that is part-way through", async () => {
  fresh();
  const calls: string[] = [];
  const h = remoteHarness(calls);
  DeploymentPanel.show(context(), h.deps, "staging");
  await settle();
  page().send({ type: "rolloutPromote", value: "bff" });
  await settle();
  assert.deepEqual(calls, ["promote bff"]);
  assert.match(text(), /Rollout promoted\./);
  page().close();
});

test("an expired session during an act says so, and its Sign in works from the notice", async () => {
  fresh();
  const calls: string[] = [];
  const h = remoteHarness(calls);
  const port = {
    getDeploymentStatus: async () => ({ rollouts: [{ name: "bff", phase: "Paused" }] }),
    rolloutAction: async () => {
      throw new DeployControlError("rollout_action", CODE_UNAUTHENTICATED, "no actor");
    },
  } as unknown as DeployControlPort;
  h.deps.deployPort = () => port;
  DeploymentPanel.show(context(), h.deps, "staging");
  await settle();
  page().send({ type: "rolloutPromote", value: "bff" });
  await settle();
  assert.match(text(), /Your session has expired\./);
  // The notice's act, although the connected bar offers no sign-in.
  page().send({ type: "signIn" });
  await settle();
  const at = recorded.executed.indexOf("memql.clusters.signIn");
  assert.deepEqual(recorded.executedArgs[at], ["staging"]);
  page().close();
});

test("the rebuild screen's Repair fix opens Repair when the folder is not a MemQL checkout", async () => {
  fresh();
  const h = harness();
  h.deps.catalog.readReceiptFile = async () => ({
    version: 1,
    kind: "install",
    createdAt: "2026-08-01T00:00:00Z",
    entries: [
      {
        stepId: "stackCheckout",
        script: "install/clone-stack.sh",
        receipt: "checkout",
        preExisting: false,
        params: {},
        result: { dest: path.join(HOME, "no-such-checkout"), commit: "abc1234def", refKind: "tag", tag: "v0.23.5" },
        changed: true,
        recordedAt: "2026-08-01T00:00:00Z",
      },
    ],
  }) as never;
  h.setConnection({ clusterName: "local", connected: true, word: "connected" });
  // From the palette or the title menu: the page opens on the rebuild screen.
  assert.equal(await DeploymentPanel.openAction(context(), h.deps, "rebuildFromCheckout"), undefined);
  await until(() => /isn't a MemQL checkout/.test(text()), "the check");
  assert.doesNotMatch(page().html, /data-act="beginRebuild"/);
  page().send({ type: "repair" });
  await settle();
  assert.equal(recorded.executed.includes("memql.clusters.repair"), true);
  page().close();
});

test("a remote cluster this editor is signed out of offers Sign in, never 'No deploy pipeline'", async () => {
  fresh();
  const h = harness({ clusters: STAGING });
  h.setConnection({ clusterName: "staging", connected: false, word: "signIn" });
  DeploymentPanel.show(context(), h.deps, "staging");
  await settle();
  assert.match(text(), /Not signed in/);
  assert.match(page().html, /data-act="signIn"/);
  assert.doesNotMatch(text(), /deploy pipeline|not answering/i);
  page().close();
});

// -----------------------------------------------------------------------------
// review fixes
// -----------------------------------------------------------------------------

/** A log pane the page runtime streams into (the runtime's own script names the selector too). */
const LIVE_LOG = /class="mq-log"[^>]*data-region="log"/;

function startUpdate(h: Harness) {
  return h.runs.start({ kind: "update", instance: "local", from: "v0.23.5", to: "v0.24.0" }, {
    installRoot: REPO_ROOT,
    receiptFile: h.deps.receiptFile,
    runsDir: h.deps.runsDir!,
    runScript: h.runner.run,
    graphs: { update: TWO_WAVES },
  })!;
}

test("a run going on this machine is the page from the first paint, before the cluster list has been read", async () => {
  fresh();
  const h = harness();
  const run = startUpdate(h);
  await until(() => h.runner.started.includes("install.binary"), "the run");
  DeploymentPanel.show(context(), h.deps);
  // No settle: the first document, before any read has landed.
  assert.match(page().html, /Updating MemQL/);
  assert.doesNotMatch(page().html, /class="mq-skeleton"/);
  h.runner.release("install.binary");
  await until(() => h.runner.started.includes("k3d.up"), "the second step");
  h.runner.release("k3d.up");
  await run.settled();
  page().close();
});

test("a cluster list that tears mid-run leaves the live run and its Cancel on screen", async () => {
  // The Cockpit writes clusters.yaml without a lock, and a run's own steps
  // rewrite it: a read that lands on a half-written file must not replace the
  // progress screen with an error that offers no way to stop the run.
  fresh();
  const h = harness();
  const run = startUpdate(h);
  await until(() => h.runner.started.includes("install.binary"), "the run");
  DeploymentPanel.show(context(), h.deps);
  await settle();
  assert.match(page().html, /data-act="cancel"/);
  h.deps.catalog.readClusters = async () => ({ ok: false as const, error: "unexpected end of the stream" });
  h.change();
  await settle();
  assert.match(page().html, /Updating MemQL/);
  assert.match(page().html, /data-act="cancel"/);
  assert.doesNotMatch(text(), /Can't read your cluster list/);
  h.runner.release("install.binary");
  await until(() => h.runner.started.includes("k3d.up"), "the second step");
  h.runner.release("k3d.up");
  await run.settled();
  page().close();
});

test("a cluster list that will not read offers the file itself as the fix", async () => {
  fresh();
  const h = harness();
  h.deps.catalog.readClusters = async () => ({ ok: false as const, error: "bad yaml at line 3" });
  DeploymentPanel.show(context(), h.deps);
  await settle();
  assert.match(page().html, /data-act="openClusterList"/);
  page().send({ type: "openClusterList" });
  await settle();
  const at = recorded.executed.indexOf("vscode.open");
  assert.notEqual(at, -1, "the list was not opened");
  assert.equal((recorded.executedArgs[at]![0] as { fsPath?: string; path?: string }).fsPath ?? "", h.deps.catalog.clustersPath);
  page().close();
});

test("a saved step log on a run's page is not a live log region, so the last run's stream cannot empty or overwrite it", async () => {
  // The page runtime writes every `log` message into every live log region,
  // and LiveView re-sends its buffer -- with a reset -- to each new document
  // that says `ready`. A page that had been watching a run would otherwise
  // wipe a recorded failure's saved output the moment its page came up.
  fresh();
  const past: Run = {
    id: "run-past",
    instance: "local",
    kind: "upgrade",
    fromVersion: "v0.23.4",
    toVersion: "v0.23.5",
    startedAt: "2026-09-25T03:00:00Z",
    finishedAt: "2026-09-25T03:04:00Z",
    status: "failed",
    items: [{ label: "clusterUp", status: "failed", detail: "Port 443 is already in use · exit 1 · log=run-past.clusterUp.log" }],
  };
  const h = harness({ runList: [past] });
  fs.mkdirSync(h.deps.runsDir!, { recursive: true });
  fs.writeFileSync(path.join(h.deps.runsDir!, "run-past.clusterUp.log"), "Bind for 0.0.0.0:443 failed: port is already allocated\n");
  h.setConnection({ clusterName: "local", connected: true, word: "connected" });

  // Watch a run to its end on this page, as the page that started it would.
  const run = startUpdate(h);
  DeploymentPanel.show(context(), h.deps);
  await until(() => h.runner.started.includes("install.binary"), "the run");
  page().send({ type: "ready" });
  assert.match(page().html, LIVE_LOG, "the run's own log is live");
  h.runner.release("install.binary");
  await until(() => h.runner.started.includes("k3d.up"), "the second step");
  h.runner.release("k3d.up");
  await run.settled();
  await settle();
  page().send({ type: "back" });
  await settle();

  page().send({ type: "openRun", value: "run-past" });
  await until(() => /port is already allocated/.test(page().html), "the saved log");
  page().send({ type: "ready" });
  await settle();
  assert.doesNotMatch(page().html, LIVE_LOG);
  page().close();
});

test("a run no longer in the record is said per whose record it was, with the bar still on the floor", async () => {
  fresh();
  const local = harness();
  DeploymentPanel.showRun(context(), local.deps, "local", "gone");
  await settle();
  assert.match(text(), /no longer in the history\. Only the latest 50 are kept\./);
  assert.match(page().html, /mq-actbar-word">Not in the history</);
  page().close();

  fresh();
  const remote = remoteHarness([]);
  DeploymentPanel.showRun(context(), remote.deps, "staging", "dep-gone");
  await settle();
  assert.match(text(), /no longer in the cluster's history\./);
  assert.doesNotMatch(text(), /latest 50/);
  page().close();
});

function checkoutReceipt(result: Record<string, unknown>) {
  return async () =>
    ({
      version: 1,
      kind: "install",
      createdAt: "2026-08-01T00:00:00Z",
      entries: [
        {
          stepId: "stackCheckout",
          script: "install/clone-stack.sh",
          receipt: "checkout",
          preExisting: false,
          params: {},
          result,
          changed: true,
          recordedAt: "2026-08-01T00:00:00Z",
        },
      ],
    }) as never;
}

test("a branch install is never offered an update from the release its cluster reports", async () => {
  // clusters.yaml records v0.23.5 for it, but the cluster runs main: "Update
  // to v0.24.0" would move a checkout of main to a tag, worded as an upgrade
  // from a version it is not on.
  fresh();
  const h = harness({
    releases: createReleaseCache({ fetch: async () => ({ tags: ["v0.24.0", "v0.23.5"], error: "" }) }),
  });
  h.deps.catalog.readReceiptFile = checkoutReceipt({
    dest: path.join(HOME, "stack"),
    commit: "3f2a9c1e5b7d",
    refKind: "branch",
    ref: "main",
  });
  h.setConnection({ clusterName: "local", connected: true, word: "connected" });
  DeploymentPanel.show(context(), h.deps);
  await settle();
  assert.match(page().html, /mq-head-meta">main @ 3f2a9c1</);
  assert.doesNotMatch(page().html, /data-act="update"/);
  assert.doesNotMatch(text(), /v0\.24\.0/);
  page().close();
});

test("a checkout act the cluster lacks says which thing is missing; a page that says it itself gets no toast", async () => {
  fresh();
  const released = harness();
  released.deps.catalog.readReceiptFile = checkoutReceipt({
    dest: path.join(HOME, "stack"),
    commit: "abc1234def",
    refKind: "tag",
    tag: "v0.23.5",
  });
  assert.equal(await DeploymentPanel.openAction(context(), released.deps, "updateAndRebuild"), "noBranch");
  page().close();

  fresh();
  const bare = harness();
  assert.equal(await DeploymentPanel.openAction(context(), bare.deps, "rebuildFromCheckout"), "noCheckout");
  page().close();

  fresh();
  const absent = harness({ presence: "absent" });
  assert.equal(await DeploymentPanel.openAction(context(), absent.deps, "rebuildFromCheckout"), undefined);
  assert.match(page().html, /data-act="install"/);
  page().close();
});

test("a remote act that fails says so in one sentence, with the engine's words one click away", async () => {
  fresh();
  const h = remoteHarness([]);
  let shown = 0;
  const logged: string[] = [];
  h.deps.showOutput = () => {
    shown += 1;
  };
  h.deps.logLine = (line) => logged.push(line);
  h.deps.deployPort = () =>
    ({
      getDeploymentStatus: async () => ({ rollouts: [{ name: "bff", phase: "Paused" }] }),
      rolloutAction: async () => {
        throw new Error("rollout bff: analysis run failed");
      },
    }) as unknown as DeployControlPort;
  DeploymentPanel.show(context(), h.deps, "staging");
  await settle();
  page().send({ type: "rolloutPromote", value: "bff" });
  await settle();
  assert.match(text(), /Couldn't promote the rollout/);
  assert.match(page().html, /data-act="openOutput">Show details</);
  assert.ok(logged.some((line) => line.includes("analysis run failed")), "the engine's words did not reach Output");
  page().send({ type: "openOutput" });
  assert.equal(shown, 1);
  page().close();
});
