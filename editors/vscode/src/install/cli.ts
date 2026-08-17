// The CLI harness: `cli.js install`, `cli.js uninstall` and `cli.js repair`.
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
// THE THIRD VERB (memql#3901, memql#3605). `repair` is not a third graph --
// GraphKind is `install | uninstall` and nothing else -- it is the INSTALL
// graph run with the parameters a previous run recorded. Every step verifies
// itself first and skips when already satisfied, so re-running the graph over a
// cluster that stopped answering IS the repair; see runInstall's doc comment,
// which has said so since #3357. What the verb adds is the one thing that
// separates a repair from an upgrade: where its parameters come from.
//
// They come from `recordedCheckout()` and its siblings in receipt.ts, CALLED
// rather than reimplemented. That function encodes a rule with a silent failure
// mode -- a tag install replays its tag, a BRANCH install must replay the
// resolved COMMIT, because replaying `--branch=main` checks out wherever main is
// today and turns "repair" into "upgrade" (memql#3605's exact failure, by the
// one route that reopens it). A second copy of that rule here would be a second
// place for it to be wrong, and wrong in a way that reads as success.
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
import {
  defaultReceiptPath,
  readReceipt,
  recordedCheckout,
  recordedDomain,
  recordedOwner,
  recordedProvider,
  recordedProviderKeyFile,
  type Receipt,
} from "./receipt.js";
import { looksLikeProviderKey, REDACTED } from "./secrets.js";
import { type ExecEvent, type ExecutionReport, type StepPlan } from "./executor.js";
import {
  installPlan,
  previewUninstall,
  runInstall,
  runUninstall,
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
  command: "install" | "uninstall" | "repair";
  json: boolean;
  dryRun: boolean;
}

