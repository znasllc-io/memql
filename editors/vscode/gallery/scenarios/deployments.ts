// The cluster page (the Deployments slice): every screen and state it has,
// rendered by the page's own screen functions, with the bars computed by the
// same act authority the panel uses -- so a capture shows what a person would
// get from that state, not a hand-drawn approximation of it.

import { bodyThemeAttr } from "../../src/webview/appearance.js";
import { pageDocument } from "../../src/webview/ui/document.js";
import type { RegionParts } from "../../src/webview/ui/liveView.js";
import type { LogLine } from "../../src/webview/ui/protocol.js";
import {
  DEPLOYMENT_STYLES,
  OTHER_VERSION,
  chooseVersionScreen,
  loadingScreen,
  localOverviewScreen,
  missingRunScreen,
  pullRebuildScreen,
  rebuildScreen,
  remoteOverviewScreen,
  runDetailScreen,
  runScreen,
  unavailableScreen,
} from "../../src/webview/deploymentScreens.js";
import { roleVisibility, visibleActions } from "../../src/deploy/actions.js";
import { localOverviewBar, remoteOverviewBar, runDetailBar } from "../../src/deploy/instanceActions.js";
import { failedStatus, runWords, type LocalRunRequest } from "../../src/deploy/localRun.js";
import type { PipelineState } from "../../src/deploy/pipelineState.js";
import type { UpgradeVerdict } from "../../src/deploy/upgrade.js";
import type { Instance, Run } from "../../src/state/deployments.js";
import type { ConnectionWord } from "../../src/state/deploymentsCatalog.js";
import { rebuildCheck } from "../../src/state/rebuildPreflight.js";
import { updateCheck } from "../../src/state/updatePreflight.js";
import { upgradePlan, upgradeSummary } from "../../src/state/upgradePlan.js";
import type { ReleaseListing } from "../../src/version/releaseCache.js";
import { GALLERY_NONCE, GALLERY_NOW, withHostScript, type GalleryTheme, type Scenario } from "../harness.js";

const GROUP = "Deployments";
const HOME = "/Users/alex";

function doc(theme: GalleryTheme, id: string, title: string, parts: RegionParts): string {
  return pageDocument({
    nonce: GALLERY_NONCE,
    title,
    themeAttr: bodyThemeAttr(theme),
    screen: id,
    styles: DEPLOYMENT_STYLES,
    ...parts,
  });
}

function scenario(id: string, title: string, build: () => RegionParts, pageTitle = "memql.localhost"): Scenario {
  return { id, group: GROUP, title, render: (theme) => doc(theme, id, pageTitle, build()) };
}

const iso = (minutesAgo: number): string => new Date(GALLERY_NOW - minutesAgo * 60_000).toISOString();

const RELEASES: ReleaseListing = { tags: ["v0.24.0", "v0.23.5", "v0.23.4", "v0.23.0", "v0.22.1"], fetchedAt: GALLERY_NOW };
const CURRENT_RELEASES: ReleaseListing = { tags: ["v0.23.5", "v0.23.4", "v0.23.0"], fetchedAt: GALLERY_NOW };

const LOCAL: Instance = {
  name: "local",
  label: "memql.localhost",
  kind: "local",
  presence: "installed-healthy",
  domain: "memql.localhost",
  version: "v0.23.5",
  versionLabel: "v0.23.5",
  connected: true,
  registered: true,
  checkout: `${HOME}/.memql/stack`,
  imageSource: "released",
  checkoutCommit: "3f2a9c1e5b7d",
};

const CHECKOUT_BUILD: Instance = {
  ...LOCAL,
  versionLabel: "Your build 8b41d07",
  checkoutBranch: "main",
  imageSource: "checkout",
  rebuild: { commit: "8b41d07aa31c", ref: "main", dirtyCount: 3, nodes: "bff agent cognition", recordedAt: iso(95) },
};

