// The instance page's own screens: the overview, and choosing a target version.
//
// The run screens it shares with the add-cluster wizard are in
// installScreens.ts; these two are the Deployments page's alone. Same three
// constraints govern them -- no DOM, no inline event handlers (the webview CSP
// forbids them, so interactivity is `data-act` attributes plus one delegated
// listener in the panel), and every value through `escapeHtml`.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #3739 #3733

import { escapeHtml } from "@znasllc-io/memql-view-kit";

import type { InstanceAction } from "../deploy/instanceActions.js";
import type { UpgradeVerdict } from "../deploy/upgrade.js";
import { displayVersion, type Instance, type Run } from "../state/deployments.js";
import { instanceRowStatus, runRowStatus } from "../state/deploymentsCatalog.js";
import type { PipelineState } from "../deploy/pipelineState.js";
import type { TagListing } from "../install/tags.js";
import type { PlannedStepView } from "../state/upgradePlan.js";
import { latestRelease, type ReleaseListing } from "../version/releaseCache.js";

/**
 * The `latest` fact, beside the instance's own `version` (memql#3996).
 *
 * UNCONDITIONAL, including when nothing has been fetched, and that is the same
 * call the page already makes about `version`: an operator opened this page to
 * ask what this cluster is, so a fact that vanished when the answer was
 * "we do not know" would read as "there is nothing newer" -- which is the one
 * reading this epic exists to prevent. `not fetched` says which of the two it
 * is, and the reason sits in the lede above, where the version sentence names
 * the fetch failure.
 *
 * The row's availability clause is NOT repeated here. It is already in the
 * lede, and a page that said "v0.19.0 available" twice would be arguing with
 * itself about which one the operator should read.
 */
function latestFact(releases: ReleaseListing | undefined): string {
  const latest = latestRelease(releases);
  return `<div class="fact"><span class="fact-key">latest</span><span class="fact-value">${escapeHtml(
    latest ?? "not fetched",
  )}</span></div>`;
}

/**
 * The one button that moves this cluster to the newest release (memql#3997).
 *
 * DRAWN FOR A REFUSED VERDICT TOO, and that is deliberate. Hiding it would
 * leave an operator looking at a row that says `v0.19.0 available` beside a
 * page that offers nothing, with no way to find out why. Pressing it produces
 * the refusal and the runbook, which is the answer they came for. The refusal
 * arrives as this page's own error line -- nothing was sent, so there is no
 * engine outcome and no audit id.
 *
 * `data-act`, not `data-choose`: the instance actions validate their id against
 * instanceActions(), and this is not one of them.
 */
function upgradeButton(verdict: UpgradeVerdict): string {
  if (verdict.kind === "none") return "";
  const detail =
    verdict.kind === "offer"
      ? verdict.confirmation
      : "This move is not a retag. Press to see what it changes and where the procedure is.";
  return `<div class="actions"><button class="primary" type="button" data-act="upgrade" title="${escapeHtml(
    detail,
  )}">${escapeHtml(verdict.label)}</button></div>`;
}

export interface OverviewInput {
  instance: Instance;
  runs: readonly Run[];
  actions: readonly InstanceAction[];
  nowMs: number;
  /** A failure this page produced, as opposed to one a step reported. */
  error: string;
  /** The release listing, or undefined when nothing has been fetched. */
  releases: ReleaseListing | undefined;
  /** Whether this instance is offered a move to the newest release. */
  upgrade: UpgradeVerdict;
}

