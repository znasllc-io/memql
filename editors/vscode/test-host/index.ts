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
import { defaultRunsDir } from "../src/state/runLog.js";
import { ClusterPresence, defaultEndpointProbe } from "../src/clusters/presence.js";
import { ConnectionManager } from "../src/connection/manager.js";
import type { AutomationTarget, RunTarget } from "../src/constructs/runnable.js";
import { StepTraceModel } from "../src/state/stepTrace.js";
import { AutomationRunPanel, StepTracePanel } from "../src/webview/automationPanel.js";
import type { AutomationPanelHost } from "../src/webview/automationPanel.js";
import { ConnectionPanel } from "../src/webview/connectionPanel.js";
import { ConstructPanel } from "../src/webview/constructPanel.js";
import { ConceptPanel } from "../src/webview/conceptPanel.js";
import { DeploymentPanel } from "../src/webview/deploymentPanel.js";
import { ResultPanel, RunPanel } from "../src/webview/runPanel.js";
import type { RunPanelHost } from "../src/webview/runPanel.js";

import { info, runCases, skip, smoke, waitFor, warn } from "./harness.js";
// A FUNCTION rather than an import-for-side-effect: cases run in registration
// order, and an `import "./live.js"` would register them FIRST -- ES imports
// are evaluated before the importing module's body, so the live cases would
// run ahead of the activation case that everything else depends on. Calling it
// at the bottom of this file is what actually puts them last.
import { registerLiveCases } from "./live.js";

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