function localRun(over: Partial<Run> & Pick<Run, "id" | "kind" | "status">): Run {
  return { instance: "local", startedAt: iso(60), items: [], ...over };
}

const LOCAL_HISTORY: Run[] = [
  localRun({ id: "r5", kind: "upgrade", status: "succeeded", fromVersion: "v0.23.4", toVersion: "v0.23.5", startedAt: iso(60 * 26), finishedAt: iso(60 * 26 - 6) }),
  localRun({ id: "r4", kind: "rebuild", status: "succeeded", startedAt: iso(60 * 50), finishedAt: iso(60 * 50 - 14) }),
  localRun({ id: "r3", kind: "upgrade", status: "failed", fromVersion: "v0.23.4", toVersion: "v0.23.5", startedAt: iso(60 * 72), finishedAt: iso(60 * 72 - 4) }),
  localRun({ id: "r2", kind: "repair", status: "interrupted", startedAt: iso(60 * 24 * 4) }),
  localRun({ id: "r1", kind: "install", status: "succeeded", toVersion: "v0.23.4", startedAt: iso(60 * 24 * 9), finishedAt: iso(60 * 24 * 9 - 12) }),
];

const NONE: UpgradeVerdict = { kind: "none", reason: "no newer release" };
const OFFER: UpgradeVerdict = {
  kind: "offer",
  target: { instanceName: "memql.localhost", from: "v0.23.5", to: "v0.24.0", flow: "upgradeToTag" },
  label: "Update to v0.24.0",
  title: "Update memql.localhost",
  confirmation: "Update memql.localhost from v0.23.5 to v0.24.0?",
  phrase: "v0.24.0",
};

function localPage(instance: Instance, connection: ConnectionWord, upgrade: UpgradeVerdict, runs: readonly Run[], releases: ReleaseListing): RegionParts {
  return localOverviewScreen({
    instance,
    bar: localOverviewBar({ instance, connection, upgrade }),
    runs,
    nowMs: GALLERY_NOW,
    upgrade,
    releases,
    detailsOpen: false,
    home: HOME,
  });
}

// ---------------------------------------------------------------------------
// the local cluster
// ---------------------------------------------------------------------------

const localCurrent = scenario("deploy-local-current", "Local, connected, up to date", () =>
  localPage(LOCAL, "connected", NONE, LOCAL_HISTORY, CURRENT_RELEASES),
);

const localUpdate = scenario("deploy-local-update", "Local, update available", () =>
  localPage(LOCAL, "connected", OFFER, LOCAL_HISTORY, RELEASES),
);

const localCheckout = scenario("deploy-local-checkout", "Local, running your own build", () =>
  localPage(CHECKOUT_BUILD, "connected", NONE, LOCAL_HISTORY, RELEASES),
);

const localDetails = scenario("deploy-local-details", "Local, Details open", () => ({
  ...localOverviewScreen({
    instance: CHECKOUT_BUILD,
    bar: localOverviewBar({ instance: CHECKOUT_BUILD, connection: "connected", upgrade: NONE }),
    runs: LOCAL_HISTORY.slice(0, 2),
    nowMs: GALLERY_NOW,
    upgrade: NONE,
    releases: RELEASES,
    detailsOpen: true,
    home: HOME,
  }),
}));

const localSignIn = scenario("deploy-local-signin", "Local, signed out", () =>
  localPage({ ...LOCAL, connected: false }, "signIn", NONE, LOCAL_HISTORY.slice(0, 3), CURRENT_RELEASES),
);

const localNotRunning = scenario("deploy-local-notrunning", "Local, not running", () =>
  localPage({ ...LOCAL, presence: "installed-unreachable", connected: false }, "unreachable", NONE, LOCAL_HISTORY.slice(0, 3), CURRENT_RELEASES),
);

