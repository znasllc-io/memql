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
import { escapeHtml, renderToHtml } from "@znasllc-io/memql-view-kit";

import type { AddClusterAction } from "../clusters/presence.js";
import type {
  FieldError,
  InputField,
  Inputs,
  StepProgress,
} from "../state/addCluster.js";
import { SUPPORTED_PROVIDERS, requiredFields } from "../state/addCluster.js";
import { failureGuidance, runIsSettled, toStepViews } from "../state/installProgress.js";

/** The collected fields, in the order they are asked for. */
export const INPUT_FIELDS: readonly InputField[] = [
  "domain",
  "ownerFirstName",
  "ownerLastName",
  "ownerEmail",
  "provider",
  "providerKeyFile",
];

/**
 * The fields rendered as a CHOICE rather than as a text box.
 *
 * `provider` is one because the set is closed and the script refuses anything
 * outside it with exit 2 -- whose guidance says "a fault in memQL rather than
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
};

/** What each field is for, in one line. */
export const FIELD_HINTS: Record<InputField, string> = {
  domain: "The cluster answers at api.<domain>. Defaults are fine if you have no preference.",
  ownerFirstName: "The cluster owner -- you.",
  ownerLastName: "",
  provider:
    "Which vendor the key below belongs to. The installer makes one authenticated call to check it before anything on this machine changes.",
  ownerEmail: "Used to create the owner account. A local cluster sends no mail.",
  providerKeyFile:
    "A PATH to a file holding the key, never the key itself: a command line is readable by every process on this machine.",
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
}

export function renderCollectScreen(input: CollectScreenInput): string {
  const required = new Set(requiredFields(input.action));
  const { values, errors } = input;

  const fields = INPUT_FIELDS.filter((field) => required.has(field))
    .map((field) => {
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
      const control =
        choices === undefined
          ? `<div class="control-row"><input id="f-${field}" data-field="${field}" value="${escapeHtml(
              values[field],
            )}">${browse}</div>`
          : `<select id="f-${field}" data-field="${field}">${choices
              .map(
                (choice) =>
                  `<option value="${escapeHtml(choice)}"${
                    choice === values[field] ? " selected" : ""
                  }>${escapeHtml(choice)}</option>`,
              )
              .join("")}</select>`;
      return `<div class="field" data-invalid="${error !== undefined}">
  <label for="f-${field}">${escapeHtml(FIELD_LABELS[field])}</label>
  ${control}
  ${hint === "" ? "" : `<div class="hint">${escapeHtml(hint)}</div>`}
  ${error === undefined ? "" : `<div class="error">${escapeHtml(error.message)}</div>`}
</div>`;
    })
    .join("");

  return `<h1>${escapeHtml(COLLECT_TITLE[input.action] ?? "Install a local cluster")}</h1>
<p class="lede">Everything is collected before any work starts, so the long part runs unattended.</p>
${fields}
<div class="actions">
  <button class="primary" type="button" data-act="begin">Start</button>
  <button class="secondary" type="button" data-act="back">Back</button>
</div>`;
}

// ---------------------------------------------------------------------------
// running
// ---------------------------------------------------------------------------

export interface RunningScreenInput {
  steps: readonly StepProgress[];
  /** True when the wording should be the repair's rather than the install's. */
  repair: boolean;
  /** Whether a run is actually in flight -- see the comment on the empty list. */
  running: boolean;
}

/**
 * The run in progress.
 *
 * The step list is view-kit's `renderInstallSteps`, fed by a projection that
 * lives in state/installProgress.ts -- this function adds no judgement of its
 * own, which is what lets what an operator sees be asserted in the unit lane.
 *
 * REPAIR IS THE SAME RUN WITH DIFFERENT WORDING. Every step verifies first
 * and skips when satisfied, so re-running the graph IS the repair; only the
 * heading and the lede differ, and there is no second code path below them.
 */
export function renderRunningScreen(input: RunningScreenInput): string {
  const { steps, repair } = input;
  const settled = runIsSettled(steps);

  // An empty list now means the run is starting for real -- `startRun()` is
  // in flight and the first `stepStarted` has not arrived. That claim was
  // false while the invocation was unwired (memql#3487), which is why this
  // text is tied to a run being in flight rather than asserted unconditionally:
  // if no run is in flight and no step has reported, nothing is happening and
  // the screen says so.
  const body =
    steps.length > 0
      ? renderToHtml(renderInstallSteps(toStepViews(steps)))
      : input.running
        ? `<p class="lede">Starting. The first step will appear here as it begins.</p>`
        : `<p class="lede">Nothing has been run.</p>`;

  // Cancel is offered for exactly as long as there is something to stop.
  // A cancelled run leaves a valid receipt -- what ran, ran, and an uninstall
  // can still take it back -- so this is safe at any point.
  const actions = settled
    ? `<button class="secondary" type="button" data-act="back">Back</button>`
    : `<button class="secondary" type="button" data-act="cancel">Cancel</button>`;

  return `<h1>${escapeHtml(repair ? "Repairing the local cluster" : "Installing a local cluster")}</h1>
<p class="lede">${escapeHtml(
    repair
      ? "Every step checks first and is skipped when it is already satisfied, so only what is actually missing runs."
      : "Each step proves itself before the next one starts.",
  )}</p>
${body}
<div class="actions">${actions}</div>`;
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
    : `${failures[0]!.description === "" ? failures[0]!.id : failures[0]!.description} failed`;

  // Each failure keeps its own name above its own guidance. With one failure
  // the name is already the heading, so repeating it would be noise.
  const blocks = failures
    .map((failure) => {
      const guidance = failureGuidance(failure.exitCode, failure.remedy);
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

  // The labels count. "Retry this step" in front of three failures names one
  // thing and does another -- the recovery re-runs the graph, and every failed
  // step goes back into it.
  return `<h1>${escapeHtml(heading)}</h1>
${blocks}
${renderToHtml(renderInstallSteps(toStepViews(input.steps)))}
<div class="actions">
  <button class="primary" type="button" data-act="retry">${
    many ? "Retry these steps" : "Retry this step"
  }</button>
  <button class="secondary" type="button" data-act="guided">${
    many ? "Switch these steps to guided" : "Switch this step to guided"
  }</button>
  <button class="secondary" type="button" data-act="cancel">Cancel</button>
</div>`;
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
 * password belong -- memQL never sees either.
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
