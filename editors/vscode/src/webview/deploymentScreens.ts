// The cluster page's screens, built from the page kit: the overview (local and
// remote), changing version, the two builds from the checkout, a run as it
// happens, and one run from the history.
//
// EVERY SCREEN IS THE KIT'S THREE PARTS. A head (the cluster's name and its
// version), a body (a short list of facts, then the history as a quiet list),
// and the action bar on the floor (the state in words and at most three acts,
// the one button last). Each function returns those three regions as HTML for
// LiveView, which assigns a document once per screen and patches it after --
// so nothing here builds a document, and the panel wraps them in one.
//
// NOTHING HERE DECIDES WHICH ACTS EXIST. deploy/instanceActions.ts computes the
// bar (state, acts, and what goes behind More) and this only draws it; the
// words a row or a state carries are state/deploymentsCatalog.ts's. A screen
// that composed its own acts would be a second authority, and the first thing
// a second authority does is offer an act the first one withheld.
//
// LOADING IS THE SHAPE OF THE CONTENT (the kit's skeleton), never a sentence;
// a read that failed says so; an empty history is a real empty state.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #4427 #4423 #3739 #3733

import { escapeHtml } from "@znasllc-io/memql-view-kit";

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
  type FactRow,
  type LogPaneInput,
} from "./ui/kit.js";
import type { RegionParts } from "./ui/liveView.js";
import type { LogLine, ProgressUpdate } from "./ui/protocol.js";

import type { PageAct, PageBar } from "../deploy/instanceActions.js";
import type { RunFailure, RunWords } from "../deploy/localRun.js";
import type { PipelineState } from "../deploy/pipelineState.js";
import type { UpgradeVerdict } from "../deploy/upgrade.js";
import { instanceLabel, type Instance, type Run } from "../state/deployments.js";
import {
  formatWhen,
  itemReason,
  relativeTime,
  runNoun,
  parseItemDetail,
  runDuration,
  runRowStatus,
  stepGroups,
  type ConnectionWord,
  type StepGroup,
} from "../state/deploymentsCatalog.js";
import type { CheckNotice, RebuildCheck } from "../state/rebuildPreflight.js";
import type { PlannedStepView } from "../state/upgradePlan.js";
import { checkoutSkew, shortCommit } from "../version/checkoutSkew.js";
import { describeVersion } from "../version/describe.js";
import type { ReleaseListing } from "../version/releaseCache.js";

/**
 * The page's own layout: the run and step lists, which the kit has no piece
 * for. LAYOUT ONLY -- every colour is a kit token (the dots are the kit's own
 * `mq-dot`), so a theme change restyles these with everything else.
 */
export const DEPLOYMENT_STYLES = `
  .dp-list { list-style: none; margin: 0; padding: 0; max-width: 80ch; }
  .dp-row { display: flex; align-items: center; gap: 10px; box-sizing: border-box; width: calc(100% + 16px);
            min-height: calc(var(--memql-control-h) + 6px); margin: 0 -8px; padding: 3px 8px; font: inherit;
            color: inherit; text-align: left; background: none; border: 0; border-radius: var(--memql-radius); }
  button.dp-row { cursor: pointer; }
  button.dp-row:hover { background: var(--memql-raised); }
  button.dp-row:focus-visible { outline: 1px solid var(--memql-focus); outline-offset: -1px; }
  .dp-row-label { flex: none; }
  .dp-row-desc { flex: 1 1 auto; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
                 color: var(--memql-muted); font-variant-numeric: tabular-nums; }
  .dp-row-reason { flex-basis: 100%; margin: -2px 0 4px 18px; color: var(--memql-muted); }
  .dp-note { margin: 6px 0 0; color: var(--memql-muted); }
  .dp-lede { margin: 0 0 14px; max-width: 80ch; color: var(--memql-muted); }
  .dp-select { appearance: auto; padding: 0 6px; }
  .dp-fact-act { margin-left: 4px; }
  .dp-fact-act > .mq-textbtn { height: auto; vertical-align: baseline; }
  .dp-log { margin: 0 0 6px 18px; }
  .dp-log .mq-disclosure { margin: 2px 0 0; }
  .dp-bullets { margin: 0; padding-left: 18px; max-width: 80ch; line-height: 1.9; }
  .dp-bullets li::marker { color: var(--memql-subtle); }
  .mq-facts + :is(.mq-field, .mq-skeleton, .mq-switch) { margin-top: 18px; }
  .mq-notice + .mq-field { margin-top: 16px; }
`;

/** A bar act as the kit draws it. */
function kitAct(a: PageAct): Act {
  return {
    act: a.id,
    label: a.label,
    ...(a.value === undefined ? {} : { value: a.value }),
    ...(a.tone === undefined ? {} : { tone: a.tone }),
  };
}

