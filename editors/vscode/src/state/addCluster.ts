// The add-a-cluster wizard's state machine: where the operator is, what they
// have typed, and what the run has reported so far.
//
// Separate from the webview for the reason every other state module here is:
// `cmd/memql-lsp/vscodeimportrule_test.go` keeps `vscode` out, which is what
// lets an operator's whole path through an install be driven under bare
// `node --test` with no workbench and no cluster. The panel is adapter wiring
// over this.
//
// NO DECISION LIVES HERE. Step order, dependencies, what may overlap, what
// needs elevation and what an uninstall touches are the graph's and the
// receipt's, and they arrive as events. This module never decides what to run
// -- only what to show.
//
// Refs: #3475 #3470 #3469 #3463

import type { ExecEvent } from "../install/executor.js";
import type { RecoveryKeyState } from "../install/recoveryKey.js";
import type { AddClusterAction } from "../clusters/presence.js";
import type { ClustersFile } from "../clusters/model.js";
import {
  composeEndpointFromDomain,
  identityBaseUrlFor,
  normalizeDomain,
  webSocketUrlFor,
} from "../connection/endpoint.js";
import type { HandoffResult } from "../install/handoff.js";
import { installDomainProblem, DEFAULT_LOCAL_DOMAIN, DEFAULT_STACK_TAG } from "../install/stackPin.js";
import { runProgressOf } from "./installProgress.js";
import type { ProgressPhase, RunProgress } from "./runProgress.js";
import {
  appendLog,
  copyStep,
  markFinished,
  markPhase,
  markStarted,
  recordsForAttempt,
  upsertStep,
} from "./stepRecords.js";

/** Where the operator is. */
export type Screen =
  /** The cards, built from the presence verdict. */
  | "landing"
  /** Everything an install needs, asked before any work starts. */
  | "collect"
  /** The remote-cluster registration form (#3475). */
  | "connect"
  /** The itemized dry run an uninstall confirms against (#3476). */
  | "uninstallPreview"
  /** Step progress. */
  | "running"
  /** One step failed; retry or switch it to guided. */
  | "failedStep"
  /** Terminal: finished, or cancelled. */
  | "done";

/**
 * How a step renders.
 *
 * SIX STATES, not two. They are the executor's four statuses plus the two a
 * run needs that an outcome cannot carry -- not started yet, and started but
 * not finished. `preserved` in particular cannot be folded into success or
 * failure: it is the uninstall keeping something the operator already had, and
 * it is the whole two-tier model.
 */
export type StepState = "pending" | "running" | "done" | "skipped" | "preserved" | "failed";

export interface StepProgress {
  id: string;
  /** The graph's short label ("Creating the cluster"); "" until the plan arrives. */
  label: string;
  description: string;
  state: StepState;
  /** The sentence a non-ok status carried. */
  reason: string;
  /** null when the step never ran. */
  exitCode: number | null;
  /** Everything the script wrote, verbatim, for the failure disclosure. */
  log: string;
  /** This step alone was switched to guided. */
  guided: boolean;
  /**
   * The exact command that fixes this failure, when the capability named one
   * (memql#3551).
   *
   * Capabilities that fail on something an operator can repair return
   * `result.remedy` -- a literal command line, not a description of one. The
   * wizard offers to type it into a terminal, which is how a step that needs
   * root gets run at all: the runner spawns everything unprivileged, so
   * `hostsBlock` and the docker group have no other route.
   *
   * Empty when the failure has no single command that fixes it.
   */
  remedy: string;
  /** When this attempt started the step, in epoch milliseconds. */
  startedAt?: number;
  /** When this attempt settled the step, in epoch milliseconds. */
  finishedAt?: number;
  /** What the running step last reported about itself (`cap_progress`). */
  phase?: ProgressPhase;
  /**
   * What the step came to on the PREVIOUS attempt, kept for display only.
   *
   * Every attempt starts every step from `pending` so the bar never runs
   * backwards within it; this is the one thing the new attempt keeps from the
   * last, so a screen can still say a step passed before.
   */
  previousState?: StepState;
}

/**
 * What an install asks for. Collected once, before anything runs.
 *
 * NO AI CREDENTIAL IS AMONG THEM (epic memql#5088). There is no vendor API key
 * anywhere in the product: both cloud vendors are reached by workload identity
 * federation, whose credential is a projected token inside a pod. And a
 * cluster THIS wizard builds could not use federation even if it were offered
 * one, because a k3d cluster's OIDC issuer is not publicly reachable and
 * neither vendor can verify a token minted by it. So the wizard collects no
 * vendor field at all, and `providerFederation` skips.
 */
export interface Inputs {
  domain: string;
  ownerFirstName: string;
  ownerLastName: string;
  ownerEmail: string;
  /**
   * The release tag to install (memql#3882, re-defaulted by memql#4429).
   *
   * THE INSTALL FORM RECOMMENDS LATEST, and that is the opposite of what this
   * field used to do. It started on `DEFAULT_STACK_TAG` and stayed there: the
   * pin was a reviewed diff, which is a good property, and it was ALSO the
   * answer an operator got when they did not choose -- so a fresh install
   * installed whatever release the extension was built against, which is
   * exactly the stale-pin failure `stackPin.ts` records four separate
   * postmortems of. A FRESH install wants the NEWEST release: its manifests
   * and its node images ship together at that tag, and every one of those
   * postmortems is a failure of not having installed it.
   *
   * So the listing seeds this field (`seedVersionFromListing`) and the picker
   * labels that entry `Latest -- vX.Y.Z (recommended)`. The pin's role NARROWS
   * to the offline fallback: it is what the field starts on, and what a machine
   * that cannot reach `git ls-remote` still installs.
   *
   * IT STARTS ON THE PIN RATHER THAN EMPTY, which the design record sketched as
   * "empty-meaning-latest". Empty is a value `validate()` refuses -- every
   * required field must be non-blank -- and the listing is ASYNC, so an operator
   * who pressed Start before it landed would be told the version is required.
   * Starting on the pin is the same observable behaviour with no race: the
   * listing overwrites it the moment it arrives, and if it never arrives the pin
   * is the answer anyway.
   *
   * Note this is the opposite pre-selection from the deployment page's tag
   * picker, which deliberately never pre-selects -- see `install/tags.ts`, which
   * states that boundary where both pickers can read it.
   */
  version: string;
}

export type InputField = keyof Inputs;

export interface FieldError {
  field: InputField;
  message: string;
}

/**
 * What the form starts with (#3473).
 *
 * A DEFAULT IS OFFERED, THE FIELD IS NOT SKIPPED. The domain is how the
 * cluster is addressed and a run pointed at the wrong one is not the run the
 * operator asked for, so it stays visible and editable rather than becoming
 * something the wizard decides silently.
 *
 * `memql.localhost` is not invented here. It is the installer's OWN default:
 * `scripts/install/hosts-entries.sh` derives its three hostnames from it when
 * given no `--domain`, `mkcert-setup.sh` covers it, and the local overlay's two
 * Ingresses carry it as their committed hostname. Picking any other value here
 * would make the form disagree with the scripts it is about to run.
 *
 * It is a DEFAULT, not the only accepted answer (memql#3593): any well-formed
 * domain now reaches the cluster, through the memql-domain ConfigMap and two
 * patches on the ArgoCD Application.
 *
 * THE OWNER FIELDS ARE DELIBERATELY BLANK. A person's name and their email are
 * facts about them, and a wizard that guessed would either be wrong or would
 * prefill somebody else's details on a shared machine. `seed-bootstrap.sh`
 * agrees -- it defaults every owner field to the empty string and exits 2
 * naming what is missing.
 */
export const DEFAULT_INPUTS: Inputs = {
  // The constant, not a copy of it (memql#3590): the form's offer and the
  // overlay's committed default are the same fact, and a literal here is how
  // they drift.
  domain: DEFAULT_LOCAL_DOMAIN,
  ownerFirstName: "",
  ownerLastName: "",
  ownerEmail: "",
  // The OFFLINE FALLBACK, and only that (memql#4429). The listing overwrites it
  // with the newest release as soon as it lands; this is what the field holds
  // until then, and what it keeps on a machine that cannot list at all.
  version: DEFAULT_STACK_TAG,
};

// -----------------------------------------------------------------------------
// registering a cluster that already exists (the `connect` screen, memql#3475)
// -----------------------------------------------------------------------------

