// The install-run screens, shared by the add-cluster wizard and Deployments.
//
// Three of `addClusterPanel.ts`'s screens are not about ADDING A CLUSTER at
// all -- they are about running the install graph, and the Deployments surface
// runs the same graph for a create, an upgrade and a repair. `collect`,
// `running` and `failedStep` are lifted here so the second caller reuses them
// instead of copying a 2200-line wizard. What stays behind is what is genuinely
// the wizard's: the landing cards, the `connect` form for registering a remote
// cluster, the uninstall preview, and `done`.
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

import type { AddClusterAction } from "../clusters/presence.js";
import type {
  FieldError,
  InputField,
  Inputs,
  StepProgress,
} from "../state/addCluster.js";
import { MAIN_BRANCH_CHOICE, isMainBranchChoice } from "../install/stackPin.js";
import { compareSemverDesc } from "../install/tags.js";
import { SUPPORTED_PROVIDERS, optionalFields, requiredFields } from "../state/addCluster.js";
import {
  failureGuidance,
  runIsSettled,
  runNarration,
  runProgress,
  toStepViews,
} from "../state/installProgress.js";
import { brandMarkSvg } from "./brandTokens.js";
import { renderRunLogPane } from "./runLogPane.js";
import { renderScreen } from "./screenLayout.js";

/** The collected fields, in the order they are asked for. */
export const INPUT_FIELDS: readonly InputField[] = [
  "domain",
  "ownerFirstName",
  "ownerLastName",
  "ownerEmail",
  "provider",
  "providerKeyFile",
  "version",
];

/**
 * The fields rendered as a CHOICE rather than as a text box.
 *
 * `provider` is one because the set is closed and the script refuses anything
 * outside it with exit 2 -- whose guidance says "a fault in MemQL rather than
 * in your machine or your answers", which would be a lie about a value the
 * operator typed. A control that cannot express the wrong answer is the fix;
 * `problemWith` is the second wall, for a message this page did not render.
 */
export const CHOICE_FIELDS: Partial<Record<InputField, readonly string[]>> = {
  provider: SUPPORTED_PROVIDERS,
};

/** The label each collected field carries. */
export const FIELD_LABELS: Record<InputField, string> = {
  domain: "Domain",
  ownerFirstName: "First name",
  ownerLastName: "Last name",
  ownerEmail: "Email address",
  provider: "AI provider",
  providerKeyFile: "AI provider key file",
  version: "Version",
};

/** What each field is for, in one line. */
export const FIELD_HINTS: Record<InputField, string> = {
  domain: "The cluster answers at api.<domain>. Defaults are fine if you have no preference.",
  ownerFirstName: "The cluster owner -- you.",
  ownerLastName: "",
  provider:
    "Which vendor the key below belongs to. Supply one and the installer makes a single authenticated call to check it before anything on this machine changes; leave both empty and it makes none.",
  ownerEmail: "Used to create the owner account. A local cluster sends no mail.",
  providerKeyFile:
    "A PATH to a file holding the key, never the key itself: a command line is readable by every process on this machine.",
  version:
    "Which MemQL release to install. Latest is preselected and is what a fresh install wants -- a release's manifests and its node images ship together at that tag. Choosing `main` instead clones the repository and BUILDS the node images from that checkout: it needs Docker and takes several minutes.",
};

export const COLLECT_TITLE: Partial<Record<AddClusterAction, string>> = {
  install: "Install a local cluster",
  installGuided: "Install a local cluster -- guided",
  repair: "Repair the local cluster",
};

// ---------------------------------------------------------------------------
// collect
// ---------------------------------------------------------------------------

export interface CollectScreenInput {
  action: AddClusterAction;
  values: Inputs;
  errors: readonly FieldError[];
  /**
   * The release tags offered for `version`, newest first (memql#3882).
   *
   * DYNAMIC, unlike CHOICE_FIELDS, which is why it rides here rather than
   * there: the set comes from `git ls-remote --tags` at page-open time, not
   * from a constant this file could hold.
   *
   * EMPTY IS AN ORDINARY OUTCOME and renders the free-text box instead --
   * `git ls-remote` needs a network and a git, and an operator on a plane has
   * neither and still has a cluster to install. That is the same degradation
   * `install/tags.ts` documents for the deployment picker, for the same reason.
   */
  versionChoices?: readonly string[];
  /**
   * The "Before it runs" checklist (memql#4195): what the run will need,
   * stated before the Start button rather than at the moment each fact bites.
   * Absent while the panel is still gathering it; the screen renders without.
   */
  preflight?: readonly PreflightItem[];
}

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

