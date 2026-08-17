// The callable run seam: install, uninstall, and the uninstall preview.
//
// WHY THIS FILE EXISTS. The graph, the runner, the executor and the receipt
// have described a complete install since the substrate epic (#3357) -- but the
// only way to START one was to spawn `npm run install-cli`. That is the whole
// reason the "+" button once reported a command for the operator to copy
// instead of running one: src/extension.ts had nothing to call. That stub named
// this file as the seam it was waiting for, and it was deleted the moment the
// page ran a graph over these functions (memql#3478) -- no path in the Clusters
// surface hands the operator a command to run in a terminal any more.
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
import { capabilityScriptPath, withInstalledTools, type RunScript } from "./runner.js";
import {
  DEFAULT_CAROOT_DIR,
  DEFAULT_IMAGE_REGISTRY,
  DEFAULT_REGISTRATION_MODE,
  DEFAULT_STACK_TAG,
  MAIN_BRANCH_CHOICE,
  imageTagForVersion,
  isMainBranchChoice,
} from "./stackPin.js";

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
  /**
   * Which code to check out. A release tag, or the `main` sentinel.
   *
   * ONE FIELD FOR BOTH (memql#3901), rather than a second `branch?: string`
   * beside it. Both front ends already thread `tag` end to end, and what the
   * operator is choosing is one thing -- "which version" -- with two kinds of
   * answer. A second field would make "neither set" and "both set" reachable
   * states that every caller would have to reason about, for no gain: the
   * translation to `--tag` versus `--branch` happens once, in `installPlan`.
   */
  tag?: string;
  /**
   * An exact commit, which OUTRANKS `tag` when set.
   *
   * The repair path, and only the repair path (memql#3901). A repair of a
   * branch install cannot replay `--branch=main` -- main has moved, so "repair"
   * would mean "upgrade", which is exactly the failure memql#3605 fixed for
   * tags. `repairSessionOptions` reads the resolved commit off the receipt and
   * puts it here.
   */
  commit?: string;
  repo?: string;
  /**
   * The newest published release, used ONLY to pick node images for a `main`
   * install. See imageTagForVersion: there is no `main` image in GHCR, so a
   * branch install runs the newest images that exist. Empty falls back to
   * DEFAULT_STACK_TAG.
   */
  latestRelease?: string;
  /** Registry the node images come from; see DEFAULT_IMAGE_REGISTRY. */
  imageRegistry?: string;
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
  /**
   * Where `install.cloneStack` puts the memQL checkout, and therefore the root
   * `k3d.up` reads deploy/ and its target revision from (memql#3491).
   *
   * ONE VALUE FEEDS BOTH STEPS, from here, rather than each defaulting
   * independently: the two scripts have their own defaults and a caller that
   * set only one would point the cluster bring-up at a directory the checkout
   * did not land in. That is the divergence this field exists to make
   * impossible.
   */
  stackDir?: string;
  /**
   * Extra environment for every step's child process.
   *
   * The one real use is `SUDO_ASKPASS`, pointing at the agent that answers
   * sudo with the password the operator gave ONCE (sudoAgent.ts). It has to
   * reach every step, because sudo's own authentication cache is keyed by
   * parent process and each step is its own process -- which is why an install
   * asked three times before this existed (memql#3568).
   */
  env?: Record<string, string>;
  /** Escape hatch: per-step flag overrides. */
  stepParams: Record<string, Record<string, string>>;
  timeoutMs?: number;
}

/**
 * What the wizard collects from an operator, before it becomes SessionOptions.
 *
 * Every field is a question the Add-a-cluster page asks (or a path the
 * extension already knows). Deliberately NOT the same shape as SessionOptions,
 * which also carries run inputs no page has a field for.
 */
export interface WizardAnswers {
  root: string;
  receiptFile: string;
  provider: string;
  /** A PATH, never the key. See SessionOptions.providerKeyFile. */
  providerKeyFile: string;
  domain: string;
  ownerEmail: string;
  ownerFirstName: string;
  ownerLastName: string;
  /**
   * The release tag to check out. Empty falls back to DEFAULT_STACK_TAG.
   *
   * A REPAIR MUST PIN THE RECORDED ONE (memql#3605): without it a repair run
   * from a newer extension silently upgraded the cluster it was meant to
   * restore.
   */
  tag?: string;
  /** See SessionOptions.commit -- set only by a repair replaying a branch install. */
  commit?: string;
  /** See SessionOptions.latestRelease -- images for a `main` install. */
  latestRelease?: string;
  timeoutMs?: number;
  /** See SessionOptions.env -- in practice, the sudo agent's SUDO_ASKPASS. */
  env?: Record<string, string>;
}