/**
 * What registering an existing cluster asks for.
 *
 * ONE SET OF FIELDS, held together, because this replaces a sequence of input
 * boxes that could not be navigated backwards and lost every answer on Escape.
 * A form is not a nicer rendering of that sequence -- it is the thing that
 * makes revising the second answer after seeing the fourth possible at all,
 * and holding the values here rather than in the DOM is what survives the
 * webview repaint each validation pass causes.
 *
 * `domain` and `token` are OPTIONAL and are still collected, for reasons that
 * are not symmetry:
 *   - the domain names where sign-in POSTs (identityBaseUrlFor); without it
 *     that derivation depends on the endpoint happening to be spelled
 *     `api.<domain>`, which a hand-registered cluster need not be;
 *   - the token is the paste-a-credential path, which "MemQL: Sign In" has
 *     made the exception rather than the rule -- so the field stays, and the
 *     ordinary answer is to leave it empty.
 */
export interface ConnectInputs {
  name: string;
  domain: string;
  endpoint: string;
  token: string;
}

export type ConnectField = keyof ConnectInputs;

export interface ConnectFieldError {
  field: ConnectField;
  message: string;
}

/**
 * The registry entry a valid form produces.
 *
 * IT CARRIES NO `local` KEY, and the absence is the point rather than an
 * omission. `local: true` means "this cluster's data is disposable", which
 * gates the mutation confirmation and, since memql#3466, decides whether the
 * tree row offers an uninstall -- read as a strict `=== true`, never as
 * truthiness. A cluster reached through THIS screen is one the operator
 * already has somewhere else; nothing here can know it is disposable, and the
 * direction that cannot destroy anything is to say nothing.
 *
 * Not-supplied and false are also not the same write. `local: false` would be
 * a key clusters.yaml's other author drops on its next save (the cockpit
 * declares the field `omitempty`), so writing it would make the two tools
 * churn the file against each other. Omitting the field entirely is the only
 * spelling that round-trips.
 */
export interface ClusterRegistration {
  name: string;
  endpoint: string;
  domain?: string;
  token?: string;
  /**
   * The issuer the cluster published, when discovery found one (memql#4624).
   *
   * Omitted rather than written empty, per connectDraft's rule: "" is an
   * explicit CLEAR to upsertCluster, and a draft that says "forget where this
   * cluster's identity service is" is not what a registration means.
   */
  issuer?: string;
}

/**
 * The domains this form refuses, and it refuses them BY NAME (memql#4431).
 *
 * THIS FORM IS FOR CLUSTERS REACHABLE OVER THE NETWORK. A local install is the
 * OTHER card on the landing screen, and it does far more than record an address:
 * it writes /etc/hosts entries, issues an mkcert leaf, creates a k3d cluster and
 * bootstraps an owner. An operator who types `memql.localhost` here gets a
 * registry entry pointing at a front door that does not exist, and the failure
 * arrives later as a connection error naming a hostname they typed themselves --
 * which reads as "MemQL is broken", not as "you wanted the other button".
 *
 * WHY IT IS A VALIDATOR AND NOT A SENTENCE IN THE HINT. Prose is advice; this is
 * a decision, and it is one the form can make with certainty. Being pure is what
 * lets the whole refusal list be driven under bare `node --test` rather than
 * through a webview.
 *
 * THE INSTALL FORM'S OWN `memql.localhost` DEFAULT IS UNTOUCHED, and the two
 * flows diverge here deliberately -- see `DEFAULT_LOCAL_DOMAIN`, which is the
 * installer's own default and the local overlay's committed Ingress host. The
 * same string is the RIGHT answer there and a wrong answer here, because there
 * the wizard is about to make it resolve and here it is only recording it.
 *
 * `.localhost` IS THE WHOLE FAMILY, not just the bare label: RFC 6761 reserves
 * the entire subtree to loopback, so `memql.localhost` and `anything.localhost`
 * resolve to this machine exactly as `localhost` does.
 *
 * Empty is accepted here and reported by the required-field check in its own
 * words, the way `installDomainProblem` does it.
 */
export function connectDomainProblem(domain: string): string | undefined {
  const trimmed = normalizeDomain(domain).toLowerCase();
  if (trimmed === "") return undefined;

  const bare = trimmed.replace(/^\[|\]$/g, "");
  const isLocalhostName = bare === "localhost" || bare.endsWith(".localhost");
  // 127.0.0.0/8 -- the whole loopback block, not just 127.0.0.1: 127.0.0.2 is
  // just as local and just as wrong here.
  const isLoopbackV4 = /^127\.\d{1,3}\.\d{1,3}\.\d{1,3}$/.test(bare);
  const isLoopbackV6 = bare === "::1" || bare === "0:0:0:0:0:0:0:1";

  if (isLocalhostName || isLoopbackV4 || isLoopbackV6) {
    return (
      "That is a local install's domain -- use \"Install a local cluster\" instead. " +
      "This form registers a cluster reachable over the network."
    );
  }

  // A FRONT-DOOR HOST PASTED INTO THE DOMAIN BOX (memql#4624).
  //
  // Everything this form writes is composed from the domain: the endpoint is
  // `api.<domain>:443` and the sign-in host is `identity.<domain>`. So pasting
  // the API host -- which is the URL an operator has most likely seen, because
  // it is the one their applications dial -- composes `api.api.example.com`
  // and `identity.api.example.com`, neither of which exists.
  //
  // Nothing caught it. The probe failed with a generic "no answer within 10s",
  // the operator clicked "Save anyway" because the cluster demonstrably works,
  // and sign-in dead-ended much later against a host that was never real.
  //
  // The labels are exactly the front door's ROLE set (component/frontdoor),
  // which MemQL derives from the domain and reserves. A domain of its own that
  // begins with one of them cannot be registered through this composition
  // anyway, so naming the mistake is strictly better than composing a host
  // that cannot answer.
  const frontDoorLabels = ["api", "identity", "mcp", "portal"];
  const firstLabel = bare.split(".")[0] ?? "";
  if (bare.includes(".") && frontDoorLabels.includes(firstLabel)) {
    const suggested = bare.slice(firstLabel.length + 1);
    return (
      `That looks like the cluster's ${firstLabel} host, not its domain. ` +
      `MemQL composes every host from the domain -- the endpoint is \`api.<domain>\` ` +
      `and sign-in is \`identity.<domain>\` -- so enter \`${suggested}\` here. ` +
      `If \`${bare}\` really is the domain, set the endpoint by hand under Advanced.`
    );
  }
  return undefined;
}

/**
 * The stand-in a domain occupies while the webview is composing the hint.
 *
 * WHY A TEMPLATE AND NOT A RULE (memql#4431). The hint under the domain box
 * updates as the operator types, which means something on the WEBVIEW side has
 * to build `api.<domain>:443`. `endpoint.ts` records at length that this
 * composition was once inlined in three places, that three copies is three
 * places to drift from the ingress that actually serves it, and that the drift
 * is invisible because every copy produces a plausible hostname.
 *
 * So the webview is handed the composition ALREADY PERFORMED, by the real
 * function, over a placeholder -- and substitutes the typed domain into it. The
 * convention still has exactly one spelling; the script only knows how to
 * replace a substring.
 *
 * The placeholder survives `normalizeDomain` untouched (it has no whitespace and
 * no leading or trailing dots), which is what makes the template come back with
 * a hole in it rather than an empty string.
 */
export const DERIVATION_PLACEHOLDER = "%DOMAIN%";

/**
 * The sentence under the domain box: what this form is about to connect to.
 *
 * A DERIVATION SHOWN IS A DERIVATION AN OPERATOR CAN CHECK. Two fields now
 * produce four values -- endpoint, sign-in host, portal URL, registry entry --
 * and the previous form asked for the endpoint outright precisely because
 * nothing displayed it. Showing it is what makes asking for it unnecessary.
 */
export function derivationLine(domain: string): string {
  const endpoint = composeEndpointFromDomain(domain);
  return endpoint === ""
    ? "MemQL will connect to api.<domain>:443."
    : `Will connect to ${endpoint}.`;
}

/**
 * What a probe is pointed at: the sign-in host, and the front door (memql#4432).
 *
 * BOTH, because they are different hosts and they fail differently. `api.<domain>`
 * serves the gRPC front door the editor dials; `identity.<domain>` serves the
 * JWKS feed and /oauth/token. A cluster whose ingress routes one and not the
 * other is a real and common half-configured state, and a probe that only asked
 * about one of them would call it healthy.
 */