/**
 * One field's markup. Extracted from `renderCollectScreen` when the AI
 * provider fields became optional (epic memql#4440): the disclosure below the
 * required fields renders exactly the same control, and a second copy of this
 * would be a second answer to what a field LOOKS LIKE on this page.
 */
function renderField(input: CollectScreenInput, field: InputField): string {
  const { values, errors } = input;
  const error = errors.find((e) => e.field === field);
  const hint = FIELD_HINTS[field];
  const choices = CHOICE_FIELDS[field];
  // THE ONE FIELD THAT NAMES A FILE GETS A FILE PICKER (memql#3547).
  //
  // Typing a path is the error-prone way to name a file, and this is the
  // path an operator is least able to check: it holds a secret, so
  // nothing on this page can echo its contents back as confirmation. The
  // picker removes the whole class -- what it returns exists, is a file,
  // and is spelled the way the filesystem spells it.
  //
  // Typing still works, and both routes end in the same validation. The
  // dialog is the extension host's (`vscode.window.showOpenDialog`);
  // a webview cannot open one itself, which is why this is a button that
  // posts a message rather than an `<input type="file">` -- and an
  // `<input type="file">` would hand back a File object with no path,
  // which is not what a `--key-file` flag can be given.
  const browse =
    field === "providerKeyFile"
      ? `<button class="secondary browse" type="button" data-act="browseKeyFile">Browse...</button>`
      : "";
  // `version` is the one field whose options are not a constant: they come
  // off the remote at page-open time. Falls through to the text box when
  // the listing is empty, which is what makes a no-network install still
  // able to name a version.
  //
  // `main` is appended to that listing as a labelled choice rather than a
  // tag (memql#3901, relabelled by memql#4430). It is NOT offered when the
  // listing is empty: with no network the field is a free-text box, and a
  // text box that accepts "main" would be an unlabelled branch, which
  // clone-stack.sh refuses on purpose -- and a from-source lane with no
  // network could not clone the repository it would build from anyway.
  const fieldChoices: readonly VersionChoice[] | undefined =
    field === "version"
      ? (input.versionChoices ?? []).length === 0
        ? undefined
        : versionChoiceList(input.versionChoices ?? [], values.version)
      : choices?.map((choice) => ({ value: choice, label: choice }));
  const control =
    fieldChoices === undefined
      ? `<div class="control-row"><input id="f-${field}" data-field="${field}" value="${escapeHtml(
          values[field],
        )}">${browse}</div>`
      : `<select id="f-${field}" data-field="${field}">${fieldChoices
          .map(
            (choice) =>
              `<option value="${escapeHtml(choice.value)}"${
                choice.value === values[field] ? " selected" : ""
              }>${escapeHtml(choice.label)}</option>`,
          )
          .join("")}</select>`;
  return `<div class="field" data-invalid="${error !== undefined}">
  <label for="f-${field}">${escapeHtml(FIELD_LABELS[field])}</label>
  ${control}
  ${hint === "" ? "" : `<div class="hint">${escapeHtml(hint)}</div>`}
  ${error === undefined ? "" : `<div class="error">${escapeHtml(error.message)}</div>`}
</div>`;
}

/**
 * The AI-provider disclosure (epic memql#4440).
 *
 * COLLAPSED, AND THAT IS THE POINT. Installation spends no inference, so the
 * honest presentation of a vendor key is "there is a slot for this if you
 * happen to have one", not a field between the operator and the Start button.
 * An operator with no key -- which after this epic is the expected case --
 * should be able to read the form top to bottom and never learn that LLM
 * vendors exist.
 *
 * FORCED OPEN when one of its fields is in error, because a validation
 * message inside a closed `<details>` is a form that refuses to start and
 * will not say why. That is the failure mode a disclosure introduces, and it
 * is the only reason this function takes the errors at all.
 */
export function renderProviderDisclosure(
  input: CollectScreenInput,
  fields: readonly InputField[],
): string {
  if (fields.length === 0) return "";
  const invalid = fields.some((field) => input.errors.some((e) => e.field === field));
  const rendered = fields.map((field) => renderField(input, field)).join("");
  return `<details class="optional-section"${invalid ? " open" : ""}>
  <summary>AI provider (optional -- configure later in the portal)</summary>
  <p class="hint">${escapeHtml(
    "Nothing here is needed to install, start, repair or upgrade the cluster, and leaving it empty makes no call to any AI vendor. Providers are configured after the install at Settings -> AI providers in the portal, where workload identity federation is the recommended path for Anthropic. Supply a key here only if you already have one and would rather it were seeded during the run.",
  )}</p>
  ${rendered}
</details>`;
}

