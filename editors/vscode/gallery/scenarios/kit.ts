// The kit specimen: every component in src/webview/ui/kit.ts, in every state,
// on pages built exactly the way a panel builds them (pageDocument + the
// page runtime), so a capture is the real thing at real size.
//
// The copy here is REAL-SHAPED on purpose -- the step labels, states and
// errors a person will actually read -- because a specimen filled with
// placeholder words hides the problems that only show with real lengths.

import { bodyThemeAttr } from "../../src/webview/appearance.js";
import { pageDocument } from "../../src/webview/ui/document.js";
import {
  actionBar,
  button,
  codeBlock,
  disclosure,
  emptyState,
  facts,
  field,
  head,
  logPane,
  notice,
  progress,
  skeleton,
  subhead,
  switchRow,
  textInput,
  type Act,
} from "../../src/webview/ui/kit.js";
import type { LogLine } from "../../src/webview/ui/protocol.js";
import { GALLERY_NONCE, GALLERY_NOW, withHostScript, type GalleryTheme, type Scenario } from "../harness.js";

const GROUP = "Kit";

interface KitPage {
  title: string;
  head?: string;
  body: string;
  actions?: string;
  styles?: string;
}

function kitDocument(theme: GalleryTheme, p: KitPage, screen: string): string {
  return pageDocument({
    nonce: GALLERY_NONCE,
    title: p.title,
    themeAttr: bodyThemeAttr(theme),
    screen,
    styles: p.styles,
    head: p.head ?? "",
    body: p.body,
    actions: p.actions ?? "",
  });
}

function scenario(id: string, title: string, build: () => KitPage): Scenario {
  return { id, group: GROUP, title, render: (theme) => kitDocument(theme, build(), id) };
}

/** A specimen caption: what the next thing is, in the gallery only. */
function caption(text: string): string {
  return `<p class="gallery-caption">${text}</p>`;
}

/** Specimen-only layout. Not kit CSS: a panel never shows two bars in its body. */
const SPECIMEN_STYLES = `
  .gallery-caption { margin: 22px 0 8px; color: var(--memql-subtle); font-size: 0.85em;
                     letter-spacing: 0.02em; }
  .gallery-caption:first-child { margin-top: 0; }
  .gallery-row { display: flex; flex-wrap: wrap; align-items: center; gap: 10px; }
  .gallery-bar { border: 1px solid var(--memql-border); border-radius: var(--memql-radius); overflow: hidden; }
  .gallery-bar .mq-actbar { border-top: 0; }
  .gallery-heads > .mq-head { padding-bottom: 14px; border-bottom: 1px dashed var(--memql-border); }
`;

const INSTALL_LOG: readonly LogLine[] = [
  { label: "Checking this computer", text: "macOS 15.3 on arm64, 32 GB memory" },
  { label: "Checking Docker", text: "Docker Desktop 4.39.0 is running" },
  { label: "Installing tools", text: "k3d v5.8.3 already in ~/.memql/bin" },
  { label: "Installing tools", text: "kubectl v1.32.2 already in ~/.memql/bin" },
  { label: "Adding local addresses", text: "*.memql.localhost resolves to 127.0.0.1" },
  { label: "Creating certificates", text: "Created a local certificate authority in ~/.memql/mkcert" },
  { label: "Creating the cluster", text: "INFO[0000] Prep: Network" },
  { label: "Creating the cluster", text: "INFO[0001] Created network 'k3d-memql'" },
  { label: "Creating the cluster", text: "INFO[0003] Creating node 'k3d-memql-server-0'" },
  { label: "Creating the cluster", text: "INFO[0011] Starting cluster 'memql'", tone: "muted" },
  { label: "Creating the cluster", text: "deployment.apps/bff condition met" },
];

const FAILED_LOG: readonly LogLine[] = [
  ...INSTALL_LOG.slice(0, 6),
  { label: "Creating the cluster", text: "INFO[0000] Prep: Network" },
  { label: "Creating the cluster", text: "ERRO[0002] Failed to create cluster 'memql'", tone: "error" },
  {
    label: "Creating the cluster",
    text: "Bind for 0.0.0.0:443 failed: port is already allocated",
    tone: "error",
  },
];

// ---------------------------------------------------------------------------
// A real page: head, facts, groups, the floor
// ---------------------------------------------------------------------------