export interface ConnectProbeTargets {
  /** `https://identity.<domain>/.well-known/jwks.json` */
  jwksUrl: string;
  /** `api.<domain>:443`, or the operator's Advanced override. */
  endpoint: string;
}

/**
 * What the host found, in the only two shapes a form can render.
 *
 * `issuer` is the RFC 8414 issuer identifier the cluster published, when it
 * published one (memql#4624). It is carried back so the registry entry can
 * RECORD it: `identityBaseUrlFor` prefers `issuer` over the `identity.<domain>`
 * convention, so writing it is what makes an identity service at a
 * non-conventional host work -- and it is what makes memql#4620's claim probe,
 * which reads `cluster.issuer` and until now found nothing ever writing it,
 * reachable at all. Absent on a cluster that publishes no document; the
 * convention then applies exactly as before.
 */
export type ConnectProbeVerdict =
  | { ok: true; issuer?: string }
  | { ok: false; reason: string };

/**
 * The probe itself, INJECTED (memql#4432).
 *
 * It needs a network and Node's https, and this module must not: keeping the
 * whole registration form -- validation, refusal, revision, the shape of the row
 * that lands -- drivable under bare `node --test` is what the screen's own
 * design record already turns on. So the DECISION lives here and only the socket
 * lives in the panel.
 */
export type ConnectProbe = (targets: ConnectProbeTargets) => Promise<ConnectProbeVerdict>;

/** Where a probe stands, as the form renders it. */
export type ConnectProbeState =
  | { state: "none" }
  | { state: "running" }
  | { state: "passed"; endpoint: string; issuer?: string }
  | { state: "failed"; endpoint: string; reason: string };

/**
 * What one click of Save should do.
 *
 * `invalid` -- the form has problems and they are on the fields.
 * `warned`  -- the probe failed; the operator is shown why and Save becomes
 *              "Save anyway". NOTHING is written.
 * `write`   -- go ahead.
 */
export type ConnectSaveOutcome = "invalid" | "warned" | "write";

const EMPTY_CONNECT: ConnectInputs = { name: "", domain: "", endpoint: "", token: "" };

/**
 * The refusal a duplicate name earns, worded EXACTLY as clusters/file.ts
 * `addCluster` words its own.
 *
 * Two walls stand between a duplicate and the file -- this synchronous check
 * over a registry already read, and addCluster's re-read at write time -- and
 * they are both necessary: clusters.yaml is shared with the Cockpit, so no
 * read this side stays authoritative, and a form that only found out at write
 * time would be back to reporting the problem after the operator had committed
 * to it. Saying the same sentence at both walls is what stops the second one
 * from reading as a different, more alarming failure than the first.
 */
export function duplicateNameMessage(name: string): string {
  return `a cluster named "${name}" already exists; edit it instead of adding it again`;
}

/**
 * The endpoint's problem, as the DIALER sees it.
 *
 * webSocketUrlFor is the function the connection layer actually calls, and it
 * throws for exactly the endpoints that cannot be dialed -- so it is the
 * validator here rather than the inspiration for one. A second parser would be
 * free to accept something the dialer later rejects, and the operator would
 * find out at connect time with the form long since closed.
 *
 * Its message names the cluster ("cluster \"x\": endpoint scheme must be...")
 * because its usual caller has no field to attach the sentence to. This one
 * does, and the label above the box already says which value is wrong, so the
 * prefix is stripped -- by exact match against the name we passed in, not by a
 * pattern, since anything else would be a parser of the validator's prose.
 */
function endpointProblem(name: string, endpoint: string): string | undefined {
  try {
    webSocketUrlFor({ name, endpoint });
    return undefined;
  } catch (err) {
    const message = err instanceof Error ? err.message : String(err);
    const prefix = `cluster "${name}": `;
    return message.startsWith(prefix) ? message.slice(prefix.length) : message;
  }
}

/**
 * What each action cannot start without.
 *
 * An install needs everything up front, because a wizard that stops to ask a
 * question nine minutes in is a wizard people abandon.
 *
 * A REPAIR NEEDS ONLY THE DOMAIN. It is the same graph re-run over a machine
 * that already has these answers recorded, and every step verifies first and
 * skips when satisfied -- so demanding the owner's name again would be asking
 * the operator for what the machine can already see. The domain stays because
 * it is how the cluster is addressed, and a repair pointed at the wrong one is
 * not a repair.
 *
 * NO AI CREDENTIAL IS COLLECTED BY ANY ACTION (epic memql#4440, then epic
 * memql#5088). memql#4440 made the vendor fields optional, on the ground that
 * installing a cluster spends no inference; memql#5088 removed them, because
 * there is no longer any such thing as a vendor API key in this product.
 * `seed-bootstrap.sh` no longer declares `--provider` or `--provider-key-file`,
 * and a capability script exits 2 on an undeclared flag -- so the fields are
 * not merely unneeded, they are unpassable.
 */
/**
 * The remedy a capability declared, or "" (memql#3551).
 *
 * DEFENSIVE ABOUT ITS OWN INPUT. The envelope is JSON a script produced, so
 * `result` can be anything at all; anything that is not a non-empty string is
 * no remedy. It is about to be offered to an operator as a command to run with
 * root, so "probably a string" is not the standard.
 */
function remedyFrom(envelope: { result?: unknown } | null | undefined): string {
  const result = envelope?.result;
  if (result === null || typeof result !== "object") return "";
  const value = (result as Record<string, unknown>).remedy;
  return typeof value === "string" ? value.trim() : "";
}

export function requiredFields(action: AddClusterAction): InputField[] {
  switch (action) {
    case "install":
    case "installGuided":
      return [
        "domain",
        "ownerFirstName",
        "ownerLastName",
        "ownerEmail",
        // Asked for LAST, and pre-filled: it is the one field with a house
        // answer, so it reads as a confirmation rather than a question.
        // A REPAIR does not collect it -- the receipt replays the version the
        // cluster was installed at (memql#3605), and asking would invite an
        // operator to silently upgrade a cluster they meant to repair.
        "version",
      ];
    case "repair":
      // THE OWNER FIELDS ARE COLLECTED, and the receipt supplies their DEFAULTS
      // (znasllc-io#3888). This used to be `["domain"]` alone, on the reasoning
      // that a repair re-runs a graph over a machine that has already answered
      // these questions. Their absence was a hard failure rather than a missing
      // convenience: `seedBootstrap` refuses a partial bootstrap set on purpose
      // -- a partial seed writes a Secret that looks healthy and leaves the
      // operator at a login page for an account that was never created -- so a
      // repair that collected none of them and pre-filled none of them died at
      // `exit 2` naming three values the wizard offered no way to supply. Every
      // other param on that step reached it: `domain` was collected, and
      // `registration-mode` has a default. Only the owner had neither.
      //
      // Nobody retypes a good answer: the panel pre-fills each from the
      // receipt. What the box buys is that a RECORDED answer which is itself
      // wrong can be corrected here, rather than re-running into the same
      // failure with no field to edit (memql#3544).
      return [
        "domain",
        "ownerFirstName",
        "ownerLastName",
        "ownerEmail",
      ];
    case "uninstall":
    case "connect":
      return [];
    case "reconnect":
      // NOTHING IS COLLECTED, which is the entire point of the action
      // (memql#3741): the domain comes off the install receipt, or off the
      // installer's own default when the receipt is gone. A field here would
      // be the form this exists to remove.
      return [];
    case "adopt":
      // The same nothing, for the same reason, on a cluster that has no
      // receipt at all (memql#5118, D8): the front door of a k3d cluster named
      // `memql` is the installer's own default hostname, and asking somebody
      // to type an address the machine already knows is the form `reconnect`
      // exists to remove. What differs is only that this one is offered on
      // evidence from k3d rather than from a receipt.
      return [];
  }
}

// `optionalFields` IS GONE, with the only two fields it ever held (epic
// memql#5088). It answered "what else is worth offering while we are here",
// and the answer was the AI-provider pair; with no vendor credential anywhere
// in the product there is nothing this wizard collects but does not wait for.
// A function returning [] for every action, a second validation pass over an
// empty list and a disclosure renderer with nothing to render would be three
// pieces of machinery describing a feature that no longer exists.

const LABELS: Record<InputField, string> = {
  domain: "domain",
  ownerFirstName: "first name",
  ownerLastName: "last name",
  ownerEmail: "email address",
  version: "version",
};

