// The Add a cluster panel, screen by screen: the landing for every verdict, the
// install and repair form, the connect form, the run (mid-run with the log
// open and closed, stopping, failed with a remedy, stopped), the done screen
// with the recovery key, and the uninstall preview with its switches off and
// on and the typed phrase wrong and right -- including the uninstall of a
// cluster MemQL has no install record for.
//
// BUILT FROM THE PANEL'S OWN SCREEN FUNCTIONS (src/webview/addClusterScreens.ts)
// over the view models the panel builds, in the document the panel assigns
// (pageDocument with the panel's stylesheet), so a capture is the page an
// operator sees. The view models are real-shaped: the step labels, the
// removal rows (built by the same removalRows / sharedToolRows the panel
// calls), the reasons a script actually gives.

import { bodyThemeAttr } from "../../src/webview/appearance.js";
import {
  ADD_CLUSTER_STYLES,
  addedScreen,
  collectScreen,
  connectScreen,
  doneScreen,
  landingScreen,
  runScreen,
  uninstallPreviewScreen,
  uninstalledScreen,
  type CollectInput,
  type ConnectInput,
  type DoneInput,
  type RunInput,
  type UninstallPreviewInput,
} from "../../src/webview/addClusterScreens.js";
import { removalRows, sharedToolRows, type RowStep } from "../../src/install/removalPreview.js";
import { landingView, type Inputs, type LandingFacts } from "../../src/state/addCluster.js";
import type { PreflightCheck } from "../../src/state/preflight.js";
import { pageDocument } from "../../src/webview/ui/document.js";
import type { RegionParts } from "../../src/webview/ui/liveView.js";
import type { LogLine } from "../../src/webview/ui/protocol.js";
import { GALLERY_NONCE, GALLERY_NOW, type GalleryTheme, type Scenario } from "../harness.js";

// ---------------------------------------------------------------------------
// the page, as the panel assigns it
// ---------------------------------------------------------------------------

function doc(theme: GalleryTheme, title: string, screen: string, parts: RegionParts, escapeAct?: string): string {
  return pageDocument({
    nonce: GALLERY_NONCE,
    title,
    themeAttr: bodyThemeAttr(theme),
    screen,
    styles: ADD_CLUSTER_STYLES,
    ...(escapeAct === undefined ? {} : { escapeAct }),
    ...parts,
  });
}

function page(group: string, id: string, title: string, tab: string, build: () => RegionParts, escapeAct?: string): Scenario {
  return { id, group, title, render: (theme) => doc(theme, tab, id, build(), escapeAct) };
}

// ---------------------------------------------------------------------------
// the landing
// ---------------------------------------------------------------------------

const LANDING = "Add a cluster: landing";

function facts(overrides: Partial<LandingFacts>): LandingFacts {
  return { verdict: "absent", registered: false, hasReceipt: false, platform: "supported", signedIn: false, ...overrides };
}

function landing(id: string, title: string, f?: LandingFacts): Scenario {
  return page(LANDING, id, title, "Add a cluster", () => landingScreen(f === undefined ? {} : { view: landingView(f) }));
}

const landings: Scenario[] = [
  landing("install-landing-detecting", "Detecting (skeleton)"),
  landing("install-landing-absent", "Nothing local", facts({})),
  landing(
    "install-landing-listed",
    "Installed and listed, signed out",
    facts({ verdict: "installed-healthy", registered: true, hasReceipt: true }),
  ),
  landing(
    "install-landing-signed-in",
    "Installed and listed, signed in",
    facts({ verdict: "installed-healthy", registered: true, hasReceipt: true, signedIn: true }),
  ),
  landing(
    "install-landing-unreachable",
    "Installed and listed, not responding",
    facts({ verdict: "installed-unreachable", registered: true, hasReceipt: true }),
  ),
  landing(
    "install-landing-listed-make-up",
    "Listed, made by make up (no install record)",
    facts({ verdict: "installed-healthy", registered: true, hasReceipt: false }),
  ),
  landing(
    "install-landing-not-listed",
    "Installed, removed from the list",
    facts({ verdict: "installed-unreachable", registered: false, hasReceipt: true }),
  ),
  landing("install-landing-make-up", "Present, made by make up, not listed", facts({ verdict: "present-unreceipted" })),
  landing("install-landing-unsupported", "Unsupported computer", facts({ platform: "unsupported" })),
];

// ---------------------------------------------------------------------------
// the install and repair form
// ---------------------------------------------------------------------------

