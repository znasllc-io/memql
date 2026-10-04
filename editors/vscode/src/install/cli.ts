// The CLI harness: `cli.js install`, `cli.js uninstall` and `cli.js repair`.
//
// The graph, the runner and the executor between them describe an install
// completely except for the handful of values that CANNOT be pinned in a
// document -- a release tag, who the cluster owner is. This file is where those
// arrive, and it is deliberately the only place they do: everything else is
// either policy the graph pins or a fact the receipt records.
//
// NO VENDOR CREDENTIAL IS AMONG THEM ANY MORE (epic memql#5088).
// `--provider-key-file` was here for as long as an install could seed one --
// always a FILE PATH, never the key, because argv is world-readable in `ps`.
// There is no vendor API key in the product now: both cloud vendors are reached
// by workload identity federation, whose credential is a projected token inside
// a pod, and `verify-provider-key.sh` no longer declares a flag for either the
// key or the vendor.
//
// WHAT IT SUPPLIES, AND WHY EACH ONE IS NOT IN THE GRAPH
//
//   --tag                   a release tag is a run input; pinning one in the
//                           document would freeze the installer to a version.
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
// THE RULE COVERS THE IMAGES TOO (memql#4068). It did not, and the gap was the
// same rule broken in a second place: a branch install's `recordedCheckout()`
// returns a commit and no TAG -- deliberately, there is no release -- and the
// image tag was DERIVED from that same empty tag rather than replayed, so it
// fell through to DEFAULT_STACK_TAG and a repair run from a newer extension build
// reconciled the recorded commit's manifests against a different release's
// engine images. The fix is `recordedCheckout().imageTag`: the resolved value
// the install already recorded, replayed rather than derived a second time. A
// TAG install was unaffected throughout, because its derivation and its record
// agree by construction -- which is why nothing noticed -- and that case is
// pinned in installMainBranch.test.ts so a fix for one cannot regress the other.
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