/** The screen each action needs first. */
function screenFor(action: AddClusterAction): Screen {
  switch (action) {
    case "uninstall":
      return "uninstallPreview";
    case "connect":
      return "connect";
    case "reconnect":
      // Straight to the hand-off screen: there is nothing to collect and
      // nothing to run, so the next thing the operator sees is the cluster in
      // their list with sign-in offered (memql#3741).
      return "done";
    default:
      return "collect";
  }
}

export class AddClusterState {
  private currentScreen: Screen = "landing";
  private chosen: AddClusterAction | undefined;
  private guidedRun = false;
  private values: Inputs = { ...DEFAULT_INPUTS };
  /** Whether `version` carries an operator's answer rather than a default. */
  private versionTouched = false;
  private fieldErrors: FieldError[] = [];
  private records: StepProgress[] = [];
  // THE PROGRESS BAR'S MEMORY. `progress(now)` never reports less than it did
  // last time within one attempt, and this is where "last time" lives; a new
  // attempt's `runStarted` sets it back to zero.
  private highWater = 0;
  // Per-step expected seconds measured on this machine (runProgress.ts
  // `historicalWeights`). Empty means the defaults, which is a fine bar.
  private weights: Readonly<Record<string, number>> = {};
  private readonly clock: () => number;
  private failedId: string | undefined;
  private wasCancelled = false;
  private didSucceed = false;
  private connectValues: ConnectInputs = { ...EMPTY_CONNECT };
  private connectProbeStatus: ConnectProbeState = { state: "none" };
  private connectErrorList: ConnectFieldError[] = [];
  private connectFailureMessage = "";
  private registry: ClustersFile | undefined;
  private handoffResult: HandoffResult | undefined;
  // Whether the run established that this cluster has an OWNER ACCOUNT.
  //
  // A FACT, NOT A CREDENTIAL (memql#3906). This field used to hold the
  // enrolment link the run minted, so the done screen could replay it. That
  // link is single-use and expires in fifteen minutes, so the button was dead
  // by the time an operator who had walked away came back to it -- and a run
  // that minted none offered no route at all, leaving a terminal as the only
  // way in. The button now mints a FRESH link when clicked, which needs no
  // stored URL, and this boolean is all that is left to remember.
  //
  // The pleasant side effect is that no ENROLMENT credential is held in panel
  // state any more. (Two credentials still are, each with its lifetime argued
  // where it lives: the claim link below, and the one-time recovery key
  // reveal, memql#4079.)
  private ownerAccountExists = false;
  // The magic link the run RECOVERED, held under exactly the same rules and for
  // a more dangerous credential -- it authenticates as the cluster OWNER
  // (memql#3884). On a FRESH install this is the one that exists and the
  // enrolment link is empty, because a cluster is claimed by its first sign-in
  // and there is no account to enrol a passkey for until that has happened.
  private claimLink = "";
  // The recovery key this run claimed, held for DISPLAY, never storage
  // (memql#4079). The claim ROTATED the key and revealed the plaintext exactly
  // once, into the run's in-memory report; the run log and the receipt
  // withhold it (memql#3908), so this field and the done screen it feeds are
  // the only place the operator can ever read it. It lives exactly as long as
  // the screen that shows it: cleared by back(), cleared by beginRun(), gone
  // with the panel -- closing the screen is goodbye, and the screen says so.
  private revealedKey = "";
  // What the claim reported, for the block's no-key renderings. `none` renders
  // nothing at all -- an uninstall, a failed run, an old receipt.
  private recoveryState: RecoveryKeyState = "none";
  // WHETHER THE OPERATOR HAS CLICKED THROUGH TO THE PLAINTEXT (memql#4194).
  //
  // HERE RATHER THAN ON THE PANEL, and that placement is the whole of
  // memql#4616. This flag used to be a field on `AddClusterPanel`, where
  // nothing ever reset it -- so the click-to-reveal screen-share guard worked
  // exactly ONCE per panel lifetime. Install, reveal, Back, repair, and the
  // done screen rendered the NEW plaintext with no click at all, defeating the
  // guard at precisely the moment a screen is most likely to be shared.
  // Sitting beside the value it describes, it is let go of in the same three
  // places the key is -- `setRecoveryKey`, `back` and `beginRun` -- so a future
  // clearing path cannot take the credential and leave a flag about it behind.
  private keyRevealed = false;
  // WHETHER THE CLIPBOARD ACTUALLY TOOK IT (memql#4615).
  //
  // A claim about the CLIPBOARD, not about the click: the panel records it only
  // after `env.clipboard.writeText` resolved, because an operator who believes
  // they copied a key they did not is worse off than one who was told to select
  // it by hand -- the same reason the copy path is loud on failure. It is what
  // `recoveryKeyWouldBeLost` reads, and therefore the only thing standing
  // between a misclick on Back and a destroyed break-glass credential.
  private keyCopied = false;

  // WHETHER THE LOG PANE IS OPEN, AND WHETHER IT IS STILL FOLLOWING THE TAIL
  // (memql#4455).
  //
  // HERE RATHER THAN IN THE DOM, and that is the whole reason these are fields
  // at all. Both panels re-render by assigning `webview.html`, which replaces
  // the entire document -- during a run that happens on every `stepLog`,
  // roughly once a second. A `<details>` an operator opened would close itself
  // a second later, while they were reading it. So the open/closed flag is
  // panel state, the toggle is a message like every other control, and the
  // renderer emits the pane only when this says so.
  private logsShown = false;
  // Pinned to the bottom until the operator scrolls up, and re-armed when they
  // scroll back down. TRUE initially because a pane opened mid-run should show
  // what is happening NOW; there is nothing above the tail worth landing on.
  private logsFollowTail = true;

  /**
   * `now` is the clock step timings are read from, in epoch milliseconds --
   * injectable so a test can say exactly how long a step has been running.
   */
  constructor(options: { now?: () => number } = {}) {
    this.clock = options.now ?? ((): number => Date.now());
  }

  get screen(): Screen {
    return this.currentScreen;
  }
  get action(): AddClusterAction | undefined {
    return this.chosen;
  }
  /** The whole RUN is guided, as opposed to one step being switched. */
  get guided(): boolean {
    return this.guidedRun;
  }
  get inputs(): Inputs {
    return { ...this.values };
  }
  /**
   * Records a problem with one field that only the extension host could find.
   *
   * `problemWith` is a pure string check because this module is deliberately
   * free of `node:fs` -- but "is there actually a readable file at that path?"
   * is the question that catches a typo, a `~` the shell never expanded, and a
   * file deleted since the last install (memql#3544). The panel asks it and
   * reports the answer here, so the operator sees it under the box they typed
   * in rather than nine minutes into a run.
   */
  noteFieldProblem(field: InputField, message: string): void {
    this.fieldErrors = this.fieldErrors.filter((e) => e.field !== field);
    this.fieldErrors.push({ field, message });
  }

  get errors(): FieldError[] {
    return [...this.fieldErrors];
  }
  get steps(): StepProgress[] {
    return this.records.map(copyStep);
  }
  get failed(): StepProgress | undefined {
    const found = this.records.find((p) => p.id === this.failedId);
    return found === undefined ? undefined : copyStep(found);
  }

  /**
   * How far the run has got at `now`: the weighted percent, the status line
   * and "Step n of m" (state/runProgress.ts).
   *
   * NOT A PURE READ, deliberately: it remembers the percent it returns, so the
   * next call within this attempt can never report less. That is what keeps
   * the bar from running backwards when a step's own count restarts below the
   * time-based estimate it replaces.
   */
  progress(now: number = this.clock()): RunProgress {
    const result = runProgressOf(this.records, now, { highWater: this.highWater }, this.weights);
    this.highWater = result.highWater;
    return result;
  }

  /** Expected seconds per step id, measured on this machine. See `weights`. */
  setStepWeights(weights: Readonly<Record<string, number>>): void {
    this.weights = { ...weights };
  }
  /**
   * EVERY failed step, in graph order.
   *
   * A wave runs concurrently and independent branches are allowed to finish, so
   * "the failure" is not always one thing. Guidance is per exit code and the
   * codes genuinely differ -- a refusal asks for something different from a
   * missing prerequisite -- so rendering one of N would hand the operator
   * confident advice about a step they may not even be looking at.
   */
  get failures(): StepProgress[] {
    return this.records.filter((p) => p.state === "failed").map(copyStep);
  }
  get connectInputs(): ConnectInputs {
    return { ...this.connectValues };
  }
  get connectErrors(): ConnectFieldError[] {
    return [...this.connectErrorList];
  }
  /** What the WRITE refused, when the form itself had nothing wrong with it. */
  get connectFailure(): string {
    return this.connectFailureMessage;
  }
  get cancelled(): boolean {
    return this.wasCancelled;
  }
  get succeeded(): boolean {
    return this.didSucceed;
  }
  /**
   * What became of the cluster once the run finished (#3477).
   *
   * Undefined until the hand-off has run, and undefined forever for an action
   * that has no hand-off -- an uninstall registers nothing.
   */
  get handoff(): HandoffResult | undefined {
    return this.handoffResult;
  }