const FORM = "Install and repair: the form";

const TAGS = ["v0.21.3", "v0.21.2", "v0.20.9", "main"];

const EMPTY: Inputs = { domain: "memql.localhost", ownerFirstName: "", ownerLastName: "", ownerEmail: "", version: "v0.21.3" };
const ADA: Inputs = {
  domain: "memql.localhost",
  ownerFirstName: "Ada",
  ownerLastName: "Lovelace",
  ownerEmail: "ada@example.com",
  version: "v0.21.3",
};

const READY: PreflightCheck[] = [
  { label: "Installer", word: "Ready", tone: "ok" },
  {
    label: "Your password",
    word: "Needed",
    tone: "attention",
    note: "Asked once, to update the hosts file and trust a local certificate.",
  },
];

function form(id: string, title: string, input: CollectInput): Scenario {
  return page(FORM, id, title, input.action === "repair" ? "Repair MemQL" : "Install MemQL", () => collectScreen(input));
}

const forms: Scenario[] = [
  form("install-options-checking", "Install, checks still running", {
    action: "install",
    values: EMPTY,
    errors: [],
    versionChoices: TAGS,
    moreOpen: false,
  }),
  form("install-options", "Install, ready", {
    action: "install",
    values: ADA,
    errors: [],
    versionChoices: TAGS,
    checks: READY,
    moreOpen: false,
  }),
  form("install-options-more", "Install, more options open", {
    action: "install",
    values: ADA,
    errors: [],
    versionChoices: TAGS,
    checks: [{ label: "Installer", word: "Ready", tone: "ok" }],
    moreOpen: true,
  }),
  form("install-options-errors", "Install, problems after Install", {
    action: "install",
    values: { ...EMPTY, ownerFirstName: "Ada", ownerEmail: "ada@" },
    errors: [
      { field: "ownerLastName", message: "Enter your last name." },
      { field: "ownerEmail", message: "Enter a valid email address." },
    ],
    versionChoices: TAGS,
    checks: READY,
    moreOpen: false,
  }),
  form("install-options-remote", "Install, from a remote window", {
    action: "install",
    values: ADA,
    errors: [],
    versionChoices: [],
    checks: READY,
    moreOpen: false,
    remoteProblem: "This is a ssh-remote window. Open a local VS Code window to install MemQL on this computer.",
  }),
  form("install-options-password-refused", "Install, back from a refused password", {
    action: "install",
    values: ADA,
    errors: [],
    versionChoices: TAGS,
    checks: READY,
    moreOpen: false,
    passwordProblem: "Your password wasn't accepted. Nothing was changed.",
  }),
  form("install-repair", "Repair, everything recorded", {
    action: "repair",
    values: ADA,
    errors: [],
    versionChoices: [],
    checks: [
      { label: "Installer", word: "Ready", tone: "ok" },
      {
        label: "Images",
        word: "Replaced",
        tone: "attention",
        note: "Your checkout build is replaced by release v0.21.3. Rebuild from checkout brings it back.",
      },
    ],
    moreOpen: false,
  }),
  form("install-repair-missing", "Repair, owner not recorded", {
    action: "repair",
    values: { ...EMPTY, version: "" },
    errors: [],
    versionChoices: [],
    checks: [{ label: "Installer", word: "Missing", tone: "error", note: "Reinstall the MemQL extension." }],
    moreOpen: false,
  }),
];

// ---------------------------------------------------------------------------
// connect to a cluster elsewhere
// ---------------------------------------------------------------------------

const CONNECT = "Connect to a cluster";

function connect(id: string, title: string, input: Partial<ConnectInput>): Scenario {
  const full: ConnectInput = {
    values: { name: "", domain: "", endpoint: "", token: "" },
    errors: [],
    failure: "",
    probe: { state: "none" },
    derivation: "",
    composedEndpoint: "",
    moreOpen: false,
    ...input,
  };
  return page(CONNECT, id, title, "Add a cluster", () => connectScreen(full), "connectEscape");
}

const TYPED = { name: "staging", domain: "staging.example.com", endpoint: "", token: "" };

