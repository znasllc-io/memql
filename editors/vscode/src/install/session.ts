// The callable run seam: install, uninstall, and the uninstall preview.
//
// WHY THIS FILE EXISTS. The graph, the runner, the executor and the receipt
// have described a complete install since the substrate epic (#3357) -- but the
// only way to START one was to spawn `npm run install-cli`. That is the whole
// reason the "+" button reported a command for the operator to copy instead of
// running one: src/extension.ts had nothing to call. Its own comment named this
// as the seam it was waiting for ("WHAT REPLACES IT: the install wizard ... the
// body of this function becomes the call to it").
//
// ONE RUN PATH, TWO FRONT ENDS. Everything below was lifted out of cli.ts
// rather than written beside it, and cli.ts now parses argv and prints, over
// exactly these functions. That is what makes "it worked from the terminal but
// not from the editor" impossible to introduce: there is no second
// orchestration to drift.
//
// WHAT IS DELIBERATELY NOT HERE. No decisions. Step order, dependencies, which
// steps may overlap, what requires elevation and what an uninstall touches all
// live in the graph and the receipt. This file supplies the handful of values
// that cannot be pinned in a document -- a release tag, where the operator's
// key file is, who owns the cluster -- and gets out of the way. A front end
// built on it renders and collects; it does not decide either.
//
// Free of `vscode` imports: it runs under plain node, and under `node --test`.
//
// Refs: #3469 #3463 #3357 #3374

import * as path from "node:path";

import {
  executeGraph,
  type ExecEvent,
  type ExecutionReport,
  type StepPlan,
} from "./executor.js";
import {
  graphDocumentPath,
  loadGraphFile,
  type Elevation,
  type Graph,
  type GraphKind,
  type Step,
} from "./graph.js";
import { entryFor, readReceipt, removalParams, type Receipt } from "./receipt.js";
import { capabilityScriptPath, type RunScript } from "./runner.js";

/**
 * The run-time inputs an install needs and a document cannot pin.
 *
 * This is CliOptions minus the three things that are about being a CLI --
 * which command was typed, whether to print JSON, whether it was a dry run.
 * The CLI keeps those; a webview has no use for any of them.
 */
export interface SessionOptions {
  /** Repository root holding scripts/ and the graph documents. */
  root: string;
  receiptFile: string;
  /** Step ids the operator explicitly does not want run. */
  skip: Set<string>;
  /** Directory the pinned tools go into; prepended to the child PATH. */
  toolDir?: string;
  tag?: string;
  repo?: string;
  /**
   * A PATH, never the key itself.
   *
   * argv is world-readable in `ps`, so a flag carrying the key would publish it
   * to every process listing on the machine for the length of the install. The
   * webview inherits that constraint unchanged -- it may collect a key, but
   * what it hands this module is a file.
   */
  providerKeyFile?: string;
  provider: string;
  domain?: string;
  ownerEmail?: string;
  ownerFirstName?: string;
  ownerLastName?: string;
  registrationMode?: string;
  /** Escape hatch: per-step flag overrides. */
  stepParams: Record<string, Record<string, string>>;
  timeoutMs?: number;
}

/** Everything a caller can vary about HOW a session runs, as opposed to what. */
export interface SessionHooks {
  onEvent?: (event: ExecEvent) => void | Promise<void>;
  /** Stops at the next wave boundary. See ExecuteOptions.signal. */
  signal?: AbortSignal;
  /** Injected by tests; the real graph is loaded from disk when absent. */
  graph?: Graph;
  /** Injected by tests; the real spawn-based runner when absent. */
  run?: RunScript;
}

// ---------------------------------------------------------------------------
// planning
// ---------------------------------------------------------------------------

/** Only set keys reach the params map: an empty flag is not the same as none. */
function present(params: Record<string, string | undefined>): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== "") out[k] = v;
  }
  return out;
}

/**
 * The install plan: the run-time half of each step's params.
 *
 * Nothing here is invented for a step the graph already pins. seedBootstrap
 * gets whatever owner fields the operator supplied, complete or not, because
 * seed-bootstrap.sh is the one place that decides what a complete bootstrap set
 * is -- and it exits 2 with the missing names when it is not.
 */
export function installPlan(opts: SessionOptions): (step: Step) => StepPlan {
  return (step: Step): StepPlan => {
    if (opts.skip.has(step.id)) {
      return { action: "skip", reason: `skipped: ${step.id}` };
    }
    let params: Record<string, string> = {};
    switch (step.id) {
      case "stackCheckout":
        params = present({ tag: opts.tag, repo: opts.repo });
        break;
      case "providerKey":
        params = present({ "key-file": opts.providerKeyFile, provider: opts.provider });
        break;
      case "seedBootstrap":
        params = present({
          domain: opts.domain,
          "owner-email": opts.ownerEmail,
          "owner-first-name": opts.ownerFirstName,
          "owner-last-name": opts.ownerLastName,
          "registration-mode": opts.registrationMode,
          provider: opts.providerKeyFile ? opts.provider : undefined,
          "provider-key-file": opts.providerKeyFile,
        });
        break;
      case "enrolmentLink":
        // The account to enrol is the owner seedBootstrap just created, and the
        // link has to point at the PUBLIC identity host rather than the
        // in-cluster one, so the domain the operator gave the installer is where
        // it comes from. Both are already collected; nothing new is asked of the
        // operator to get a passkey out of the install.
        params = present({
          "user-email": opts.ownerEmail,
          "base-url": opts.domain ? `https://identity.${opts.domain}` : undefined,
        });
        break;
      default:
        break;
    }
    if (opts.toolDir && step.script === "install.binary") {
      params = { ...params, dest: opts.toolDir };
    }
    return { action: "run", params: { ...params, ...(opts.stepParams[step.id] ?? {}) } };
  };
}

