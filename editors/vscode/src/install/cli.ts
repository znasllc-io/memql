// The CLI harness: `cli.js install` and `cli.js uninstall`.
//
// The graph, the runner and the executor between them describe an install
// completely except for the handful of values that CANNOT be pinned in a
// document -- a release tag, where the operator's API key file lives, who the
// cluster owner is. This file is where those arrive, and it is deliberately
// the only place they do: everything else is either policy the graph pins or a
// fact the receipt records.
//
// WHAT IT SUPPLIES, AND WHY EACH ONE IS NOT IN THE GRAPH
//
//   --tag                   a release tag is a run input; pinning one in the
//                           document would freeze the installer to a version.
//   --provider-key-file     the operator's key lives on their machine. It is
//                           always a FILE PATH and there is deliberately no
//                           flag that takes the key itself: argv is
//                           world-readable in `ps`, so a --provider-key would
//                           publish an Anthropic key to every process listing
//                           on the machine for the length of the install.
//   the owner fields        who owns this cluster is not a property of the
//                           software. seed-bootstrap.sh exits 2 on an
//                           INCOMPLETE set by design, so this CLI passes
//                           through whatever it was given and lets the script
//                           refuse -- one place decides what "complete" means.
//   --path / --pre-existing on uninstall these come from the RECEIPT, not from
//                           the operator: only the install knows where the
//                           artifact landed and whether it was already there.
//
// Free of `vscode` imports: this runs as plain node.
//
// Refs: #3374 #3357

import * as path from "node:path";

import {
  graphDocumentPath,
  loadGraphFile,
  type Graph,
  type GraphKind,
  type Step,
} from "./graph.js";
import {
  defaultReceiptPath,
  entryFor,
  readReceipt,
  removalParams,
  type Receipt,
} from "./receipt.js";
import { capabilityScriptPath } from "./runner.js";
import { executeGraph, type ExecEvent, type ExecutionReport, type StepPlan } from "./executor.js";

export class CliError extends Error {}

export interface CliOptions {
  command: "install" | "uninstall";
  /** Repository root holding scripts/ and the graph documents. */
  root: string;
  receiptFile: string;
  /** Step ids the operator explicitly does not want run. */
  skip: Set<string>;
  /** Directory the pinned tools go into; prepended to the child PATH. */
  toolDir?: string;
  tag?: string;
  repo?: string;
  providerKeyFile?: string;
  provider: string;
  domain?: string;
  ownerEmail?: string;
  ownerFirstName?: string;
  ownerLastName?: string;
  registrationMode?: string;
  /** Escape hatch: --param=<stepId>.<flag>=<value>. */
  stepParams: Record<string, Record<string, string>>;
  json: boolean;
  dryRun: boolean;
  timeoutMs?: number;
}

/** Flags that take a value, mapped to their CliOptions field. */
const VALUE_FLAGS: Record<string, keyof CliOptions> = {
  root: "root",
  receipt: "receiptFile",
  "tool-dir": "toolDir",
  tag: "tag",
  repo: "repo",
  "provider-key-file": "providerKeyFile",
  provider: "provider",
  domain: "domain",
  "owner-email": "ownerEmail",
  "owner-first-name": "ownerFirstName",
  "owner-last-name": "ownerLastName",
  "registration-mode": "registrationMode",
};

const DEFAULT_ROOT = path.resolve(__dirname, "..", "..", "..");