import {
  graphDocumentPath,
  installGraphPath,
  loadGraphFile,
  type Graph,
  type GraphKind,
  type Step,
} from "./graph.js";
import {
  defaultReceiptPath,
  readReceipt,
  recordedCheckout,
  recordedDomain,
  recordedOwner,
  type Receipt,
} from "./receipt.js";
import { type ExecEvent, type ExecutionReport, type StepPlan } from "./executor.js";
import {
  imagesFromSource,
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
    // THE FROM-SOURCE LANE, NAMEABLE FROM A TERMINAL (memql#5067).
    //
    // `imagesFromSource` was reachable two ways and neither could be used by a
    // CI job: `--tag=main`, which also decides WHAT is cloned, and a receipt on
    // a repair. So the only way to put `install-main.json` under test was to
    // install main's tip -- which is not the branch under review, and a lane
    // that green-lights the wrong tree is worse than no lane.
    //
    // Separating it from the version is what the option already is (see
    // SessionOptions.imagesFromSource): `--commit=<sha> --from-source` clones
    // the revision under test and BUILDS its node images, which is the pair the
    // wizard's "install from main" makes and no job could express.
    if (name === "from-source") {
      opts.imagesFromSource = true;
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
        `unknown flag --${name} (known: ${[...Object.keys(VALUE_FLAGS), "skip", "param", "timeout", "json", "dry-run", "from-source"]
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

  // AND NO --from-source, for exactly the same reason (memql#5067). The lane an
  // install ran on is a recorded fact -- `recordedCheckout().fromSource`, which
  // `repairOptions` reads -- so the flag could only ever contradict the receipt
  // or agree with it redundantly. Asserting it against a RELEASE install would
  // rebuild the images from a tag's source and call the result a repair, which
  // is the lane crossing memql#4246 gives its own verb.
  if (opts.command === "repair" && opts.imagesFromSource !== undefined) {
    throw new CliError(
      "repair takes no --from-source: the receipt records which lane the install ran on, " +
        "and rebuilding a release install's images from its checkout is `rebuild`, not `repair`",
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
 *     "upgrade" (memql#3605, reopened by the route memql#3901 added). It also
 *     carries the recorded IMAGE tag, which is the same rule about the other
 *     half of a release: with no tag to derive from, the images fell back to
 *     this build's pin and the repair upgraded them alone, leaving the recorded
 *     commit's manifests running against another release's engine (memql#4068).
 *   - `recordedOwner` + `recordedDomain` are what `seedBootstrap` refuses an
 *     incomplete set of (znasllc-io#3888, memql#3736). The refusal is right;
 *     a caller that reached it with three empty strings was wrong.
 *
 * REFUSES RATHER THAN GUESSES, and the refusal names the remedy:
 *
 *   NO RECORDED CHECKOUT -- `installPlan` would fall through to
 *   DEFAULT_STACK_TAG, so a repair would silently install whatever version this
 *   build pins. That is the failure this verb exists to make unreachable, so it
 *   is refused where it starts rather than where it lands.
 *
 * IT USED TO REFUSE A THIRD TIME, on NO USABLE KEY PATH, because `providerKey`
 * gated every mutating step and the run could not pass wave 2 without one.
 * That refusal is gone with the key (epic memql#5088): both AI vendors are
 * reached by workload identity federation, the step is `providerFederation`
 * and it skips satisfied on this lane, and no receipt records a key path any
 * more -- so keeping the refusal would have made every repair impossible for
 * the one reason that can no longer be true.
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

  const owner = recordedOwner(receipt);
  return {
    ...opts,
    tag: checkout.tag || undefined,
    commit: checkout.commit || undefined,
    // The IMAGES the recorded install ran, replayed rather than re-derived
    // (memql#4068). `undefined` when the receipt carries none -- a receipt
    // written before `--image-tag` existed, or an install that failed before
    // `clusterUp` -- and `installPlan` then derives, which is the only answer
    // available and the correct one for the tag installs those receipts are.
    imageTag: checkout.imageTag || undefined,
    // AND THE LANE THOSE IMAGES CAME FROM (memql#4430). A from-source install
    // records NO image tag -- there is no registry in its plan at all -- so the
    // line above is empty for it and `installPlan` would derive the pin and hand
    // a cluster running `memql-<node>:local` a GHCR registry. That is memql#4068
    // exactly, by a route that opened when the main lane stopped pulling images.
    imagesFromSource: checkout.fromSource || undefined,
    domain: recordedDomain(receipt) || opts.domain,
    ownerEmail: owner.email || opts.ownerEmail,
    ownerFirstName: owner.firstName || opts.ownerFirstName,
    ownerLastName: owner.lastName || opts.ownerLastName,
  };
}

// `usableKeyPath` IS GONE (epic memql#5088). It turned a recorded `--key-file`
// value into one a run could use, answering "" for the two things a receipt
// could hold that are not paths: the redaction marker where an operator pasted
// a key into the key-FILE box (memql#3545), and -- on a receipt written before
// that guard existed -- the key itself. No receipt records a key path any more,
// so there is nothing left to sanitise on the way back out.

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
    // THE LANE'S OWN DOCUMENT (memql#4430). A dry run that printed install.json
    // for a `--tag=main` run would show sixteen steps and no build, which is not
    // the plan the same command would execute -- and a dry run is read precisely
    // because somebody wants to know that before committing to it.
    const graph = await loadGraphFile(
      kind === "install"
        ? installGraphPath(opts.root, imagesFromSource(opts))
        : graphDocumentPath(kind, opts.root),
    );
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
      // The graph's short label leads, as it does on the editor's run screen;
      // the id follows in brackets because it is what `--skip` and the
      // receipt are keyed by.
      log(`--> ${event.step.label} [${event.step.id}]`);
      return;
    case "stepPhase":
      log(
        `    ${event.label}${
          event.done !== undefined && event.total !== undefined ? ` (${event.done}/${event.total})` : ""
        }`,
      );
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
