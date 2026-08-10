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
// WHAT MOVED, AND WHY (memql#3469). The ORCHESTRATION -- the plan functions,
// the child environment, the decision to load a graph and execute it -- now
// lives in ./session.ts, because the "+" button needed to start an install
// without spawning a process and there was no function to call. What is left
// here is what makes this a CLI and nothing else: argv parsing, and printing.
// The plan functions are re-exported so this module stays the CLI's single
// import surface.
//
// Free of `vscode` imports: this runs as plain node.
//
// Refs: #3469 #3374 #3357

import * as path from "node:path";

import { graphDocumentPath, loadGraphFile, type Graph, type GraphKind, type Step } from "./graph.js";
import { defaultReceiptPath } from "./receipt.js";
import { type ExecEvent, type ExecutionReport, type StepPlan } from "./executor.js";
import {
  installPlan,
  previewUninstall,
  runInstall,
  runUninstall,
  uninstallPlan,
  type SessionOptions,
} from "./session.js";

// The plan functions are the session's, and are re-exported rather than
// re-implemented: two copies would be exactly the divergence #3469 exists to
// prevent.
export { installPlan, uninstallPlan } from "./session.js";

export class CliError extends Error {}

/**
 * What the CLI has that a session does not: which command was typed, and the
 * two output modes. Everything else is a run input and lives in SessionOptions.
 */
export interface CliOptions extends SessionOptions {
  command: "install" | "uninstall";
  json: boolean;
  dryRun: boolean;
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
// running
// --------------------------------------------------------------------------

export async function run(opts: CliOptions, log: (line: string) => void = (l) => console.error(l)): Promise<number> {
  const kind: GraphKind = opts.command === "install" ? "install" : "uninstall";

  // The dry run stays here rather than moving into the session, because it is
  // PRINTING -- and for uninstall it is now printed from the same structured
  // preview the wizard renders (previewUninstall), so the two can never
  // describe different removals.
  if (opts.dryRun) {
    if (kind === "uninstall") {
      const preview = await previewUninstall(opts);
      log(`${preview.graph}: ${preview.steps.length} steps`);
      for (const step of preview.steps) {
        if (step.action === "skip") {
          log(`  - ${step.id}  SKIP (${step.reason})`);
          continue;
        }
        const flags = Object.entries(step.params).map(([k, v]) => `--${k}=${v}`);
        const tier = step.preserved ? "  PRESERVED" : "";
        log(`  - ${step.id}${tier}  ${step.script} ${flags.join(" ")}`.trimEnd());
      }
      return 0;
    }
    const graph = await loadGraphFile(graphDocumentPath(kind, opts.root));
    printPlan(graph, installPlan(opts), log);
    return 0;
  }

  const report =
    kind === "install"
      ? await runInstall(opts, { onEvent: (event) => logEvent(event, log) })
      : await runUninstall(opts, { onEvent: (event) => logEvent(event, log) });

  printSummary(report, log);
  if (opts.json) process.stdout.write(`${JSON.stringify(report, null, 2)}\n`);
  // A cancelled run did not do what was asked, even when nothing failed.
  return report.ok && report.cancelled !== true ? 0 : 1;
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
