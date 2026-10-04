// The Deployments page's run screens: the rebuild and update forms, and the
// running and failed screens it draws for a deploy, a rebuild and an update.
//
// WHAT LEFT. The Add a cluster panel drew its collect, run and uninstall
// screens from here too; it is on the page kit now (addClusterScreens.ts), and
// the renderers only it used went with it. What stays is what
// webview/deploymentPanel.ts still draws, until that page moves to the kit.
//
// A PURE LIFT. Every renderer below is the panel's method with `this` replaced
// by arguments and nothing else changed -- same markup, same wording, same
// branches. That is the point: a regression in the change that follows must
// have exactly one possible cause, and it cannot be "something was improved in
// passing".
//
// THE THREE CONSTRAINTS THAT GOVERN THIS FILE, unchanged by the move:
//
//  1. NO DOM. These return strings; the panel interpolates them into a
//     document. view-kit's renderers are reached through renderToHtml for the
//     same reason.
//  2. NO INLINE EVENT HANDLERS. Interactivity is `data-act` / `data-field` /
//     `data-remedy` attributes plus one delegated listener in the panel,
//     because the webview's Content-Security-Policy forbids inline script.
//  3. ESCAPING IS NOT OPTIONAL. Every value that reaches the output goes
//     through `escapeHtml` -- a domain, a step's remedy and an operator's own
//     typing all arrive here as untrusted text.
//
// Deliberately free of `vscode` imports. It lives under src/webview/ because
// it renders a webview's body, but it is not an adapter: it holds no panel
// lifecycle, so it stays out of the allow-list in
// cmd/memql-lsp/vscodeimportrule_test.go and remains testable under bare
// `node --test`.
//
// Refs: #3738 #3733

import { renderInstallSteps } from "@znasllc-io/memql-view-kit";
import type { PreflightItem } from "../state/preflight.js";
import { escapeHtml, renderToHtml } from "@znasllc-io/memql-view-kit";

import type { StepProgress } from "../state/addCluster.js";
import type { UpdateStrategy } from "../state/updatePreflight.js";
import {
  failureGuidance,
  runIsSettled,
  runProgressOf,
  toStepViews,
} from "../state/installProgress.js";
import type { RunProgress } from "../state/runProgress.js";
import { brandMarkSvg } from "./brandTokens.js";
import { renderRunLogPane } from "./runLogPane.js";
import { renderScreen } from "./screenLayout.js";

/**
 * The preflight checklist, above the actions so Start is an informed click.
 *
 * EXPORTED since memql#4246: the rebuild screen renders the same list, from
 * state/rebuildPreflight.ts, and a second renderer would be a second answer to
 * what a warning LOOKS LIKE in this extension. The two checklists share
 * `PreflightItem` precisely so they can share this.
 */
export function renderPreflight(items: readonly PreflightItem[] | undefined): string {
  if (items === undefined || items.length === 0) return "";
  const rows = items
    .map(
      (item) => `<li class="preflight-item ${item.state}">
  <span class="preflight-mark">${item.state === "ok" ? "OK" : "NOTE"}</span>
  <span class="preflight-label">${escapeHtml(item.label)}</span>
  <span class="preflight-detail">${escapeHtml(item.detail)}</span>
</li>`,
    )
    .join("");
  return `<h2 class="preflight-heading">Before it runs</h2>
<ul class="preflight">${rows}</ul>`;
}

// ---------------------------------------------------------------------------
// rebuild from checkout (memql#4246)
// ---------------------------------------------------------------------------

export interface RebuildScreenInput {
  /** The checkout the images are built FROM -- named, never assumed. */
  checkoutDir: string;
  /** Comma-separated node types, or "" for all app nodes. */
  nodes: string;
  /**
   * The rebuild checklist (state/rebuildPreflight.ts). Absent while the panel
   * is still gathering it -- git and the Docker probe are both async -- and the
   * screen renders without, exactly as the collect screen does.
   */
  preflight?: readonly PreflightItem[];
}