const localNotListed = scenario("deploy-local-notlisted", "Local, installed but not in the list", () =>
  localPage({ ...LOCAL, registered: false, connected: false }, "none", NONE, LOCAL_HISTORY.slice(0, 2), CURRENT_RELEASES),
);

const localEmpty = scenario("deploy-local-empty", "Local, no history yet", () =>
  localPage({ ...LOCAL, versionLabel: "main @ 3f2a9c1", version: "", checkoutBranch: "main" }, "connected", NONE, [], CURRENT_RELEASES),
);

const localRefused = scenario("deploy-local-refused", "Local, update needs manual steps", () =>
  localPage(
    { ...LOCAL, version: "v0.18.0", versionLabel: "v0.18.0" },
    "connected",
    {
      kind: "refused",
      target: { instanceName: "memql.localhost", from: "v0.18.0", to: "v0.24.0", flow: "upgradeToTag" },
      label: "Update to v0.24.0",
      message: "v0.24.0 needs manual upgrade steps.",
      barriers: [],
      docHref: "docs/public/operate/upgrade-barriers.md",
    },
    LOCAL_HISTORY.slice(0, 2),
    RELEASES,
  ),
);

const localAbsent = scenario(
  "deploy-local-absent",
  "Nothing installed",
  () =>
    localPage(
      { name: "local", kind: "local", presence: "absent", connected: false, registered: false },
      "none",
      NONE,
      [localRun({ id: "u1", kind: "uninstall", status: "succeeded", startedAt: iso(60 * 3), finishedAt: iso(60 * 3 - 1) })],
      RELEASES,
    ),
  "Local cluster",
);

const localUnreceipted = scenario("deploy-local-unreceipted", "A cluster this editor didn't install", () =>
  localPage({ name: "local", label: "memql.localhost", kind: "local", presence: "present-unreceipted", connected: false, registered: false }, "none", NONE, [], RELEASES),
);

// ---------------------------------------------------------------------------
// a remote cluster
// ---------------------------------------------------------------------------

const REMOTE: Instance = {
  name: "staging",
  label: "staging.memql.example.com",
  kind: "remote",
  presence: "installed-healthy",
  domain: "staging.memql.example.com",
  version: "v0.23.5",
  versionLabel: "v0.23.5",
  connected: true,
  currentDeploymentId: "dep-7f3a91",
  pendingDeploymentId: "dep-9c2e44",
};

function remoteRun(over: Partial<Run> & Pick<Run, "id" | "status">): Run {
  return { instance: "staging", kind: "rollout", startedAt: iso(60), items: [], ...over };
}

const REMOTE_HISTORY: Run[] = [
  remoteRun({ id: "dep-9c2e44", status: "running", fromVersion: "v0.23.5", toVersion: "v0.24.0", startedAt: iso(12) }),
  remoteRun({ id: "dep-7f3a91", status: "succeeded", fromVersion: "v0.23.4", toVersion: "v0.23.5", startedAt: iso(60 * 30), finishedAt: iso(60 * 30 - 7) }),
  remoteRun({ id: "dep-5d0b12", status: "superseded", fromVersion: "v0.23.0", toVersion: "v0.23.4", startedAt: iso(60 * 24 * 6), finishedAt: iso(60 * 24 * 6 - 9) }),
  remoteRun({ id: "dep-2a8f30", status: "failed", fromVersion: "v0.23.0", toVersion: "v0.23.3", startedAt: iso(60 * 24 * 8), finishedAt: iso(60 * 24 * 8 - 3) }),
];

const PRESENT: PipelineState = {
  kind: "present",
  line: "",
  engineMessage: "",
  actions: visibleActions(roleVisibility("owner")),
  rollouts: ["bff"],
};

function remotePage(instance: Instance, connection: ConnectionWord, pipeline: PipelineState | undefined, runs: readonly Run[]): RegionParts {
  return remoteOverviewScreen({
    instance,
    bar: remoteOverviewBar({ instance, connection, upgrade: NONE, pipeline, visibility: roleVisibility("owner"), runs }),
    connection,
    runs,
    nowMs: GALLERY_NOW,
    pipeline,
    upgrade: NONE,
    detailsOpen: false,
  });
}