/** Flags that take a value, mapped to their CliOptions field. */
const VALUE_FLAGS: Record<string, keyof CliOptions> = {
  root: "root",
  receipt: "receiptFile",
  "tool-dir": "toolDir",
  tag: "tag",
  // The EXACT commit to check out, which outranks --tag. See
  // SessionOptions.commit: a repair reads it off the receipt, and the
  // cluster-lane CI job passes the revision under test so ArgoCD reconciles
  // THAT tree rather than the pinned release. Not a thing to reach for by
  // hand -- `--tag` is how a person names a version -- but the value has to be
  // expressible from a terminal for anything but the wizard to install a
  // revision that is not a release.
  commit: "commit",
  repo: "repo",
  "image-registry": "imageRegistry",
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
  if (command !== "install" && command !== "uninstall" && command !== "repair") {
    throw new CliError(
      `usage: cli.js install|uninstall|repair [flags] (got ${command ? JSON.stringify(command) : "nothing"})`,
    );
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

  // A REPAIR MAY NOT BE HANDED A VERSION (memql#3605). A repair returns the
  // cluster to the state its receipt describes; upgrading is a different verb,
  // which the operator picks by name. Silently preferring the recorded ref over
  // a `--tag` the operator typed would be the safe behaviour and the confusing
  // one -- the flag would appear to work and do nothing -- so the flag is
  // refused where it can only mean something the verb does not do.
  if (opts.command === "repair" && (opts.tag !== undefined || opts.commit !== undefined)) {
    throw new CliError(
      "repair takes no --tag/--commit: it replays the checkout the receipt recorded, " +
        "and installing a different version is `install`, not `repair`",
    );
  }

  return opts;
}

/**
 * A repair's run inputs: the install graph's, as a previous run recorded them.
 *
 * ONE RULE, STATED ONCE: **the receipt answers, and the command line may only
 * supply what the receipt does not carry.** A repair is defined as returning
 * the cluster to the state its receipt describes, so every value it needs is
 * already a recorded fact; the flags remain useful only for the run inputs no
 * install records (`--root`, `--receipt`, `--timeout`, `--param`, `--skip`).
 *
 * Each reader below is the one the extension's Repair button calls, and calling
 * them rather than re-deriving from `entry.params` is the point of this
 * function existing at all:
 *
 *   - `recordedCheckout` decides tag-versus-commit. A tag install replays its
 *     tag; a BRANCH install must replay the resolved COMMIT, because replaying
 *     `--branch=main` checks out wherever main is today and makes "repair" mean
 *     "upgrade" (memql#3605, reopened by the route memql#3901 added).
 *   - `recordedProvider` travels with `recordedProviderKeyFile`: re-asserting
 *     the default vendor over a recorded OpenAI key verifies it against
 *     Anthropic and reports exit 3, REFUSED -- "the key is bad", about a key
 *     that is fine (memql#3473).
 *   - `recordedOwner` + `recordedDomain` are what `seedBootstrap` refuses an
 *     incomplete set of (znasllc-io#3888, memql#3736). The refusal is right;
 *     a caller that reached it with three empty strings was wrong.
 *
 * REFUSES RATHER THAN GUESSES, twice, and both refusals name the remedy:
 *
 *   NO RECORDED CHECKOUT -- `installPlan` would fall through to
 *   DEFAULT_STACK_TAG, so a repair would silently install whatever version this
 *   build pins. That is the failure this verb exists to make unreachable, so it
 *   is refused where it starts rather than where it lands.
 *
 *   NO USABLE KEY PATH -- the run cannot pass wave 2 (`providerKey` gates every
 *   mutating step), and the failure it would produce is exit 2, whose guidance
 *   reads "a fault in memQL rather than in your machine". Say the true thing
 *   before anything runs. `--provider-key-file` still supplies it, because a
 *   path the receipt never carried is not a value the receipt is overriding.
 */
export function repairOptions(opts: CliOptions, receipt: Receipt | null): CliOptions {
  if (receipt === null) {
    throw new CliError(
      `no receipt at ${opts.receiptFile} -- a repair replays a recorded install, and there is ` +
        `no record here. Install rather than repair.`,
    );
  }

  const checkout = recordedCheckout(receipt);
  if (checkout.tag === "" && checkout.commit === "") {
    throw new CliError(
      "the receipt records no checkout, so a repair has nothing to replay -- it would fall " +
        "back to this build's pinned release, which is an install, not a repair. Install instead.",
    );
  }

  const keyFile = opts.providerKeyFile || usableKeyPath(recordedProviderKeyFile(receipt));
  if (keyFile === "") {
    throw new CliError(
      "the receipt records no usable AI-provider key path. Install rather than repair, so the " +
        "key can be collected and verified, or pass --provider-key-file.",
    );
  }

  const owner = recordedOwner(receipt);
  return {
    ...opts,
    tag: checkout.tag || undefined,
    commit: checkout.commit || undefined,
    provider: recordedProvider(receipt) || opts.provider,
    providerKeyFile: keyFile,
    domain: recordedDomain(receipt) || opts.domain,
    ownerEmail: owner.email || opts.ownerEmail,
    ownerFirstName: owner.firstName || opts.ownerFirstName,
    ownerLastName: owner.lastName || opts.ownerLastName,
  };
}

/**
 * A recorded `--key-file` value, or "" when it is not one.
 *
 * The receipt can hold something that is not a path: the redaction marker where
 * an operator pasted a key into the key-FILE box (memql#3545), or -- on a
 * receipt written before that guard existed -- the key itself. Handing either
 * to `--key-file` produces a confusing failure deep in the run; treating it as
 * "nothing recorded" produces the honest refusal above.
 *
 * The same two lines the wizard applies (`usablePath` in addClusterPanel.ts).
 * Duplicated rather than shared because that file imports `vscode`, which this
 * one must not; the shared half -- what a key looks like -- is imported.
 */
function usableKeyPath(recorded: string): string {
  if (recorded === REDACTED) return "";
  return looksLikeProviderKey(recorded) ? "" : recorded;
}

// --------------------------------------------------------------------------
// running
// --------------------------------------------------------------------------

export async function run(
  rawOpts: CliOptions,
  log: (line: string) => void = (l) => console.error(l),
): Promise<number> {
  // REPAIR IS THE INSTALL GRAPH (see the header). The only difference is where
  // its parameters come from, and that resolution happens HERE -- before the
  // dry run, so `repair --dry-run` prints the plan a repair would actually
  // execute rather than the plan an install would.
  const opts =
    rawOpts.command === "repair" ? repairOptions(rawOpts, await readReceipt(rawOpts.receiptFile)) : rawOpts;
  const kind: GraphKind = opts.command === "uninstall" ? "uninstall" : "install";

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
    case "runStarted":
      log(`==> ${event.steps.length} steps: ${event.steps.map((s) => s.id).join(", ")}`);
      return;
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
