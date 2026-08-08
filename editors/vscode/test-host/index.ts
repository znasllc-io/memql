// The Extension Development Host smoke lane (memql#3302).
//
// WHAT THIS LANE IS FOR, AND WHAT IT IS NOT FOR.
//
// B1 shipped 235 automated tests and zero verification in a real VS Code
// host. Both defects its review caught passed every one of those tests and
// failed only against a host:
//
//   1. the SDK dereferenced the bare global `WebSocket` to read
//      `WebSocket.OPEN`. Both operands of a comparison are evaluated, so on
//      the extension host's Node 20 -- which has no global WebSocket -- that
//      threw ReferenceError before `readyState` was ever consulted.
//   2. `createFileSystemWatcher` given a plain string glob silently watches
//      only paths INSIDE the workspace folders. `~/.memql/clusters.yaml` is
//      outside any workspace, so the watcher never fired and three handlers
//      were dead code -- with no error, anywhere, ever.
//
// Neither is reachable by a unit test, because neither is a logic bug. Both
// are the runtime and the host API not behaving the way the code assumed. So
// every case below asserts something that is ONLY true (or only false) in a
// real host: the extension host's Node runtime, the workbench's command
// registry, the platform's file-watcher backend, the webview machinery.
//
// Logic that a unit test already covers is deliberately NOT re-asserted here.
// The fast `npm test` lane owns that, runs in about a second, and needs no
// Electron.
//
// NO LIVE CLUSTER IS REQUIRED. Every connection-dependent behaviour -- listing
// concepts, paging rows, running a construct, deploying -- is out of scope for
// a smoke lane and belongs to the manual checklist (see
// docs/public/language/vscode-runtime-panel-verification.md). The panels below
// are opened against a disconnected ConnectionManager on purpose: that is the
// state a webview must survive, and it is the one this lane can guarantee.
// Anything that would need a cluster calls skip() and says so loudly.

import * as assert from "node:assert/strict";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";

import * as vscode from "vscode";
import { WebSocket as NodeWebSocket } from "ws";

import { defaultClustersPath } from "../src/clusters/file.js";
import { ConnectionManager } from "../src/connection/manager.js";
import type { AutomationTarget, RunTarget } from "../src/constructs/runnable.js";
import { StepTraceModel } from "../src/state/stepTrace.js";
import { AutomationRunPanel, StepTracePanel } from "../src/webview/automationPanel.js";
import type { AutomationPanelHost } from "../src/webview/automationPanel.js";
import { ClusterPanel } from "../src/webview/clusterPanel.js";
import { ConceptPanel } from "../src/webview/conceptPanel.js";
import { ResultPanel, RunPanel } from "../src/webview/runPanel.js";
import type { RunPanelHost } from "../src/webview/runPanel.js";

import { info, runCases, skip, smoke, waitFor, warn } from "./harness.js";

const EXTENSION_ID = "znasllc.memql";

// The activity-bar container id from contributes.viewsContainers, and the
// command the workbench auto-generates for it. Asserting on the generated
// command is how a headless process observes that the container exists at
// all -- there is no enumeration API for view containers.
const VIEW_CONTAINER_COMMAND = "workbench.view.extension.memql";

function extension(): vscode.Extension<unknown> {
  const ext = vscode.extensions.getExtension(EXTENSION_ID);
  assert.ok(
    ext !== undefined,
    `extension ${EXTENSION_ID} is not installed in this host; the runner's --extensionDevelopmentPath is wrong or the manifest's publisher/name changed`
  );
  return ext;
}

// A stand-in ExtensionContext for the panel cases. The panels touch exactly
// one member of it (`subscriptions` -- verified by grep over src/webview), so
// a full context is not needed and faking one would only hide which member is
// actually load-bearing.
function fakeContext(): vscode.ExtensionContext {
  return { subscriptions: [] as { dispose(): unknown }[] } as unknown as vscode.ExtensionContext;
}

/** Every open editor/webview tab's label, across all groups. */
function openTabLabels(): string[] {
  return vscode.window.tabGroups.all.flatMap((group) => group.tabs.map((tab) => tab.label));
}

async function closeAllTabs(): Promise<void> {
  await vscode.commands.executeCommand("workbench.action.closeAllEditors");
}

// -----------------------------------------------------------------------------
// Activation
// -----------------------------------------------------------------------------