const remoteConnected = scenario(
  "deploy-remote-connected",
  "Remote, owner, prepared version and a rollout",
  () => remotePage(REMOTE, "connected", PRESENT, REMOTE_HISTORY),
  "staging",
);

const remoteOutcome = scenario(
  "deploy-remote-outcome",
  "Remote, after starting a deployment",
  () => ({
    ...remoteOverviewScreen({
      instance: { ...REMOTE, pendingDeploymentId: undefined },
      bar: remoteOverviewBar({
        instance: { ...REMOTE, pendingDeploymentId: undefined },
        connection: "connected",
        upgrade: NONE,
        pipeline: { ...PRESENT, rollouts: [] },
        visibility: roleVisibility("developer"),
        runs: REMOTE_HISTORY,
      }),
      connection: "connected",
      runs: REMOTE_HISTORY,
      nowMs: GALLERY_NOW,
      pipeline: { ...PRESENT, rollouts: [] },
      upgrade: NONE,
      outcome: { tone: "info", line: "Deploying v0.24.0.", auditId: "aud-31c9", signIn: false },
      detailsOpen: false,
    }),
  }),
  "staging",
);

const remoteSignIn = scenario(
  "deploy-remote-signin",
  "Remote, signed out",
  () => remotePage({ ...REMOTE, connected: false, currentDeploymentId: undefined, pendingDeploymentId: undefined }, "signIn", undefined, []),
  "staging",
);

const remoteNotConfigured = scenario(
  "deploy-remote-notconfigured",
  "Remote, no deploy pipeline",
  () =>
    remotePage(
      { ...REMOTE, pendingDeploymentId: undefined },
      "connected",
      {
        kind: "notConfigured",
        line: "Deployments aren't set up for this cluster.",
        engineMessage: "deployment status unavailable (FAILED_PRECONDITION)",
        actions: [],
        rollouts: [],
      },
      REMOTE_HISTORY.slice(1),
    ),
  "staging",
);

// ---------------------------------------------------------------------------
// change version
// ---------------------------------------------------------------------------

const INSTALL_GRAPH = {
  name: "install",
  kind: "install" as const,
  description: "gallery",
  steps: ["detect", "dockerAccess", "toolK3d", "toolKubectl", "toolMkcert", "hostsBlock", "browserTrust", "localCA", "stackCheckout", "providerFederation", "clusterUp", "seedBootstrap", "frontDoor", "magicLink", "enrolmentLink", "recoveryKey"].map((id) => ({
    id,
    label: id,
    description: id,
    script: "x",
    elevation: "none" as const,
    retained: false,
    dependsOn: [],
    readOnly: id === "detect" || id === "dockerAccess" || id === "frontDoor",
  })),
} as unknown as Parameters<typeof upgradePlan>[0]["graph"];

const chooseLoading = scenario("deploy-choose-loading", "Change version, loading the list", () =>
  chooseVersionScreen({ instance: LOCAL, listing: undefined, choice: "", typed: "", typedError: "", target: "", plan: [], summary: "", sameVersion: false }),
);

const chooseEmpty = scenario("deploy-choose-empty", "Change version, nothing chosen", () =>
  chooseVersionScreen({ instance: LOCAL, listing: RELEASES, choice: "", typed: "", typedError: "", target: "", plan: [], summary: "", sameVersion: false }),
);

const chooseChosen = scenario("deploy-choose-chosen", "Change version, v0.24.0 chosen", () => {
  const plan = upgradePlan({ graph: INSTALL_GRAPH, from: "v0.23.5", to: "v0.24.0" });
  return chooseVersionScreen({ instance: LOCAL, listing: RELEASES, choice: "v0.24.0", typed: "", typedError: "", target: "v0.24.0", plan, summary: upgradeSummary(plan), sameVersion: false });
});