  setHandoff(result: HandoffResult): void {
    this.handoffResult = result;
    this.currentScreen = "done";
  }

  /**
   * Whether there is an owner account on this cluster to enrol against
   * (memql#3408, memql#3906).
   *
   * The done screen's question. It is durable -- an account does not expire
   * between the run finishing and the operator clicking -- which is exactly
   * what the link it replaced was not.
   */
  get canEnrol(): boolean {
    return this.ownerAccountExists;
  }

  setOwnerAccountExists(exists: boolean): void {
    this.ownerAccountExists = exists;
  }

  /** Whether the run recovered a magic link, so the cluster can be claimed. */
  get hasClaimLink(): boolean {
    return this.claimLink !== "";
  }

  /** The link itself, for the host-side opener only. */
  get claimUrl(): string {
    return this.claimLink;
  }

  setClaimUrl(url: string): void {
    this.claimLink = url;
  }

  /** The one-time reveal, for the done screen and the copy button only. */
  get revealedRecoveryKey(): string {
    return this.revealedKey;
  }

  get recoveryKeyState(): RecoveryKeyState {
    return this.recoveryState;
  }

  setRecoveryKey(key: string, state: RecoveryKeyState): void {
    this.revealedKey = key;
    this.recoveryState = state;
    // A NEW VALUE IS A NEW SECRET (memql#4616, memql#4615). `beginRun` has
    // already re-armed both flags by the time a run reaches this call, which
    // makes these two lines belt to that brace -- and the belt is what a second
    // writer of this setter (a reconnect, a receipt read, whatever the next
    // screen needs) gets for free rather than has to remember. Inheriting
    // "already revealed" would put a rotated key on screen unasked; inheriting
    // "already copied" would let Back destroy one nobody has.
    this.keyRevealed = false;
    this.keyCopied = false;
  }

  /**
   * Whether the operator has clicked through the reveal (memql#4194).
   *
   * The done screen renders a button rather than the value until this is true,
   * so the screen can sit open -- in a share, on a projector -- with the
   * plaintext out of the DOM entirely rather than merely styled away.
   */
  get recoveryKeyRevealed(): boolean {
    return this.keyRevealed;
  }

  /** Whether the clipboard took the key, as opposed to being asked to. */
  get recoveryKeyCopied(): boolean {
    return this.keyCopied;
  }

  /**
   * Whether leaving this screen right now would destroy the key for good
   * (memql#4615).
   *
   * THE QUESTION BACK HAS TO ASK. `back()` deliberately lets go of the
   * plaintext, only the key's HASH was ever stored, and the done screen renders
   * Back as an ordinary secondary button in the same row as the credential
   * buttons -- so one misclick permanently destroys the cluster's break-glass
   * credential, and the screen's own copy ("closing this screen is goodbye")
   * knew the stakes while doing nothing about them.
   *
   * A GETTER HERE, not a condition in the panel, for the reason
   * `primaryHandoffAction` gives: `addClusterPanel.ts` imports `vscode`, so a
   * decision written into it is a decision nothing under `node --test` can
   * reach -- and this one decides whether a credential survives.
   *
   * `claimed` with a non-empty key is the ONLY state holding a plaintext.
   * `alreadyClaimed`, `awaitingOwner` and `revealLost` carry no value to lose
   * -- `revealLost` least of all, since its whole content is that the value
   * was already lost (memql#4628) -- and prompting over those would train an
   * operator to click through the one prompt on this surface that matters.
   */
  get recoveryKeyWouldBeLost(): boolean {
    return this.recoveryState === "claimed" && this.revealedKey !== "" && !this.keyCopied;
  }

  /** The reveal was clicked. One way only; `setRecoveryKey` re-arms it. */
  revealRecoveryKey(): void {
    this.keyRevealed = true;
  }

  /**
   * The clipboard took the key.
   *
   * Called from the panel's SUCCESS path alone. A failed write leaves this
   * false, which is what keeps the Back prompt in front of an operator whose
   * copy silently did not happen.
   */
  recordRecoveryKeyCopied(): void {
    this.keyCopied = true;
  }

  /** Whether the run's output is disclosed. */
  get logsOpen(): boolean {
    return this.logsShown;
  }

  /** Whether the pane should still be pinned to the tail on the next render. */
  get logsFollow(): boolean {
    return this.logsFollowTail;
  }

  /**
   * The operator pressed the disclosure.
   *
   * RE-ARMS THE TAIL ON OPEN, because a pane being opened is a pane nobody has
   * scrolled yet, and the honest landing place for one opened during a run is
   * whatever is happening now. Closing leaves the flag alone: it is answered
   * again the next time the pane is opened.
   */
  toggleLogs(): void {
    this.logsShown = !this.logsShown;
    if (this.logsShown) this.logsFollowTail = true;
  }

  /**
   * The pane was scrolled, and whether it ended up at the bottom.
   *
   * RECORDED, NEVER REPAINTED -- the same call every keystroke on these forms
   * makes. A render replaces the document, so answering a scroll with one would
   * fight the operator for the scrollbar.
   */
  setLogsFollow(follow: boolean): void {
    this.logsFollowTail = follow;
  }

  /**
   * Which action the done screen leads with.
   *
   * A FUNCTION RATHER THAN THREE CONDITIONALS IN AN HTML TEMPLATE, because this
   * is the decision that was wrong (memql#3884) and a decision inside a
   * template string is one no test can reach -- the panel imports `vscode`, so
   * nothing under `node --test` can render it.
   *
   * The order is the operator's own dependency order, not a preference:
   *
   *  - `enrol` when the cluster HAS an owner account. That is every cluster
   *    this installer builds -- `seedBootstrap` creates the owner from the
   *    seeded values -- and the account holds no human credential, so an
   *    enrolment link is the only route to a first one. It no longer depends on
   *    the run having produced a link (memql#3906): the button mints its own.
   *  - `claim` when it does NOT, and a magic link was recovered. This is the
   *    hand-rolled case, a cluster brought up with no bootstrap env: nobody
   *    owns it, so signing in once is what creates the account.
   *  - `signIn` only when neither applies -- a re-run against a cluster whose
   *    owner exists and whose log window no longer holds a link. Leading with
   *    it in the `claim` case is what sent an operator to authenticate against
   *    an account that had never been created.
   */
  get primaryHandoffAction(): "enrol" | "claim" | "signIn" | "none" {
    const handoff = this.handoffResult;
    if (handoff === undefined || !handoff.ok) return "none";
    if (this.canEnrol) return "enrol";
    if (this.hasClaimLink) return "claim";
    return handoff.canSignIn ? "signIn" : "none";
  }

  // `providerSetupUrl` IS GONE (epic memql#5088), and both halves of it were
  // wrong by the time it was removed.
  //
  // Its GATE read `providerKeyFile`, a field that no longer exists: it
  // suppressed the line for an install that had supplied a key, and no install
  // can.
  //
  // Its ADDRESS was `https://portal.<domain>/settings/providers`, and epic
  // memql#4984 retired the portal -- `clusters/consoleUrl.ts` in this same tree
  // records that host as one nothing serves, having been caught by it twice.
  //
  // Nothing replaces it, because the sentence it accompanied was an invitation
  // to go and configure a vendor provider, and THIS cluster cannot use one: a
  // k3d cluster's OIDC issuer is not publicly reachable, so no vendor can
  // verify a token minted by it, and federation is the only door left. What the
  // done screen says instead is where a local cluster's models actually come
  // from -- see `installScreens.ts`.

  // ---------------------------------------------------------------------------
  // routing
  // ---------------------------------------------------------------------------

  chooseAction(action: AddClusterAction): void {
    this.chosen = action;
    // Guided is a property of the RUN, not a second screen. The collect step is
    // identical either way; the difference appears when steps execute, where a
    // guided step renders its command and waits on the same verify.
    this.guidedRun = action === "installGuided";
    this.currentScreen = screenFor(action);
    this.fieldErrors = [];
    this.clearConnectProblems();
  }