smoke("the extension activates in a real host without throwing", async () => {
  const ext = extension();
  // activate() resolves with the exports (void here) and REJECTS if activate()
  // threw. That rejection is the whole assertion: a module-scope reference to
  // something the host's runtime does not provide -- exactly the B1 WebSocket
  // shape -- surfaces here and nowhere in the unit lane.
  await ext.activate();
  assert.equal(ext.isActive, true, "extension.activate() resolved but isActive is false");

  // Everything downstream lives behind the trust gate, so a lane running in an
  // untrusted window would report a long row of green tests that asserted
  // nothing. Fail loudly instead. The runner passes --disable-workspace-trust.
  assert.equal(
    vscode.workspace.isTrusted,
    true,
    "workspace is not trusted, so registerRuntimeSurface never ran and the rest of this lane would assert nothing -- the runner must pass --disable-workspace-trust"
  );
});

// -----------------------------------------------------------------------------
// Manifest / implementation drift
// -----------------------------------------------------------------------------

smoke("every command the manifest contributes is registered", async () => {
  const ext = extension();
  await ext.activate();

  const contributed: string[] = (
    (ext.packageJSON as { contributes?: { commands?: { command: string }[] } }).contributes
      ?.commands ?? []
  ).map((c) => c.command);
  assert.ok(
    contributed.length > 0,
    "contributes.commands is empty; the manifest shape changed and this guard now protects nothing"
  );

  // getCommands(true) filters out the workbench's internal commands. The
  // extension's own are not internal, so they must all be here.
  const registered = new Set(await vscode.commands.getCommands(true));
  const missing = contributed.filter((c) => !registered.has(c));
  assert.deepEqual(
    missing,
    [],
    `contributes.commands declares ${missing.length} command(s) that activate() never registered. The palette offers them and they fail with "command not found". Register them, or drop them from the manifest: ${missing.join(", ")}`
  );

  info(`${contributed.length} contributed commands, all registered`);
});

smoke("the activity-bar container and its views exist", async () => {
  const ext = extension();
  await ext.activate();

  const viewIds: string[] = (
    (ext.packageJSON as { contributes?: { views?: Record<string, { id: string }[]> } }).contributes
      ?.views?.memql ?? []
  ).map((v) => v.id);
  assert.ok(
    viewIds.length > 0,
    "contributes.views.memql is empty; the view container moved and this guard now protects nothing"
  );

  const registered = new Set(await vscode.commands.getCommands(true));

  // The workbench generates one command per view container and one `.focus`
  // per view. Their presence is the observable proof that the contributions
  // were accepted -- a typo in the container id, or a view pointed at a
  // container that does not exist, produces a manifest that loads clean and a
  // panel that never appears.
  assert.ok(
    registered.has(VIEW_CONTAINER_COMMAND),
    `${VIEW_CONTAINER_COMMAND} is not registered, so the memQL activity-bar container was not contributed`
  );
  const missingViews = viewIds.filter((id) => !registered.has(`${id}.focus`));
  assert.deepEqual(
    missingViews,
    [],
    `these contributed views have no generated .focus command, so the workbench did not accept them: ${missingViews.join(", ")}`
  );

  // Actually reveal it. Registration and rendering are different failures.
  await vscode.commands.executeCommand(VIEW_CONTAINER_COMMAND);
  info(`view container revealed; views: ${viewIds.join(", ")}`);
});

// -----------------------------------------------------------------------------
// The extension host's Node runtime (B1 defect 1)
// -----------------------------------------------------------------------------

smoke("the host runtime supports the extension's WebSocket strategy", async () => {
  info(
    `extension host runtime: node ${process.versions.node}, electron ${process.versions.electron ?? "n/a"}, vscode ${vscode.version}`
  );

  // The ambient condition that produced B1's first defect. Node gained a
  // global WebSocket at 22; the extension host's floor is lower, so on a
  // conforming host this is "undefined" -- and an unguarded `WebSocket.OPEN`
  // anywhere in the loaded bundle is a latent ReferenceError.
  const globalWs = (globalThis as { WebSocket?: unknown }).WebSocket;
  if (globalWs === undefined) {
    info(
      "no global WebSocket on this host -- the exact condition under which the bare `WebSocket.OPEN` dereference threw. The SDK's numeric readyState constants (sdk/ts/src/client/wsReadyState.ts) are what make the connect path work here."
    );
  } else {
    warn(
      "this host DOES expose a global WebSocket, so it cannot reproduce the B1 ReferenceError. The lane still checks the injected implementation below, but on this host the guard against a regression is the SDK's own unit tests, not this case."
    );
  }

  // The substitute the extension injects (see defaultDial in
  // src/connection/manager.ts). Two things only a host can tell us: that the
  // `ws` package loads inside Electron's extension host at all, and that
  // reading `readyState` off one of its sockets and comparing it against a
  // plain number -- the pattern the fix installed -- does not throw.
  //
  // Port 1 is never listenable, so this fails fast and locally. We only ever
  // observe the CONNECTING state and tear it down; nothing leaves the machine.
  const socket = new NodeWebSocket("ws://127.0.0.1:1/");
  // A `ws` socket that errors with no 'error' listener throws on the process.
  socket.on("error", () => {
    /* expected: nothing is listening on port 1 */
  });
  try {
    assert.equal(
      socket.readyState,
      0,
      "a freshly constructed ws socket should report CONNECTING (0); the numeric readyState contract the SDK relies on does not hold on this runtime"
    );
  } finally {
    socket.terminate();
  }
});