/** The action bar a `PageBar` describes. */
export function barHtml(b: PageBar, extra: readonly Act[] = []): string {
  return actionBar({
    state: b.state,
    ...(b.detail === undefined ? {} : { detail: b.detail }),
    tone: b.tone,
    acts: [...extra, ...b.acts.map(kitAct)],
  });
}

/** A page-produced sentence: an act that was refused, a confirmation that did not match. */
export interface PageNotice {
  tone: "info" | "warn" | "error";
  line: string;
  next?: string;
  acts?: readonly Act[];
}

function pageNotice(n: PageNotice | undefined): string {
  if (n === undefined) return "";
  return notice({ tone: n.tone, line: n.line, ...(n.next === undefined ? {} : { next: n.next }), ...(n.acts === undefined ? {} : { acts: n.acts }) });
}

/** A path as the page shows it: under the home directory it reads `~/...`. */
function masked(path: string, home: string): string {
  const trimmed = home.replace(/[\\/]+$/, "");
  return trimmed.length < 2 ? path : path.split(trimmed).join("~");
}

/** Where MemQL OS answers for a domain, as shown and as opened. */
export function memqlOsAddress(domain: string | undefined): { shown: string; url: string } | undefined {
  const d = (domain ?? "").trim().replace(/^https?:\/\//, "").replace(/\/+$/, "");
  if (d === "") return undefined;
  return { shown: `os.${d}`, url: `https://os.${d}/` };
}

/** A fact whose value carries one quiet act beside it (Open). */
function factWithAct(value: string, act: Act, mono = true): string {
  const text = mono ? `<span class="mq-mono">${escapeHtml(value)}</span>` : escapeHtml(value);
  return `${text}<span class="dp-fact-act">${button({ ...act, tone: "text" })}</span>`;
}

// ---------------------------------------------------------------------------
// the history
// ---------------------------------------------------------------------------

/**
 * The history, newest first, as a quiet list: a dot, what happened, and from
 * where to where and when. Each row opens the run.
 */
export function runList(runs: readonly Run[], nowMs: number, preparedId = ""): string {
  if (runs.length === 0) return emptyState({ line: "No history yet." });
  const rows = runs
    .map((run) => {
      const row = runRowStatus(run, nowMs, { prepared: preparedId !== "" && run.id === preparedId });
      return (
        `<li><button type="button" class="dp-row" data-act="openRun" data-value="${escapeHtml(run.id)}"` +
        ` title="${escapeHtml(row.tooltip)}"><span class="mq-dot" data-tone="${row.tone}" aria-hidden="true"></span>` +
        `<span class="dp-row-label">${escapeHtml(row.label)}</span>` +
        `<span class="dp-row-desc">${escapeHtml(row.description)}</span></button></li>`
      );
    })
    .join("");
  return `<ul class="dp-list" aria-label="History">${rows}</ul>`;
}

/**
 * A SAVED log, drawn with the kit's log pane but NOT as a live log region.
 *
 * The page runtime writes every `log` message into every `data-region="log"`
 * on the page, and LiveView re-sends its buffer -- with a reset -- whenever a
 * new document says `ready`. A run's page that had been watching a run would
 * therefore empty a recorded step's log, or fill it with the last run's
 * lines, the moment the run's own page came up. A saved log is a record, not
 * a stream, so it is not addressable by the stream.
 */
export function savedLogPane(i: LogPaneInput): string {
  return logPane(i).replace(' data-region="log"', "");
}

/** A Details disclosure over facts, or "" when there are none. */
function details(open: boolean, rows: readonly FactRow[], extraHtml = ""): string {
  const present = rows.filter((row) => (row.value ?? "") !== "" || row.valueHtml !== undefined);
  if (present.length === 0 && extraHtml === "") return "";
  return disclosure({
    act: "toggleDetails",
    id: "dp-details",
    label: "Details",
    open,
    bodyHtml: (present.length === 0 ? "" : facts(present)) + extraHtml,
  });
}

// ---------------------------------------------------------------------------
// loading and unavailable
// ---------------------------------------------------------------------------

/** Before the first read lands: the shape of the page, and no words on it. */
export function loadingScreen(): RegionParts {
  return { head: "", body: skeleton({ shape: "page", rows: 4, label: "Loading cluster" }), actions: "" };
}

export interface UnavailableInput {
  title: string;
  line: string;
  next?: string;
  /** The fix, when the page can do it (open the file the fault is in). */
  noticeActs?: readonly Act[];
  bar: { state: string; acts: readonly Act[] };
}

/**
 * The page could not be built: the cluster list would not read, or the cluster
 * it was opened for is no longer in it. Said, with the way out -- never the
 * skeleton forever.
 */
export function unavailableScreen(i: UnavailableInput): RegionParts {
  return {
    head: head({ title: i.title }),
    body: notice({
      tone: "error",
      line: i.line,
      ...(i.next === undefined ? {} : { next: i.next }),
      ...(i.noticeActs === undefined || i.noticeActs.length === 0 ? {} : { acts: i.noticeActs }),
    }),
    actions: actionBar({ state: i.bar.state, tone: "error", acts: i.bar.acts }),
  };
}

// ---------------------------------------------------------------------------
// the local cluster
// ---------------------------------------------------------------------------

export interface LocalOverviewInput {
  instance: Instance;
  bar: PageBar;
  runs: readonly Run[];
  nowMs: number;
  upgrade: UpgradeVerdict;
  releases: ReleaseListing | undefined;
  /** A page-produced sentence, at the top of the body. */
  notice?: PageNotice;
  detailsOpen: boolean;
  /** For showing paths as `~/...`. */
  home: string;
}

/** Whether the local page says anything about the machine beyond "absent". */
function isInstalled(instance: Instance): boolean {
  return instance.presence === "installed-healthy" || instance.presence === "installed-unreachable";
}

/**
 * The local cluster: its name and version, a short list of facts, its history.
 *
 * THE FACTS ARE ONLY THE KNOWN ONES. No "not fetched", no "not recorded": a
 * fact that is not known is not a row. The update fact appears only when it
 * says something the bar does not -- up to date, or a newer release that needs
 * manual steps (with the guide beside it); an update that can be taken is the
 * bar's primary act, and saying it twice is saying it once too many.
 */
export function localOverviewScreen(i: LocalOverviewInput): RegionParts {
  const { instance } = i;
  const installed = isInstalled(instance);
  const title = installed ? instanceLabel(instance) : "Local cluster";
  const meta = installed ? (instance.versionLabel ?? "") : "";

  let body = pageNotice(i.notice);
  if (instance.presence === "absent") {
    body += `<p class="dp-lede">Install a local cluster to run MemQL on this computer.</p>`;
  } else if (instance.presence === "present-unreceipted") {
    body += `<p class="dp-lede">This computer has a MemQL cluster that this editor didn't install.</p>`;
  }

  if (installed) {
    const rows: FactRow[] = [];
    const os = memqlOsAddress(instance.domain);
    if (os !== undefined) rows.push({ label: "MemQL OS", valueHtml: factWithAct(os.shown, { act: "openOs", label: "Open" }) });
    if (instance.imageSource === "checkout" && instance.rebuild !== undefined) {
      const when = relativeTime(instance.rebuild.recordedAt, i.nowMs);
      const dirty = instance.rebuild.dirtyCount;
      rows.push({
        label: "Built",
        value: [when, dirty !== undefined && dirty > 0 ? `${dirty} uncommitted ${dirty === 1 ? "file" : "files"}` : ""]
          .filter((part) => part !== "")
          .join(" · "),
      });
    } else {
      const update = updateFact(instance, i.upgrade, i.releases);
      if (update !== undefined) rows.push(update);
    }
    if (skewDiverged(instance)) rows.push({ label: "Extension", value: "From a different commit than your checkout" });
    if (rows.length > 0) body += facts(rows);
  }

  if (i.runs.length > 0 || installed) {
    body += subhead("History") + runList(i.runs, i.nowMs);
  }

  if (installed) {
    body += details(i.detailsOpen, localDetailRows(instance, i.home));
  }

  return {
    head: head({ title, ...(meta === "" ? {} : { meta }) }),
    body,
    actions: barHtml(i.bar),
  };
}

/** The update fact, when it says something the bar does not. */
function updateFact(instance: Instance, upgrade: UpgradeVerdict, releases: ReleaseListing | undefined): FactRow | undefined {
  if (upgrade.kind === "refused") {
    return {
      label: "Update",
      valueHtml:
        `${escapeHtml(upgrade.message)}` +
        (upgrade.docHref === ""
          ? ""
          : `<span class="dp-fact-act">${button({ act: "openGuide", label: "How to update", value: upgrade.docHref, tone: "text" })}</span>`),
    };
  }
  if (upgrade.kind === "offer") return undefined;
  const described = describeVersion({ recorded: instance.version, listing: releases });
  return described.state === "current" ? { label: "Update", value: "Up to date" } : undefined;
}

function skewDiverged(instance: Instance): boolean {
  if ((instance.checkout ?? "") === "") return false;
  return (
    checkoutSkew({
      extensionCommit: instance.extensionCommit,
      extensionDirty: instance.extensionDirty,
      checkoutCommit: instance.checkoutCommit,
    }).state === "diverged"
  );
}

/**
 * What a support case needs about a local cluster, one click away: where the
 * checkout is (with a way to open it), which images it runs, the last rebuild,
 * and which build of this extension is driving it.
 */
function localDetailRows(instance: Instance, home: string): FactRow[] {
  const rows: FactRow[] = [];
  const checkout = instance.checkout ?? "";
  if (checkout !== "") rows.push({ label: "Source folder", valueHtml: factWithAct(masked(checkout, home), { act: "openCheckout", label: "Open" }) });
  if (instance.checkoutBranch !== undefined && instance.checkoutBranch !== "") rows.push({ label: "Branch", value: instance.checkoutBranch, mono: true });
  if (instance.checkoutCommit !== undefined && instance.checkoutCommit !== "") rows.push({ label: "Commit", value: shortCommit(instance.checkoutCommit), mono: true });
  if (instance.imageSource !== undefined) {
    rows.push({ label: "Images", value: instance.imageSource === "checkout" ? "Built from your checkout" : "Released" });
  }
  if (instance.rebuild !== undefined) {
    rows.push({ label: "Last build", value: shortCommit(instance.rebuild.commit), mono: true });
    if (instance.rebuild.nodes !== "") rows.push({ label: "Services built", value: instance.rebuild.nodes, mono: true });
  }
  const skew = checkoutSkew({
    extensionCommit: instance.extensionCommit,
    extensionDirty: instance.extensionDirty,
    checkoutCommit: instance.checkoutCommit,
  });
  if (skew.state === "same") rows.push({ label: "Extension build", value: "Same commit as your checkout" });
  if (skew.state === "diverged") rows.push({ label: "Extension build", value: skew.terse });
  if (instance.domain !== undefined) rows.push({ label: "Domain", value: instance.domain, mono: true });
  return rows;
}

// ---------------------------------------------------------------------------
// a remote cluster
// ---------------------------------------------------------------------------

/** What the last deploy-control act came to. */
export interface ActOutcome {
  tone: "info" | "error";
  line: string;
  /** The act's audit event, for Details. */
  auditId: string;
  /** The fix is a sign-in. */
  signIn: boolean;
}

export interface RemoteOverviewInput {
  instance: Instance;
  bar: PageBar;
  connection: ConnectionWord;
  runs: readonly Run[];
  nowMs: number;
  pipeline: PipelineState | undefined;
  upgrade: UpgradeVerdict;
  outcome?: ActOutcome;
  notice?: PageNotice;
  detailsOpen: boolean;
}

/**
 * A remote cluster: what it runs, its deployment history, and the acts the
 * caller's role and its pipeline allow -- all on the bar.
 *
 * NOT CONNECTED IS NOT "NO PIPELINE". The old page headed an editor that was
 * merely signed out "No deploy pipeline is configured for this cluster"; the
 * history now says what it needs ("Sign in to see history.") and the bar
 * offers exactly that act.
 */
export function remoteOverviewScreen(i: RemoteOverviewInput): RegionParts {
  const { instance } = i;
  let body = pageNotice(i.notice);
  if (i.outcome !== undefined) {
    // The fix beside the sentence: a sign-in when that is what failed;
    // otherwise the engine's own words, which are in the Output channel.
    const acts: Act[] = i.outcome.signIn
      ? [{ act: "signIn", label: "Sign in" }]
      : i.outcome.tone === "error"
        ? [{ act: "openOutput", label: "Show details" }]
        : [];
    body += notice({ tone: i.outcome.tone, line: i.outcome.line, ...(acts.length === 0 ? {} : { acts }) });
  }
  if (i.connection === "connected" && i.pipeline !== undefined && i.pipeline.line !== "") {
    body += notice({ tone: "info", line: i.pipeline.line });
  }
  if (i.upgrade.kind === "refused" && i.connection === "connected") {
    body += notice({
      tone: "info",
      line: i.upgrade.message,
      ...(i.upgrade.docHref === "" ? {} : { acts: [{ act: "openGuide", label: "How to update", value: i.upgrade.docHref }] }),
    });
  }

  const rows: FactRow[] = [];
  const os = memqlOsAddress(instance.domain);
  if (os !== undefined) rows.push({ label: "MemQL OS", valueHtml: factWithAct(os.shown, { act: "openOs", label: "Open" }) });
  if (rows.length > 0) body += facts(rows);

  body += subhead("History");
  switch (i.connection) {
    case "connected":
      body += runList(i.runs, i.nowMs, instance.pendingDeploymentId ?? "");
      break;
    case "connecting":
      body += skeleton({ shape: "list", rows: 3, label: "Loading history" });
      break;
    case "signIn":
      body += emptyState({ line: "Sign in to see history." });
      break;
    default:
      body += emptyState({ line: "Connect to see history." });
  }

  const detailRows: FactRow[] = [
    { label: "Audit reference", value: i.outcome?.auditId ?? "", mono: true },
    { label: "Running", value: instance.currentDeploymentId ?? "", mono: true },
    { label: "Prepared", value: instance.pendingDeploymentId ?? "", mono: true },
    { label: "Status", value: i.pipeline?.engineMessage ?? "" },
  ];
  body += details(i.detailsOpen, detailRows);

  return {
    head: head({ title: instanceLabel(instance), ...((instance.versionLabel ?? "") === "" ? {} : { meta: instance.versionLabel }) }),
    body,
    actions: barHtml(i.bar),
  };
}

// ---------------------------------------------------------------------------
// change version
// ---------------------------------------------------------------------------

/** The select's value for "type a version instead". */
export const OTHER_VERSION = "__other__";

export interface ChooseVersionInput {
  instance: Instance;
  /** Undefined while the list is loading. */
  listing: ReleaseListing | undefined;
  /** The select's value: a tag, OTHER_VERSION, or "" before a choice. */
  choice: string;
  /** What was typed, when typing. */
  typed: string;
  /** A problem with what was typed, or "". */
  typedError: string;
  /** The version the run would move to, once one is chosen and valid; else "". */
  target: string;
  plan: readonly PlannedStepView[];
  summary: string;
  sameVersion: boolean;
}

/**
 * Changing version: one question, and what answering it will change.
 *
 * ONE CONTROL. A select of the published versions, newest marked Latest and
 * this cluster's marked Current, with Other... for a version the list does not
 * have; when the list could not be loaded, the text field is the only control
 * and the page says why in one line. The list NEVER pre-selects: a version the
 * page chose silently is not one the operator can be held to.
 *
 * THE ACT APPEARS WHEN IT IS LEGAL. "Change to v0.22.0" is on the bar once a
 * valid version is chosen; before that the bar says "Choose a version". It
 * used to be a disabled Start at the top of the page.
 */
export function chooseVersionScreen(i: ChooseVersionInput): RegionParts {
  const { instance } = i;
  const current = (instance.version ?? "").trim();
  let body = "";
  const label = instance.versionLabel ?? "";
  if (label !== "") body += facts([{ label: "Current", value: label }]);

  if (i.listing === undefined) {
    body += skeleton({ shape: "form", rows: 1, label: "Loading versions" });
  } else {
    const tags = i.listing.tags;
    const typing = tags.length === 0 || i.choice === OTHER_VERSION;
    if (tags.length > 0) {
      const options = [`<option value=""${i.choice === "" ? " selected" : ""}>Choose a version</option>`]
        .concat(
          tags.map((tag, n) => {
            const marks = [n === 0 ? "Latest" : "", tag === current ? "Current" : ""].filter((m) => m !== "");
            return `<option value="${escapeHtml(tag)}"${tag === i.choice ? " selected" : ""}>${escapeHtml(
              marks.length === 0 ? tag : `${tag} · ${marks.join(" · ")}`,
            )}</option>`;
          }),
        )
        .concat(`<option value="${OTHER_VERSION}"${i.choice === OTHER_VERSION ? " selected" : ""}>Other…</option>`)
        .join("");
      body += field({
        id: "dp-version",
        label: "Version",
        controlHtml: `<select class="mq-input dp-select" id="dp-version" data-field="version">${options}</select>`,
      });
    }
    if (typing) {
      body += field({
        id: "dp-version-typed",
        label: tags.length === 0 ? "Version" : "Version tag",
        ...(tags.length === 0 ? { hint: "Couldn't load the list of versions." } : {}),
        ...(i.typedError === "" ? {} : { error: i.typedError }),
        controlHtml: textInput({
          id: "dp-version-typed",
          field: "typed",
          value: i.typed,
          placeholder: "v0.24.0",
          invalid: i.typedError !== "",
          describedBy: i.typedError !== "" ? "dp-version-typed-error" : tags.length === 0 ? "dp-version-typed-hint" : undefined,
          enterAct: "beginChange",
        }),
      });
    }
  }

  if (i.target !== "") {
    if (i.sameVersion) {
      body += notice({ tone: "info", line: `Already on ${i.target}.`, next: "This re-applies it, the same as Repair." });
    }
    if (instance.imageSource === "checkout") {
      body += notice({ tone: "warn", line: `Your own build is replaced with released ${i.target} images.` });
    }
    const changing = i.plan.filter((step) => step.effect === "runs");
    if (changing.length > 0) {
      body +=
        subhead("What changes") +
        `<ul class="dp-bullets">${changing.map((step) => `<li>${escapeHtml(step.detail)}</li>`).join("")}</ul>` +
        (i.summary === "" ? "" : `<p class="dp-note">${escapeHtml(i.summary)}</p>`);
    }
  }

  const cancel: Act = { act: "back", label: "Cancel" };
  const actions =
    i.target === ""
      ? actionBar({ state: "Choose a version", tone: "idle", acts: [cancel] })
      : actionBar({
          state: "Ready",
          tone: "idle",
          acts: [cancel, { act: "beginChange", label: `Change to ${i.target}`, value: i.target, tone: "primary" }],
        });

  return {
    head: head({ title: "Change version", back: { act: "back", label: instanceLabel(instance) } }),
    body,
    actions,
  };
}

// ---------------------------------------------------------------------------
// building from the checkout
// ---------------------------------------------------------------------------

/** One check notice, with its fix as an act when the page can do it. */
function checkNotice(n: CheckNotice): string {
  const acts: Act[] =
    n.fix === "checkAgain"
      ? [{ act: "checkAgain", label: "Check again" }]
      : n.fix === "repair"
        ? [{ act: "repair", label: "Repair" }]
        : [];
  return notice({ tone: n.tone, line: n.line, ...(n.next === undefined ? {} : { next: n.next }), ...(acts.length === 0 ? {} : { acts }) });
}

function checkBody(check: RebuildCheck | undefined, home: string): string {
  if (check === undefined) return skeleton({ shape: "facts", rows: 2, label: "Checking the source" });
  return (
    facts(check.facts.map((f) => ({ label: f.label, value: f.mono === true ? masked(f.value, home) : f.value, ...(f.mono === true ? { mono: true } : {}) }))) +
    check.notices.map(checkNotice).join("")
  );
}

function servicesField(nodes: string): string {
  return field({
    id: "dp-nodes",
    label: "Services",
    hint: "Leave empty to rebuild all of them, or name some: bff, agent.",
    controlHtml: textInput({ id: "dp-nodes", field: "nodes", value: nodes, placeholder: "All", describedBy: "dp-nodes-hint" }),
  });
}

function checkBar(check: RebuildCheck | undefined, blockedState: string, act: Act): string {
  const cancel: Act = { act: "back", label: "Cancel" };
  if (check === undefined) return actionBar({ state: "Checking", tone: "busy", acts: [cancel] });
  if (check.blocked) return actionBar({ state: blockedState, tone: "warn", acts: [cancel] });
  return actionBar({ state: "Ready", tone: "idle", acts: [cancel, act] });
}

export interface RebuildScreenInput {
  instance: Instance;
  /** Undefined while the checks run. */
  check: RebuildCheck | undefined;
  nodes: string;
  home: string;
}

/** Rebuild from checkout: what will be built, what needs attention, and which services. */
export function rebuildScreen(i: RebuildScreenInput): RegionParts {
  return {
    head: head({ title: "Rebuild from checkout", back: { act: "back", label: instanceLabel(i.instance) } }),
    body: checkBody(i.check, i.home) + servicesField(i.nodes),
    actions: checkBar(i.check, "Can't rebuild yet", { act: "beginRebuild", label: "Rebuild", tone: "primary" }),
  };
}

export interface PullRebuildScreenInput extends RebuildScreenInput {
  merge: boolean;
  /** Whether to offer the merge switch at all: only when the checkout may have commits of its own. */
  offerMerge: boolean;
}

/** Pull and rebuild: the rebuild, with the latest code pulled first. */
export function pullRebuildScreen(i: PullRebuildScreenInput): RegionParts {
  const merge = i.offerMerge
    ? switchRow({
        id: "dp-merge",
        label: "Merge with my commits",
        note: i.merge ? "Needs everything committed first." : "Off: the pull stops if your commits aren't on the branch.",
        checked: i.merge,
        data: { field: "merge" },
      })
    : "";
  return {
    head: head({ title: "Pull and rebuild", back: { act: "back", label: instanceLabel(i.instance) } }),
    body: checkBody(i.check, i.home) + servicesField(i.nodes) + (i.check === undefined ? "" : merge),
    actions: checkBar(i.check, "Can't pull yet", { act: "beginPullRebuild", label: "Pull and rebuild", tone: "primary" }),
  };
}

// ---------------------------------------------------------------------------
// a run, as it happens
// ---------------------------------------------------------------------------

export interface RunScreenInput {
  words: RunWords;
  status: "running" | "stopping" | "failed" | "done" | "stopped";
  progress: ProgressUpdate;
  failure: RunFailure | undefined;
  cancellable: boolean;
  logsOpen: boolean;
  /**
   * Lines to render in the log. The panel passes none and streams them with
   * LiveView.log; the gallery passes some, to show a log with lines in it.
   */
  lines?: readonly LogLine[];
  /** The render's clock (tests and the gallery). */
  now?: number;
}

/**
 * One run, in the one progress screen every long operation uses: the mark,
 * the title in the act's own words, the bar, one status line, "Step 6 of 16 ·
 * 3:12", and "Show logs". Only the status of the run changes the regions; the
 * bar, the status line and the log arrive as LiveView.progress/log messages.
 */
export function runScreen(i: RunScreenInput): RegionParts {
  const p = i.progress;
  let body = progress({
    title: p.title ?? i.words.title,
    ...(p.percent === undefined ? {} : { percent: p.percent }),
    status: p.status,
    ...(p.stepText === undefined ? {} : { stepText: p.stepText }),
    ...(p.startedAt === undefined ? {} : { startedAt: p.startedAt }),
    ...(p.endedAt === undefined ? {} : { endedAt: p.endedAt }),
    state: p.state,
    ...(i.now === undefined ? {} : { now: i.now }),
  });
  if (i.status === "failed" && i.failure !== undefined) {
    body += notice({
      tone: "error",
      line: i.failure.reason,
      ...(i.failure.remedy === ""
        ? {}
        : { codeHtml: codeBlock({ text: i.failure.remedy, act: { act: "runRemedy", label: "Run in terminal" } }) }),
    });
  }
  body += disclosure({
    act: "toggleLogs",
    id: "run-logs",
    label: "Show logs",
    openLabel: "Hide logs",
    open: i.logsOpen,
    bodyHtml: logPane({
      id: "run-log",
      ariaLabel: `${i.words.title} log`,
      lines: i.lines ?? [],
      empty: "No output yet",
      acts: [
        { act: "copyLog", label: "Copy" },
        { act: "openOutput", label: "Open in Output" },
      ],
    }),
  });

  const back: Act = { act: "back", label: "Back" };
  let actions: string;
  switch (i.status) {
    case "running":
      actions = actionBar({
        state: i.words.busy,
        tone: "busy",
        ...(i.cancellable ? {} : { detail: "Can't be stopped partway" }),
        acts: i.cancellable ? [{ act: "cancel", label: "Cancel" }] : [],
      });
      break;
    case "stopping":
      actions = actionBar({ state: "Stopping", tone: "busy", acts: [] });
      break;
    case "stopped":
      actions = actionBar({ state: "Stopped", tone: "idle", acts: [back, { act: "retry", label: "Start again", tone: "primary" }] });
      break;
    case "failed":
      actions = actionBar({
        state: i.words.failed,
        tone: "error",
        acts: i.failure?.retryable === false ? [back] : [back, { act: "retry", label: "Retry", tone: "primary" }],
      });
      break;
    case "done":
      actions = actionBar({ state: i.words.done, tone: "live", acts: [{ act: "back", label: "Done", tone: "primary" }] });
      break;
  }
  return { head: "", body, actions };
}

// ---------------------------------------------------------------------------
// one run from the history
// ---------------------------------------------------------------------------

export interface RunDetailInput {
  instance: Instance;
  run: Run;
  bar: PageBar;
  nowMs: number;
  /** Step id -> its short label, from the graph documents. */
  labels: ReadonlyMap<string, string>;
  /** A failed step's saved output, by file name; absent while it is read. */
  logs: ReadonlyMap<string, string>;
  /** Which failed steps' logs are open, by file name. */
  openLogs: ReadonlySet<string>;
  detailsOpen: boolean;
}

/**
 * One run: what happened, when, why it failed, and what each step came to.
 *
 * THE STEPS ARE NAMED IN WORDS ("Creating the cluster"), not by id, and the
 * ones that found their work already done are one line ("9 already in
 * place"), because a repair skips most of an install and fifteen rows of
 * "skipped" bury the one that did something. A failed step says why, and its
 * saved output opens beneath it. Ids, raw results, exit codes and stamps are
 * under Details, for a support case.
 */
export function runDetailScreen(i: RunDetailInput): RegionParts {
  const { run, instance } = i;
  const meta = transitionMeta(run);

  const factRows: FactRow[] = [];
  const when = formatWhen(run.startedAt, i.nowMs);
  if (when !== "") factRows.push({ label: "Started", value: when });
  const took = runDuration(run.startedAt, run.finishedAt);
  if (took !== "") factRows.push({ label: "Took", value: took });
  let body = factRows.length === 0 ? "" : facts(factRows);

  if (run.status === "failed") {
    const failed = run.items.find((item) => item.status === "failed");
    const reason = failed === undefined ? "" : itemReason(failed);
    if (reason !== "") body += notice({ tone: "error", line: reason });
  } else if (run.status === "interrupted") {
    body += notice({ tone: "warn", line: "This run stopped when the editor closed." });
  }

  body += instance.kind === "remote" ? servicesList(run) : stepsList(run, i);

  const raw = run.items
    .map((item) => `${item.label}  ${item.status}${(item.detail ?? "") === "" ? "" : `  ${item.detail}`}`)
    .join("\n");
  body += details(
    i.detailsOpen,
    [
      { label: "Run", value: run.id, mono: true },
      { label: "Started at", value: run.startedAt, mono: true },
      { label: "Finished at", value: run.finishedAt ?? "", mono: true },
    ],
    raw === "" ? "" : savedLogPane({ id: "dp-raw", ariaLabel: "Recorded steps", lines: raw.split("\n").map((text) => ({ text })) }),
  );

  return {
    head: head({ title: runNoun(run), ...(meta === "" ? {} : { meta }), back: { act: "back", label: instanceLabel(instance) } }),
    body,
    actions: barHtml(i.bar),
  };
}

/** The head's meta for a run: its version move, or its build. */
function transitionMeta(run: Run): string {
  const to = (run.toVersion ?? "").trim();
  const from = (run.fromVersion ?? "").trim();
  if (from !== "" && to !== "") return `${from} → ${to}`;
  return to;
}

/** A dot's tone for a step's status. */
function stepTone(status: StepGroup["status"]): "idle" | "live" | "busy" | "warn" | "error" {
  switch (status) {
    case "ok":
    case "preserved":
      return "live";
    case "failed":
      return "error";
    case "running":
      return "busy";
    default:
      return "idle";
  }
}

function stepsList(run: Run, i: RunDetailInput): string {
  if (run.items.length === 0) return subhead("Steps") + emptyState({ line: "No steps were recorded." });
  const groups = stepGroups(run.items, i.labels);
  const shown = groups.filter((g) => g.status !== "skipped" && !(g.status === "pending" && run.status !== "running"));
  const skipped = groups.filter((g) => g.status === "skipped").reduce((n, g) => n + g.items.length, 0);
  const notRun = run.status === "running" ? 0 : groups.filter((g) => g.status === "pending").reduce((n, g) => n + g.items.length, 0);
  const rows = shown
    .map((g) => {
      const failedItem = g.items.find((item) => item.status === "failed");
      const kept = g.status === "preserved" ? `<span class="dp-row-desc">Kept: you had it before MemQL</span>` : "";
      let html =
        `<li class="dp-row"><span class="mq-dot" data-tone="${stepTone(g.status)}" aria-hidden="true"></span>` +
        `<span class="dp-row-label">${escapeHtml(g.label)}</span>${kept}</li>`;
      if (failedItem !== undefined) {
        const logFile = parseItemDetail(failedItem.detail).logFile;
        if (logFile !== "") {
          const text = i.logs.get(logFile);
          html += `<li class="dp-log">${disclosure({
            act: "toggleStepLog",
            id: stepLogDisclosureId(logFile),
            label: "Show log",
            openLabel: "Hide log",
            open: i.openLogs.has(logFile),
            bodyHtml:
              text === undefined
                ? skeleton({ shape: "list", rows: 2, label: "Loading the log" })
                : savedLogPane({
                    id: `dp-steplog-pane-${failedItem.label}`,
                    ariaLabel: `${g.label} log`,
                    lines: text === "" ? [] : text.replace(/\n+$/, "").split("\n").map((line) => ({ text: line })),
                    empty: "The log is empty",
                  }),
          })}</li>`;
        }
      }
      return html;
    })
    .join("");
  const notes = [
    skipped > 0 ? `${skipped} already in place` : "",
    notRun > 0 ? `${notRun} didn't run` : "",
  ].filter((n) => n !== "");
  return (
    subhead("Steps") +
    (rows === "" ? "" : `<ul class="dp-list">${rows}</ul>`) +
    (notes.length === 0 ? "" : `<p class="dp-note">${escapeHtml(notes.join(" · "))}</p>`)
  );
}

/**
 * The disclosure over a failed step's saved log, keyed by the log's file name
 * so the page's toggle names the file the host should read.
 */
export function stepLogDisclosureId(logFile: string): string {
  return `dp-log-${logFile}`;
}

/** The log file a step-log disclosure is about, or "" for any other id. */
export function stepLogFileOf(disclosureId: string): string {
  return disclosureId.startsWith("dp-log-") ? disclosureId.slice("dp-log-".length) : "";
}

/** A remote run's services: name, version and replicas; the digest is in Details. */
function servicesList(run: Run): string {
  if (run.items.length === 0) return subhead("Services") + emptyState({ line: "No service details for this deployment." });
  const rows = run.items
    .map((item) => {
      const detail = (item.detail ?? "").split(" · ").filter((part) => !part.startsWith("digest ")).join(" · ");
      return (
        `<li class="dp-row"><span class="mq-dot" data-tone="${stepTone(item.status)}" aria-hidden="true"></span>` +
        `<span class="dp-row-label">${escapeHtml(item.label)}</span><span class="dp-row-desc">${escapeHtml(detail)}</span></li>`
      );
    })
    .join("");
  return subhead("Services") + `<ul class="dp-list">${rows}</ul>`;
}

/**
 * A run that is no longer in the record: said, with the way back.
 *
 * WHERE IT WENT DEPENDS ON WHOSE RECORD IT WAS. This machine keeps its latest
 * RUN_LOG_KEEP runs and prunes the rest; a remote cluster's history is the
 * cluster's own rows, which this editor neither keeps nor prunes.
 */
export function missingRunScreen(instance: Instance, keep: number): RegionParts {
  return {
    head: head({ title: instance.kind === "remote" ? "Deployment" : "Run", back: { act: "back", label: instanceLabel(instance) } }),
    body: emptyState({
      line:
        instance.kind === "remote"
          ? "This deployment is no longer in the cluster's history."
          : `This run is no longer in the history. Only the latest ${keep} are kept.`,
    }),
    // The way back is the head's link; the bar says where the run stands.
    actions: actionBar({ state: "Not in the history", tone: "idle", acts: [] }),
  };
}

export { masked as maskHome };