export function renderCollectScreen(input: CollectScreenInput): string {
  const required = new Set(requiredFields(input.action));
  const optional = new Set(optionalFields(input.action));

  const fields = INPUT_FIELDS.filter((field) => required.has(field))
    .map((field) => renderField(input, field))
    .join("");
  // Filtered from INPUT_FIELDS rather than taken from `optionalFields`
  // directly, so the disclosure lists them in the same declared order as
  // everything else on the page.
  const disclosure = renderProviderDisclosure(
    input,
    INPUT_FIELDS.filter((field) => optional.has(field)),
  );

  return renderScreen({
    title: COLLECT_TITLE[input.action] ?? "Install a local cluster",
    actions: `<button class="primary" type="button" data-act="begin">Start</button>
  <button class="secondary" type="button" data-act="back">Back</button>`,
    // THE CHECKLIST MOVES UP WITH THE BUTTON, NOT DOWN AWAY FROM IT
    // (memql#4453 over memql#4195). "Before it runs" was placed directly above
    // Start so that pressing Start was an informed click; with Start hoisted to
    // the top, leaving the checklist among the fields would have put it below
    // everything it warns about. In the status slot it sits immediately under
    // the actions row -- above the form rather than at the end of it -- so it is
    // now visible WITHOUT SCROLLING, which it was not before on a form this
    // long. What changed is that it is beside the button instead of under it.
    status: `<p class="lede">Everything is collected before any work starts, so the long part runs unattended.</p>
${renderPreflight(input.preflight)}`,
    details: `${fields}
${disclosure}`,
  });
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
export type RunMode = "install" | "repair" | "deploy" | "rebuild" | "uninstall";

export interface RunningScreenInput {
  steps: readonly StepProgress[];
  mode: RunMode;
  /** Whether a run is actually in flight -- see the comment on the empty list. */
  running: boolean;
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
}

const RUN_HEADING: Readonly<Record<RunMode, string>> = {
  install: "Installing a local cluster",
  repair: "Repairing the local cluster",
  deploy: "Deploying to the local cluster",
  rebuild: "Rebuilding the local cluster from its checkout",
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
 * steps AHEAD (state/addCluster.ts says why), so `settled / total` is a fact
 * about the graph rather than an animation. Before that event lands there is no
 * total, and the bar renders INDETERMINATE rather than at 0% -- "we do not know
 * yet" and "nothing has happened yet" are different claims and only one of them
 * is true then.
 *
 * NO INLINE `style` ATTRIBUTE, and this is not a stylistic choice. Every panel
 * here runs under `style-src 'nonce-...'` with no `'unsafe-inline'`, and a
 * nonce cannot apply to a style ATTRIBUTE -- so `style="width: 42%"` is not
 * merely discouraged, it is dropped by the browser and the bar renders empty at
 * every value. The width arrives through `data-percent` against rules
 * brandTokens.ts generates for 0..100.
 */
export function renderRunBlock(input: RunningScreenInput): string {
  const progress = runProgress(input.steps);
  const narration = runNarration(progress);
  const determinate = progress.total > 0;
  const settled = runIsSettled(input.steps);
  // THE FAILED STEP, NOT WHATEVER IS STILL RUNNING. This read
  // `narration.message` first, which is the description of the steps currently
  // IN FLIGHT -- so a failure in one branch of a wave was announced under the
  // name of a healthy step in another ("Issuing the certificate ... failed").
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
          : narration.message;

  // `aria-valuetext` carries the human position so a screen reader hears
  // "step 4 of 14" rather than "42 percent", which is the number the sighted
  // reader is getting from the sentence rather than from the bar.
  const bar = determinate
    ? `<div class="run-bar" role="progressbar" aria-valuemin="0" aria-valuemax="100" aria-valuenow="${
        progress.percent
      }"${
        narration.position === ""
          ? ""
          : ` aria-valuetext="${escapeHtml(narration.position)}"`
      }><div class="run-bar-fill" data-percent="${progress.percent}"></div></div>`
    : `<div class="run-bar indeterminate" role="progressbar" aria-valuetext="Starting"><div class="run-bar-fill indeterminate"></div></div>`;

  const position =
    narration.position === "" || settled || failed !== undefined
      ? ""
      : ` <span class="run-position">${escapeHtml(narration.position)}</span>`;

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

  // GUIDED IS A WIZARD CONCEPT, AND A REBUILD HAS NO USE FOR IT (memql#4246).
  // It re-runs one step with the operator driving the privileged part by hand
  // in their own terminal, which is what makes `hostsBlock` and the docker
  // group reachable at all. A rebuild is ONE unprivileged step, so the control
  // there is a second Retry wearing a name that promises something else.
  //
  // Decided from `mode`, which this input already carries, rather than from a
  // flag beside it: two fields saying which run this is could be passed
  // disagreeing, and the heading above would then name one run while the
  // buttons below served another.
  const guided =
    input.mode === "rebuild"
      ? ""
      : `<button class="secondary" type="button" data-act="guided">${
          many ? "Switch these steps to guided" : "Switch this step to guided"
        }</button>`;

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
  ${guided}
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

/**
 * The version options, with the current value guaranteed present and IN ORDER.
 *
 * A `<select>` silently drops a value that is not one of its options, so a
 * current value the remote listing does not carry -- a tag cut after this
 * extension was built, or a listing that came back partial -- would leave the
 * field showing the newest release while `values.version` still said something
 * else. The operator would then install a version the page never offered them.
 * So the value is GUARANTEED PRESENT, and that property is what this keeps.
 *
 * WHAT IT NO LONGER DOES IS HOIST IT (memql#4429). This used to return
 * `[current, ...rest]`, which put the current value at the TOP of a list whose
 * whole meaning is its order: `compareSemverDesc` sorts newest-first, and the
 * hoist then read `v0.19.1, v0.20.3, v0.19.2, ...` -- a picker that looks
 * mis-sorted because it is. Guarantee-present and queue-jumping are two
 * properties that arrived in one line; only the first was ever wanted, and the
 * sorted insert below is the first without the second.
 */
export function withCurrentInSortedPosition(
  choices: readonly string[],
  current: string,
): readonly string[] {
  const trimmed = current.trim();
  if (trimmed === "" || choices.includes(trimmed)) return choices;
  return [...choices, trimmed].sort(compareSemverDesc);
}

/** One entry in the version picker: what gets submitted, and what is read. */
export interface VersionChoice {
  value: string;
  label: string;
}

/**
 * The label the newest listed release carries (memql#4429).
 *
 * THE VALUE IS THE REAL TAG, never a sentinel. What the operator submits when
 * they take the recommendation is an ordinary version string, so `installPlan`,
 * `imageTagFor` and the receipt all see exactly what they would have seen had
 * the tag been picked by name -- and the receipt that install writes NAMES the
 * version rather than the word "latest", which is the difference between a
 * cluster whose version can be read back and one whose version depends on when
 * it was installed.
 */
export function latestLabel(tag: string): string {
  return `Latest -- ${tag} (recommended)`;
}

/**
 * What the `main` entry says it is (memql#4430).
 *
 * IT IS A LANE, NOT A VERSION, and the label says which lane. `main` builds the
 * node images FROM THE CHECKOUT it just cloned -- there is no release image
 * behind it and none is fetched -- so the operator taking it needs Docker, a
 * repository they can clone, and several minutes. The field hint states the
 * cost; this states the audience.
 *
 * IT USED TO STATE A SKEW INSTEAD (memql#3901): main's manifests and scripts
 * with the newest RELEASE's node images, because no `main` image is published.
 * That skew is gone -- the images are built here now -- so the sentence
 * describing it is gone with it rather than left standing over a decision that
 * reversed.
 */
export const MAIN_CHOICE_LABEL = "main -- build from source (for MemQL developers)";

/**
 * The version picker's entries: newest first, Latest labelled, `main` last.
 *
 * `main` IS OFFERED, AND IT IS NOT MIXED IN WITH THE RELEASE TAGS. It goes last,
 * after the releases, and its label says what it is -- because it is a different
 * KIND of answer, and an operator scanning a list of `v0.18.0`-shaped strings
 * would reasonably read a bare "main" as just another one.
 *
 * THE LATEST LABEL ATTACHES TO THE LISTING'S OWN NEWEST, not to whatever sorts
 * first. Those differ in exactly the case the sorted insert above exists for: a
 * current value the listing does not carry can sort ABOVE everything listed, and
 * calling that "Latest (recommended)" would be a recommendation the listing does
 * not support. It stays present, in order, unlabelled.
 */
export function versionChoiceList(
  choices: readonly string[],
  current: string,
): readonly VersionChoice[] {
  const listed = choices.filter((c) => c !== MAIN_BRANCH_CHOICE);
  const newest = listed[0] ?? "";
  const releases = withCurrentInSortedPosition(
    listed,
    isMainBranchChoice(current) ? "" : current,
  );
  return [
    ...releases.map((value) => ({
      value,
      label: value === newest && newest !== "" ? latestLabel(value) : value,
    })),
    { value: MAIN_BRANCH_CHOICE, label: MAIN_CHOICE_LABEL },
  ];
}