// -----------------------------------------------------------------------------
// File watching outside the workspace (B1 defect 2)
// -----------------------------------------------------------------------------

smoke("a watcher fires for a path outside the workspace", async () => {
  await extension().activate();

  const clustersPath = defaultClustersPath();
  const clustersDir = path.dirname(clustersPath);

  // The premise of the whole case. If HOME were inside the opened workspace,
  // a plain string glob would work and this case would prove nothing -- so
  // assert the premise rather than assume the runner arranged it.
  const folders = vscode.workspace.workspaceFolders ?? [];
  assert.ok(folders.length > 0, "no workspace folder is open, so 'outside the workspace' is vacuous");
  for (const folder of folders) {
    assert.ok(
      !clustersPath.startsWith(folder.uri.fsPath + path.sep),
      `${clustersPath} is INSIDE workspace folder ${folder.uri.fsPath}; the runner failed to isolate HOME and this case is vacuous`
    );
  }
  info(`clusters path ${clustersPath} is outside the ${folders.length} open workspace folder(s)`);

  // The runner gives the host a PRISTINE temp HOME, so this directory exists
  // only if activation created it -- which registerRuntimeSurface must do
  // before watching, because a non-recursive watch of a missing directory
  // cannot be established and, on the declared 1.91 floor, never recovers when
  // the directory later appears. That is a third instance of the B1 bug class
  // (nothing throws, every unit test passes, the feature is silently dead) and
  // this assertion is its regression guard.
  assert.ok(
    fs.existsSync(clustersDir),
    `${clustersDir} does not exist after activation. registerRuntimeSurface must create the watch base before calling createFileSystemWatcher, or the Clusters tree never refreshes on an external edit for any user who has not run the Cockpit.`
  );

  fs.rmSync(clustersPath, { force: true });

  // The form the extension actually uses: a RelativePattern with a base Uri.
  const watcher = vscode.workspace.createFileSystemWatcher(
    new vscode.RelativePattern(vscode.Uri.file(clustersDir), path.basename(clustersPath))
  );
  // A negative control on the form B1 shipped. Kept because the failure being
  // guarded is "someone simplifies this back to a string and nothing breaks
  // loudly" -- seeing the string form stay silent in the same run, against
  // the same file, is what makes the RelativePattern requirement concrete
  // rather than folklore.
  const stringWatcher = vscode.workspace.createFileSystemWatcher(clustersPath);

  let fired = false;
  let stringFired = false;
  watcher.onDidCreate(() => {
    fired = true;
  });
  watcher.onDidChange(() => {
    fired = true;
  });
  stringWatcher.onDidCreate(() => {
    stringFired = true;
  });
  stringWatcher.onDidChange(() => {
    stringFired = true;
  });

  try {
    // KEEP WRITING until it fires, rather than write once and wait.
    //
    // createFileSystemWatcher returns before the watcher is established in the
    // file-watcher service, and VS Code does not replay events from before
    // that point -- so a single write timed against a fixed sleep is a race,
    // and one this lane lost on its first run (a 1s settle was not enough on
    // this machine; 3s was). A sleep long enough to be safe everywhere is a
    // sleep long enough to be a bad test. Touching the file repeatedly makes
    // the establishment delay irrelevant: whenever the watcher comes up, the
    // next write is one it sees.
    await waitFor(
      `a RelativePattern watcher on ${clustersPath} to fire`,
      () => {
        fs.writeFileSync(clustersPath, `clusters: []\n# ${Date.now()}\n`, "utf8");
        return fired;
      },
      30_000,
      500
    );

    // The negative control gets the same treatment plus a grace period, so
    // "the string form did not fire" is a statement about the string form and
    // not about it having been given less of a chance.
    for (let i = 0; i < 6 && !stringFired; i += 1) {
      fs.writeFileSync(clustersPath, `clusters: []\n# control ${i}\n`, "utf8");
      await new Promise((resolve) => setTimeout(resolve, 500));
    }

    // Reported, never asserted. VS Code is entitled to widen string-glob
    // watching in a future release, and this lane must not go red the day it
    // does -- what matters is that the form the extension uses works.
    if (stringFired) {
      warn(
        "the bare string-glob watcher ALSO fired for a path outside the workspace. That is not a failure, but the RelativePattern requirement documented in registerRuntimeSurface may no longer be load-bearing on this VS Code version."
      );
    } else {
      info(
        "the bare string-glob watcher did not fire -- B1's second defect reproduced exactly, and the RelativePattern form is what fixes it"
      );
    }
  } finally {
    watcher.dispose();
    stringWatcher.dispose();
  }
});