export function renderInstanceOverview(input: OverviewInput): string {
  const { instance } = input;
  const status = instanceRowStatus(instance, input.releases);

  const actions = input.actions
    .map(
      (action) =>
        `<button class="${action.id === "uninstall" ? "secondary destructive" : "primary"}" type="button" data-choose="${escapeHtml(
          action.id,
        )}" title="${escapeHtml(action.detail)}">${escapeHtml(action.label)}</button>`,
    )
    .join("");

  // AN INSTANCE WITH NO RUNS IS NOT AN EMPTY STATE, and this is the sentence
  // that says so rather than a placeholder implying something is missing.
  // "Installed, never upgraded" is the normal case.
  const runs =
    input.runs.length === 0
      ? `<p class="lede">No deployments have been recorded for this instance yet.</p>`
      : `<ul class="runs">${input.runs
          .map((run) => {
            const row = runRowStatus(run, input.nowMs);
            return `<li class="run" data-status="${escapeHtml(run.status)}">
  <span class="run-kind">${escapeHtml(row.label)}</span>
  <span class="run-detail">${escapeHtml(row.description)}</span>
</li>`;
          })
          .join("")}</ul>`;

  const error =
    input.error === "" ? "" : `<p class="error">${escapeHtml(input.error)}</p>`;

  return `<h1>${escapeHtml(instance.name)}</h1>
<p class="lede">${escapeHtml(status.tooltip)}</p>
${error}
<div class="facts">
  <div class="fact"><span class="fact-key">kind</span><span class="fact-value">${escapeHtml(
    instance.kind,
  )}</span></div>
  <div class="fact"><span class="fact-key">version</span><span class="fact-value">${escapeHtml(
    displayVersion(instance.version),
  )}</span></div>
  ${latestFact(input.releases)}
  <div class="fact"><span class="fact-key">domain</span><span class="fact-value">${escapeHtml(
    instance.domain ?? "not recorded",
  )}</span></div>
</div>
${upgradeButton(input.upgrade)}
<div class="actions">${actions}</div>
<h2>Deployments</h2>
${runs}`;
}

export interface ChooseTagInput {
  instance: Instance;
  listing: TagListing;
  /** What the operator has picked or typed. Empty until they do. */
  target: string;
  /** A problem with what they typed, or "" while there is none. */
  tagError: string;
  /** The projection, once a target is chosen. Empty before that. */
  plan: readonly PlannedStepView[];
  summary: string;
  sameVersion: boolean;
}

/**
 * Choosing where to move the cluster to.
 *
 * THE LIST NEVER PRE-SELECTS. Its first option is an empty one, so the field
 * starts on "no choice made" rather than on the newest release: a version the
 * page picked silently is not a version the operator can be held to, which is
 * the same argument stackPin.ts makes about the default pin.
 *
 * AND THERE IS ALWAYS A TEXT BOX. It is the only control when the listing
 * failed -- no git, no network, a checkout that is not a repository -- and the
 * reason is printed beside it, so an operator knows they are typing because the
 * network is down and not because the project has no releases.
 */
export function renderChooseTag(input: ChooseTagInput): string {
  const current = displayVersion(input.instance.version);

  // MARKED, NOT SELECTED (memql#3996). The list arrives newest-first
  // (install/tags.ts), so the newest is simply the first -- and saying which
  // one it is costs nothing, while selecting it would make the field arrive
  // already answered. The empty first option stays first and stays the
  // selected one until the operator picks: a version the page chose silently
  // is not a version they can be held to.
  const newest = input.listing.tags[0];
  const options = [`<option value=""${input.target === "" ? " selected" : ""}></option>`]
    .concat(
      input.listing.tags.map(
        (tag) =>
          `<option value="${escapeHtml(tag)}"${
            tag === input.target ? " selected" : ""
          }>${escapeHtml(tag)}${tag === newest ? " (newest)" : ""}</option>`,
      ),
    )
    .join("");

  const picker =
    input.listing.tags.length === 0
      ? ""
      : `<div class="field">
  <label for="tag-pick">Release tag</label>
  <select id="tag-pick" data-act="pickTag">${options}</select>
</div>`;

  const listingNote =
    input.listing.error === ""
      ? ""
      : `<p class="notice">${escapeHtml(input.listing.error)} Type the tag below.</p>`;

  const typed = `<div class="field" data-invalid="${input.tagError !== ""}">
  <label for="tag-type">${escapeHtml(
    input.listing.tags.length === 0 ? "Release tag" : "...or type one",
  )}</label>
  <input id="tag-type" data-field="tag" value="${escapeHtml(input.target)}">
  ${
    input.tagError === ""
      ? ""
      : `<div class="error">${escapeHtml(input.tagError)}</div>`
  }
</div>`;

  const sameNote = input.sameVersion
    ? `<p class="notice">This cluster is already on ${escapeHtml(
        input.target,
      )}. Deploying it again reconciles the overlay, which is what a repair does.</p>`
    : "";

  const plan =
    input.plan.length === 0
      ? ""
      : `<h2>Will run</h2>
<p class="lede">${escapeHtml(input.summary)}</p>
<ul class="plan">${input.plan
          .map(
            (step) => `<li class="plan-step" data-effect="${escapeHtml(step.effect)}">
  <span class="plan-mark">${step.effect === "runs" ? "-&gt;" : "ok"}</span>
  <span class="plan-id">${escapeHtml(step.id)}</span>
  <span class="plan-detail">${escapeHtml(step.detail)}</span>
</li>`,
          )
          .join("")}</ul>`;

  return `<h1>Create deployment</h1>
<p class="lede">This cluster is on ${escapeHtml(
    current,
  )}. Pick the release to move it to; nothing runs until you start it.</p>
${listingNote}
${picker}
${typed}
${sameNote}
${plan}
<div class="actions">
  <button class="primary" type="button" data-act="beginDeploy"${
    input.target === "" || input.tagError !== "" ? " disabled" : ""
  }>Start</button>
  <button class="secondary" type="button" data-act="back">Back</button>
</div>`;
}