const chooseLane = scenario("deploy-choose-lane", "Change version over your own build", () => {
  const plan = upgradePlan({ graph: INSTALL_GRAPH, from: "v0.23.5", to: "v0.23.4" });
  return chooseVersionScreen({ instance: CHECKOUT_BUILD, listing: RELEASES, choice: "v0.23.4", typed: "", typedError: "", target: "v0.23.4", plan, summary: upgradeSummary(plan), sameVersion: false });
});

const chooseOther = scenario("deploy-choose-other", "Change version, typing one that isn't valid", () =>
  chooseVersionScreen({ instance: LOCAL, listing: RELEASES, choice: OTHER_VERSION, typed: "0.25", typedError: "Use a version like v0.24.0.", target: "", plan: [], summary: "", sameVersion: false }),
);

const chooseOffline = scenario("deploy-choose-offline", "Change version, list unavailable", () =>
  chooseVersionScreen({ instance: LOCAL, listing: { tags: [], fetchedAt: 0, error: "could not resolve host" }, choice: "", typed: "", typedError: "", target: "", plan: [], summary: "", sameVersion: false }),
);

// ---------------------------------------------------------------------------
// building from the checkout
// ---------------------------------------------------------------------------

const STATE = { commit: "8b41d07aa31c", ref: { kind: "branch" as const, name: "main" }, dirtyCount: 3, deployDirty: false };

const rebuildChecking = scenario("deploy-rebuild-checking", "Rebuild, checking", () =>
  rebuildScreen({ instance: CHECKOUT_BUILD, check: undefined, nodes: "", home: HOME }),
);

const rebuildReady = scenario("deploy-rebuild-ready", "Rebuild, ready (first build of a released cluster)", () =>
  rebuildScreen({
    instance: LOCAL,
    check: rebuildCheck({ dockerReachable: true, checkoutDir: `${HOME}/.memql/stack`, checkoutIsMemql: true, state: STATE, imageSource: "released", releasedTag: "v0.23.5" }),
    nodes: "",
    home: HOME,
  }),
);

const rebuildBlocked = scenario("deploy-rebuild-blocked", "Rebuild, Docker isn't running", () =>
  rebuildScreen({
    instance: CHECKOUT_BUILD,
    check: rebuildCheck({ dockerReachable: false, checkoutDir: `${HOME}/.memql/stack`, checkoutIsMemql: true, state: { ...STATE, deployDirty: true }, imageSource: "checkout", releasedTag: "v0.23.5" }),
    nodes: "bff, agent",
    home: HOME,
  }),
);

const pullReady = scenario("deploy-pull-ready", "Pull and rebuild, ready", () =>
  pullRebuildScreen({
    instance: CHECKOUT_BUILD,
    check: updateCheck({
      dockerReachable: true,
      checkoutDir: `${HOME}/.memql/stack`,
      checkoutIsMemql: true,
      state: STATE,
      imageSource: "checkout",
      releasedTag: "v0.23.5",
      update: { head: "8b41d07", branch: "main", detached: false, dirtyCount: 3, inProgress: "", shallow: false, remote: "origin", remoteHead: "c0ffee1", remoteError: "", ahead: 2, behind: 4 },
    }),
    nodes: "",
    home: HOME,
    merge: false,
    offerMerge: true,
  }),
);

const pullBlocked = scenario("deploy-pull-blocked", "Pull and rebuild, a merge in progress", () =>
  pullRebuildScreen({
    instance: CHECKOUT_BUILD,
    check: updateCheck({
      dockerReachable: true,
      checkoutDir: `${HOME}/.memql/stack`,
      checkoutIsMemql: true,
      state: STATE,
      imageSource: "checkout",
      releasedTag: "v0.23.5",
      update: { head: "8b41d07", branch: "main", detached: false, dirtyCount: 0, inProgress: "a merge", shallow: false, remote: "origin", remoteHead: "", remoteError: "", ahead: undefined, behind: undefined },
    }),
    nodes: "",
    home: HOME,
    merge: false,
    offerMerge: false,
  }),
);