/**
 * The one thing a rebuild asks before it runs.
 *
 * ONE FIELD, AND IT IS OPTIONAL. Everything else a rebuild needs is already
 * recorded -- where the checkout is, which Application, which cluster -- and a
 * form that asked again would invite an answer that disagrees with the machine.
 * The node list is the exception because it is not a fact about the machine: it
 * is what the developer wants built THIS time, and an empty box means "all of
 * them", which is the script's own default rather than a value invented here.
 *
 * The checklist above it is where the lane crossing is stated. That is the
 * whole reason this screen exists instead of the button running immediately.
 */
export function renderRebuildScreen(input: RebuildScreenInput): string {
  return renderScreen({
    title: "Rebuild from checkout",
    actions: `<button class="primary" type="button" data-act="beginRebuild">Start</button>
  <button class="secondary" type="button" data-act="back">Back</button>`,
    // The checklist rides in the status area for the reason renderCollectScreen
    // states: it is the thing Start needs to be informed BY, so it belongs
    // beside Start rather than at the end of what Start acts on.
    status: `<p class="lede">Builds the node images from ${escapeHtml(
      input.checkoutDir,
    )}, imports them into the cluster, points its Application at them, and restarts.</p>
${renderPreflight(input.preflight)}`,
    details: `<div class="field">
  <label for="f-nodes">Node types to rebuild (comma-separated; empty = all app nodes)</label>
  <input id="f-nodes" data-field="nodes" value="${escapeHtml(input.nodes)}">
  <div class="hint">For example: bff, agent. Leave it empty to rebuild every app node.</div>
</div>`,
  });
}

// ---------------------------------------------------------------------------
// update from origin and rebuild (memql#4578)
// ---------------------------------------------------------------------------

export interface UpdateScreenInput {
  /** The checkout that gets updated and then built. Named, never assumed. */
  checkoutDir: string;
  /** Comma-separated node types, or "" for all app nodes. */
  nodes: string;
  /** What the run does when the checkout has commits the branch does not. */
  strategy: UpdateStrategy;
  /**
   * The update checklist (state/updatePreflight.ts). Absent while the panel is
   * still gathering it -- git and the Docker probe are both async -- and the
   * screen renders without, exactly as the rebuild and collect screens do.
   */
  preflight?: readonly PreflightItem[];
  /**
   * Whether the checklist found something that makes Start pointless.
   *
   * TWO CASES ONLY (see `updateIsBlocked`), and both are refusals the script
   * makes before it fetches. Everything else is a MAYBE that only the run can
   * settle, and a Start disabled on a maybe would withhold a button whose
   * outcome is very often success.
   */
  blocked?: boolean;
}

/**
 * What the screen says while it is still finding out.
 *
 * The rebuild screen shows nothing in this gap and its Start stays live,
 * because nothing that Start SENDS depends on the facts. Here one thing does --
 * the branch, which is read off the checkout when it is on one and off the
 * install's record when it is not -- so Start waits, and waiting silently
 * reads as a broken button.
 */
const UPDATE_GATHERING = `<p class="hint">Checking what an update would do...</p>`;

/**
 * The two things an update asks before it runs.
 *
 * The node list is the rebuild screen's field, unchanged and for its reasons.
 * The strategy is the one genuinely new question, and it is a question rather
 * than a policy because the two answers suit two different people: somebody
 * testing main wants to be told their own commits are in the way, and somebody
 * mid-feature wants them combined. Neither is a safe default for the other.
 *
 * IT DEFAULTS TO THE ONE THAT CHANGES LESS. Refusing leaves the checkout
 * exactly as it was and costs a second click; combining writes a commit and can
 * stop half-way with conflicts to resolve. A default that can leave a developer
 * in a merge they did not ask for is the wrong way round.
 */