  back(): void {
    this.chosen = undefined;
    this.guidedRun = false;
    this.currentScreen = "landing";
    this.fieldErrors = [];
    // The recovery key's display lifetime IS the done screen (memql#4079).
    // Back is the only way off it short of closing the panel, so leaving lets
    // go of the plaintext; what was typed into forms stays, a credential does
    // not.
    //
    // THE PANEL ASKS FIRST NOW (memql#4615). This method destroys a credential
    // that only ever existed here -- only its hash was stored -- so the adapter
    // consults `recoveryKeyWouldBeLost` before calling it and puts a modal in
    // the way of a misclick. That guard is deliberately NOT in here: this
    // module is what the CLI and any future front end drive too, and a state
    // machine that refused a transition on its own would make "go back" mean
    // different things on different surfaces.
    this.revealedKey = "";
    this.recoveryState = "none";
    // AND THE FLAGS ABOUT IT (memql#4616). Leaving re-arms the reveal, so the
    // next key has to be clicked for as well; and re-arms the copy, so nobody
    // inherits "already copied" from a secret that no longer exists.
    this.keyRevealed = false;
    this.keyCopied = false;
    // THE OWNER MAGIC LINK GOES TOO, and until memql#4617 it was the one
    // credential on this screen that survived Back.
    //
    // It is also the most dangerous one: it authenticates as the cluster OWNER
    // and is issued UNBOUND -- whoever opens it completes it. An operator who
    // installed, claimed in the browser (which CONSUMES the link), came back
    // and repaired was offered "Claim this cluster" again, wired to the spent
    // URL and landing on an error page. `magic-link.sh` reads a 24h log window,
    // so the spent link keeps being "recoverable" long after it stopped
    // working. Exactly the failure memql#3906 removed for the enrolment link,
    // which this line finally removes for its more dangerous sibling.
    this.claimLink = "";
    // The problems go, what was TYPED stays. Back is one click away from every
    // form on this page, and an operator who lands on the cards by accident
    // must not have to retype four fields to get where they were.
    this.clearConnectProblems();
  }

  // ---------------------------------------------------------------------------
  // collecting
  // ---------------------------------------------------------------------------

  /**
   * Records one field, and clears only that field's error.
   *
   * Only that one: re-validating everything on each keystroke would erase the
   * errors on fields the operator has not reached yet, so the form would keep
   * forgetting what it had already told them.
   */
  setInput(field: InputField, value: string): void {
    // TOUCHING THE VERSION FIELD IS RECORDED, and it is recorded HERE because
    // this is the one place an operator's own answer reaches the field
    // (memql#4429). The tag listing arrives asynchronously and seeds this field
    // when it does; an operator who has already chosen must not have that choice
    // replaced by a network call landing a second later.
    if (field === "version") this.versionTouched = true;
    this.values[field] = value;
    this.fieldErrors = this.fieldErrors.filter((e) => e.field !== field);
    const problem = this.problemWith(field, value);
    if (problem !== undefined) this.fieldErrors.push({ field, message: problem });
  }

  /**
   * Offers the newest listed release as the version, unless the operator chose.
   *
   * WHY A SEED RATHER THAN A DEFAULT (memql#4429). "Latest" cannot be a
   * constant: it is whatever `git ls-remote` answers at page-open time, which is
   * later than the moment `DEFAULT_INPUTS` is read. So the field starts on the
   * offline fallback and this raises it to the newest release when the listing
   * lands -- the picker labels that same entry `Latest ... (recommended)`, so
   * the label and the selection are one fact rather than two that can disagree.
   *
   * IT IS A NO-OP ONCE THE OPERATOR HAS TOUCHED THE FIELD. That is the whole
   * reason it is a method on this state machine rather than a line in the panel:
   * "has anyone chosen yet" is form state, and the panel discards its DOM on
   * every repaint.
   *
   * An empty argument is ignored rather than written: an empty listing is not a
   * version, and blanking the field would turn a failed network call into
   * "A version is required."
   */
  seedVersionFromListing(newest: string): void {
    const tag = newest.trim();
    if (tag === "" || this.versionTouched) return;
    this.values.version = tag;
    this.fieldErrors = this.fieldErrors.filter((e) => e.field !== "version");
  }

  /** Whether the operator has answered the version field themselves. */
  get versionWasChosen(): boolean {
    return this.versionTouched;
  }

  /**
   * Every problem with what has been entered so far, for the action chosen.
   *
   * ONE PASS AGAIN (epic memql#5088). memql#4440 added a second, over
   * `optionalFields`, because demoting `providerKeyFile` out of the required
   * list would otherwise have moved it out of the only loop that ran
   * `problemWith` over it -- silently stopping the paste-the-key refusal that
   * kept a credential off a world-readable command line. Both fields are gone
   * now, so the second pass has nothing to iterate and the trap it guarded
   * against no longer has a subject.
   */
  validate(): FieldError[] {
    const action = this.chosen;
    if (action === undefined) return [];
    const errors: FieldError[] = [];
    for (const field of requiredFields(action)) {
      const value = this.values[field];
      const problem =
        value.trim() === "" ? `A ${LABELS[field]} is required.` : this.problemWith(field, value);
      if (problem !== undefined) errors.push({ field, message: problem });
    }
    return errors;
  }

  /** Shape checks that apply whether or not the field is required. */
  private problemWith(field: InputField, value: string): string | undefined {
    const trimmed = value.trim();
    if (trimmed === "") return undefined;
    if (field === "ownerEmail" && !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(trimmed)) {
      return "That does not look like an email address.";
    }
    if (field === "domain" && /\s/.test(trimmed)) {
      return "A domain cannot contain spaces.";
    }
    // A DOMAIN THE CLUSTER CANNOT SERVE (memql#3590). The release's local overlay
    // pins its Ingress hosts and identity issuer, so a custom domain resolves and
    // then answers as the wrong site -- which the operator would discover at
    // `frontDoor`, as a failure against a hostname they typed themselves. The
    // reason lives with the pinned release facts; see installDomainProblem.
    if (field === "domain") {
      const problem = installDomainProblem(trimmed);
      if (problem !== undefined) return problem;
    }
    // THE PASTE-THE-KEY REFUSAL IS GONE WITH THE FIELD IT GUARDED (epic
    // memql#5088). memql#3545 refused a value beginning `sk-` in the
    // `providerKeyFile` box, because the alternative was handing it to a
    // capability script as `--key-file=sk-ant-...` -- argv, which `ps` shows to
    // every process on the machine -- and then writing it verbatim into the
    // install receipt. There is no box, no flag and no vendor key now.
    //
    // `install/secrets.ts` KEEPS its half of that pair, and deliberately.
    // memql#3545 built two walls: this one, where a person can be told what to
    // do instead, and `redactSecrets` on the receipt WRITE, which covers every
    // other way a param can reach the file -- the CLI, a graph-pinned param, a
    // surface nobody has written yet. Only the first had a subject to lose.
    return undefined;
  }

  /**
   * Moves to the run, or refuses and shows why.
   *
   * Returns whether it started, so the caller does not have to re-derive the
   * answer from the screen.
   */
  beginRun(): boolean {
    const errors = this.validate();
    this.fieldErrors = errors;
    if (errors.length > 0) return false;
    this.currentScreen = "running";
    this.failedId = undefined;
    this.wasCancelled = false;
    this.didSucceed = false;
    // A second run must not show the first run's outcome while it is going.
    this.handoffResult = undefined;
    // Nor the first run's finding about the owner account: a repair that has
    // not reached seedBootstrap yet has established nothing, and carrying the
    // previous run's answer through would offer enrolment while the cluster is
    // mid-rebuild.
    this.ownerAccountExists = false;
    // And the previous run's recovery key with it: a repair over a claimed
    // cluster reports alreadyClaimed and no key, and the first run's plaintext
    // showing through that would be a stale reveal (memql#4079).
    this.revealedKey = "";
    this.recoveryState = "none";
    // With the two flags about it, so the next run's key is hidden until it is
    // asked for and is not treated as one somebody already has (memql#4616,
    // memql#4615).
    this.keyRevealed = false;
    this.keyCopied = false;
    // And the previous run's magic link (memql#4617). A repair correctly
    // reports `linkState=none` -- identity only logs a link that was REQUESTED
    // -- so without this line the repair's done screen offers the INSTALL's
    // link, which the operator has almost certainly already spent. Every
    // sibling above was already cleared here; this one was missed.
    this.claimLink = "";
    // A NEW RUN STARTS CLOSED. The disclosure being open is a fact about the
    // failure the operator was just reading, not a preference that should
    // outlive it -- and carrying it forward would open a pane onto the previous
    // run's output while this one has produced none.
    this.logsShown = false;
    this.logsFollowTail = true;
    this.highWater = 0;
    return true;
  }