// ---------------------------------------------------------------------------
// a run, as it happens
// ---------------------------------------------------------------------------

const UPDATE: LocalRunRequest = { kind: "update", instance: "local", label: "memql.localhost", from: "v0.23.5", to: "v0.24.0" };
const REBUILD: LocalRunRequest = { kind: "rebuild", instance: "local", label: "memql.localhost", checkout: `${HOME}/.memql/stack`, nodes: "" };
const STARTED = GALLERY_NOW - 184_000;

const UPDATE_LOG: LogLine[] = [
  { label: "Checking this computer", text: "macOS 15.3 on arm64, 32 GB memory" },
  { label: "Checking Docker", text: "Docker Desktop 4.39.0 is running" },
  { label: "Installing tools", text: "k3d v5.8.3 already in ~/.memql/bin" },
  { label: "Downloading MemQL", text: "HEAD is now at 1c2d3e4 release: v0.24.0" },
  { label: "Creating the cluster", text: "application.argoproj.io/memql-local configured" },
  { label: "Creating the cluster", text: "Waiting for bff to sync", tone: "muted" },
];

function runPage(
  request: LocalRunRequest,
  status: "running" | "stopping" | "failed" | "done" | "stopped",
  over: { percent?: number; statusLine: string; stepText: string; cancellable: boolean; logsOpen: boolean; failure?: { label: string; reason: string; remedy: string; retryable: boolean } },
): RegionParts {
  const words = runWords(request);
  return runScreen({
    words,
    status,
    progress: {
      ...(over.percent === undefined ? {} : { percent: over.percent }),
      status: over.statusLine,
      stepText: over.stepText,
      startedAt: STARTED,
      ...(status === "running" || status === "stopping" ? {} : { endedAt: GALLERY_NOW }),
      state: status === "stopped" ? "stopping" : status,
      ...(status === "done" ? { title: request.kind === "rebuild" ? "Rebuilt from your checkout" : "local is on v0.24.0" } : {}),
    },
    failure: over.failure,
    cancellable: over.cancellable,
    logsOpen: over.logsOpen,
    lines: over.logsOpen ? UPDATE_LOG : [],
    now: GALLERY_NOW,
  });
}

const runRunning = scenario("deploy-run-running", "Updating, running, logs closed", () =>
  runPage(UPDATE, "running", { percent: 58, statusLine: "Starting services 5 of 9", stepText: "Step 11 of 16", cancellable: true, logsOpen: false }),
);

const runLogs = scenario("deploy-run-logs", "Updating, running, logs open", () =>
  runPage(UPDATE, "running", { percent: 58, statusLine: "Starting services 5 of 9", stepText: "Step 11 of 16", cancellable: true, logsOpen: true }),
);

const runRebuilding = scenario("deploy-run-rebuild", "Rebuilding, one long step (no Cancel)", () =>
  runPage(REBUILD, "running", { percent: 34, statusLine: "Building images 3 of 9", stepText: "", cancellable: false, logsOpen: false }),
);

const runStopping = scenario("deploy-run-stopping", "Changing version, stopping", () =>
  runPage({ kind: "changeVersion", instance: "local", label: "memql.localhost", from: "v0.23.5", to: "v0.23.4" }, "stopping", { percent: 41, statusLine: "Stopping after the current step", stepText: "Step 9 of 16", cancellable: false, logsOpen: false }),
);