/**
 * The uninstall plan: read straight off the receipt.
 *
 * Two facts only the install knows, and both come back from here:
 *
 *   - WHERE the artifact landed, as the `--path` / `--caroot` / `--cluster`
 *     the removal needs;
 *   - WHETHER the installer created it. `--pre-existing=true` is an
 *     unconditional refusal inside remove-artifact.sh, so passing the recorded
 *     verdict faithfully is what keeps a developer's own k3d cluster, mkcert CA
 *     or checkout when they uninstall memQL. When the receipt says the artifact
 *     pre-existed, the refusal that follows is the expected outcome, so the step
 *     is planned as preservedOnRefusal and reports `preserved` rather than
 *     failing the run.
 *
 * A step whose install counterpart left no receipt has nothing to remove. That
 * skip is SATISFIED: the state it would have established already holds, so the
 * removals waiting on it still run. Without that, an install that stopped
 * before the cluster would leave an uninstall that takes nothing back at all.
 */
export function uninstallPlan(
  receipt: Receipt,
  skip: Set<string> = new Set(),
): (step: Step) => StepPlan {
  return (step: Step): StepPlan => {
    if (skip.has(step.id)) return { action: "skip", reason: `skipped: ${step.id}` };

    const installStep = step.reverses ?? "";
    const entry = installStep ? entryFor(receipt, installStep) : undefined;
    if (!entry) {
      return {
        action: "skip",
        reason: `the receipt has no ${installStep || "matching"} entry -- nothing to remove`,
        satisfied: true,
      };
    }
    const params = removalParams(entry);
    if (!params) {
      return { action: "skip", reason: `${installStep} left no artifact behind`, satisfied: true };
    }
    return { action: "run", params, preservedOnRefusal: entry.preExisting };
  };
}

// ---------------------------------------------------------------------------
// running
// ---------------------------------------------------------------------------

/**
 * Runs the install graph.
 *
 * REPAIR IS THIS FUNCTION. There is no separate entry point and no mode flag,
 * because every step runs its verify first and skips when already satisfied --
 * the same `changed=false` behaviour up.sh has. Re-running the graph over a
 * cluster that stopped answering IS the repair; only the wording around it
 * differs, and wording is the front end's business.
 */
export async function runInstall(
  opts: SessionOptions,
  hooks: SessionHooks = {},
): Promise<ExecutionReport> {
  const graph = hooks.graph ?? (await loadGraphFor("install", opts));
  return execute(graph, installPlan(opts), opts, hooks, opts.receiptFile);
}

/**
 * Runs the uninstall graph, reversing what the receipt recorded.
 *
 * Refuses without a receipt rather than falling back to the graph's own idea of
 * what an install creates: that would be guessing at the operator's machine,
 * and the artifacts in question are a k3d cluster, a hosts block and a CA in
 * the system trust store.
 *
 * No receiptFile is passed to the executor: an uninstall REVERSES the record,
 * it does not add to it.
 */
export async function runUninstall(
  opts: SessionOptions,
  hooks: SessionHooks = {},
): Promise<ExecutionReport> {
  const graph = hooks.graph ?? (await loadGraphFor("uninstall", opts));
  const receipt = await requireReceipt(opts);
  return execute(graph, uninstallPlan(receipt, opts.skip), opts, hooks, undefined);
}

function execute(
  graph: Graph,
  plan: (step: Step) => StepPlan,
  opts: SessionOptions,
  hooks: SessionHooks,
  receiptFile: string | undefined,
): Promise<ExecutionReport> {
  return executeGraph({
    graph,
    plan,
    scriptPath: (step) => capabilityScriptPath(step.script, opts.root),
    receiptFile,
    timeoutMs: opts.timeoutMs,
    env: childEnv(opts),
    onEvent: hooks.onEvent,
    signal: hooks.signal,
    ...(hooks.run ? { run: hooks.run } : {}),
  });
}

/**
 * The tools the installer just placed are not on PATH.
 *
 * install.binary drops k3d / kubectl / mkcert into ~/.memql/bin, which no shell
 * has ever heard of, and the very next steps (mkcert -install, k3d cluster
 * create) look them up on PATH. Prepending the directory for the child
 * processes is what makes the graph's own ordering mean anything.
 */
function childEnv(opts: SessionOptions): NodeJS.ProcessEnv {
  const home = process.env.HOME ?? "";
  const toolDir = opts.toolDir ?? path.join(home, ".memql", "bin");
  return { ...process.env, PATH: `${toolDir}${path.delimiter}${process.env.PATH ?? ""}` };
}