const clusterPage = scenario("kit-page", "A cluster page", () => ({
  title: "Local cluster",
  head: head({
    title: "Local cluster",
    meta: "v0.21.3",
    back: { act: "back", label: "Clusters" },
    asideActs: [
      { act: "edit", label: "Edit" },
      { act: "remove", label: "Remove from list" },
    ],
  }),
  body:
    facts([
      { label: "Address", value: "https://api.memql.localhost", mono: true },
      { label: "Signed in as", value: "alex@example.com" },
      { label: "Session", value: "Renews in 52 minutes", muted: true },
      { label: "Installed by", value: "this editor" },
      { label: "Nodes", value: "" },
    ]) +
    subhead("MemQL OS") +
    facts([
      { label: "Address", value: "https://os.memql.localhost", mono: true },
      { label: "Edition", value: "Community" },
    ]) +
    subhead("Deployments", "0") +
    emptyState({ line: "Nothing is deployed to this cluster yet.", acts: [{ act: "deploy", label: "Deploy", tone: "secondary" }] }),
  actions: actionBar({
    state: "Connected",
    detail: "Signed in as alex@example.com",
    tone: "live",
    acts: [
      { act: "disconnect", label: "Disconnect" },
      { act: "openOs", label: "Open MemQL OS", tone: "primary" },
    ],
  }),
}));

// ---------------------------------------------------------------------------
// Buttons, heads and facts
// ---------------------------------------------------------------------------

const buttons = scenario("kit-buttons", "Buttons, heads and facts", () => ({
  title: "Buttons",
  styles: SPECIMEN_STYLES,
  head: head({ title: "Buttons, heads and facts", meta: "kit specimen" }),
  body:
    caption("Buttons: primary, secondary, danger, text") +
    `<div class="gallery-row">${[
      button({ act: "a", label: "Install", tone: "primary" }),
      button({ act: "b", label: "Deploy", tone: "secondary" }),
      button({ act: "c", label: "Uninstall", tone: "danger" }),
      button({ act: "d", label: "Cancel" }),
      button({ act: "e", label: "Show details", tone: "text" }),
    ].join("")}</div>` +
    caption("In flight") +
    `<div class="gallery-row">${[
      button({ act: "a", label: "Signing in", tone: "primary", busy: true }),
      button({ act: "b", label: "Checking", tone: "secondary", busy: true }),
      button({ act: "c", label: "Stopping", busy: true }),
    ].join("")}</div>` +
    caption("Heads") +
    `<div class="gallery-heads">${[
      head({ title: "Clusters" }),
      head({ title: "Deployments", meta: "12" }),
      head({ title: "staging", meta: "v0.21.3", back: { act: "back", label: "Deployments" } }),
      head({
        title: "A cluster with a long name that wraps onto a second line in a narrow pane",
        meta: "v0.21.3",
        asideActs: [
          { act: "edit", label: "Edit" },
          { act: "remove", label: "Remove from list" },
        ],
      }),
    ].join("")}</div>` +
    subhead("Facts", "mono, muted, missing") +
    facts([
      { label: "Address", value: "https://api.memql.localhost", mono: true },
      { label: "Image", value: "acrmemql.azurecr.io/memql/bff@sha256:5f1c9a0e27b84c3d9e6f0a1b2c3d4e5f60718293a4b5c6d7e8f9", mono: true },
      { label: "Signed in as", value: "alex@example.com" },
      { label: "Session", value: "Not signed in", muted: true },
      { label: "Version", value: "" },
      { label: "Status", valueHtml: `<strong>Healthy</strong> · 9 of 9 services` },
    ]),
}));

// ---------------------------------------------------------------------------
// Action bars
// ---------------------------------------------------------------------------

function barSpecimen(label: string, bar: string): string {
  return caption(label) + `<div class="gallery-bar">${bar}</div>`;
}