/**
 * The wizard's answers as a run.
 *
 * WHY THIS IS A FUNCTION AND NOT AN OBJECT LITERAL IN THE PANEL. It was a
 * literal, and `webview/addClusterPanel.ts` imports `vscode`, which the unit
 * lane excludes by design -- so nothing could assert what the wizard actually
 * passes. `tag` was simply absent from that literal for the whole life of the
 * wizard's install path, and every install started from the "+" button died at
 * stackCheckout on `exit 2: missing required parameter: tag` while the same
 * install from `cli.js --tag=...` worked (memql#3560).
 *
 * Moving the construction to this side of the vscode line is what lets
 * `installSession.test.ts` audit it against what the capability scripts require
 * -- the check that has to hold for the NEXT required param as well.
 */
export function installSessionOptions(answers: WizardAnswers): SessionOptions {
  return {
    root: answers.root,
    receiptFile: answers.receiptFile,
    skip: new Set<string>(),
    provider: answers.provider,
    providerKeyFile: answers.providerKeyFile,
    domain: answers.domain,
    ownerEmail: answers.ownerEmail,
    ownerFirstName: answers.ownerFirstName,
    ownerLastName: answers.ownerLastName,
    tag: answers.tag,
    commit: answers.commit,
    latestRelease: answers.latestRelease,
    stepParams: {},
    timeoutMs: answers.timeoutMs,
    env: answers.env,
  };
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
 * The front door, as the three steps that build and check it need it named.
 *
 * ONE DERIVATION, FOUR CONSUMERS (memql#3590, memql#3593). The page asks for a
 * domain and it
 * reached `seedBootstrap` and `enrolmentLink` only; `hostsBlock`, `localCA` and
 * `frontDoor` each fell back to their own hardcoded defaults. So identity
 * was bootstrapped for the typed domain while the hosts block, the certificate
 * and the front-door probe named another -- three artifacts that each look
 * correct alone and cannot work together.
 *
 * It was invisible because the field's default WAS that same hardcoded value, where the
 * hardcoded value and the typed one agree. Anyone who changed it got a DNS
 * failure at `frontDoor` against hostnames they never asked for, which reads as a
 * broken installer.
 *
 * Derived HERE rather than in each case of the switch below, because the failure
 * of the three disagreeing is silent. One function is the only shape in which
 * "the certificate covers what the probe dials" is true by construction.
 *
 * Empty in, empty out: `present()` drops empty values, so a run with no domain
 * passes no flag at all and each script keeps its own default. That is different
 * from passing an empty flag, and the scripts treat it as different.
 */
export interface FrontDoor {
  /** Everything the hosts block points at 127.0.0.1: the subdomains and the apex. */
  hostnames: string[];
  /** What the certificate must cover: the wildcard and the apex. */
  certNames: string[];
  /** What the front-door probe dials -- the hosts that carry their OWN exact rule, never a wildcard-served one. */
  probeHosts: string[];
}

// The subdomains memQL puts on a front door. Kept as data, and TWO lists
// because the hosts block and the probe answer different questions.
//
// The HOSTS BLOCK must name everything something will type, which includes
// `portal.` -- the origin the portal's bundle moves to in memql#3711. The
// wildcard certificate already covers it and identity already allows it as a
// CORS origin (component/envregistry/domain.go), so the hosts entry is the one
// remaining piece, and it is cheap to place ahead of the Ingress: a name
// resolving to 127.0.0.1 with nothing serving it fails visibly, whereas a name
// that does not resolve fails in the resolver, before any request is made.
//
// The PROBE dials only the hosts that carry their OWN EXACT Ingress rule --
// NOT "an Ingress rule today", which every one of the five front-door hosts
// now has: #3711 gives `portal.` a rule too, through the WILDCARD
// (memql#3714's edge-front-door.yaml). That is precisely the case this list
// must exclude: `check_precedence` (scripts/install/verify-frontdoor.sh)
// treats every `--hosts` entry as a name that should be answered by a backend
// OF ITS OWN, and a wildcard-served name fails that check with a message that
// reads backwards -- "the wildcard rule swallowed this exact host instead of
// yielding to it" -- when the wildcard serving it is the intended behaviour,
// not a defect. So `portal.` stays out of PROBE_SUBDOMAINS not because it
// lacks a rule, but because the rule it has is not its own. PROBE_SUBDOMAINS
// must stay a subset of
// HOSTS_BLOCK_SUBDOMAINS -- asserted in installSession.test.ts.
const HOSTS_BLOCK_SUBDOMAINS = ["api", "identity", "portal"] as const;
const PROBE_SUBDOMAINS = ["api", "identity"] as const;

export function frontDoorFor(domain: string): FrontDoor | undefined {
  const apex = domain.trim().replace(/^\.+|\.+$/g, "");
  if (apex === "") return undefined;
  return {
    // The apex last, matching the block hosts-entries.sh documents.
    hostnames: [...HOSTS_BLOCK_SUBDOMAINS.map((s) => `${s}.${apex}`), apex],
    // The WILDCARD, not the subdomains: it is what the local overlay's
    // `memql-front-door-tls` secret carries, so a cluster whose ingress adds
    // another host does not need a reissued certificate.
    certNames: [`*.${apex}`, apex],
    probeHosts: PROBE_SUBDOMAINS.map((s) => `${s}.${apex}`),
  };
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
    const stackDir = resolveStackDir(opts);
    const frontDoor = frontDoorFor(opts.domain ?? "");
    let params: Record<string, string> = {};
    switch (step.id) {
      case "stackCheckout":
        // THE DEFAULT IS APPLIED HERE, not in either front end. `--tag` is a
        // run input the CLI collects and the wizard has no field for, and
        // `present()` drops empty values -- so a webview that simply did not
        // set it produced a run with no `--tag` at all, which clone-stack.sh
        // refuses (exit 2). Defaulting at the one place both front ends go
        // through is what makes "the wizard forgot an input the CLI collects"
        // unable to happen again (memql#3560).
        //
        // AND THE ONE PLACE THE THREE REF KINDS ARE TOLD APART (memql#3901).
        // clone-stack.sh takes exactly one of --tag / --branch / --commit, so
        // the translation belongs here for the same reason the default does:
        // both front ends pass the operator's single "which version" answer and
        // neither needs to know how the script spells it.
        //
        // Precedence is commit > main > tag, and it is not arbitrary. A commit
        // is only ever set by a repair replaying what the receipt recorded, and
        // that has to win over any version the surrounding session carries --
        // otherwise a repair of a branch install becomes an upgrade, which is
        // memql#3605's failure by a new route.
        params = opts.commit
          ? present({ commit: opts.commit, repo: opts.repo, dest: stackDir })
          : isMainBranchChoice(opts.tag ?? "")
            ? present({ branch: MAIN_BRANCH_CHOICE, repo: opts.repo, dest: stackDir })
            : present({ tag: opts.tag || DEFAULT_STACK_TAG, repo: opts.repo, dest: stackDir });
        break;
      case "clusterUp":
        // The checkout stackCheckout just created. Without it k3d.up derives a
        // root from its own location, which in a packaged extension is the
        // staged tree -- no deploy/, and not a git tree, so the ArgoCD target
        // revision silently became "main". The graph already declares the
        // dependency; this is the value finally flowing along it.
        // The checkout, AND where the node images come from. Without the
        // second, ArgoCD applies an overlay whose images only exist on a
        // machine that built them, and every pod lands in ImagePullBackOff
        // (memql#3572). Both are values, not topology: same manifests, same
        // overlay, same sync path.
        params = present({
          "repo-root": stackDir,
          "image-registry": opts.imageRegistry || DEFAULT_IMAGE_REGISTRY,
          // CONVERTED, not passed through: git tags carry the `v` and image
          // tags do not. See imageTagFor.
          //
          // AND NEVER "main" (memql#3901). build-engine-images.yml publishes
          // memql-<node>:<version> on a release dispatch only -- there is no
          // main tag and no nightly in GHCR -- so a `main` install runs the
          // newest images that exist, which is the newest published release.
          // The version picker states that skew rather than hiding it. See
          // imageTagForVersion.
          "image-tag": imageTagForVersion(opts.tag ?? "", opts.latestRelease ?? ""),
          // The FOURTH consumer of the typed domain (memql#3593). The other
          // three place it on the MACHINE -- the hosts block, the certificate,
          // the front-door probe. This one places it in the CLUSTER: k3d.up
          // seeds the memql-domain ConfigMap every node derives its issuer from
          // and, when the domain differs from the overlay's committed default,
          // patches the two Ingress hostnames on the ArgoCD Application.
          //
          // Without it the cluster serves the default while the machine is set
          // up for something else, which is memql#3593 exactly: a domain that
          // resolves and then answers as the wrong site.
          domain: opts.domain,
        });
        break;
      case "hostsBlock":
        // The hostnames the operator's domain implies, so the block points at
        // what they asked for rather than at this file's idea of a domain
        // (memql#3590).
        params = present({ hostnames: frontDoor?.hostnames.join(",") });
        break;
      case "localCA":
        // PINNED, not inherited (memql#3576). mkcert reads CAROOT out of
        // XDG_DATA_HOME, which snapd points at a revision-scoped directory
        // for a snap-packaged editor -- so the CA landed somewhere the
        // operator's own mkcert would never look, under a path that moves on
        // the next refresh.
        //
        // And issued for the DOMAIN THAT WAS TYPED (memql#3590): a certificate
        // covering someone else's front door is an untrusted front door.
        params = present({
          caroot: path.join(process.env.HOME ?? "", DEFAULT_CAROOT_DIR),
          hostnames: frontDoor?.certNames.join(","),
        });
        break;
      case "frontDoor":
        // Probing the hosts this install actually created. Against the default
        // hostnames it was checking a front door nobody built, and reporting a
        // broken installer for a cluster that was fine (memql#3590).
        params = present({ hosts: frontDoor?.probeHosts.join(",") });
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
          // DEFAULTED HERE, like the release tag and for the same reason: it is a
          // run input the CLI collects and the wizard has no field for, and
          // `present()` drops empty values -- so a webview that simply did not
          // set it reached seed-bootstrap.sh without it. That script refuses an
          // INCOMPLETE bootstrap set on purpose, because a partial seed writes a
          // Secret that looks healthy, brings the cluster up green, and leaves
          // the operator at a login page for an account that was never created.
          // The refusal was right; the wizard was wrong (memql#3568).
          //
          // invite_only is the answer for the cluster this wizard builds: one
          // owner, bootstrapped from these very values, on a machine reachable
          // on the operator's own machine. `open` would let anyone who can reach it register.
          // An operator who wants otherwise passes --registration-mode.
          "registration-mode": opts.registrationMode || DEFAULT_REGISTRATION_MODE,
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
      // Either the step declares no receipt at all, or no run of it ever
      // recorded where it wrote -- which for the steps that record a location
      // on success means it never got as far as writing (memql#3564). Both are
      // "nothing here", and both are SATISFIED: the state this removal would
      // have established already holds, so the removals waiting on it run.
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
 *
 * DELEGATED, NOT COPIED (memql#3911). The composition itself belongs to the
 * spawner now, because a capability script run by anything OTHER than this
 * session used to get a bare `process.env` and fail to find kubectl -- which is
 * exactly what happened to `mintOwnershipLink`. What stays here is the one
 * thing this session knows and the runner does not: an operator-chosen
 * `toolDir`.
 *
 * The overlay is applied BEFORE the PATH composition so an `opts.env` naming
 * its own PATH is still given the tool directory, rather than silently losing
 * it to a spread ordering.
 */
function childEnv(opts: SessionOptions): NodeJS.ProcessEnv {
  return withInstalledTools({ ...process.env, ...(opts.env ?? {}) }, opts.toolDir);
}

/**
 * The checkout directory both stackCheckout and clusterUp are given.
 *
 * Mirrors clone-stack.sh's own default (~/.memql/src) rather than leaving each
 * script to apply its own, so the two cannot disagree about where the stack
 * landed. The script default remains for a bare CLI invocation with no --dest.
 */
function resolveStackDir(opts: SessionOptions): string {
  const explicit = (opts.stackDir ?? "").trim();
  if (explicit !== "") return explicit;
  return path.join(process.env.HOME ?? "", ".memql", "src");
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
  /**
   * This removal takes away something that is NOT memQL-only, so the operator
   * chooses (memql#3566). k3d, kubectl, mkcert and the local CA are general
   * tools they may now depend on; the cluster, the checkout and the hosts block
   * are not. Carried from the graph so the wizard can offer the shared ones
   * unticked and remove the rest without asking.
   */
  shared: boolean;
  /** What else the shared thing is good for -- shown beside the checkbox. */
  sharedReason: string;
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
        shared: step.shared,
        sharedReason: step.sharedReason,
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
      shared: step.shared,
      sharedReason: step.sharedReason,
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
