// The instance page's own screens: the overview, choosing a target version, and
// one deployment in detail.
//
// The run screens it shares with the add-cluster wizard are in
// installScreens.ts; these are the Deployments page's alone. Same three
// constraints govern them -- no DOM, no inline event handlers (the webview CSP
// forbids them, so interactivity is `data-act` attributes plus one delegated
// listener in the panel), and every value through `escapeHtml`.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #4427 #4423 #3739 #3733

import { escapeHtml } from "@znasllc-io/memql-view-kit";

import { renderDisclosure, renderScreen } from "./screenLayout.js";

import type { InstanceAction } from "../deploy/instanceActions.js";
import type { UpgradeVerdict } from "../deploy/upgrade.js";
import { displayVersion, type Instance, type Run, type RunItem } from "../state/deployments.js";
import {
  checkoutVersionText,
  instanceRowStatus,
  runDuration,
  runRowStatus,
  versionTransition,
} from "../state/deploymentsCatalog.js";
import type { PipelineState } from "../deploy/pipelineState.js";
import type { TagListing } from "../install/tags.js";
import { releasedImages, returnsToReleasedImages } from "../state/imageLane.js";
import type { PlannedStepView } from "../state/upgradePlan.js";
import { renderPreflight } from "./installScreens.js";
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
 *
 * IT NO LONGER CARRIES ITS OWN ACTIONS ROW (memql#4453). It used to sit in a
 * row of its own, between the facts and the instance's buttons -- which under
 * the actions-first doctrine would put two action rows on one page, in
 * different places, one of them below the fold. It is a button now, and the
 * screen puts it in the single row at the top.
 */
function upgradeButton(verdict: UpgradeVerdict): string {
  if (verdict.kind === "none") return "";
  const detail =
    verdict.kind === "offer"
      ? verdict.confirmation
      : "This move is not a retag. Press to see what it changes and where the procedure is.";
  return `<button class="primary" type="button" data-act="upgrade" title="${escapeHtml(
    detail,
  )}">${escapeHtml(verdict.label)}</button>`;
}

/**
 * The troubleshooting tier, demoted rather than deleted (memql#4456).
 *
 * WHAT THIS IS FOR. These pages had accreted a second audience: beside the
 * facts an operator opened the page to read -- what is this cluster, what
 * version, is there a newer one -- sat receipt-derived paths, image-lane
 * bookkeeping, raw RFC3339 stamps and node-by-node digests. Each is genuinely
 * useful about once a quarter, when something is wrong, and the cost of
 * having them inline the rest of the time is that the page reads as a
 * diagnostic dump rather than as a product.
 *
 * NOTHING IS DELETED. The support case still has every one of them; they are
 * one click away instead of first. That distinction is the whole decision --
 * removing them would trade one bad failure mode for a worse one, because the
 * quarter they are wanted is the quarter nobody can get at them.
 *
 * THE SAME COMPONENT AS THE RUN LOG (memql#4455), on purpose. "Material worth
 * keeping and not worth leading with" is one idea, and a second implementation
 * of it would be a second answer to what collapsed looks like here.
 */
function renderDiagnostics(open: boolean, body: string): string {
  if (body.trim() === "") return "";
  return renderDisclosure({
    act: "toggleDiagnostics",
    summary: "Show diagnostics",
    summaryOpen: "Hide diagnostics",
    open,
    body,
  });
}

/** Fact rows for the Diagnostics pane, or "" when every one of them is empty. */
function diagnosticFacts(rows: readonly string[]): string {
  const present = rows.filter((row) => row !== "");
  if (present.length === 0) return "";
  return `<div class="facts">${present.join("\n  ")}</div>`;
}

/** One `fact` row, or "" when there is nothing to state. */
function fact(key: string, value: string): string {
  if (value === "") return "";
  return `<div class="fact"><span class="fact-key">${escapeHtml(
    key,
  )}</span><span class="fact-value">${escapeHtml(value)}</span></div>`;
}

/**
 * A LOCAL instance's receipt-derived facts.
 *
 * Every one of these comes off the same install receipt the product facts come
 * off; what separates them is who is asking. "Which directory did the install
 * clone into" is a question with exactly one audience, and it is not the
 * audience opening a cluster page to see whether it is healthy.
 */
function localDiagnosticFacts(instance: Instance): string {
  return diagnosticFacts([
    fact("kind", instance.kind),
    fact("checkout", instance.checkout ?? ""),
    fact("image source", instance.imageSource ?? ""),
    fact("rebuild commit", instance.rebuild?.commit ?? ""),
    fact("rebuilt at", instance.rebuild?.recordedAt ?? ""),
    fact("rebuilt nodes", instance.rebuild?.nodes ?? ""),
  ]);
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
  /**
   * Whether the Diagnostics section is disclosed (memql#4456).
   *
   * REQUIRED rather than defaulted: the flag lives in the panel, and a default
   * would let a caller forget to thread it and ship a section that can never
   * open -- silently, because a closed disclosure looks exactly the same
   * whether or not anything is listening to its toggle.
   */
  diagnosticsOpen: boolean;
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

  // ONE ACTIONS ROW, AT THE TOP (memql#4453). The move-to-newest button and the
  // instance's own actions used to render in two separate rows in two separate
  // places, both below the facts; they are one row now, and it is the first
  // thing under the heading. WHICH buttons appear is still entirely
  // `instanceActions`' and `upgradeButton`'s decision -- this only decides where.
  return renderScreen({
    title: instance.name,
    actions: `${upgradeButton(input.upgrade)}${actions}`,
    status: `<p class="lede">${escapeHtml(status.tooltip)}</p>
${error}`,
    details: `<div class="facts">
  <div class="fact"><span class="fact-key">version</span><span class="fact-value">${escapeHtml(
    displayVersion(instance.version),
  )}</span></div>
  ${latestFact(input.releases)}
  <div class="fact"><span class="fact-key">domain</span><span class="fact-value">${escapeHtml(
    instance.domain ?? "not recorded",
  )}</span></div>
</div>
<h2>Deployments</h2>
${runs}`,
    logs: renderDiagnostics(input.diagnosticsOpen, localDiagnosticFacts(instance)),
  });
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
      : input.listing.refusedPlatform === true
        ? `<p class="lede">${escapeHtml(input.listing.error)}</p>`
        : `<p class="notice">${escapeHtml(input.listing.error)} Type the tag below.</p>`;

  if (input.listing.refusedPlatform === true) {
    return renderScreen({
      title: "Create deployment",
      actions: `<button class="secondary" type="button" data-act="back">Back</button>`,
      status: listingNote,
    });
  }

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

  // THE LANE CROSSING, ON THE PATH THAT WOULD OTHERWISE MAKE IT SILENTLY
  // (memql#4246). This screen is reached from the same row as Rebuild from
  // checkout, it re-runs `clusterUp` -- which rewrites the Application's image
  // overrides back to released ones -- and unlike Repair and Upgrade it asks
  // for no confirmation. So it is the likeliest place a developer's own build
  // stops running without anything having said so.
  //
  // Rendered through `renderPreflight` with a one-item list rather than as a
  // bespoke paragraph, so it looks like every other thing this extension states
  // before a run, and worded by the shared helper, so it cannot drift from what
  // the other three surfaces say.
  const laneNote =
    input.instance.imageSource === "checkout"
      ? renderPreflight([
          {
            label: "Image source",
            state: "attention",
            detail: returnsToReleasedImages(input.instance.name, input.target),
          },
        ])
      : "";

  // Start is DISABLED until a target is chosen rather than absent, which is the
  // one thing actions-first changes about this screen: a button that appears
  // only once the form is complete is a button an operator cannot plan around,
  // and at the top of the page its absence would read as "this screen does
  // nothing". Disabled says "choose below, then press this".
  return renderScreen({
    title: "Create deployment",
    actions: `<button class="primary" type="button" data-act="beginDeploy"${
      input.target === "" || input.tagError !== "" ? " disabled" : ""
    }>Start</button>
  <button class="secondary" type="button" data-act="back">Back</button>`,
    // THE LANE CROSSING RIDES WITH THE BUTTON (memql#4453 over memql#4246). It
    // is the one thing on this screen that says a developer's own build is
    // about to stop running, and this is the path that would otherwise make
    // that change silently -- so it goes where Start is, not at the end of the
    // form Start acts on. Above the fold now, where it was previously last.
    status: `<p class="lede">This cluster is on ${escapeHtml(
      current,
    )}. Pick the release to move it to; nothing runs until you start it.</p>
${listingNote}
${laneNote}`,
    details: `${picker}
${typed}
${sameNote}
${plan}`,
  });
}

// ---------------------------------------------------------------------------
// the remote instance
// ---------------------------------------------------------------------------

/**
 * WHICH record "Deploy" ships, named on the page (memql#4017).
 *
 * The button used to ship `runs[0]` -- the newest record in the catalog the
 * page last read -- re-derived at the CLICK. The id is now resolved when the
 * page is BUILT (`Instance.pendingDeploymentId`), so what ships is the record
 * the operator was looking at; printing it is the other half, and it is why
 * there is no modal. A confirmation naming the target tells them after they
 * have decided; a line above the button tells them before.
 *
 * DRAWN ONLY WHERE THE DEPLOY ACTION IS. A reader sees the deployment history
 * and none of the actions (deploy/actions.ts, `visibleActions`); telling them
 * nothing is cut would answer a question their page never raised.
 *
 * The version is a courtesy, not the identity: it comes off the run list this
 * page already rendered, and is simply omitted when the record is not in it.
 * The ID is what the ship names and what an audit line will carry.
 */
function shipTargetLine(input: RemoteOverviewInput): string {
  if (!input.pipeline.actions.some((action) => action.id === "deploy")) return "";
  const target = (input.instance.pendingDeploymentId ?? "").trim();
  if (target === "") {
    return `<p class="notice">Nothing is cut, so Deploy has no record to ship. Cut a version first.</p>`;
  }
  const version = input.runs.find((run) => run.id === target)?.toVersion ?? "";
  return `<p class="notice">Deploy ships ${escapeHtml(target)}${
    version === "" ? "" : ` (${escapeHtml(version)})`
  }.</p>`;
}

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
  /** Whether the Diagnostics section is disclosed. See `OverviewInput`. */
  diagnosticsOpen: boolean;
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

  const actions = pipeline.actions
    .map(
      (action) =>
        `<button class="${
          action.typeToConfirm ? "secondary destructive" : "primary"
        }" type="button" data-deploy="${escapeHtml(action.id)}" title="${escapeHtml(
          action.description,
        )}">${escapeHtml(action.label)}</button>`,
    )
    .join("");

  // THE RUN ROWS KEEP THEIR HEADLINE; THE PER-TIER BREAKDOWN DOES NOT
  // (memql#4456). What a deployment DID -- kind, when, how it ended -- is what
  // this list is for. Which node types it declared, at how many replicas, at
  // which digest, is a different question with a different audience, and
  // rendering it inline for every historical run turned a five-row history into
  // fifty rows of digests.
  const runs =
    input.runs.length === 0
      ? `<p class="lede">No deployments have been recorded for this cluster${
          instance.connected ? "" : ", and this editor is not connected to it"
        }.</p>`
      : input.runs
          .map((run) => {
            const row = runRowStatus(run, input.nowMs);
            return `<div class="run-block" data-status="${escapeHtml(run.status)}">
  <div class="run">
    <span class="run-kind">${escapeHtml(row.label)}</span>
    <span class="run-detail">${escapeHtml(row.description)}</span>
  </div>
</div>`;
          })
          .join("");

  // THE ITEMS ARE LABELLED "NODE TYPES", NEVER "STEPS", wherever they render.
  // A local run's items are capability-script executions and a remote run's are
  // per-tier `deploymentNodeSpec` rows -- a declaration of version, replicas and
  // digest, not an account of something that ran. The label is what stops one
  // being read as the other, and demoting them into Diagnostics does not make
  // that distinction less load-bearing; it makes it easier to miss, so the
  // label travels with them.
  const nodeTypes = input.runs
    .filter((run) => run.items.length > 0)
    .map((run) => {
      const row = runRowStatus(run, input.nowMs);
      return `<div class="run-block">
  <div class="items-label">Node types -- ${escapeHtml(row.label)}</div>
  <ul class="runs">${run.items
    .map(
      (item) => `<li class="run">
  <span class="run-kind">${escapeHtml(item.label)}</span>
  <span class="run-detail">${escapeHtml(item.detail ?? "")}</span>
</li>`,
    )
    .join("")}</ul>
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

  return renderScreen({
    title: instance.name,
    actions: `${upgradeButton(input.upgrade)}${actions}`,
    status: `<p class="lede">${escapeHtml(status.tooltip)}</p>
${error}
${outcome}
<h2>${escapeHtml(pipeline.title)}</h2>
<p class="lede">${escapeHtml(pipeline.detail)}</p>
${shipTargetLine(input)}`,
    details: `<div class="facts">
  <div class="fact"><span class="fact-key">version</span><span class="fact-value">${escapeHtml(
    displayVersion(instance.version),
  )}</span></div>
  ${latestFact(input.releases)}
  <div class="fact"><span class="fact-key">domain</span><span class="fact-value">${escapeHtml(
    instance.domain ?? "not recorded",
  )}</span></div>
</div>
<h2>Deployments</h2>
${runs}`,
    logs: renderDiagnostics(
      input.diagnosticsOpen,
      `${diagnosticFacts([fact("kind", "remote")])}
${nodeTypes}`,
    ),
  });
}

// ---------------------------------------------------------------------------
// one deployment, in detail (memql#4427)
// ---------------------------------------------------------------------------

/**
 * WHY THIS SCREEN EXISTS. Deployment rows carried no `command` at all, so
 * clicking one did nothing -- the most direct way there is to teach an operator
 * that a view is decorative. Everything it shows was already recorded and had
 * nowhere to be read: the per-step outcomes an install writes after every wave,
 * the per-tier specs a remote rollout declares, the reason a run failed.
 *
 * WHAT IT REFUSES TO INVENT. Three facts are printed only when the record
 * carries them, and are OMITTED rather than defaulted otherwise -- a duration
 * for a run still in flight, a finish time for one that was interrupted, a
 * version transition for a run that recorded none. Each of those defaults would
 * be a claim about the run rather than about the read, which is the distinction
 * `displayVersion` was written for and the one this page is most exposed to.
 */
export interface RunDetailInput {
  /** The instance the run belongs to, for its lane and its action set. */
  instance: Instance;
  run: Run;
  /** From `runDetailActions` -- the instance's own set, reordered. */
  actions: readonly InstanceAction[];
  nowMs: number;
  /**
   * The outcome line of the last action taken from this page, or "" before any.
   *
   * CARRIED HERE AS WELL AS ON THE REMOTE OVERVIEW, because the remote action
   * buttons now exist on both screens and `runDeploy` reports through this one
   * field. Without it, pressing Deploy from a run's detail page would show the
   * operator nothing at all -- the engine's line, audit id included, would land
   * in a screen that does not draw it, which is indistinguishable from an
   * action that never ran.
   */
  outcome: string;
  /** A failure this page produced, as opposed to one a step reported. */
  error: string;
  /** Whether the Diagnostics section is disclosed. See `OverviewInput`. */
  diagnosticsOpen: boolean;
}

/**
 * The version fact, which is not always a version.
 *
 * A REBUILD HAS NO TRANSITION TO PRINT, and that is a property of the data
 * rather than a gap: `RunRecorder.begin` is called for a rebuild with neither
 * `fromVersion` nor `toVersion`, because the run does not move the cluster
 * between releases -- it moves it between LANES, from released images to images
 * built from the checkout on this machine. The wording comes from
 * state/imageLane.ts so this page cannot drift from what the four surfaces that
 * warn about the crossing already say.
 *
 * AND THE COMMIT IS NOT ATTRIBUTED TO THIS RUN. `Run` carries no commit --
 * `instance.rebuild` does, and it describes the LAST rebuild, which is this one
 * only if no other has happened since. So the commit appears as a separate fact
 * about the instance, labelled as such, and never inside the sentence about
 * what this run did.
 */
function runVersionFact(instance: Instance, run: Run): string {
  const transition = versionTransition(run);
  if (transition !== "") return transition;
  if (run.kind === "rebuild") return "built from the checkout";
  return "";
}

/**
 * WHY A FAILED RUN NAMES A STEP.
 *
 * `Run` has no `reason` field, and adding one would be inventing a second place
 * for something already recorded: the reason a run failed is the `detail` of
 * whichever item failed, written by the executor as it happened. Reading it off
 * the items is what makes the sentence true by construction -- there is nothing
 * for it to disagree with.
 *
 * More than one item can have failed (a wave fails as a unit), so they are all
 * named. A failed run with no failed item is possible -- an abort writes the
 * status directly -- and it says only that, rather than inventing a culprit.
 */
function failureReason(run: Run): string {
  if (run.status !== "failed") return "";
  const failed = run.items.filter((item) => item.status === "failed");
  if (failed.length === 0) return "";
  return failed
    .map((item) => (item.detail === undefined || item.detail === "" ? item.label : `${item.label}: ${item.detail}`))
    .join("; ");
}

/**
 * The items, under the label that says what KIND of item they are.
 *
 * "Steps" for a local run and "Node types" for a remote one, never one word for
 * both. A local run's items are capability-script executions -- things that ran
 * -- and a remote run's are `deploymentNodeSpec` rows, which are DECLARATIONS
 * of version, replicas and digest. The rule is state/deployments.ts's (property
 * 2 in its header) and renderRemoteInstance already obeys it; a single label
 * here would be the place the asymmetry finally got hidden.
 *
 * NO ITEMS IS NOT AN EMPTY STATE, and the sentence says which of the two
 * no-items cases this is. A remote run this editor read no specs for and a
 * local run that never got to record a step look identical as an empty list and
 * are completely different facts.
 */
function runItems(instance: Instance, run: Run): string {
  const label = instance.kind === "remote" ? "Node types" : "Steps";
  if (run.items.length === 0) {
    return `<h2>${label}</h2>
<p class="lede">${escapeHtml(
      instance.kind === "remote"
        ? "No per-tier specs were read for this deployment."
        : "This run recorded no steps.",
    )}</p>`;
  }
  return `<h2>${label}</h2>
<ul class="runs">${run.items.map(itemRow).join("")}</ul>`;
}

function itemRow(item: RunItem): string {
  // The status travels as a data attribute as well as text so the stylesheet
  // can mark a failure without this fragment choosing a colour -- the same
  // arrangement `data-status` already has on the run blocks.
  return `<li class="run" data-status="${escapeHtml(item.status)}">
  <span class="run-kind">${escapeHtml(item.label)}</span>
  <span class="run-detail">${escapeHtml(
    [item.status, item.detail ?? ""].filter((part) => part !== "").join("  "),
  )}</span>
</li>`;
}

export function renderRunDetail(input: RunDetailInput): string {
  const { instance, run } = input;
  const row = runRowStatus(run, input.nowMs);
  // TWO ROUTES, ONE SET. A local action is a `data-choose` the panel narrows
  // against `instanceActions`; a deploy-control action is a `data-deploy` the
  // panel runs through the deploy controller -- exactly the two the instance
  // pages already post. The BUTTONS come from one place either way, which is
  // what keeps this page from becoming a second authority on what an instance
  // offers; only the wire differs, and it differs because the machinery does.
  const actions = input.actions
    .map((action) => {
      const destructive = action.id === "uninstall" || action.typeToConfirm === true;
      const attribute =
        action.deployAction === undefined
          ? `data-choose="${escapeHtml(action.id)}"`
          : `data-deploy="${escapeHtml(action.deployAction)}"`;
      return `<button class="${
        destructive ? "secondary destructive" : "primary"
      }" type="button" ${attribute} title="${escapeHtml(action.detail)}">${escapeHtml(
        action.label,
      )}</button>`;
    })
    .join("");

  // The lane fact, for a cluster running a developer's own build. Named from
  // the INSTANCE and labelled as the instance's, never folded into the run's
  // sentence -- see runVersionFact. It stays in the PRODUCT tier: which images
  // a cluster is running is the difference between a developer's build and a
  // release, which is a fact about what the cluster IS.
  const lane =
    instance.imageSource === "checkout"
      ? fact("image source", checkoutVersionText(instance))
      : instance.kind === "local" && run.kind === "rebuild"
        ? // A rebuild whose cluster has since gone back to released images. Said
          // plainly, because the page would otherwise describe a checkout build
          // beside a cluster that is not running one.
          fact("image source", `${releasedImages(displayVersion(instance.version))} today`)
        : "";

  const outcome =
    input.outcome === ""
      ? ""
      : `<p class="${
          input.outcome.startsWith("ERROR") ? "error" : "notice"
        }">${escapeHtml(input.outcome)}</p>`;

  // WHAT MOVES INTO DIAGNOSTICS HERE IS THE RAW STAMPS, AND ONLY THOSE
  // (memql#4456). `duration` stays -- "4m 12s" is a fact an operator reads --
  // while `2026-08-14T10:00:00Z` twice over is a machine-readable rendering of
  // the same thing, and the run's own id is a support-case handle. The ITEMS
  // stay too: this is the run detail page, and what the run did is the reason
  // it exists (#4427 owns that content; this epic owns the layout around it).
  const diagnostics = diagnosticFacts([
    fact("run id", run.id),
    fact("started", run.startedAt),
    fact("finished", run.finishedAt ?? ""),
  ]);

  return renderScreen({
    title: row.label,
    actions: `${actions}
  <button class="secondary" type="button" data-act="back">Back</button>`,
    status: `<p class="lede">${escapeHtml(row.tooltip)}</p>
${input.error === "" ? "" : `<p class="error">${escapeHtml(input.error)}</p>`}
${outcome}`,
    details: `<div class="facts">
  ${fact("cluster", instance.name)}
  ${fact("kind", run.kind)}
  ${fact("versions", runVersionFact(instance, run))}
  ${lane}
  ${fact("status", run.status)}
  ${fact("reason", failureReason(run))}
  ${fact("duration", runDuration(run.startedAt, run.finishedAt))}
</div>
${runItems(instance, run)}`,
    logs: renderDiagnostics(input.diagnosticsOpen, diagnostics),
  });
}