export function renderUpdateScreen(input: UpdateScreenInput): string {
  // DISABLED WHILE GATHERING, unlike the rebuild screen's, and for one concrete
  // reason: the run is sent a `--branch`, resolved as "the branch the checkout
  // is on, or the one the install recorded when it is on none". A detached
  // checkout is ordinary here -- a release install detaches at a tag and a
  // repair detaches at an exact commit -- so a Start pressed in the second
  // before the read lands would send no branch at all and be refused for a
  // missing parameter the operator never saw a field for.
  const gathering = input.preflight === undefined;
  const start =
    input.blocked === true || gathering
      ? `<button class="primary" type="button" data-act="beginUpdate" disabled>Start</button>`
      : `<button class="primary" type="button" data-act="beginUpdate">Start</button>`;
  const option = (value: UpdateStrategy, label: string): string =>
    `<option value="${value}"${input.strategy === value ? " selected" : ""}>${escapeHtml(label)}</option>`;
  return renderScreen({
    title: "Update from origin and rebuild",
    actions: `${start}
  <button class="secondary" type="button" data-act="back">Back</button>`,
    status: `<p class="lede">Brings ${escapeHtml(
      input.checkoutDir,
    )} up to date with the latest code, then builds the node images from it, imports them into the cluster, and restarts. Your uncommitted changes come along; if they cannot, this stops and tells you which files are involved.</p>
${gathering ? UPDATE_GATHERING : renderPreflight(input.preflight)}`,
    details: `<div class="field">
  <label for="f-strategy">If this checkout has commits the branch does not</label>
  <select id="f-strategy" data-field="strategy">${option(
    "fastForward",
    "Stop and tell me (nothing is changed)",
  )}${option("merge", "Combine them with the latest")}</select>
  <div class="hint">Combining writes a commit, so it needs everything committed or set aside first.</div>
</div>
<div class="field">
  <label for="f-nodes">Node types to rebuild (comma-separated; empty = all app nodes)</label>
  <input id="f-nodes" data-field="nodes" value="${escapeHtml(input.nodes)}">
  <div class="hint">For example: bff, agent. Leave it empty to rebuild every app node.</div>
</div>`,
  });
}

// ---------------------------------------------------------------------------
// running
// ---------------------------------------------------------------------------

/**
 * Which run this is, which is the only thing the wording turns on.
 *
 * Three words for ONE code path. Install, repair and deploy are the same graph
 * with the same steps and the same verify-then-skip behaviour -- what differs
 * is what the operator asked for, and a heading that said "Installing" over a
 * deployment to another tag would describe a reinstall of their machine.
 *
 * `uninstall` joined them for the BLOCK, not for the screen (memql#4454). The
 * removal keeps its own five-phase screen in the wizard -- it has no Retry and
 * no guided mode, and its wording is about taking a cluster apart -- but it is
 * still a graph run with steps ahead of it, so it renders the same mark, the
 * same bar and the same one-line narration through `renderRunBlock`. A
 * separate progress display for the one run that removes things would be the
 * place the two drifted.
 */
export type RunMode = "install" | "repair" | "deploy" | "rebuild" | "update" | "uninstall";

export interface RunningScreenInput {
  steps: readonly StepProgress[];
  mode: RunMode;
  /** Whether a run is actually in flight -- see the comment on the empty list. */
  running: boolean;
  /**
   * Whether this run KEPT anything (memql#5118, D8).
   *
   * Only an uninstall can, and it changes what the finished run says about
   * itself. Optional rather than required, unlike `logsOpen` below, because an
   * absent value is CORRECT for every other mode -- there is nothing a caller
   * could silently get wrong by leaving it out.
   */
  kept?: boolean;
  /**
   * Whether the log disclosure is open, from the STATE module (memql#4455).
   *
   * REQUIRED RATHER THAN DEFAULTED, and the compile error is the point. Both
   * panels re-render wholesale, so this flag has to be threaded from panel
   * state or the pane can never stay open -- and a default of `false` would
   * make that failure silent: the toggle would appear to do nothing, once per
   * second, with nothing in any log to say why.
   */
  logsOpen: boolean;
  /** Whether the pane is still pinned to the tail. See `LogPaneInput.follow`. */
  logsFollow: boolean;
  /**
   * The run's progress, from the state machine's own `progress(now)`, which
   * remembers its high-water mark so the bar never runs backwards.
   *
   * Optional so a caller holding only a step list (a gallery page, a test)
   * still renders: the block then computes it from the steps at `now`, without
   * that memory.
   */
  progress?: RunProgress;
  /** The moment to compute progress at when `progress` is absent. Defaults to the clock. */
  now?: number;
}