export function parseCliArgs(argv: string[], env: NodeJS.ProcessEnv = process.env): CliOptions {
  const [command, ...rest] = argv;
  if (command !== "install" && command !== "uninstall") {
    throw new CliError(`usage: cli.js install|uninstall [flags] (got ${command ? JSON.stringify(command) : "nothing"})`);
  }

  const opts: CliOptions = {
    command,
    root: DEFAULT_ROOT,
    receiptFile: defaultReceiptPath(env.HOME ?? undefined),
    skip: new Set<string>(),
    provider: "anthropic",
    stepParams: {},
    json: false,
    dryRun: false,
  };

  for (const arg of rest) {
    if (!arg.startsWith("--")) {
      throw new CliError(`unexpected positional argument ${JSON.stringify(arg)} -- every input is a --flag=value`);
    }
    const body = arg.slice(2);
    const eq = body.indexOf("=");
    const name = eq >= 0 ? body.slice(0, eq) : body;
    const value = eq >= 0 ? body.slice(eq + 1) : "";

    if (name === "json") {
      opts.json = true;
      continue;
    }
    if (name === "dry-run") {
      opts.dryRun = true;
      continue;
    }
    if (name === "skip") {
      for (const id of value.split(",").map((s) => s.trim()).filter(Boolean)) opts.skip.add(id);
      continue;
    }
    if (name === "timeout") {
      const seconds = Number(value);
      if (!Number.isFinite(seconds) || seconds < 0) throw new CliError(`--timeout must be a number of seconds`);
      opts.timeoutMs = seconds * 1000;
      continue;
    }
    if (name === "param") {
      // --param=<stepId>.<flag>=<value>
      const dot = value.indexOf(".");
      const inner = value.indexOf("=");
      if (dot < 0 || inner < dot) {
        throw new CliError(`--param must be spelled <step>.<flag>=<value> (got ${JSON.stringify(value)})`);
      }
      const stepId = value.slice(0, dot);
      const flag = value.slice(dot + 1, inner);
      const flagValue = value.slice(inner + 1);
      (opts.stepParams[stepId] ??= {})[flag] = flagValue;
      continue;
    }
    const field = VALUE_FLAGS[name];
    if (!field) {
      throw new CliError(
        `unknown flag --${name} (known: ${[...Object.keys(VALUE_FLAGS), "skip", "param", "timeout", "json", "dry-run"]
          .sort()
          .join(", ")})`,
      );
    }
    if (eq < 0) throw new CliError(`--${name} needs a value`);
    (opts as unknown as Record<string, string>)[field] = value;
  }

  return opts;
}