const actionBars = scenario("kit-actionbars", "Action bars", () => ({
  title: "Action bars",
  styles: SPECIMEN_STYLES,
  head: head({ title: "Action bars", meta: "state left, acts right, one button last" }),
  body:
    barSpecimen(
      "idle, one act",
      actionBar({ state: "Not installed", tone: "idle", acts: [{ act: "install", label: "Install", tone: "primary" }] }),
    ) +
    barSpecimen(
      "live, two acts",
      actionBar({
        state: "Connected",
        detail: "Signed in as alex@example.com",
        tone: "live",
        acts: [
          { act: "open", label: "Open MemQL OS", tone: "primary" },
          { act: "disconnect", label: "Disconnect" },
        ],
      }),
    ) +
    barSpecimen(
      "busy, no button",
      actionBar({ state: "Installing", detail: "Step 11 of 16", tone: "busy", acts: [{ act: "cancel", label: "Cancel" }] }),
    ) +
    barSpecimen(
      "warn, three acts",
      actionBar({
        state: "Signed out",
        detail: "The session ended on this computer",
        tone: "warn",
        acts: [
          { act: "remove", label: "Remove from list" },
          { act: "code", label: "Use a code instead" },
          { act: "signIn", label: "Sign in", tone: "primary" },
        ],
      }),
    ) +
    barSpecimen(
      "error",
      actionBar({
        state: "Couldn't connect",
        detail: "The cluster did not answer at api.memql.localhost",
        tone: "error",
        acts: [
          { act: "terminal", label: "Run in terminal" },
          { act: "retry", label: "Retry", tone: "primary" },
        ],
      }),
    ) +
    barSpecimen(
      "confirm, danger",
      actionBar({
        state: "Uninstall",
        tone: "warn",
        confirm: "Removes the cluster, its data and the 4 items above. Nothing else on this computer changes.",
        acts: [
          { act: "back", label: "Cancel" },
          { act: "uninstall", label: "Uninstall and delete data", tone: "danger" },
        ],
      }),
    ),
}));

// ---------------------------------------------------------------------------
// Forms and switches
// ---------------------------------------------------------------------------

const forms = scenario("kit-forms", "Fields and switches", () => ({
  title: "Uninstall MemQL",
  head: head({ title: "Uninstall MemQL", back: { act: "back", label: "Local cluster" } }),
  body:
    subhead("Connection") +
    field({
      id: "f-address",
      label: "Address",
      hint: "Where the cluster answers, for example https://api.memql.example.com",
      controlHtml: textInput({ id: "f-address", field: "address", type: "url", value: "https://api.memql.localhost", describedBy: "f-address-hint" }),
    }) +
    field({
      id: "f-name",
      label: "Name",
      error: "A cluster named local is already in the list.",
      controlHtml: textInput({ id: "f-name", field: "name", value: "local", invalid: true, describedBy: "f-name-error" }),
    }) +
    field({
      id: "f-email",
      label: "Email",
      controlHtml: textInput({ id: "f-email", field: "email", type: "email", placeholder: "you@example.com" }),
    }) +
    subhead("Also remove") +
    switchRow({ id: "s-k3d", label: "k3d", note: "Other clusters on this computer may use it.", checked: false, data: { "switch-act": "shared", value: "k3d" } }) +
    switchRow({ id: "s-kubectl", label: "kubectl", note: "Installed by MemQL into ~/.memql/bin.", checked: true, data: { "switch-act": "shared", value: "kubectl" } }) +
    switchRow({ id: "s-ca", label: "Local certificate authority", checked: false, disabled: true, note: "Still used by another cluster." }) +
    switchRow({
      id: "s-data",
      label: "Delete the cluster's data",
      note: "Removes every database in the cluster. This can't be undone.",
      checked: true,
      tone: "danger",
      data: { "switch-act": "deleteData" },
    }) +
    field({
      id: "f-confirm",
      label: "Type delete memql data to confirm",
      controlHtml: textInput({ id: "f-confirm", field: "confirm", value: "delete memql" }),
    }) +
    switchRow({ id: "s-danger-off", label: "Delete downloaded images", tone: "danger", checked: false }),
  actions: actionBar({
    state: "Ready to uninstall",
    tone: "idle",
    confirm: "Removes the cluster and its data, kubectl and the local addresses. Nothing else on this computer changes.",
    acts: [
      { act: "back", label: "Cancel" },
      { act: "uninstall", label: "Uninstall and delete data", tone: "danger" },
    ],
  }),
}));

// ---------------------------------------------------------------------------
// Notices, code, disclosures, the log, empty
// ---------------------------------------------------------------------------