const RUN_HEADING: Readonly<Record<RunMode, string>> = {
  install: "Installing a local cluster",
  repair: "Repairing the local cluster",
  deploy: "Deploying to the local cluster",
  rebuild: "Rebuilding the local cluster from its checkout",
  update: "Updating your checkout and rebuilding the local cluster from it",
  uninstall: "Removing the local cluster",
};

const RUN_LEDE: Readonly<Record<RunMode, string>> = {
  install: "Each step proves itself before the next one starts.",
  repair:
    "Every step checks first and is skipped when it is already satisfied, so only what is actually missing runs.",
  // The same sentence as a repair, and the same fact: only the checkout and the
  // reconcile have work to do when nothing but the tag has changed.
  deploy:
    "Every step checks first and is skipped when it is already satisfied, so only what is actually missing runs.",
  // NOT the verify-then-skip sentence, because a rebuild does not: it is one
  // step that always does its work. What it needs to say instead is the SHAPE
  // of that work, since it is a single progress row that takes minutes.
  rebuild:
    "Build, import, point the cluster at the images, restart. Each step reports as it goes.",
  // The update runs FIRST and can stop the run before a single image is built,
  // which is the one thing this sentence has to say that the rebuild's does
  // not: an operator watching the first row is watching the step that decides
  // whether the other one happens at all.
  update:
    "Update first, then build, import, point the cluster at the images, restart. If your own changes cannot be brought up to date, nothing is built.",
  // Each step reverses one entry in the receipt, in the order the graph gives
  // -- each tool outlives the artifact it is needed to remove.
  uninstall:
    "Each step reverses one entry in the receipt, so every tool outlives the artifact it is needed to remove.",
};

/**
 * What a finished run of each kind says about itself.
 *
 * A SENTENCE, NOT A WORD. "Done" tells an operator the process exited; these
 * say what now exists, which is what they were waiting to hear.
 */
const RUN_DONE: Readonly<Record<RunMode, string>> = {
  install: "Installed. The cluster is up and ready to sign in to.",
  repair: "Repaired. Everything that was missing has been put back.",
  deploy: "Deployed. The cluster is running the version you chose.",
  rebuild: "Rebuilt. The cluster is running the images built from your checkout.",
  update: "Up to date and rebuilt. The cluster is running the images built from your checkout.",
  uninstall: "Removed. Everything the install put on this machine has been taken back.",
};

/**
 * A step's description with its full stop taken off, for embedding in a phrase.
 *
 * The descriptions are SENTENCES -- the CLI prints them as sentences and the
 * narration line renders them as one -- so they end in a stop. Every place that
 * builds a longer phrase around one ("<description> failed", "<description> --
 * and 2 more in progress") otherwise reads "...services in it. failed", which
 * is the kind of small wrongness that makes a product look unfinished. Found by
 * rendering the screens rather than by reading them.
 */
function phrase(description: string): string {
  return description.replace(/\.$/, "");
}