const connects: Scenario[] = [
  connect("install-connect", "Empty", {}),
  connect("install-connect-typed", "Typed, with the derived address", {
    values: TYPED,
    derivation: "Connects to api.staging.example.com:443",
    composedEndpoint: "api.staging.example.com:443",
  }),
  connect("install-connect-checking", "Checking the cluster", {
    values: TYPED,
    derivation: "Connects to api.staging.example.com:443",
    composedEndpoint: "api.staging.example.com:443",
    probe: { state: "running" },
  }),
  connect("install-connect-unreachable", "Not reachable: Add anyway", {
    values: TYPED,
    derivation: "Connects to api.staging.example.com:443",
    composedEndpoint: "api.staging.example.com:443",
    probe: {
      state: "failed",
      endpoint: "api.staging.example.com:443",
      reason: "no answer within 10 seconds",
    },
  }),
  connect("install-connect-errors", "Problems, more options open", {
    values: { name: "local", domain: "memql.localhost", endpoint: "", token: "mql_pat_abc" },
    errors: [
      { field: "name", message: 'a cluster named "local" already exists; edit it instead of adding it again' },
      { field: "domain", message: "That's this computer. Go back and choose the local cluster instead." },
      { field: "token", message: "A personal access token can't be used here. Leave this empty and sign in." },
    ],
    derivation: "Connects to api.memql.localhost:443",
    composedEndpoint: "api.memql.localhost:443",
  }),
  page(CONNECT, "install-connect-added", "Added", "Add a cluster", () =>
    addedScreen({
      name: "staging",
      address: "api.staging.example.com:443",
      osUrl: "https://os.staging.example.com/",
      hasToken: false,
      reachable: true,
    }),
  ),
];

// ---------------------------------------------------------------------------
// the run
// ---------------------------------------------------------------------------

const RUN = "Install: the run";
const STARTED = GALLERY_NOW - 192_000;

const LOG: LogLine[] = [
  { label: "Checking this computer", text: "Platform: darwin/arm64" },
  { text: "k3d      present=false" },
  { text: "docker access: ok" },
  { label: "Checking Docker", text: "Docker Desktop 4.39.0 is answering" },
  { label: "Installing tools", text: "Downloading k3d v5.9.0 into ~/.memql/bin" },
  { label: "Adding local addresses", text: "*.memql.localhost -> 127.0.0.1 (3 entries)" },
  { label: "Installing tools", text: "Downloading kubectl v1.36.3 into ~/.memql/bin" },
  { text: "sha256 verified" },
  { label: "Creating certificates", text: "Created a local certificate authority in ~/.memql/mkcert" },
  { label: "Creating the cluster", text: "INFO[0000] Prep: Network" },
  { text: "INFO[0001] Created network 'k3d-memql'" },
  { text: "INFO[0003] Creating node 'k3d-memql-server-0'" },
  { text: "INFO[0011] Starting cluster 'memql'" },
  { text: "INFO:  Waiting up to 900s for the MemQL workloads to become Available..." },
  { text: "INFO:  still waiting (60s/900s): identity, bff" },
];

const FAILED_LOG: LogLine[] = [
  ...LOG.slice(0, 9),
  { label: "Creating the cluster", text: "INFO[0000] Prep: Network", anchor: true },
  { text: "ERRO[0002] Failed to create cluster 'memql'", tone: "error" },
  { text: "Bind for 0.0.0.0:443 failed: port is already allocated", tone: "error" },
  { text: "exit 5: port 443 is already in use on this computer", tone: "error" },
];

function run(id: string, title: string, input: Partial<RunInput>): Scenario {
  const full: RunInput = {
    mode: "install",
    phase: "running",
    status: "Creating the cluster",
    stepText: "Step 10 of 16",
    startedAt: STARTED,
    failures: [],
    retryable: false,
    logsOpen: false,
    logLines: LOG,
    now: GALLERY_NOW,
    ...input,
  };
  const tab = full.mode === "uninstall" ? "Uninstall MemQL" : full.mode === "repair" ? "Repair MemQL" : "Install MemQL";
  return page(full.mode === "uninstall" ? UNINSTALL_RUN : RUN, id, title, tab, () => runScreen(full));
}

const PORT_FAILURE = {
  id: "clusterUp",
  line: "Port 443 is already in use on this computer.",
  next: "Retrying often helps. If it fails again, the log has the details.",
  remedy: "k3d cluster stop memql",
};