// --------------------------------------------------------------------------
// planning
// --------------------------------------------------------------------------

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
export function installPlan(opts: CliOptions): (step: Step) => StepPlan {
  return (step: Step): StepPlan => {
    if (opts.skip.has(step.id)) {
      return { action: "skip", reason: `--skip=${step.id}` };
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
 *     pre-existed, the refusal that follows is the expected outcome, so the
 *     step is planned as preservedOnRefusal and reports `preserved` instead of
 *     failing the run.
 *
 * A step whose install counterpart left no receipt has nothing to remove. That
 * skip is SATISFIED: the state it would have established already holds, so the
 * removals waiting on it still run. Without that, an install that stopped
 * before the cluster would leave an uninstall that takes nothing back at all.
 */
export function uninstallPlan(receipt: Receipt, skip: Set<string> = new Set()): (step: Step) => StepPlan {
  return (step: Step): StepPlan => {
    if (skip.has(step.id)) return { action: "skip", reason: `--skip=${step.id}` };

    const installStep = step.reverses ?? "";
    const entry = installStep ? entryFor(receipt, installStep) : undefined;
    if (!entry) {
      return { action: "skip", reason: `the receipt has no ${installStep || "matching"} entry -- nothing to remove`, satisfied: true };
    }
    const params = removalParams(entry);
    if (!params) {
      return { action: "skip", reason: `${installStep} left no artifact behind`, satisfied: true };
    }
    return { action: "run", params, preservedOnRefusal: entry.preExisting };
  };
}

// --------------------------------------------------------------------------
// running
// --------------------------------------------------------------------------

export async function run(opts: CliOptions, log: (line: string) => void = (l) => console.error(l)): Promise<number> {
  const kind: GraphKind = opts.command === "install" ? "install" : "uninstall";
  const graph = await loadGraphFile(graphDocumentPath(kind, opts.root));

  let plan: (step: Step) => StepPlan;
  if (kind === "install") {
    plan = installPlan(opts);
  } else {
    const receipt = await readReceipt(opts.receiptFile);
    if (!receipt) {
      throw new CliError(
        `no receipt at ${opts.receiptFile} -- an uninstall removes what an install recorded, ` +
          `and without that record it would be guessing at the operator's machine`,
      );
    }
    plan = uninstallPlan(receipt, opts.skip);
  }

  if (opts.dryRun) {
    printPlan(graph, plan, log);
    return 0;
  }

  const report = await executeGraph({
    graph,
    plan,
    scriptPath: (step) => capabilityScriptPath(step.script, opts.root),
    receiptFile: kind === "install" ? opts.receiptFile : undefined,
    timeoutMs: opts.timeoutMs,
    env: childEnv(opts),
    onEvent: (event) => logEvent(event, log),
  });

  printSummary(report, log);
  if (opts.json) process.stdout.write(`${JSON.stringify(report, null, 2)}\n`);
  return report.ok ? 0 : 1;
}

/**
 * The tools the installer just placed are not on PATH.
 *
 * install.binary drops k3d / kubectl / mkcert into ~/.memql/bin, which no
 * shell has ever heard of, and the very next steps (mkcert -install, k3d
 * cluster create) look them up on PATH. Prepending the directory for the child
 * processes is what makes the graph's own ordering mean anything.
 */
function childEnv(opts: CliOptions): NodeJS.ProcessEnv {
  const home = process.env.HOME ?? "";
  const toolDir = opts.toolDir ?? path.join(home, ".memql", "bin");
  return { ...process.env, PATH: `${toolDir}${path.delimiter}${process.env.PATH ?? ""}` };
}

function printPlan(graph: Graph, plan: (step: Step) => StepPlan, log: (line: string) => void): void {
  log(`${graph.name}: ${graph.steps.length} steps`);
  for (const step of graph.steps) {
    const decision = plan(step);
    if (decision.action === "skip") {
      log(`  - ${step.id}  SKIP (${decision.reason})`);
      continue;
    }
    const params = { ...decision.params, ...(step.params ?? {}) };
    const flags = Object.entries(params).map(([k, v]) => `--${k}=${v}`);
    log(`  - ${step.id}  ${step.script} ${flags.join(" ")}`.trimEnd());
  }
}

function logEvent(event: ExecEvent, log: (line: string) => void): void {
  switch (event.type) {
    case "waveStarted":
      log(`==> wave ${event.index + 1}: ${event.ids.join(", ")}`);
      return;
    case "stepStarted":
      log(`--> ${event.step.id}: ${event.step.description}`);
      return;
    case "stepLog":
      log(`    [${event.step.id}] ${event.line}`);
      return;
    case "stepFinished": {
      const o = event.outcome;
      log(`<-- ${o.id}: ${o.status.toUpperCase()}${o.reason ? ` -- ${o.reason}` : ""}`);
      return;
    }
  }
}

function printSummary(report: ExecutionReport, log: (line: string) => void): void {
  log("");
  log(`${report.graph}: ${report.ok ? "OK" : "FAILED"}`);
  for (const o of report.outcomes) {
    log(`  ${o.status.padEnd(9)} ${o.id}${o.reason ? ` -- ${o.reason}` : ""}`);
  }
}

/** Entry point. Kept tiny so everything above stays testable as a library. */
export async function main(argv: string[] = process.argv.slice(2)): Promise<number> {
  try {
    return await run(parseCliArgs(argv));
  } catch (err) {
    console.error(`ERROR: ${(err as Error).message}`);
    return err instanceof CliError ? 2 : 1;
  }
}

// Self-execution guard.
//
// `require.main === module` alone is not enough here: esbuild INLINES this file
// into every bundle that imports it, including the test bundle, where that
// condition is true for the test entry point and the CLI would run itself
// against `node --test`'s argv. The filename check is what distinguishes "this
// bundle IS the CLI" from "this bundle contains the CLI".
if (
  typeof require !== "undefined" &&
  require.main === module &&
  path.basename(__filename).startsWith("cli.")
) {
  void main().then((code) => {
    process.exitCode = code;
  });
}