/**
 * The branded run block: the mark, a bar, and one line about what is happening
 * (memql#4454).
 *
 * WHAT IT REPLACED. The run screen WAS the step checklist -- thirteen rows,
 * each accumulating the verbatim stderr of the script behind it. That is a
 * truthful record and a poor headline: it answers "what did kubectl print"
 * when the question an operator has during a ten-minute install is "how much
 * longer, and is it going well". The checklist is still here; it is below the
 * fold, where a record belongs.
 *
 * THE BAR IS DETERMINATE BECAUSE THE NUMBER IS REAL. `runStarted` seeds the
 * steps AHEAD (state/addCluster.ts says why), and each step carries the time
 * it is expected to take, so the percent is the share of the run's expected
 * time already behind it (state/runProgress.ts) rather than an animation.
 * Before that event lands there is no plan, and the bar renders INDETERMINATE
 * rather than at 0% -- "we do not know yet" and "nothing has happened yet" are
 * different claims and only one of them is true then.
 *
 * THE LINE UNDER IT IS THE STEP'S SHORT LABEL, or the phase it last reported
 * ("Starting services 5 of 9"), never its long description: the description
 * is the checklist's, below.
 *
 * NO INLINE `style` ATTRIBUTE, and this is not a stylistic choice. Every panel
 * here runs under `style-src 'nonce-...'` with no `'unsafe-inline'`, and a
 * nonce cannot apply to a style ATTRIBUTE -- so `style="width: 42%"` is not
 * merely discouraged, it is dropped by the browser and the bar renders empty at
 * every value. The width arrives through `data-percent` against rules
 * brandTokens.ts generates for 0..100.
 */
export function renderRunBlock(input: RunningScreenInput): string {
  const progress = input.progress ?? runProgressOf(input.steps, input.now ?? Date.now());
  const determinate = input.steps.length > 0;
  const settled = runIsSettled(input.steps);
  // THE FAILED STEP, NOT WHATEVER IS STILL RUNNING. This once read the
  // narration of the steps currently IN FLIGHT first -- so a failure in one
  // branch of a wave was announced under the name of a healthy step in another
  // ("Issuing the certificate ... failed").
  // A wave runs under Promise.all and independent branches are allowed to
  // finish, so the two are routinely different steps. The FIRST failure is the
  // one named, the same rule `AddClusterState.failedId` follows and for the
  // same reason: the others may be consequences of it.
  const failed = input.steps.find((step) => step.state === "failed");

  // WHICH TERMINAL STATE THIS IS, DERIVED RATHER THAN PASSED. A run that is not
  // in flight and has not settled is one that STOPPED -- cancelled, or aborted
  // with the panel still on this screen. Deriving it means the block cannot
  // disagree with the step list beside it, which a third boolean threaded from
  // two different panels eventually would.
  const message = failed !== undefined
    ? `${phrase(failed.description === "" ? failed.id : failed.description)} failed -- see the log below.`
    : settled
      ? RUN_DONE[input.mode]
      : input.steps.length === 0
        ? input.running
          ? "Starting. The first step will appear here as it begins."
          : "Nothing has been run."
        : !input.running
          ? "Stopped. Nothing further will run; what had already finished is still done."
          : progress.status;

  // `aria-valuetext` carries the human position so a screen reader hears
  // "Step 4 of 14" rather than "42 percent", which is the number the sighted
  // reader is getting from the sentence rather than from the bar.
  const bar = determinate
    ? `<div class="run-bar" role="progressbar" aria-valuemin="0" aria-valuemax="100" aria-valuenow="${
        progress.percent
      }"${
        progress.stepText === ""
          ? ""
          : ` aria-valuetext="${escapeHtml(progress.stepText)}"`
      }><div class="run-bar-fill" data-percent="${progress.percent}"></div></div>`
    : `<div class="run-bar indeterminate" role="progressbar" aria-valuetext="Starting"><div class="run-bar-fill indeterminate"></div></div>`;

  const position =
    progress.stepText === "" || settled || failed !== undefined
      ? ""
      : ` <span class="run-position">${escapeHtml(progress.stepText)}</span>`;

  return `<div class="run-block">
  <div class="run-mark">${brandMarkSvg(48)}</div>
  ${bar}
  <p class="run-message">${escapeHtml(message)}${position}</p>
</div>`;
}