const runs: Scenario[] = [
  run("install-run-starting", "Starting (before the plan)", {
    status: "Starting",
    stepText: "",
    startedAt: GALLERY_NOW - 2_000,
    logLines: [],
  }),
  run("install-run", "Running, logs closed", { percent: 41 }),
  run("install-run-logs", "Running, logs open", { percent: 62, status: "Starting services 5 of 9", logsOpen: true }),
  run("install-run-stopping", "Stopping after the current step", {
    percent: 62,
    phase: "stopping",
    status: "Stopping after the current step",
  }),
  run("install-run-finishing", "Failed while other steps finish", {
    percent: 58,
    phase: "finishing",
    status: "Couldn't create the cluster",
    failures: [PORT_FAILURE],
    retryable: true,
    logsOpen: true,
    logLines: FAILED_LOG,
  }),
  run("install-run-failed", "Failed, with a remedy, log at the failed step", {
    percent: 58,
    phase: "failed",
    status: "Couldn't create the cluster",
    endedAt: GALLERY_NOW,
    failures: [PORT_FAILURE],
    retryable: true,
    logsOpen: true,
    logLines: FAILED_LOG,
  }),
  run("install-run-failed-password", "Failed: needs administrator access", {
    percent: 21,
    phase: "failed",
    status: "Couldn't add local addresses",
    stepText: "Step 6 of 16",
    endedAt: GALLERY_NOW,
    failures: [
      {
        id: "hostsBlock",
        line: "Couldn't edit /etc/hosts without administrator access.",
        next: "Run the command in a terminal, then retry.",
        remedy: "sudo ~/.memql/src/scripts/install/hosts-entries.sh --action=add --domain=memql.localhost",
      },
    ],
    retryable: true,
    logsOpen: false,
  }),
  run("install-run-failed-final", "Failed, not retryable", {
    percent: 4,
    phase: "failed",
    status: "Couldn't check this computer",
    stepText: "Step 1 of 16",
    endedAt: GALLERY_NOW,
    failures: [
      {
        id: "detect",
        line: "This computer can't run a local MemQL cluster.",
        next: "It runs on linux/amd64, darwin/arm64. Retrying won't change that.",
      },
    ],
    retryable: false,
    logLines: LOG.slice(0, 3),
  }),
  run("install-run-stopped", "Stopped", {
    percent: 41,
    phase: "stopped",
    status: "Stopped",
    endedAt: GALLERY_NOW,
  }),
  // No plan ever arrived, so the bar is empty; the installer's own error is
  // detail, and it is in the log, opened.
  run("install-run-couldnt-start", "Couldn't start", {
    phase: "failed",
    percent: 0,
    status: "Couldn't start",
    stepText: "",
    endedAt: GALLERY_NOW,
    failures: [{ id: "", line: "The install couldn't start.", next: "The log has the details." }],
    logsOpen: true,
    logLines: [
      {
        text: "ENOENT: no such file or directory, open '~/.vscode/extensions/znasllc.memql/staged/scripts/install/graph/install.json'",
        tone: "error",
        anchor: true,
      },
    ],
  }),
  run("install-repair-run", "Repairing", { mode: "repair", percent: 73, status: "Checking secure access", stepText: "Step 13 of 16" }),
];

// ---------------------------------------------------------------------------
// done
// ---------------------------------------------------------------------------

const DONE = "Install: done";

function done(id: string, title: string, input: Partial<DoneInput>): Scenario {
  const full: DoneInput = {
    kind: "installed",
    name: "memql",
    address: "api.memql.localhost:443",
    osUrl: "https://os.memql.localhost/",
    signedIn: false,
    canEnrol: false,
    claim: false,
    startedAt: GALLERY_NOW - 731_000,
    endedAt: GALLERY_NOW,
    stepText: "16 steps",
    logsOpen: false,
    logLines: LOG,
    now: GALLERY_NOW,
    ...input,
  };
  return page(DONE, id, title, full.kind === "added" ? "Add a cluster" : "Install MemQL", () => doneScreen(full));
}

const KEY = "mql_rk_7Q2x-9fKp-L3vD-8wZr-Tn4B-Hc6M-Ye1J";