smoke("both MemQL colour themes are installed and can be applied", async () => {
  // The one item on this epic's manual checklist that a unit test structurally
  // cannot reach (memql#4420, memql#4422). test/themes.test.ts proves the two
  // JSON files match the palette, that the manifest points at them, and that
  // the VSIX packs them -- but "VS Code found the file, parsed it and made it
  // the active theme" is a claim about the WORKBENCH, and the workbench is
  // only here.
  //
  // APPLYING each theme is the assertion, not enumerating them. There is no
  // API for "what does the theme picker list", and a check that only read
  // `contributes.themes` back off packageJSON would pass for a theme whose
  // `path` points at nothing -- which is exactly the failure mode a missing
  // file produces: the picker lists it and choosing it silently does nothing.
  // Setting it and watching `activeColorTheme.kind` follow proves the whole
  // path, because a label VS Code cannot resolve leaves the current theme in
  // place and the kind unchanged.
  const ext = extension();
  await ext.activate();

  const themes: { label: string; uiTheme: string }[] =
    (ext.packageJSON as { contributes?: { themes?: { label: string; uiTheme: string }[] } })
      .contributes?.themes ?? [];
  assert.deepEqual(
    themes.map((t) => t.label).sort(),
    ["MemQL Dark", "MemQL Light"],
    "contributes.themes is not the pair this lane knows how to apply"
  );

  const workbench = vscode.workspace.getConfiguration("workbench");
  const original = workbench.get<string>("colorTheme");
  try {
    for (const [label, expected] of [
      ["MemQL Light", vscode.ColorThemeKind.Light],
      ["MemQL Dark", vscode.ColorThemeKind.Dark],
    ] as const) {
      await vscode.workspace
        .getConfiguration("workbench")
        .update("colorTheme", label, vscode.ConfigurationTarget.Global);
      await waitFor(
        `${label} to become the active theme`,
        () => vscode.window.activeColorTheme.kind === expected
      );
      info(`${label} applied; activeColorTheme.kind = ${vscode.window.activeColorTheme.kind}`);
    }
  } finally {
    // Restore, so a later case reads the theme the host started with rather
    // than whichever one this loop left behind.
    await vscode.workspace
      .getConfiguration("workbench")
      .update("colorTheme", original, vscode.ConfigurationTarget.Global);
  }
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
    `${VIEW_CONTAINER_COMMAND} is not registered, so the MemQL activity-bar container was not contributed`
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

smoke("the run-log directory is watched, so a run in flight repaints the tree", async () => {
  await extension().activate();

  // The Deployments tree's third refresh trigger, and the one most exposed to
  // the B1 bug class above: unlike clusters.yaml and the receipt, ~/.memql/runs
  // is a DIRECTORY that has never existed on a machine that has never run an
  // install -- which is every machine the first time. A non-recursive watch of
  // a missing directory cannot be established and, on the declared 1.91 floor,
  // never recovers when the directory later appears. So activation has to
  // create it, and only a real host can show whether it did.
  const runsDir = defaultRunsDir();
  assert.ok(
    fs.existsSync(runsDir),
    `${runsDir} does not exist after activation. The Deployments tree must create the watch base before calling createFileSystemWatcher, or a run in flight never repaints the tree -- and nothing throws when it does not.`
  );

  const runFile = path.join(runsDir, "20260814T120000Z-install-hostsmoke.json");
  const watcher = vscode.workspace.createFileSystemWatcher(
    new vscode.RelativePattern(vscode.Uri.file(runsDir), "*.json")
  );
  let fired = false;
  watcher.onDidCreate(() => {
    fired = true;
  });
  watcher.onDidChange(() => {
    fired = true;
  });

  try {
    // Written repeatedly for the reason the case above documents: the watcher
    // is not established when createFileSystemWatcher returns, and a run log
    // is rewritten per step anyway -- so this is also what the real write
    // pattern looks like.
    await waitFor(
      `a RelativePattern watcher on ${runsDir} to fire`,
      () => {
        fs.writeFileSync(
          runFile,
          `{"version":1,"run":{"id":"20260814T120000Z-install-hostsmoke","instance":"local","kind":"install","startedAt":"2026-08-14T12:00:00Z","status":"running","items":[]}}\n`,
          "utf8"
        );
        return fired;
      },
      30_000,
      500
    );
  } finally {
    watcher.dispose();
    fs.rmSync(runFile, { force: true });
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

  ConnectionPanel.open(
    context,
    {
      clustersPath: path.join(os.tmpdir(), "memql-smoke-no-such-clusters.yaml"),
      connections,
      readExpiry: async () => undefined,
    },
    "smoke-cluster",
  );
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

  // The construct detail page (memql#3752), opened on a PROMOTED construct --
  // the case with no file at all, whose source has nowhere else to be shown
  // and which is where a developer first meets the seeded-versus-trained
  // distinction. It is also the case that would throw if the page assumed a
  // file existed.
  ConstructPanel.open(
    context,
    {
      name: "trainedResponder",
      kind: "logic",
      namespace: "",
      origin: "promoted",
      originPath: "",
      description: "host smoke fixture",
      runnable: true,
      runnableKind: "logic",
      args: [{ name: "spaceId", type: "string", required: true }],
      boundConcept: "",
      sourceHash: "abc123",
      source: "logic trainedResponder {\n  body { return 1 }\n}",
    },
    {
      // Rejecting rather than pretending, like the two hosts above: nothing
      // here clicks the cluster-source button, and a stub that resolved would
      // let a page that reached for the cluster on OPEN sail through.
      viewSourceFromCluster: () => {
        throw new Error("no cluster source in the smoke lane");
      },
      // `browseRowsInPortal` was the second dep here and is GONE with the
      // portal (epic memql#4984). It opened the portal's /concepts/<id> page
      // for a concept; MemQL OS has no concept browser, so the button was
      // removed rather than pointed at a page that answers 404.
    },
    // The cluster this record was "read from" (memql#4253). The smoke lane has
    // no connection, so "" is the honest answer -- and it is the value that
    // makes the two deps above refuse rather than fire, which is what keeps
    // them unreachable here.
    ""
  );
  expected.push("Construct: trainedResponder");

  // The instance page (memql#3739). Opened against a machine with NO local
  // cluster, which is the state it has to render first and the one an operator
  // on a fresh checkout sees: presence says `absent`, so the page offers
  // Create deployment and nothing else. The title carries the instance name,
  // which is how this case sees that the catalog read actually completed --
  // the panel opens titled "MemQL deployment" and renames itself once it has
  // read the machine.
  DeploymentPanel.show(context, {
    catalog: {
      clustersPath: path.join(os.tmpdir(), "memql-smoke-no-such-clusters.yaml"),
      receiptPath: path.join(os.tmpdir(), "memql-smoke-no-such-receipt.json"),
      runsDir: path.join(os.tmpdir(), "memql-smoke-no-such-runs"),
      presence: async () => ({
        verdict: "absent" as const,
        evidence: { receipt: false, registry: false },
        endpoint: "",
      }),
    },
    installRoot: os.tmpdir(),
    receiptFile: path.join(os.tmpdir(), "memql-smoke-no-such-receipt.json"),
    runsDir: path.join(os.tmpdir(), "memql-smoke-no-such-runs"),
    refreshTree: () => undefined,
    // Rejecting rather than pretending, like the two hosts above: nothing here
    // clicks a button, and a stub that resolved would let a page that reached
    // for the wizard on OPEN sail through.
    openInstallFlow: () => {
      throw new Error("no install flow in the smoke lane");
    },
  });
  expected.push("Deployment: local");

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

smoke("the remote instance page renders all three pipeline states", async () => {
  await closeAllTabs();
  const context = fakeContext();

  // Driven entirely through injected seams, because none of the three states
  // needs a cluster to be true and two of them are FAILURES of a read -- the
  // point of the design is that neither is an error, so neither should need a
  // broken cluster to reach.
  const base = {
    catalog: {
      clustersPath: path.join(os.tmpdir(), "memql-smoke-remote-clusters.yaml"),
      receiptPath: path.join(os.tmpdir(), "memql-smoke-no-such-receipt.json"),
      runsDir: path.join(os.tmpdir(), "memql-smoke-no-such-runs"),
      presence: async () => ({
        verdict: "absent" as const,
        evidence: { receipt: false, registry: false },
        endpoint: "",
      }),
      readClusters: async () => ({
        ok: true as const,
        file: { clusters: [{ name: "staging", endpoint: "api.example.com:443" }], selectedCluster: "staging" },
      }),
    },
    installRoot: os.tmpdir(),
    receiptFile: path.join(os.tmpdir(), "memql-smoke-no-such-receipt.json"),
    runsDir: path.join(os.tmpdir(), "memql-smoke-no-such-runs"),
    refreshTree: () => undefined,
    openInstallFlow: () => {
      throw new Error("no install flow in the smoke lane");
    },
  };

  try {
    for (const [label, port] of [
      // Not connected: the read never happened, which is its own sentence and
      // not a claim that the cluster has no deploy console.
      ["no connection", undefined],
      // Refused by the role gate.
      [
        "role gate",
        {
          getDeploymentStatus: () => Promise.reject(new Error("PERMISSION_DENIED")),
        } as never,
      ],
      // Answered.
      ["answered", { getDeploymentStatus: () => Promise.resolve({ environment: "staging" }) } as never],
    ] as const) {
      DeploymentPanel.show(
        context,
        { ...base, ...(port === undefined ? {} : { deployPort: () => port }) },
        "staging",
      );
      await waitFor(
        `the remote page (${label}) to title itself (saw: ${openTabLabels().join(", ")})`,
        () => openTabLabels().includes("Deployment: staging"),
        15_000
      );
      info(`remote instance page rendered: ${label}`);
    }
  } finally {
    await closeAllTabs();
    for (const entry of context.subscriptions) {
      entry.dispose();
    }
  }
});

// -----------------------------------------------------------------------------
// Cluster documents (memql#4248)
// -----------------------------------------------------------------------------

// TWO THINGS ONLY A HOST CAN ANSWER, and both are decided by machinery this
// process does not own: whether the workbench routes a NON-file uri to the
// registered content provider at all, and whether the language client -- whose
// selector this task narrowed to `scheme: 'file'` -- leaves that document
// alone. A unit test can assert the selector literal and nothing more.
//
// THE POSITIVE CONTROL IS THE POINT OF THE FIRST HALF. "No diagnostics" is
// true of a language server that never started, so this first proves the
// instrument moves: a deliberately broken .memql ON DISK must be diagnosed. If
// that fails, the second assertion was never going to mean anything.
//
// No cluster is required and none is used: the uri names a cluster nothing is
// connected to, so the provider answers with its not-connected notice, which is
// the state a reader hits most often anyway.
smoke("a cluster document opens read-only with no language-server diagnostics", async () => {
  const ext = extension();
  await ext.activate();

  const folder = vscode.workspace.workspaceFolders?.[0];
  assert.ok(folder !== undefined, "no workspace folder, so the language server has no root to serve");
  const brokenPath = path.join(folder.uri.fsPath, "cluster-document-control.memql");
  // The shape cmd/memql-lsp's own TestPublishDiagnostics_CleanAndBroken uses:
  // a logic missing its mandatory body, which is a lowering error.
  fs.writeFileSync(brokenPath, "logic oops {\n", "utf8");

  try {
    const broken = vscode.Uri.file(brokenPath);
    await vscode.window.showTextDocument(await vscode.workspace.openTextDocument(broken));
    await waitFor(
      "the language server to diagnose a broken .memql on disk (the control: without it, 'no diagnostics' below proves nothing)",
      () => vscode.languages.getDiagnostics(broken).length > 0,
      20_000
    );
    info("control: the language server is attached and diagnosing file: documents");

    const uri = vscode.Uri.parse("memql-cluster://nowhere/cognition/queries.memql?kind=query&name=x");
    const doc = await vscode.workspace.openTextDocument(uri);
    assert.match(doc.getText(), /Not connected to nowhere/);
    // The lens is registered for `{ scheme, language: 'memql' }`, so a document
    // the workbench types as plaintext would silently draw no header lens.
    assert.equal(
      doc.languageId,
      "memql",
      "the cluster document is not typed as memql, so its header lens would never render"
    );

    await new Promise((r) => setTimeout(r, 1500));
    assert.equal(vscode.languages.getDiagnostics(uri).length, 0, "the LSP must not diagnose a cluster document");
  } finally {
    await closeAllTabs();
    fs.rmSync(brokenPath, { force: true });
  }
});

// -----------------------------------------------------------------------------
// The portal handoff (memql#4251)
// -----------------------------------------------------------------------------

// WHAT ONLY A HOST CAN ANSWER: that activate() returns the api at all, that a
// real `vscode.Uri` parses into the path and query the handler reads, and --
// the one that matters -- that a link naming a cluster nobody registered
// changes NOTHING on disk. The parse rules and the landing decision are
// unit-tested away from `vscode` in src/handoff/; what those tests structurally
// cannot see is the file the handler is one prompt away from writing.
//
// The runner points HOME at a temp directory, so `~/.memql/clusters.yaml` below
// is that one and not the developer's own.
//
// NOTHING CLICKS "Add cluster...". The offer is a non-modal
// showInformationMessage, which resolves undefined when nothing answers it, so
// the handler falls through to `noCluster` and the byte-for-byte comparison is
// the assertion: a link may offer to add a cluster, and may never add one.

smoke("a portal link for an unregistered cluster is refused without side effects", async () => {
  const ext = extension();
  const api = (await ext.activate()) as { handleOpenUri(uri: vscode.Uri): Promise<{ outcome: string }> };
  const file = path.join(os.homedir(), ".memql", "clusters.yaml");
  const before = await fs.promises.readFile(file, "utf8").catch(() => "");
  const result = await api.handleOpenUri(
    vscode.Uri.parse("vscode://znasllc.memql/open?v=1&cluster=nowhere.test&kind=query&name=x")
  );
  assert.equal(result.outcome, "noCluster");
  const after = await fs.promises.readFile(file, "utf8").catch(() => "");
  assert.equal(after, before, "a link must not write clusters.yaml");
});

smoke("an ARTIFACT link for an unregistered cluster is refused without side effects", async () => {
  // The same claim as the case above, for the target MemQL OS actually fires
  // (memql#4748). It is worth stating twice because the artifact path forks
  // BEFORE the catalog read and could have grown its own way to reach the
  // registry: a link may offer to add a cluster, and may never add one.
  const ext = extension();
  const api = (await ext.activate()) as { handleOpenUri(uri: vscode.Uri): Promise<{ outcome: string }> };
  const file = path.join(os.homedir(), ".memql", "clusters.yaml");
  const before = await fs.promises.readFile(file, "utf8").catch(() => "");
  const result = await api.handleOpenUri(
    vscode.Uri.parse("vscode://znasllc.memql/open?v=1&cluster=nowhere.test&kind=artifact&id=v1%3Alibrary%3Aartifact%3Ax")
  );
  assert.equal(result.outcome, "noCluster");
  const after = await fs.promises.readFile(file, "utf8").catch(() => "");
  assert.equal(after, before, "an artifact link must not write clusters.yaml");
});

smoke("an artifact link with no id is refused by name, like any malformed link", async () => {
  // The headline bug this epic fixes points the other way too: every artifact
  // link used to be refused as "missing name", and a link that genuinely says
  // nothing must still be refused -- now naming the field it is actually
  // missing.
  const ext = extension();
  const api = (await ext.activate()) as {
    handleOpenUri(uri: vscode.Uri): Promise<{ outcome: string; detail: string }>;
  };
  const result = await api.handleOpenUri(
    vscode.Uri.parse("vscode://znasllc.memql/open?v=1&cluster=a.test&kind=artifact")
  );
  assert.equal(result.outcome, "refused");
  assert.match(result.detail, /\bid\b/);
});

smoke("a malformed portal link is refused by name", async () => {
  const ext = extension();
  const api = (await ext.activate()) as {
    handleOpenUri(uri: vscode.Uri): Promise<{ outcome: string; detail: string }>;
  };
  const result = await api.handleOpenUri(
    vscode.Uri.parse("vscode://znasllc.memql/open?v=9&cluster=a.test&kind=query&name=x")
  );
  assert.equal(result.outcome, "refused");
  assert.match(result.detail, /v=9/);
});

// -----------------------------------------------------------------------------
// Sign-in (memql#3403)
// -----------------------------------------------------------------------------

// The manifest half. "every command the manifest contributes is registered"
// above already proves the handlers exist; what it cannot see is whether an
// operator has any way to REACH them. A command contributed with no palette
// entry and no menu entry is registered, passes that case, and is invisible.
smoke("sign-in and sign-out are reachable from the palette and the Clusters view", async () => {
  const ext = extension();
  await ext.activate();

  const contributes = (
    ext.packageJSON as {
      contributes?: {
        menus?: {
          commandPalette?: { command: string; when?: string }[];
          "view/item/context"?: { command: string; when?: string }[];
        };
      };
    }
  ).contributes;

  const palette = contributes?.menus?.commandPalette ?? [];
  const itemContext = contributes?.menus?.["view/item/context"] ?? [];
  assert.ok(palette.length > 0 && itemContext.length > 0, "the menus contribution shape changed");

  for (const command of ["memql.clusters.signIn", "memql.clusters.signOut"]) {
    const inPalette = palette.find((entry) => entry.command === command);
    assert.ok(
      inPalette !== undefined,
      `${command} is not in contributes.menus.commandPalette, so it never appears in the palette`
    );
    // Every runtime command lives behind the trust gate, because
    // registerRuntimeSurface -- which registers them -- only runs in a trusted
    // window. A palette entry without the clause offers a command that is not
    // there.
    assert.equal(
      inPalette.when,
      "isWorkspaceTrusted",
      `${command}'s palette entry must carry the trust clause the runtime surface is gated on`
    );

    const inMenu = itemContext.find(
      (entry) =>
        entry.command === command && (entry.when ?? "").includes("viewItem == memqlCluster")
    );
    assert.ok(
      inMenu !== undefined,
      `${command} is not contributed to the Clusters view's item context menu, so right-clicking a cluster does not offer it`
    );
  }

  info("sign-in and sign-out are contributed to both surfaces");
});

// The handler half, driven through the workbench's command registry rather than
// called directly -- which is the only way to observe that the command
// RESOLVES. Two things can only fail in a host: `withProgress` and the
// `vscode.env` capabilities the flow binds. A handler that parked on a modal,
// or threw reaching for a member that does not exist outside an editor, hangs
// or rejects here and nowhere in the fast lane.
//
// The fixture cluster deliberately names NO identity service (no `issuer`, no
// `domain`, and an endpoint with no `api.` prefix to imply one), so the
// flow refuses with kind `misconfigured` before a port is bound or a browser is
// opened. That is what makes this case safe to run unattended: no network, no
// browser, and nothing persisted.
smoke("the sign-in command runs to completion in a real host", async () => {
  await extension().activate();

  const node = {
    cluster: { name: "smoke-no-issuer", endpoint: "10.0.0.4:50051" },
    selected: false,
  };

  // executeCommand resolves with the handler's return value and REJECTS if it
  // threw, so awaiting it is the assertion. A 20s ceiling turns a handler that
  // parked on a dialog into a failure rather than a hung lane.
  await Promise.race([
    vscode.commands.executeCommand("memql.clusters.signIn", node),
    new Promise((_resolve, reject) =>
      setTimeout(
        () =>
          reject(
            new Error(
              "memql.clusters.signIn did not resolve within 20s -- the handler is parked on something (an awaited message box, or a flow that opened a listener it should not have)"
            )
          ),
        20_000
      )
    ),
  ]);

  // Nothing was written. A cluster the flow refused before it started must not
  // acquire a client_id, a token, or an entry of its own.
  const clustersPath = defaultClustersPath();
  const raw = fs.existsSync(clustersPath) ? fs.readFileSync(clustersPath, "utf8") : "";
  assert.ok(
    !raw.includes("smoke-no-issuer"),
    `a refused sign-in wrote to ${clustersPath}:\n${raw}`
  );

  info("sign-in refused a cluster with no identity service without opening anything");
});

// Sign-out is the other side of the same wiring, and it is the half that
// actually TOUCHES both stores: SecretStorage (which exists only in a host) and
// the shared clusters.yaml.
smoke("the sign-out command clears the stored credential", async () => {
  await extension().activate();

  const clustersPath = defaultClustersPath();
  fs.writeFileSync(
    clustersPath,
    ["clusters:", "  - name: smoke-signout", "    endpoint: 10.0.0.4:50051", "    token: header.payload.signature", ""].join(
      "\n"
    ),
    "utf8"
  );

  const node = { cluster: { name: "smoke-signout", endpoint: "10.0.0.4:50051" }, selected: false };
  await vscode.commands.executeCommand("memql.clusters.signOut", node);

  const raw = fs.readFileSync(clustersPath, "utf8");
  assert.ok(
    raw.includes("smoke-signout"),
    `sign-out removed the cluster itself, not only its credential:\n${raw}`
  );
  assert.ok(
    !raw.includes("header.payload.signature"),
    `sign-out left the access token in ${clustersPath}:\n${raw}`
  );

  info("sign-out cleared the token and left the cluster entry intact");
});

// -----------------------------------------------------------------------------
// The "+" page (memql#3472)
// -----------------------------------------------------------------------------

// Every workbench command below is raced against a deadline. `waitFor` bounds
// how long a condition may stay false, but it AWAITS its check -- so a command
// that never resolves (a handler parked on a dialog the workbench will not show
// while the window is unfocused, say) would hang the whole lane with no output
// at all, which is the worst failure a CI can produce. A bounded command turns
// that into a named assertion failure.
async function withinDeadline<T>(what: string, work: Promise<T>, ms: number): Promise<T> {
  const marker = Symbol("timeout");
  const raced = await Promise.race([
    work,
    new Promise<typeof marker>((resolve) => setTimeout(() => resolve(marker), ms)),
  ]);
  if (raced === marker) {
    throw new Error(`timed out after ${ms}ms waiting for ${what}`);
  }
  return raced as T;
}

/** The page's title, which the workbench also uses as its tab label. */
const ADD_CLUSTER_TAB = "Add a MemQL cluster";

/** Every open tab carrying the add-a-cluster page's label, across all groups. */
function addClusterTabs(): vscode.Tab[] {
  return vscode.window.tabGroups.all
    .flatMap((group) => group.tabs)
    .filter((tab) => tab.label === ADD_CLUSTER_TAB);
}

// WHAT ONLY A HOST CAN SAY ABOUT THE "+".
//
// Which cards a verdict produces is decided in a pure module and pinned in the
// fast lane -- test/clusterPresence.test.ts deep-equals the whole menu for every
// verdict, order included -- so re-proving it here would buy duplicate coverage
// at the price of the flakiest thing in this lane. What that lane structurally
// cannot reach is whether the workbench really opens the page when the command
// runs, and whether a second "+" reveals the panel that is already open instead
// of starting a second wizard over the same machine. Both are properties of the
// tab model and `WebviewPanel.reveal`, and neither exists outside a host.
//
// WHAT THIS LANE CANNOT REACH EITHER, SAID PLAINLY. A landing card is a
// `<button data-choose>` inside the webview's iframe, and the extension host has
// no API that dispatches a DOM event into it: messages travel host -> webview
// through `postMessage`, and the click direction exists only inside the page's
// own script. Calling the panel's message handler from here would be this file
// talking to itself, not the workbench doing anything, so it is not attempted --
// which screen a chosen action lands on is state/addCluster.ts's decision and is
// unit-tested there.
//
// THIS SECTION REPLACED THE QUICK-PICK CASES (memql#3478). They drove
// `showAddClusterMenu` against a real quick pick; the menu, its one caller and
// the installer stub behind it are gone, and a page is what the "+" opens now.
smoke("the + opens the add-a-cluster page, and a second + reveals the one panel", async () => {
  await extension().activate();
  await closeAllTabs();

  try {
    // executeCommand resolves with the handler's return value and REJECTS if it
    // threw, so awaiting it is half the assertion: a panel constructor that
    // reached for something the host does not provide fails here.
    await withinDeadline(
      "memql.clusters.add to resolve",
      Promise.resolve(vscode.commands.executeCommand("memql.clusters.add")),
      15_000
    );

    // Tabs appear asynchronously -- createWebviewPanel returns before the
    // workbench has the tab in its model.
    await waitFor(
      `a "${ADD_CLUSTER_TAB}" tab to appear (saw: ${openTabLabels().join(", ")})`,
      () => addClusterTabs().length > 0,
      15_000
    );

    const input = addClusterTabs()[0]?.input;
    assert.ok(
      input instanceof vscode.TabInputWebview,
      `the "${ADD_CLUSTER_TAB}" tab is not a webview; something else in this extension now claims that label`
    );
    // CONTAINS, not equals: the workbench namespaces the type it hands back
    // (`mainThreadWebview-<viewType>`), so pinning the exact string would assert
    // an internal naming convention rather than the extension's own view type.
    assert.ok(
      input.viewType.includes("memqlAddCluster"),
      `the page opened under view type ${input.viewType}, which does not name memqlAddCluster`
    );

    // ONE PANEL. A second "Add a cluster" tab would be a second wizard over one
    // machine, and two runs against one k3d cluster is not a state anything
    // downstream is prepared for. The invariant lives in AddClusterPanel.show,
    // which reveals the open panel instead of constructing another -- and a
    // reveal is only observable against a real tab model.
    await withinDeadline(
      "a second memql.clusters.add to resolve",
      Promise.resolve(vscode.commands.executeCommand("memql.clusters.add")),
      15_000
    );

    // SETTLE FIRST, THEN HOLD. Two assertions in sequence, because there are
    // two different things that could be wrong and only one of them is a bug.
    //
    // `show` reveals the open panel with ViewColumn.Beside, which MOVES it to
    // another editor group. On VS Code 1.91.0 the tab model reports the panel
    // in both groups while that move is in flight, so an assertion fired the
    // instant the command resolves sees two tabs for one panel and fails --
    // which it did, on 1.91.0 only, while stable passed. That is model lag, not
    // a second wizard: a WebviewPanel owns exactly one tab, and `show` returned
    // the existing panel rather than constructing anything.
    //
    // So: wait for the model to settle at one, which a genuine second panel can
    // never satisfy because nothing would close it. THEN hold the assertion for
    // two seconds, which is what catches a late second tab. Collapsing these
    // into one poll would either fail on the transient or pass on the tab that
    // has not appeared yet.
    await waitFor(
      `the tab model to settle at one "${ADD_CLUSTER_TAB}" tab after the reveal ` +
        `(saw: ${openTabLabels().join(", ")})`,
      () => addClusterTabs().length === 1,
      10_000
    );
    for (let i = 0; i < 8; i += 1) {
      assert.equal(
        addClusterTabs().length,
        1,
        `a second "+" opened another page: ${openTabLabels().join(", ")}`
      );
      await new Promise((resolve) => setTimeout(resolve, 250));
    }

    info(`the + opened exactly one ${ADD_CLUSTER_TAB} tab and revealed it on the second press`);
  } finally {
    // Whatever happened, do not leave the page open for the cases below.
    await closeAllTabs();
  }
});

smoke("the presence probe answers in the host runtime, with no Docker and no hang", async () => {
  // The acceptance item that a unit test can only assert about an INJECTED
  // probe: the real one, on the host's own Node, against an address nothing
  // serves. Port 1 is never listenable, so this is a local, immediate refusal
  // -- nothing leaves the machine, and no container runtime is consulted.
  const started = Date.now();
  const answered = await defaultEndpointProbe("127.0.0.1:1", 1_000);
  const took = Date.now() - started;

  assert.equal(answered, false, "something answered on port 1, which cannot be a MemQL cluster");
  assert.ok(took < 5_000, `the probe took ${took}ms against a dead port; the menu would have waited on it`);
  info(`probe against a dead port answered in ${took}ms`);
});

smoke("presence detection reads this host's real HOME and finds nothing installed", async () => {
  await extension().activate();

  // The runner gives the host a pristine temp HOME, so the honest answer here
  // is `absent` -- and getting it exercises the real receipt and clusters.yaml
  // readers against a real (empty) home directory rather than injected stubs.
  const presence = new ClusterPresence({ clustersPath: defaultClustersPath() });
  const result = await presence.get();
  assert.equal(
    result.verdict,
    "absent",
    `expected no local cluster under the temp HOME ${os.homedir()}, got ${result.verdict} (evidence: receipt=${result.evidence.receipt}, registry=${result.evidence.registry})`
  );
  assert.equal(result.endpoint, "", "nothing should have been dialed with no evidence to dial about");
});

// -----------------------------------------------------------------------------
// Explicitly out of scope
// -----------------------------------------------------------------------------

// Not a placeholder for work left undone -- a standing, deliberate skip that
// keeps the lane honest about the line it draws. A smoke lane that quietly
// omitted the cluster-dependent half would read as covering more than it does.
//
// The line moved once (memql#3337): the read-only half of "what happens after
// you connect" is now exercised by ./live.ts when a credentialed cluster is
// configured. What stays here is what a process cannot assert about itself.
smoke("connection-dependent behaviour beyond a read-only probe", () => {
  skip(
    "by design: no writes and no pixels. Running a mutation, the write confirmation, session-defining an edited buffer, the deploy actions and their type-to-confirm phrases, and every 'does it look right' item are covered by the manual checklist in docs/public/language/vscode-runtime-panel-verification.md. The read-only live cases are in ./live.ts."
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
  registerLiveCases();
  await runCases();
}