const runFailed = scenario("deploy-run-failed", "Updating, failed", () =>
  runPage(UPDATE, "failed", {
    percent: 44,
    statusLine: failedStatus("Creating the cluster"),
    stepText: "Step 11 of 16",
    cancellable: false,
    logsOpen: true,
    failure: {
      label: "Creating the cluster",
      reason: "Port 443 is already in use on this computer.",
      remedy: "k3d cluster stop memql",
      retryable: true,
    },
  }),
);

const runDone = scenario("deploy-run-done", "Updating, done", () =>
  runPage(UPDATE, "done", { percent: 100, statusLine: "Ready to use", stepText: "16 steps", cancellable: false, logsOpen: false }),
);

const runRebuilt = scenario("deploy-run-rebuilt", "Rebuilding, done", () =>
  runPage(REBUILD, "done", { percent: 100, statusLine: "local now runs your build (8b41d07, 3 uncommitted files).", stepText: "", cancellable: false, logsOpen: false }),
);

/** The live run as the page gets it: the document first, then the host's messages. */
const runLive: Scenario = {
  id: "deploy-run-live",
  group: GROUP,
  title: "Updating, progress and log streamed in",
  render(theme) {
    const parts = runPage(UPDATE, "running", { statusLine: "Starting", stepText: "", cancellable: true, logsOpen: true });
    const lines = JSON.stringify(UPDATE_LOG);
    return withHostScript(
      doc(theme, "deploy-run-live", "memql.localhost", { ...parts, body: parts.body.replace(/<div class="mq-log-line"[\s\S]*?<\/div><\/div>/g, "") }),
      `
setTimeout(function () {
  window.postMessage({ type: 'progress', percent: 62, status: 'Waiting for bff to sync', stepText: 'Step 11 of 16', startedAt: ${STARTED}, state: 'running' }, '*');
  window.postMessage({ type: 'log', lines: ${lines}, reset: true }, '*');
}, 50);
`,
    );
  },
};

// ---------------------------------------------------------------------------
// one run from the history
// ---------------------------------------------------------------------------

const LABELS = new Map([
  ["detect", "Checking this computer"],
  ["dockerAccess", "Checking Docker"],
  ["toolK3d", "Installing tools"],
  ["toolKubectl", "Installing tools"],
  ["toolMkcert", "Installing tools"],
  ["hostsBlock", "Adding local addresses"],
  ["stackCheckout", "Downloading MemQL"],
  ["clusterUp", "Creating the cluster"],
  ["seedBootstrap", "Creating your account"],
  ["frontDoor", "Checking secure access"],
]);

const FAILED_RUN: Run = localRun({
  id: "r3",
  kind: "upgrade",
  status: "failed",
  fromVersion: "v0.23.4",
  toVersion: "v0.23.5",
  startedAt: iso(60 * 72),
  finishedAt: iso(60 * 72 - 4),
  items: [
    { label: "detect", status: "skipped", detail: "the condition dependents needed already holds" },
    { label: "dockerAccess", status: "ok" },
    { label: "toolK3d", status: "skipped" },
    { label: "toolKubectl", status: "skipped" },
    { label: "toolMkcert", status: "skipped" },
    { label: "hostsBlock", status: "skipped" },
    { label: "stackCheckout", status: "ok", detail: "commit=1c2d3e4 dest=/Users/alex/.memql/stack refKind=tag" },
    { label: "clusterUp", status: "failed", detail: "Port 443 is already in use on this computer. · log=r3.clusterUp.log" },
    { label: "seedBootstrap", status: "pending" },
    { label: "frontDoor", status: "pending" },
  ],
});

const detailFailed = scenario("deploy-detail-failed", "A failed run from the history", () =>
  runDetailScreen({
    instance: LOCAL,
    run: FAILED_RUN,
    bar: runDetailBar({ instance: LOCAL, run: FAILED_RUN, connection: "connected", runInFlight: false }),
    nowMs: GALLERY_NOW,
    labels: LABELS,
    logs: new Map([["r3.clusterUp.log", "INFO[0000] Prep: Network\nERRO[0002] Failed to create cluster 'memql'\nBind for 0.0.0.0:443 failed: port is already allocated"]]),
    openLogs: new Set(["r3.clusterUp.log"]),
    detailsOpen: false,
  }),
);