const dones: Scenario[] = [
  done("install-done", "Installed, recovery key to save", {
    canEnrol: true,
    recoveryKey: { state: "claimed", value: KEY, revealed: false, copied: false },
  }),
  done("install-done-key-shown", "Recovery key shown and copied", {
    canEnrol: true,
    recoveryKey: { state: "claimed", value: KEY, revealed: true, copied: true },
  }),
  done("install-done-signed-in", "Repaired, already signed in", {
    kind: "repaired",
    signedIn: true,
    recoveryKey: { state: "alreadyClaimed", value: "", revealed: false, copied: false },
  }),
  done("install-done-claim", "Nobody owns it yet: claim", {
    claim: true,
    recoveryKey: { state: "awaitingOwner", value: "", revealed: false, copied: false },
  }),
  done("install-done-key-lost", "Recovery key lost", {
    canEnrol: true,
    recoveryKey: { state: "revealLost", value: "", revealed: false, copied: false },
  }),
  done("install-done-not-listed", "Installed, not added to the list", {
    notListed: true,
  }),
  done("install-done-reconnected", "Local cluster added (Connect to it)", {
    kind: "added",
    startedAt: undefined,
    endedAt: undefined,
    stepText: undefined,
    logLines: [],
  }),
];

// ---------------------------------------------------------------------------
// uninstall
// ---------------------------------------------------------------------------

const UNINSTALL = "Uninstall: the preview";
const UNINSTALL_RUN = "Uninstall: the run";

function step(id: string, overrides: Partial<RowStep> = {}): RowStep {
  return {
    id,
    description: "",
    action: "run",
    reason: "",
    preserved: false,
    target: "",
    elevation: "none",
    shared: false,
    sharedReason: "",
    ...overrides,
  };
}

const HOME = "/Users/ada";

/** What previewUninstall plans for a full install, as the preview carries it. */
const RECEIPTED = {
  removals: [
    step("removeCluster", { params: { kind: "stack", cluster: "memql" } }),
    step("removeCheckout", { params: { kind: "checkout", path: `${HOME}/.memql/src` } }),
    step("removeHostsBlock", { elevation: "sudo", params: { kind: "hostsEntries", "hosts-file": "/etc/hosts" } }),
    step("removeLocalCA", {
      shared: true,
      elevation: "user-trust",
      dependsOn: ["removeCluster"],
      params: { kind: "mkcertCA", caroot: `${HOME}/.memql/mkcert` },
    }),
    step("removeToolK3d", { shared: true, dependsOn: ["removeCluster"], params: { kind: "binary", path: `${HOME}/.memql/bin/k3d` } }),
    step("removeToolKubectl", {
      shared: true,
      dependsOn: ["removeCluster"],
      params: { kind: "binary", path: `${HOME}/.memql/bin/kubectl` },
    }),
    step("removeToolMkcert", {
      shared: true,
      dependsOn: ["removeLocalCA"],
      params: { kind: "binary", path: `${HOME}/.memql/bin/mkcert` },
    }),
  ],
  preserved: [] as RowStep[],
};

/** A receipted install that ADOPTED a cluster somebody already had: the cluster is kept. */
const ADOPTED = {
  removals: RECEIPTED.removals.filter((s) => s.id !== "removeCluster"),
  preserved: [step("removeCluster", { preserved: true, params: { kind: "stack", cluster: "memql" } })],
};

/** No install record at all: `make up`'s cluster, the one thing the preview can plan. */
const UNRECEIPTED = {
  removals: [] as RowStep[],
  preserved: [step("removeCluster", { preserved: true, params: { kind: "stack", cluster: "memql" } })],
};

function preview(
  id: string,
  title: string,
  plan: { removals: RowStep[]; preserved: RowStep[] },
  extra: Partial<UninstallPreviewInput> = {},
): Scenario {
  return page(
    UNINSTALL,
    id,
    title,
    "Uninstall MemQL",
    () =>
      uninstallPreviewScreen({
        loading: false,
        rows: removalRows(plan, HOME),
        sharedTools: sharedToolRows(plan),
        chosen: new Set<string>(),
        clusterName: "memql",
        ...extra,
      }),
    "uninstallBack",
  );
}