/**
 * The full step record, below the decision and below the headline.
 *
 * STILL EVERY STEP AND EVERY STATE -- nothing was dropped when it stopped being
 * the headline. view-kit's `renderInstallSteps` over the projection in
 * state/installProgress.ts, exactly as before; what changed is where it sits.
 */
function renderStepDetails(steps: readonly StepProgress[]): string {
  if (steps.length === 0) return "";
  return `<h2 class="steps-heading">Steps</h2>
<div class="step-list">${renderToHtml(renderInstallSteps(toStepViews(steps)))}</div>`;
}

/**
 * The run in progress.
 *
 * REPAIR IS THE SAME RUN WITH DIFFERENT WORDING. Every step verifies first and
 * skips when satisfied, so re-running the graph IS the repair; only the heading
 * and the lede differ, and there is no second code path below them.
 *
 * ACTIONS FIRST (memql#4453): Cancel is offered for exactly as long as there is
 * something to stop, and it is offered at the TOP, because an operator who
 * wants out of a ten-minute run should not have to scroll past the run to
 * find the way out. A cancelled run leaves a valid receipt -- what ran, ran,
 * and an uninstall can still take it back -- so this is safe at any point.
 */
export function renderRunningScreen(input: RunningScreenInput): string {
  const settled = runIsSettled(input.steps);
  return renderScreen({
    title: RUN_HEADING[input.mode],
    actions: settled
      ? `<button class="secondary" type="button" data-act="back">Back</button>`
      : `<button class="secondary" type="button" data-act="cancel">Cancel</button>`,
    status: `<p class="lede">${escapeHtml(RUN_LEDE[input.mode])}</p>
${renderRunBlock(input)}`,
    details: renderStepDetails(input.steps),
    logs: renderRunLogPane({
      steps: input.steps,
      open: input.logsOpen,
      follow: input.logsFollow,
    }),
  });
}

// ---------------------------------------------------------------------------
// failedStep
// ---------------------------------------------------------------------------

export interface FailedScreenInput extends RunningScreenInput {
  failures: readonly StepProgress[];
}

/**
 * A step failed, and what that means -- for EVERY step that failed.
 *
 * ONE BLOCK PER FAILURE (memql#3474). A wave runs under `Promise.all` and the
 * executor deliberately lets independent branches finish, so a run can arrive
 * here with several failures. This screen used to render guidance for
 * whichever one resolved last, which is a scheduling accident: the exit codes
 * genuinely differ, and a refusal (3) asks for something entirely different
 * from a missing prerequisite (4). Showing one of N is confident advice about
 * a step the operator may not even be looking at.
 *
 * BOTH RECOVERIES ARE ALWAYS OFFERED. `failureGuidance().retryable` says
 * whether an UNCHANGED retry could plausibly differ -- it does not gate the
 * button, because the operator may have fixed the cause in another window
 * while this panel sat here, and we cannot know that.
 */