async function loadGraphFor(kind: GraphKind, opts: SessionOptions): Promise<Graph> {
  return loadGraphFile(graphDocumentPath(kind, opts.root));
}

async function requireReceipt(opts: SessionOptions): Promise<Receipt> {
  const receipt = await readReceipt(opts.receiptFile);
  if (!receipt) {
    throw new Error(
      `no receipt at ${opts.receiptFile} -- an uninstall removes what an install recorded, ` +
        `and without that record it would be guessing at the operator's machine`,
    );
  }
  return receipt;
}

// ---------------------------------------------------------------------------
// the uninstall preview
// ---------------------------------------------------------------------------

/**
 * One step, as a preview renders it.
 *
 * Flat and already-decided, so the UI has nothing left to work out. `preserved`
 * is a separate field rather than a status string because the operator is being
 * asked to consent, and "this will be removed" and "this will be kept" are the
 * two answers that consent is about.
 */
export interface PlannedStep {
  id: string;
  description: string;
  script: string;
  /** The install step this one undoes. Empty on an install graph. */
  reverses: string;
  /**
   * What this step will ask of the operator.
   *
   * Carried into the preview because "remove the mkcert CA from your system
   * trust store" and "delete a directory" are not the same consent, and an
   * itemized list that did not distinguish them would flatten the one item an
   * operator most needs to see coming.
   */
  elevation: Elevation;
  action: "run" | "skip";
  params: Record<string, string>;
  /**
   * Why this step will not remove anything: it was skipped, or the artifact is
   * being preserved. Empty only for a step that will genuinely remove
   * something.
   */
  reason: string;
  /** The artifact pre-existed the install, so it stays. */
  preserved: boolean;
  /** What will be removed, in words -- e.g. `cluster memql`. Empty for a skip. */
  target: string;
}

export interface UninstallPreview {
  graph: string;
  /** Every step, in graph order, including the ones that will do nothing. */
  steps: PlannedStep[];
  /** The steps that will actually remove something. */
  removals: PlannedStep[];
  /** Artifacts the install found already present. They stay. */
  preserved: PlannedStep[];
}

/**
 * What an uninstall would do, without doing any of it.
 *
 * THE PREVIEW IS THE CONFIRMATION (design D6). An uninstall is irreversible and
 * touches a k3d cluster, /etc/hosts and the system trust store, so the operator
 * confirms an itemized list rather than a yes/no box. That makes two properties
 * load-bearing, and both are tested:
 *
 *   - a PRESERVED artifact is reported separately from a removal. Listing it as
 *     a removal would ask consent for something that is not going to happen;
 *     omitting it would hide that the uninstall leaves something behind.
 *   - a step with nothing to remove does not appear as a removal at all.
 *
 * Nothing is executed. The plan function is pure over the receipt, which is
 * exactly why an itemized preview is possible without a dry-run mode inside the
 * scripts.
 */
export async function previewUninstall(
  opts: SessionOptions,
  hooks: SessionHooks = {},
): Promise<UninstallPreview> {
  const graph = hooks.graph ?? (await loadGraphFor("uninstall", opts));
  const receipt = await requireReceipt(opts);
  const plan = uninstallPlan(receipt, opts.skip);

  const steps: PlannedStep[] = graph.steps.map((step) => {
    const decision = plan(step);
    if (decision.action === "skip") {
      return {
        id: step.id,
        description: step.description,
        script: step.script,
        reverses: step.reverses ?? "",
        elevation: step.elevation,
        action: "skip",
        params: {},
        reason: decision.reason,
        preserved: false,
        target: "",
      };
    }
    const params = { ...decision.params, ...(step.params ?? {}) };
    const preserved = decision.preservedOnRefusal === true;
    return {
      id: step.id,
      description: step.description,
      script: step.script,
      reverses: step.reverses ?? "",
      elevation: step.elevation,
      action: "run",
      params,
      // A PRESERVED step carries its reason too, not just a skip. The preview
      // IS the confirmation, so the preserved rows are where the operator
      // learns what the uninstall is deliberately declining to touch and why --
      // and the only place that knows why is here, where the receipt's
      // pre-existence verdict is in hand. Leaving it empty pushed that sentence
      // into whichever consumer rendered the row.
      reason: preserved ? "the installer found it already on this machine" : "",
      preserved,
      target: describeTarget(params),
    };
  });

  return {
    graph: graph.name,
    steps,
    removals: steps.filter((s) => s.action === "run" && !s.preserved),
    preserved: steps.filter((s) => s.preserved),
  };
}

/**
 * The artifact a removal names, phrased for a human.
 *
 * Read off the removal params the receipt produced rather than from a second
 * table, so a preview can never name something different from what the step
 * will actually be told to remove.
 */
function describeTarget(params: Record<string, string>): string {
  for (const key of ["path", "cluster", "caroot", "hosts-file"]) {
    const value = params[key];
    if (value !== undefined && value !== "") return `${key} ${value}`;
  }
  return "";
}