const previews: Scenario[] = [
  preview("install-uninstall-loading", "Reading what is installed", RECEIPTED, { loading: true }),
  preview("install-uninstall", "Switches off", RECEIPTED),
  preview("install-uninstall-switches-on", "Tools and certificate authority on", RECEIPTED, {
    chosen: new Set(["removeToolK3d", "removeToolKubectl", "removeLocalCA", "removeToolMkcert"]),
  }),
  preview("install-uninstall-kept-cluster", "An adopted cluster is kept", ADOPTED, {
    deleteData: { on: false, phrase: "" },
  }),
  preview("install-uninstall-data-wrong", "Delete data on, phrase wrong", ADOPTED, {
    deleteData: { on: true, phrase: "delete memql" },
  }),
  preview("install-uninstall-data-right", "Delete data on, phrase right", ADOPTED, {
    deleteData: { on: true, phrase: "delete memql data" },
  }),
  preview("install-uninstall-no-receipt", "No install record (make up): kept until data is deleted", UNRECEIPTED, {
    deleteData: { on: false, phrase: "" },
  }),
  preview("install-uninstall-no-receipt-confirmed", "No install record, delete confirmed", UNRECEIPTED, {
    deleteData: { on: true, phrase: "delete memql data" },
  }),
  preview("install-uninstall-nothing", "Nothing to uninstall, still in the list", UNRECEIPTED, {
    nothingHere: { removeFromList: true },
  }),
  // A read that failed is a failure, never "nothing here".
  preview("install-uninstall-unreadable", "Couldn't work out what would be removed", UNRECEIPTED, {
    unreadable: true,
  }),
];

const UNINSTALL_LOG: LogLine[] = [
  { label: "Removing the cluster", text: "Deleting k3d cluster 'memql'..." },
  { text: "INFO[0000] Deleting cluster 'memql'" },
  { text: "INFO[0004] Removing cluster details from default kubeconfig..." },
  { label: "Removing downloaded files", text: "Removed ~/.memql/src" },
];

const uninstallRuns: Scenario[] = [
  run("install-uninstall-running", "Uninstalling", {
    mode: "uninstall",
    percent: 38,
    status: "Removing the cluster",
    stepText: "Step 1 of 7",
    startedAt: GALLERY_NOW - 21_000,
    logLines: UNINSTALL_LOG.slice(0, 3),
  }),
  run("install-uninstall-stopping", "Stopping", {
    mode: "uninstall",
    phase: "stopping",
    percent: 38,
    status: "Stopping after the current step",
    stepText: "Step 1 of 7",
    startedAt: GALLERY_NOW - 21_000,
    logLines: UNINSTALL_LOG.slice(0, 3),
  }),
  run("install-uninstall-failed", "Failed: the hosts file needs the password", {
    mode: "uninstall",
    phase: "failed",
    percent: 81,
    status: "Couldn't remove local addresses",
    stepText: "Step 3 of 7",
    startedAt: GALLERY_NOW - 40_000,
    endedAt: GALLERY_NOW,
    failures: [
      {
        id: "removeHostsBlock",
        line: "Couldn't edit /etc/hosts without administrator access.",
        next: "Run the command in a terminal, then retry.",
        remedy: "sudo ~/.memql/src/scripts/install/remove-artifact.sh --kind=hostsEntries --marker=memql",
      },
    ],
    retryable: true,
    logsOpen: true,
    logLines: [
      ...UNINSTALL_LOG,
      { label: "Removing local addresses", text: "sudo: a password is required", tone: "error", anchor: true },
      { text: "exit 4: couldn't edit /etc/hosts without administrator access", tone: "error" },
    ],
  }),
  run("install-uninstall-stopped", "Stopped", {
    mode: "uninstall",
    phase: "stopped",
    percent: 38,
    status: "Stopped",
    stepText: "Step 2 of 7",
    startedAt: GALLERY_NOW - 30_000,
    endedAt: GALLERY_NOW,
    logLines: UNINSTALL_LOG,
  }),
  page(UNINSTALL_RUN, "install-uninstall-done", "Uninstalled, kept one", "Uninstall MemQL", () =>
    uninstalledScreen({
      removed: 3,
      kept: 1,
      followUpProblem: "",
      startedAt: GALLERY_NOW - 52_000,
      endedAt: GALLERY_NOW,
      logsOpen: false,
      logLines: UNINSTALL_LOG,
      now: GALLERY_NOW,
    }),
  ),
  page(UNINSTALL_RUN, "install-uninstall-done-followup", "Uninstalled, list not updated", "Uninstall MemQL", () =>
    uninstalledScreen({
      removed: 4,
      kept: 0,
      followUpProblem: 'the cluster is off this machine, but "memql" could not be removed from the cluster list: permission denied',
      stillListed: true,
      startedAt: GALLERY_NOW - 52_000,
      endedAt: GALLERY_NOW,
      logsOpen: false,
      logLines: UNINSTALL_LOG,
      now: GALLERY_NOW,
    }),
  ),
];

export const scenarios: readonly Scenario[] = [
  ...landings,
  ...forms,
  ...connects,
  ...runs,
  ...dones,
  ...previews,
  ...uninstallRuns,
];