const REMOTE_RUN: Run = remoteRun({
  id: "dep-5d0b12",
  status: "succeeded",
  fromVersion: "v0.23.0",
  toVersion: "v0.23.4",
  startedAt: iso(60 * 24 * 6),
  finishedAt: iso(60 * 24 * 6 - 9),
  items: [
    { label: "agent", status: "ok", detail: "v0.23.4 (inherited) · 2 replicas · digest sha256:91ac03d" },
    { label: "bff", status: "ok", detail: "v0.23.4 (inherited) · 2 replicas · digest sha256:5f1c9a0" },
    { label: "cognition", status: "ok", detail: "v0.23.4 (pinned) · 1 replica · digest sha256:0de771b" },
  ],
});

const detailRemote = scenario(
  "deploy-detail-remote",
  "A remote deployment, rollback offered",
  () =>
    runDetailScreen({
      instance: REMOTE,
      run: REMOTE_RUN,
      bar: runDetailBar({ instance: REMOTE, run: REMOTE_RUN, connection: "connected", pipeline: PRESENT, visibility: roleVisibility("owner"), runInFlight: false }),
      nowMs: GALLERY_NOW,
      labels: new Map(),
      logs: new Map(),
      openLogs: new Set(),
      detailsOpen: false,
    }),
  "staging",
);

const detailWhileRunning = scenario("deploy-detail-busy", "A past run while another is going", () =>
  runDetailScreen({
    instance: LOCAL,
    run: LOCAL_HISTORY[0]!,
    bar: runDetailBar({ instance: LOCAL, run: LOCAL_HISTORY[0]!, connection: "connected", runInFlight: true }),
    nowMs: GALLERY_NOW,
    labels: LABELS,
    logs: new Map(),
    openLogs: new Set(),
    detailsOpen: false,
  }),
);

const detailMissing = scenario("deploy-detail-missing", "A run no longer in the record", () => missingRunScreen(LOCAL, 50));

const detailMissingRemote = scenario("deploy-detail-gone", "A deployment no longer in the cluster's history", () =>
  missingRunScreen(REMOTE, 50),
);

// ---------------------------------------------------------------------------
// loading and unavailable
// ---------------------------------------------------------------------------

const loading = scenario("deploy-loading", "Loading", () => loadingScreen(), "Cluster");

const unavailable = scenario(
  "deploy-unavailable",
  "The cluster list won't read",
  () =>
    unavailableScreen({
      title: "Cluster",
      line: "Can't read your cluster list.",
      next: "clusters.yaml line 14: mapping values are not allowed here",
      noticeActs: [{ act: "openClusterList", label: "Open the list" }],
      bar: { state: "Unavailable", acts: [{ act: "back", label: "Try again", tone: "primary" }] },
    }),
  "Cluster",
);

export const scenarios: readonly Scenario[] = [
  localCurrent,
  localUpdate,
  localCheckout,
  localDetails,
  localSignIn,
  localNotRunning,
  localNotListed,
  localEmpty,
  localRefused,
  localAbsent,
  localUnreceipted,
  remoteConnected,
  remoteOutcome,
  remoteSignIn,
  remoteNotConfigured,
  chooseLoading,
  chooseEmpty,
  chooseChosen,
  chooseLane,
  chooseOther,
  chooseOffline,
  rebuildChecking,
  rebuildReady,
  rebuildBlocked,
  pullReady,
  pullBlocked,
  runRunning,
  runLogs,
  runRebuilding,
  runStopping,
  runFailed,
  runDone,
  runRebuilt,
  runLive,
  detailFailed,
  detailRemote,
  detailWhileRunning,
  detailMissing,
  detailMissingRemote,
  loading,
  unavailable,
];