  // ---------------------------------------------------------------------------
  // registering an existing cluster (memql#3475)
  // ---------------------------------------------------------------------------

  /**
   * Hands over the registry the duplicate-name check reads.
   *
   * A SNAPSHOT, and knowingly one. clusters.yaml is shared with the MemQL
   * Cockpit, so this list is only true of the moment it was read -- which is
   * why it is the FIRST of two walls and not the only one: `addCluster` re-reads
   * the file at write time and refuses there too. What this one buys is the
   * refusal arriving while the operator still has the name in front of them.
   *
   * Never supplied (a clusters.yaml that would not parse, a panel that has not
   * finished reading it) simply means the check cannot fire; the write-time
   * wall still does.
   */
  setRegistry(file: ClustersFile): void {
    this.registry = file;
  }

  /**
   * Records one field of the registration form.
   *
   * It VALIDATES NOTHING and only drops the error the field was carrying.
   * Every value arrives here on the way to an action -- the webview re-sends
   * the whole form with each message, because a render replaces its HTML
   * wholesale and the DOM is therefore not where form state lives -- so
   * checking here would re-derive on every click what connectDraft is about to
   * derive anyway. Dropping the stale error is the part that matters: an
   * operator who has just changed a field should not still be reading the
   * complaint about what it used to say.
   */
  setConnectInput(field: ConnectField, value: string): void {
    this.connectValues[field] = value;
    this.connectErrorList = this.connectErrorList.filter((e) => e.field !== field);
    // A VERDICT IS ABOUT THE VALUES IT WAS GIVEN (memql#4432). Once any of them
    // changes, the previous probe describes a cluster the operator is no longer
    // registering -- and a stale PASS is the dangerous direction: it would let a
    // corrected domain be written on the strength of a reachability check that
    // ran against the typo. Clearing it also retracts the "Save anyway" the
    // failed state was offering, so the next click probes again.
    this.connectProbeStatus = { state: "none" };
  }

  /** Where the reachability probe stands, for the form to render. */
  get connectProbe(): ConnectProbeState {
    return this.connectProbeStatus;
  }

  /**
   * The two hosts a probe checks, derived exactly as a save would derive them.
   *
   * THE SAME DERIVATION THE ROW GETS, called rather than copied: probing one
   * endpoint and registering another is a green check mark over a cluster that
   * will not dial. `identityBaseUrlFor` is the `identity.` half of the same
   * convention `composeEndpointFromDomain` is the `api.` half of, and it already
   * prefers the domain and falls back to reading it out of an `api.` endpoint --
   * which is exactly right when Advanced has overridden the endpoint.
   */
  connectProbeTargets(): ConnectProbeTargets {
    const domain = normalizeDomain(this.connectValues.domain);
    const endpoint = this.connectValues.endpoint.trim() || composeEndpointFromDomain(domain);
    const identity = identityBaseUrlFor({ name: "", endpoint, domain });
    return {
      jwksUrl: identity === undefined ? "" : `${identity}/.well-known/jwks.json`,
      endpoint,
    };
  }

  /**
   * What one press of Save does: refuse, warn, or write (memql#4432).
   *
   * A FAILED PROBE WARNS AND NEVER BLOCKS. Registering a cluster records how to
   * reach it; it does not require that the cluster is up right now. An operator
   * legitimately registers one that is stopped, half-deployed, or behind a VPN
   * they have not connected yet, and a form that refused would be wrong about
   * all three. So the first click reports the reason and the second writes --
   * which is also why the button relabels itself rather than a second control
   * appearing: the operator is confirming the SAME action, informed.
   *
   * THE ORDER MATTERS. Validation runs first, so a probe is never spent on a
   * form that cannot be saved anyway -- and the localhost family is refused by
   * that validation (memql#4431), which is what keeps the mkcert false-negative
   * out of this path entirely: Node's fetch cannot verify a local mkcert leaf,
   * so a `memql.localhost` probe would fail for a reason that says nothing about
   * the cluster. Public domains carry public chains.
   */
  async prepareConnectSave(probe: ConnectProbe): Promise<ConnectSaveOutcome> {
    const errors = this.validateConnect();
    this.connectErrorList = errors;
    this.connectFailureMessage = "";
    if (errors.length > 0) {
      this.connectProbeStatus = { state: "none" };
      return "invalid";
    }

    // The second click on an unchanged form: "Save anyway".
    if (this.connectProbeStatus.state === "failed") return "write";

    const targets = this.connectProbeTargets();
    this.connectProbeStatus = { state: "running" };
    let verdict: ConnectProbeVerdict;
    try {
      verdict = await probe(targets);
    } catch (err) {
      // A THROW IS A FAILED PROBE, NOT A FAILED SAVE. The injected function is
      // the panel's; if it breaks, the operator must still be able to register
      // their cluster, so this degrades to the warn-and-confirm path rather than
      // surfacing an exception on a form about someone else's DNS.
      verdict = { ok: false, reason: err instanceof Error ? err.message : String(err) };
    }

    if (verdict.ok) {
      this.connectProbeStatus = {
        state: "passed",
        endpoint: targets.endpoint,
        ...(verdict.issuer === undefined || verdict.issuer.trim() === ""
          ? {}
          : { issuer: verdict.issuer.trim() }),
      };
      return "write";
    }
    this.connectProbeStatus = { state: "failed", endpoint: targets.endpoint, reason: verdict.reason };
    return "warned";
  }

  /**
   * Every problem with the registration form, in field order.
   *
   * EVERY FIELD IS CHECKED before returning, the way coerceArgs checks every
   * argument: the operator sees all the problems at once rather than one per
   * attempt, which is the difference between one correction pass and four.
   */
  validateConnect(): ConnectFieldError[] {
    const values = this.connectValues;
    const errors: ConnectFieldError[] = [];
    const name = values.name.trim();
    const domain = normalizeDomain(values.domain);

    if (name === "") {
      errors.push({ field: "name", message: "A cluster name is required." });
    } else if (this.registry?.clusters.some((c) => c.name === name) === true) {
      errors.push({ field: "name", message: duplicateNameMessage(name) });
    }

    // THE DOMAIN IS REQUIRED NOW (memql#4431). It was optional, and the endpoint
    // was the first-class field -- which asked the operator for the DERIVED value
    // and left the source of the derivation as an afterthought. It is also the
    // value that names where sign-in POSTs (`identityBaseUrlFor`), so a
    // registration without one leaves that derivation depending on the endpoint
    // happening to be spelled `api.<domain>`. Two answers, and everything else
    // follows from them.
    if (domain === "") {
      errors.push({
        field: "domain",
        message:
          "A domain is required: MemQL composes the cluster's endpoint, sign-in host and portal URL from it.",
      });
    } else if (/\s/.test(domain)) {
      errors.push({ field: "domain", message: "A domain cannot contain spaces." });
    } else if (domain.includes("://")) {
      errors.push({
        field: "domain",
        message: "A domain is a hostname, not a URL -- drop the scheme.",
      });
    } else {
      const local = connectDomainProblem(domain);
      if (local !== undefined) errors.push({ field: "domain", message: local });
    }

    // The endpoint is DERIVED from the domain, and the Advanced box OVERRIDES it
    // for the rare non-standard front door. This is the same `api.<domain>:443`
    // convention identityBaseUrlFor reads back off a registered endpoint, called
    // rather than copied.
    const endpoint = values.endpoint.trim() || composeEndpointFromDomain(domain);
    if (endpoint === "") {
      errors.push({
        field: "endpoint",
        message:
          "An endpoint is required: the cluster's gRPC host:port, or a domain above to compose it from.",
      });
    } else {
      const problem = endpointProblem(name === "" ? "this cluster" : name, endpoint);
      if (problem !== undefined) errors.push({ field: "endpoint", message: problem });
    }

    const token = values.token.trim();
    if (token.startsWith("mql_pat_")) {
      errors.push({
        field: "token",
        message:
          "That is a Personal Access Token, and the mesh cannot verify one: it checks bearers against the identity service's JWKS feed, so a PAT fails before any lookup. Paste the `access_token` from POST <identity>/oauth/token, or leave this empty and run \"MemQL: Sign In\".",
      });
    } else if (/\s/.test(token)) {
      errors.push({
        field: "token",
        message:
          "An access token contains no whitespace -- this one looks like it picked up a line break on the way in.",
      });
    }

    return errors;
  }