// ---------------------------------------------------------------------------
// the remote instance
// ---------------------------------------------------------------------------

export interface RemoteOverviewInput {
  instance: Instance;
  runs: readonly Run[];
  pipeline: PipelineState;
  nowMs: number;
  /** The outcome line of the last action taken, or "" before any. */
  outcome: string;
  /** A failure this page produced, as opposed to one the engine reported. */
  error: string;
  /** The release listing, or undefined when nothing has been fetched. */
  releases: ReleaseListing | undefined;
  /** Whether this instance is offered a move to the newest release. */
  upgrade: UpgradeVerdict;
}

/**
 * A remote instance: what it runs, what deployed it, and what can be done.
 *
 * THE ITEMS ARE LABELLED "NODE TYPES", NEVER "STEPS". A local run's items are
 * capability-script executions and a remote run's are per-tier
 * `deploymentNodeSpec` rows -- a declaration of version, replicas and digest,
 * not an account of something that ran. The label is what stops one being read
 * as the other, and it is the only place the asymmetry between the two kinds of
 * run is visible to an operator.
 */
export function renderRemoteInstance(input: RemoteOverviewInput): string {
  const { instance, pipeline } = input;
  const status = instanceRowStatus(instance, input.releases);

  const actions =
    pipeline.actions.length === 0
      ? ""
      : `<div class="actions">${pipeline.actions
          .map(
            (action) =>
              `<button class="${
                action.typeToConfirm ? "secondary destructive" : "primary"
              }" type="button" data-deploy="${escapeHtml(action.id)}" title="${escapeHtml(
                action.description,
              )}">${escapeHtml(action.label)}</button>`,
          )
          .join("")}</div>`;

  const runs =
    input.runs.length === 0
      ? `<p class="lede">No deployments have been recorded for this cluster${
          instance.connected ? "" : ", and this editor is not connected to it"
        }.</p>`
      : input.runs
          .map((run) => {
            const row = runRowStatus(run, input.nowMs);
            const items =
              run.items.length === 0
                ? ""
                : `<div class="items-label">Node types</div>
<ul class="runs">${run.items
                    .map(
                      (item) => `<li class="run">
  <span class="run-kind">${escapeHtml(item.label)}</span>
  <span class="run-detail">${escapeHtml(item.detail ?? "")}</span>
</li>`,
                    )
                    .join("")}</ul>`;
            return `<div class="run-block" data-status="${escapeHtml(run.status)}">
  <div class="run">
    <span class="run-kind">${escapeHtml(row.label)}</span>
    <span class="run-detail">${escapeHtml(row.description)}</span>
  </div>
  ${items}
</div>`;
          })
          .join("");

  const outcome =
    input.outcome === ""
      ? ""
      : `<p class="${
          input.outcome.startsWith("ERROR") ? "error" : "notice"
        }">${escapeHtml(input.outcome)}</p>`;
  const error = input.error === "" ? "" : `<p class="error">${escapeHtml(input.error)}</p>`;

  return `<h1>${escapeHtml(instance.name)}</h1>
<p class="lede">${escapeHtml(status.tooltip)}</p>
${error}
${outcome}
<div class="facts">
  <div class="fact"><span class="fact-key">kind</span><span class="fact-value">remote</span></div>
  <div class="fact"><span class="fact-key">version</span><span class="fact-value">${escapeHtml(
    displayVersion(instance.version),
  )}</span></div>
  ${latestFact(input.releases)}
  <div class="fact"><span class="fact-key">domain</span><span class="fact-value">${escapeHtml(
    instance.domain ?? "not recorded",
  )}</span></div>
</div>
${upgradeButton(input.upgrade)}
<h2>${escapeHtml(pipeline.title)}</h2>
<p class="lede">${escapeHtml(pipeline.detail)}</p>
${actions}
<h2>Deployments</h2>
${runs}`;
}
