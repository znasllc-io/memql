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
import { MAIN_BRANCH_CHOICE } from "../install/stackPin.js";
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
    "Which vendor the key below belongs to. The installer makes one authenticated call to check it before anything on this machine changes.",
  ownerEmail: "Used to create the owner account. A local cluster sends no mail.",
  providerKeyFile:
    "A PATH to a file holding the key, never the key itself: a command line is readable by every process on this machine.",
  version:
    "Which MemQL release to install. The default is the version this extension was built and tested against; pick another only if you need a specific one.",
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
   * The newest published release, named in the `main` choice's label so an
   * operator can see WHICH engine images a development install would run
   * (memql#3901). Empty renders as "the newest release".
   */
  latestRelease?: string;
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
      // `version` is the one field whose options are not a constant: they come
      // off the remote at page-open time. Falls through to the text box when
      // the listing is empty, which is what makes a no-network install still
      // able to name a version.
      //
      // `main` is appended to that listing as a labelled choice rather than a
      // tag (memql#3901). It is NOT offered when the listing is empty: with no
      // network the field is a free-text box, and a text box that accepts "main"
      // would be an unlabelled branch, which clone-stack.sh refuses on purpose.
      const fieldChoices: readonly VersionChoice[] | undefined =
        field === "version"
          ? (input.versionChoices ?? []).length === 0
            ? undefined
            : versionChoiceList(
                input.versionChoices ?? [],
                values.version,
                input.latestRelease ?? "",
              )
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
    })
    .join("");

  return `<h1>${escapeHtml(COLLECT_TITLE[input.action] ?? "Install a local cluster")}</h1>
<p class="lede">Everything is collected before any work starts, so the long part runs unattended.</p>
${fields}
${renderPreflight(input.preflight)}
<div class="actions">
  <button class="primary" type="button" data-act="begin">Start</button>
  <button class="secondary" type="button" data-act="back">Back</button>
</div>`;
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
  return `<h1>Rebuild from checkout</h1>
<p class="lede">Builds the node images from ${escapeHtml(
    input.checkoutDir,
  )}, imports them into the cluster, points its Application at them, and restarts.</p>
<div class="field">
  <label for="f-nodes">Node types to rebuild (comma-separated; empty = all app nodes)</label>
  <input id="f-nodes" data-field="nodes" value="${escapeHtml(input.nodes)}">
  <div class="hint">For example: bff, agent. Leave it empty to rebuild every app node.</div>
</div>
${renderPreflight(input.preflight)}
<div class="actions">
  <button class="primary" type="button" data-act="beginRebuild">Start</button>
  <button class="secondary" type="button" data-act="back">Back</button>
</div>`;
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
 */
export type RunMode = "install" | "repair" | "deploy" | "rebuild";

export interface RunningScreenInput {
  steps: readonly StepProgress[];
  mode: RunMode;
  /** Whether a run is actually in flight -- see the comment on the empty list. */
  running: boolean;
}

const RUN_HEADING: Readonly<Record<RunMode, string>> = {
  install: "Installing a local cluster",
  repair: "Repairing the local cluster",
  deploy: "Deploying to the local cluster",
  rebuild: "Rebuilding the local cluster from its checkout",
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
};

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
  const { steps } = input;
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

  return `<h1>${escapeHtml(RUN_HEADING[input.mode])}</h1>
<p class="lede">${escapeHtml(RUN_LEDE[input.mode])}</p>
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
 * The version options, with the current value guaranteed present and first.
 *
 * A `<select>` silently drops a value that is not one of its options, so a
 * pinned default the remote listing does not carry -- a tag cut after this
 * extension was built, or a listing that came back partial -- would leave the
 * field showing the newest release while `values.version` still said something
 * else. The operator would then install a version the page never offered them.
 *
 * Putting the current value first also keeps the pin at the top of the list,
 * which is where an operator scanning for "the one I was given" looks.
 */
export function dedupeKeepingDefault(
  choices: readonly string[],
  current: string,
): readonly string[] {
  const rest = choices.filter((c) => c !== current);
  return current === "" ? rest : [current, ...rest];
}

/** One entry in the version picker: what gets submitted, and what is read. */
export interface VersionChoice {
  value: string;
  label: string;
}

/**
 * The version picker's entries (memql#3901).
 *
 * `main` IS OFFERED, AND IT IS NOT MIXED IN WITH THE RELEASE TAGS. It goes last,
 * after a separator, and its label says what it is -- because it is a different
 * KIND of answer, and an operator scanning a list of `v0.18.0`-shaped strings
 * would reasonably read a bare "main" as just another one.
 *
 * The label states the skew rather than hiding it. A `main` install gets main's
 * CHECKOUT -- the deploy manifests ArgoCD reconciles and the install scripts the
 * run executes -- and the newest RELEASE's node images, because
 * build-engine-images.yml publishes on a release dispatch only and there is no
 * `main` image in GHCR. So an unreleased manifest or script fix arrives and an
 * unreleased engine fix does not. Saying so here is the difference between an
 * operator who knows why their engine bug is still present and one who files
 * an issue about it (see imageTagForVersion).
 */
export function versionChoiceList(
  choices: readonly string[],
  current: string,
  latestRelease: string,
): readonly VersionChoice[] {
  const releases = dedupeKeepingDefault(
    choices.filter((c) => c !== MAIN_BRANCH_CHOICE),
    current === MAIN_BRANCH_CHOICE ? "" : current,
  );
  const images = latestRelease.trim() === "" ? "the newest release" : latestRelease.trim();
  return [
    ...releases.map((value) => ({ value, label: value })),
    {
      value: MAIN_BRANCH_CHOICE,
      label: `main -- development (manifests and scripts from main, node images from ${images})`,
    },
  ];
}