const notices = scenario("kit-notices", "Notices, disclosures and the log", () => ({
  title: "Notices",
  styles: SPECIMEN_STYLES,
  head: head({ title: "Notices, disclosures and the log" }),
  body:
    notice({ tone: "info", line: "A newer version is available.", next: "v0.21.4 fixes sign-in on Windows." }) +
    notice({
      tone: "warn",
      line: "Your session ends in 5 minutes.",
      acts: [{ act: "signIn", label: "Sign in again" }],
    }) +
    notice({
      tone: "error",
      line: "Docker isn't running.",
      next: "Start Docker Desktop, then retry.",
      codeHtml: codeBlock({ text: "open -a Docker", act: { act: "runInTerminal", label: "Run in terminal" } }),
      acts: [
        { act: "retry", label: "Retry" },
        { act: "details", label: "Show details" },
      ],
    }) +
    caption("A command on its own") +
    codeBlock({ text: "memql-cockpit worker run --config ~/.memql/worker.yaml", act: { act: "copy", label: "Copy" } }) +
    disclosure({ act: "toggleDiag", id: "diag", label: "Show diagnostics", openLabel: "Hide diagnostics", open: false, meta: "3 checks", bodyHtml: facts([{ label: "Docker", value: "4.39.0" }]) }) +
    disclosure({
      act: "toggleLogs",
      id: "logs",
      label: "Show logs",
      openLabel: "Hide logs",
      open: true,
      meta: lineCount(INSTALL_LOG.length),
      bodyHtml: logPane({
        id: "install-log",
        ariaLabel: "Install log",
        lines: INSTALL_LOG,
        acts: [
          { act: "copyLog", label: "Copy" },
          { act: "openOutput", label: "Open in Output" },
        ],
      }),
    }) +
    caption("An empty log pane") +
    logPane({ id: "empty-log", ariaLabel: "Deploy log", lines: [], empty: "No output yet" }) +
    caption("Empty state") +
    emptyState({
      line: "No clusters yet.",
      acts: [
        { act: "install", label: "Install a local cluster", tone: "primary" },
        { act: "add", label: "Add an existing cluster", tone: "secondary" },
      ],
    }),
}));

// ---------------------------------------------------------------------------
// The progress screen, in each state
// ---------------------------------------------------------------------------

const STARTED = GALLERY_NOW - 192_000;

function progressPage(id: string, title: string, build: () => KitPage): Scenario {
  return { id, group: "Progress", title, render: (theme) => kitDocument(theme, build(), id) };
}

function logsDisclosure(open: boolean, lines: readonly LogLine[]): string {
  return disclosure({
    act: "toggleLogs",
    id: "run-logs",
    label: "Show logs",
    openLabel: "Hide logs",
    open,
    meta: lineCount(lines.length),
    bodyHtml: logPane({
      id: "run-log",
      ariaLabel: "Install log",
      lines,
      acts: [
        { act: "copyLog", label: "Copy" },
        { act: "openOutput", label: "Open in Output" },
      ],
    }),
  });
}

const CANCEL: Act = { act: "cancel", label: "Cancel" };

function lineCount(n: number): string {
  return n === 1 ? "1 line" : `${n} lines`;
}

const progressStart = progressPage("progress-start", "Starting (indeterminate)", () => ({
  title: "Installing MemQL",
  body:
    progress({ title: "Installing MemQL", status: "Checking this computer", stepText: "Step 1 of 16", startedAt: GALLERY_NOW - 2_000, state: "running", now: GALLERY_NOW }) +
    logsDisclosure(false, INSTALL_LOG.slice(0, 1)),
  actions: actionBar({ state: "Installing", tone: "busy", acts: [CANCEL] }),
}));

const progressRunning = progressPage("progress-running", "Running (40%)", () => ({
  title: "Installing MemQL",
  body:
    progress({ title: "Installing MemQL", percent: 40, status: "Creating the cluster", stepText: "Step 10 of 16", startedAt: STARTED, state: "running", now: GALLERY_NOW }) +
    logsDisclosure(false, INSTALL_LOG),
  actions: actionBar({ state: "Installing", tone: "busy", acts: [CANCEL] }),
}));

const progressLogs = progressPage("progress-logs", "Running, logs open", () => ({
  title: "Installing MemQL",
  body:
    progress({ title: "Installing MemQL", percent: 62, status: "Starting services 5 of 9", stepText: "Step 10 of 16", startedAt: STARTED, state: "running", now: GALLERY_NOW }) +
    logsDisclosure(true, INSTALL_LOG),
  actions: actionBar({ state: "Installing", tone: "busy", acts: [CANCEL] }),
}));

const progressStopping = progressPage("progress-stopping", "Stopping", () => ({
  title: "Installing MemQL",
  body:
    progress({ title: "Installing MemQL", percent: 62, status: "Stopping after the current step", stepText: "Step 10 of 16", startedAt: STARTED, state: "stopping", now: GALLERY_NOW }) +
    logsDisclosure(false, INSTALL_LOG),
  actions: actionBar({ state: "Stopping", tone: "busy", acts: [] }),
}));