export function renderFailedScreen(input: FailedScreenInput): string {
  const { failures } = input;
  if (failures.length === 0) return renderRunningScreen(input);

  const many = failures.length > 1;
  const heading = many
    ? `${failures.length} steps failed`
    : `${phrase(failures[0]!.description === "" ? failures[0]!.id : failures[0]!.description)} failed`;

  // Each failure keeps its own name above its own guidance. With one failure
  // the name is already the heading, so repeating it would be noise.
  //
  // WHAT IS NO LONGER PASSED TO `failureGuidance` IS THE LOG (memql#4456). It
  // used to fall back to `failure.log` when the capability named no reason,
  // which put verbatim stderr into the screen's status area -- the one place
  // D4 says it must never be. The `reason` is the capability's own sentence,
  // written for humans by contract; when there is none, the guidance for the
  // exit code stands on its own and the output is one disclosure away.
  const blocks = failures
    .map((failure) => {
      const guidance = failureGuidance(failure.exitCode, failure.remedy, failure.reason);
      const name = failure.description === "" ? failure.id : failure.description;
      // WHAT THE STEP ITSELF SAID, above the generic advice for its exit code.
      // The guidance is keyed on a number and so can only ever be about a
      // CLASS of failure; the capability's own sentence is about this one.
      const said =
        failure.reason === "" ? "" : `<p class="said">${escapeHtml(failure.reason)}</p>`;
      return `${many ? `<h2>${escapeHtml(name)}</h2>` : ""}
${said}
<p class="lede">${escapeHtml(guidance.headline)}</p>
<p>${escapeHtml(guidance.advice)}</p>
${renderRemedy(failure)}`;
    })
    .join("");

  // NO "SWITCH TO GUIDED" (memql#5118 audit). It set a flag nothing that
  // runs a step ever read and re-ran the graph exactly as Retry does; the
  // remedy's "Open a terminal with this command" is the real manual path.
  // THE ACTIONS STAY AT THE TOP ON A FAILURE (memql#4453), and this is the
  // screen most likely to be argued into an exception. It is the same argument
  // as everywhere else, only sharper: an operator reading a failure is an
  // operator deciding what to do next, and the failure summary is the INPUT to
  // that decision rather than a queue in front of it. The labels count --
  // "Retry this step" in front of three failures names one thing and does
  // another, because the recovery re-runs the graph and every failed step goes
  // back into it.
  return renderScreen({
    title: heading,
    actions: `<button class="primary" type="button" data-act="retry">${
      many ? "Retry these steps" : "Retry this step"
    }</button>
  <button class="secondary" type="button" data-act="cancel">Cancel</button>`,
    status: `${renderRunBlock(input)}
${blocks}`,
    details: renderStepDetails(input.steps),
    // OPEN, AND ANCHORED ON THE FIRST FAILURE. `AddClusterState` set `logsOpen`
    // when the step failed; the anchor is passed here so the panel's script can
    // bring that step's output into view rather than leaving the operator to
    // scroll a pane for the one block that matters.
    logs: renderRunLogPane({
      steps: input.steps,
      open: input.logsOpen,
      follow: input.logsFollow,
      focusStepId: failures[0]!.id,
    }),
  });
}

/**
 * The one command that fixes this failure, and a button that runs it
 * (memql#3551).
 *
 * WHY THIS EXISTS AT ALL. The runner spawns every capability UNPRIVILEGED,
 * with no sudo, pkexec or askpass anywhere in the extension -- and two steps
 * in the install graph need root: `hostsBlock` edits /etc/hosts, and the
 * docker gate's remedy adds the operator to a group. Without a handoff, the
 * wizard's only honest move on those is to print a command and stop, which is
 * where it had quietly arrived: the uninstall preview even promises "[sudo]
 * needs your password", a promise nothing in the code fulfilled.
 *
 * THE COMMAND IS NOT TYPED FOR THE OPERATOR TO WATCH IT RUN. The panel's
 * handler puts it in the terminal WITHOUT a newline, so nothing executes until
 * a person reads it and presses Enter. A privileged command that ran itself
 * the instant a button was clicked would be a worse thing than the problem it
 * solves, and the operator's own shell is where their sudo prompt and their
 * password belong -- MemQL never sees either.
 *
 * The button carries the step's ID and NOT the command: the panel looks the
 * command up against the failures it recorded, so nothing running in that
 * iframe can choose what the operator is invited to run as root.
 */
export function renderRemedy(failure: { id: string; remedy: string }): string {
  if (failure.remedy === "") return "";
  return `<p>Run this to fix it:</p>
<pre class="remedy">${escapeHtml(failure.remedy)}</pre>
<div class="actions">
  <button class="secondary" type="button" data-remedy="${escapeHtml(failure.id)}">
    Open a terminal with this command
  </button>
</div>`;
}