  /**
   * The entry to write, or undefined with the reasons recorded.
   *
   * Returning the entry rather than writing it keeps this module free of the
   * filesystem, which is what lets the whole form -- validation, refusal,
   * revision, the shape of the row that lands -- be driven under bare
   * `node --test`.
   *
   * EMPTY OPTIONAL FIELDS ARE OMITTED, not written as "". Against a new entry
   * the two produce the same file, but they mean opposite things to
   * upsertCluster ("" is an explicit CLEAR), and a draft that says "clear the
   * token" is one refactor away from being handed to the update path.
   */
  connectDraft(): ClusterRegistration | undefined {
    const errors = this.validateConnect();
    this.connectErrorList = errors;
    this.connectFailureMessage = "";
    if (errors.length > 0) return undefined;

    const name = this.connectValues.name.trim();
    const domain = normalizeDomain(this.connectValues.domain);
    const token = this.connectValues.token.trim();
    const draft: ClusterRegistration = {
      name,
      endpoint: this.connectValues.endpoint.trim() || composeEndpointFromDomain(domain),
    };
    if (domain !== "") draft.domain = domain;
    if (token !== "") draft.token = token;
    // Recorded from the probe that just ran, not re-derived: this is the
    // cluster's own answer about where its identity service is (memql#4624).
    if (this.connectProbeStatus.state === "passed") {
      const issuer = (this.connectProbeStatus.issuer ?? "").trim();
      if (issuer !== "") draft.issuer = issuer;
    }
    return draft;
  }

  /** Records why the WRITE refused an otherwise-valid form. */
  failConnect(message: string): void {
    this.connectFailureMessage = message;
  }

  /**
   * Throws the draft away and returns to the cards.
   *
   * Escape and the close button both end here, and both must leave NOTHING
   * behind: a half-filled form is not a partial cluster, and the old sequence's
   * habit of writing whatever it had collected before the operator gave up is
   * the failure this screen exists to end. Nothing is written because nothing
   * writes except an explicit save -- this clears the values so a later visit
   * starts clean rather than resuming a draft the operator abandoned.
   */
  discardConnect(): void {
    this.connectProbeStatus = { state: "none" };
    this.connectValues = { ...EMPTY_CONNECT };
    this.clearConnectProblems();
    this.chosen = undefined;
    this.guidedRun = false;
    this.currentScreen = "landing";
  }

  private clearConnectProblems(): void {
    this.connectErrorList = [];
    this.connectFailureMessage = "";
  }

  // ---------------------------------------------------------------------------
  // folding the run
  // ---------------------------------------------------------------------------

  /**
   * Folds one executor event into the progress list.
   *
   * NEVER THROWS ON AN EVENT IT DOES NOT KNOW. This runs against a union that
   * the executor is free to extend, and a wizard that crashed on a new event
   * type would lose a run that was otherwise going fine.
   */
  apply(event: ExecEvent): void {
    switch (event?.type) {
      case "runStarted": {
        // THE STEPS AHEAD, not just the ones behind. Seeding the list here is
        // what makes `pending` reachable in a forward run at all -- without it
        // a step first appears when it STARTS, so the checklist grows from
        // empty and never says how much is left.
        //
        // A NEW ATTEMPT STARTS EVERY STEP FROM PENDING (Retry, a repair, or
        // the next run on the same page), with a fresh high-water mark. The
        // steps that already passed re-report as skipped within a second;
        // counting them done in between would put the bar ahead of the run and
        // then pull it back. stepRecords.recordsForAttempt keeps what each came
        // to last time as `previousState`.
        this.records = recordsForAttempt(this.records, event.steps);
        this.highWater = 0;
        return;
      }
      case "stepStarted": {
        const entry = this.upsert(event.step.id, event.step.label, event.step.description);
        markStarted(entry, this.clock());
        return;
      }
      case "stepPhase": {
        const entry = this.upsert(event.step.id, event.step.label, event.step.description);
        markPhase(entry, event.label, event.done, event.total);
        return;
      }
      case "stepLog": {
        const entry = this.upsert(event.step.id, event.step.label, event.step.description);
        appendLog(entry, event.line);
        return;
      }
      case "stepFinished": {
        const entry = this.upsert(event.step.id, event.step.label, event.step.description);
        markFinished(entry, event.outcome, this.clock());
        // Off the ENVELOPE, not parsed out of the human sentence: the capability
        // contract puts structured facts in `result`, and a remedy recovered by
        // pattern-matching prose would break the first time a message was
        // reworded (memql#3551).
        entry.remedy = remedyFrom(event.outcome.envelope);
        if (entry.state === "failed") {
          // THE FIRST FAILURE IS KEPT, not the last to resolve. A wave runs
          // under Promise.all and independent branches are deliberately allowed
          // to finish, so several steps can fail in one wave -- and overwriting
          // made the headline whichever one happened to settle last, a
          // scheduling accident. The earliest failure is the one the others may
          // be consequences of, which is the rule state/uninstallRun.ts already
          // states. Every failure is rendered (see `failures`); this only
          // decides which one the page LEADS with.
          if (this.failedId === undefined) this.failedId = entry.id;
          this.currentScreen = "failedStep";
          // FAILURE OPENS THE LOG (memql#4455). The pane is collapsed by
          // default because for the twelve minutes an install is going well
          // nobody wants kubectl's account of it -- but at the moment something
          // breaks the log IS the product, and making the operator find a
          // toggle first would be design spite. The pane anchors on this step:
          // `failedId` is already the FIRST failure rather than the last to
          // settle, so the anchor lands on the one the others may be
          // consequences of.
          this.logsShown = true;
        }
        return;
      }
      default:
        // waveStarted, and anything added later. Nothing to show.
        return;
    }
  }

  private upsert(id: string, label: string, description: string): StepProgress {
    return upsertStep(this.records, id, label, description);
  }

  // ---------------------------------------------------------------------------
  // recovery
  // ---------------------------------------------------------------------------

  /**
   * Puts every failed step back to pending and returns to the run.
   */
  retry(): void {
    // EVERY failed step, not only the one being led with. The retry re-runs the
    // whole graph -- each step verifies first and skips when satisfied, which is
    // the same property that makes repair an install re-run -- so leaving the
    // other failures marked `failed` would show the operator a stale verdict
    // about a step that is being attempted again in front of them.
    const failed = this.records.filter((p) => p.state === "failed");
    if (failed.length === 0) return;
    for (const entry of failed) this.resetForAnotherAttempt(entry);
    this.failedId = undefined;
    this.currentScreen = "running";
  }

  /**
   * Marks the failed steps guided, and only those steps.
   *
   * PER STEP, deliberately. An operator who would rather run the one command
   * that needs sudo by hand should not be dropped into a fully manual install
   * for the other eleven.
   */
  switchToGuided(): void {
    const failed = this.records.filter((p) => p.state === "failed");
    if (failed.length === 0) return;
    for (const entry of failed) {
      entry.guided = true;
      this.resetForAnotherAttempt(entry);
    }
    this.failedId = undefined;
    this.currentScreen = "running";
  }

  /**
   * Drops every trace of the attempt that just failed.
   *
   * THE LOG GOES WITH THE REST. `apply()` APPENDS each `stepLog` line, so an
   * attempt that kept the previous output would render both runs concatenated
   * inside one disclosure with no boundary -- and the failure being read would
   * be the one that is no longer happening.
   */
  private resetForAnotherAttempt(entry: StepProgress): void {
    if (entry.state !== "pending" && entry.state !== "running") entry.previousState = entry.state;
    entry.state = "pending";
    entry.reason = "";
    entry.exitCode = null;
    entry.log = "";
    delete entry.startedAt;
    delete entry.finishedAt;
    delete entry.phase;
  }

  /**
   * Ends the run at the operator's request.
   *
   * The progress list is KEPT. What ran, ran -- the receipt records it and an
   * uninstall can take it back, so a cancel that cleared the display would tell
   * the operator less than the machine actually knows.
   */
  cancel(): void {
    this.wasCancelled = true;
    this.didSucceed = false;
    this.failedId = undefined;
    this.currentScreen = "done";
  }

  finish(report: { ok: boolean; cancelled?: boolean }): void {
    this.wasCancelled = report.cancelled === true;
    this.didSucceed = report.ok && report.cancelled !== true;
    this.currentScreen = "done";
  }
}