const progressFailed = progressPage("progress-failed", "Failed", () => ({
  title: "Installing MemQL",
  body:
    progress({ title: "Installing MemQL", percent: 58, status: "Couldn't create the cluster", stepText: "Step 10 of 16", startedAt: STARTED, endedAt: GALLERY_NOW, state: "failed", now: GALLERY_NOW }) +
    notice({
      tone: "error",
      line: "Port 443 is already in use on this computer.",
      next: "Another local cluster is running. Stop it, then retry.",
      codeHtml: codeBlock({ text: "k3d cluster stop memql", act: { act: "runInTerminal", label: "Run in terminal" } }),
    }) +
    logsDisclosure(true, FAILED_LOG),
  actions: actionBar({
    state: "Couldn't install",
    tone: "error",
    acts: [CANCEL, { act: "retry", label: "Retry", tone: "primary" }],
  }),
}));

const progressDone = progressPage("progress-done", "Done", () => ({
  title: "MemQL is installed",
  body:
    progress({ title: "MemQL is installed", percent: 100, status: "Ready to sign in", stepText: "16 steps", startedAt: GALLERY_NOW - 731_000, endedAt: GALLERY_NOW, state: "done", now: GALLERY_NOW }) +
    logsDisclosure(false, INSTALL_LOG),
  actions: actionBar({
    state: "Installed",
    tone: "live",
    acts: [{ act: "signIn", label: "Sign in", tone: "primary" }],
  }),
}));

// ---------------------------------------------------------------------------
// Loading
// ---------------------------------------------------------------------------

const skeletonPage = scenario("kit-skeleton-page", "Loading a page", () => ({
  title: "Loading",
  body: skeleton({ shape: "page", label: "Loading cluster details" }),
}));

const skeletons = scenario("kit-skeletons", "Loading shapes", () => ({
  title: "Loading shapes",
  styles: SPECIMEN_STYLES,
  head: head({ title: "Loading shapes" }),
  body:
    caption("facts") +
    skeleton({ shape: "facts", rows: 4, label: "Loading cluster details" }) +
    caption("list") +
    skeleton({ shape: "list", rows: 3, label: "Loading deployments" }) +
    caption("form") +
    skeleton({ shape: "form", rows: 2, label: "Loading settings" }),
}));

// ---------------------------------------------------------------------------
// The runtime, driven: host messages applied to a live page
// ---------------------------------------------------------------------------

/**
 * A page that receives what a LiveView would send after `ready`: progress,
 * a burst of log lines, a patch of the action bar and a disclosure opened by
 * the host. The capture is the runtime's work, not the renderer's.
 */
const runtimeLive: Scenario = {
  id: "runtime-live",
  group: "Runtime",
  title: "Progress, log and patch applied in place",
  render(theme) {
    const doc = kitDocument(
      theme,
      {
        title: "Installing MemQL",
        body:
          progress({ title: "Installing MemQL", status: "Checking this computer", stepText: "Step 1 of 16", startedAt: STARTED, state: "running", now: GALLERY_NOW }) +
          logsDisclosure(false, []),
        actions: actionBar({ state: "Installing", tone: "busy", acts: [CANCEL] }),
      },
      "runtime-live",
    );
    const lines = JSON.stringify(
      Array.from({ length: 40 }, (_, n) => ({ label: "Creating the cluster", text: `INFO[${String(n).padStart(4, "0")}] line ${n + 1}` })),
    );
    const stopping = JSON.stringify(actionBar({ state: "Stopping", detail: "After the current step", tone: "busy", acts: [] }));
    return withHostScript(
      doc,
      `
setTimeout(function () {
  window.postMessage({ type: 'progress', percent: 71, status: 'Starting services 7 of 9', stepText: 'Step 10 of 16', startedAt: ${STARTED}, state: 'running' }, '*');
  window.postMessage({ type: 'log', lines: ${lines} }, '*');
  window.postMessage({ type: 'setDisclosure', id: 'run-logs', open: true }, '*');
  window.postMessage({ type: 'patch', regions: { actions: ${stopping} } }, '*');
}, 50);
`,
    );
  },
};

export const scenarios: readonly Scenario[] = [
  clusterPage,
  buttons,
  actionBars,
  forms,
  notices,
  skeletonPage,
  skeletons,
  progressStart,
  progressRunning,
  progressLogs,
  progressStopping,
  progressFailed,
  progressDone,
  runtimeLive,
];