// -----------------------------------------------------------------------------
// Webview surfaces
// -----------------------------------------------------------------------------

// The panels are opened against a DISCONNECTED ConnectionManager. That is not
// a compromise forced by the absence of a cluster -- it is the state each
// panel has to render before a connection exists, and the one an operator sees
// first. What the cluster would add (real rows, a real topology) is the
// manual checklist's job.
smoke("every webview surface opens without throwing", async () => {
  await closeAllTabs();

  const context = fakeContext();
  const connections = new ConnectionManager();

  const concept = {
    id: "v1:smoke:widget",
    version: "v1",
    domain: "smoke",
    entity: "widget",
    description: "host smoke fixture",
    type: "concept",
  };

  const runTarget: RunTarget = {
    uri: "file:///smoke/queries.memql",
    kind: "query",
    name: "smokeQuery",
    args: [],
  };

  const automationTarget: AutomationTarget = {
    uri: "file:///smoke/automations.memql",
    name: "smokeAutomation",
  };

  // Hosts that reject rather than pretend: nothing in this case clicks a
  // button, and a stub that silently resolved would let a panel that DOES
  // call out on open sail through.
  const runHost: RunPanelHost = {
    run: () => Promise.reject(new Error("no cluster in the smoke lane")),
    saveConfig: () => Promise.reject(new Error("no cluster in the smoke lane")),
    concepts: () => new Map(),
    openRow: () => undefined,
  };
  const automationHost: AutomationPanelHost = {
    run: () => Promise.reject(new Error("no cluster in the smoke lane")),
    saveConfig: () => Promise.reject(new Error("no cluster in the smoke lane")),
    browseRows: () => Promise.reject(new Error("no cluster in the smoke lane")),
    concept: () => undefined,
  };

  const expected: string[] = [];

  ConceptPanel.open(context, connections, concept);
  expected.push(`Concept: ${concept.entity}`);

  ClusterPanel.open(context, connections, "smoke-cluster");
  expected.push("Cluster: smoke-cluster");

  RunPanel.open(context, runHost, runTarget);
  expected.push(`Run: ${runTarget.name}`);

  ResultPanel.show(context, runHost, {
    status: "ok",
    target: runTarget,
    rows: [],
    raw: {},
    ranDeployedDefinition: false,
    injected: false,
  });

  AutomationRunPanel.open(context, automationHost, automationTarget);
  expected.push(`Run automation: ${automationTarget.name}`);

  StepTracePanel.show(context, automationTarget, new StepTraceModel());

  try {
    // Tabs appear asynchronously -- createWebviewPanel returns before the
    // workbench has the tab in its model.
    await waitFor(
      `webview tabs ${expected.join(", ")} to appear (saw: ${openTabLabels().join(", ")})`,
      () => {
        const labels = openTabLabels();
        return expected.every((label) => labels.includes(label));
      },
      15_000
    );
    info(`open tabs: ${openTabLabels().join(", ")}`);
  } finally {
    await closeAllTabs();
    for (const entry of context.subscriptions) {
      entry.dispose();
    }
  }
});

// -----------------------------------------------------------------------------
// Explicitly out of scope
// -----------------------------------------------------------------------------

// Not a placeholder for work left undone -- a standing, deliberate skip that
// keeps the lane honest about the line it draws. A smoke lane that quietly
// omitted the cluster-dependent half would read as covering more than it does.
smoke("connection-dependent behaviour", () => {
  skip(
    "by design: this lane never dials a cluster. Connecting, browsing concepts, paging rows, running a construct or an automation, and the Cluster tab's live data are covered by the manual checklist in docs/public/language/vscode-runtime-panel-verification.md."
  );
});

/**
 * run is the entry point @vscode/test-electron loads inside the host.
 *
 * The returned promise's rejection is the lane's failure signal -- the host
 * exits nonzero on it, and on nothing else.
 */
export async function run(): Promise<void> {
  info(`HOME=${os.homedir()}`);
  await runCases();
}
