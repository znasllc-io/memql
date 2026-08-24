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
import { looksLikeProviderKey } from "../install/secrets.js";
import { installDomainProblem, DEFAULT_LOCAL_DOMAIN, DEFAULT_STACK_TAG } from "../install/stackPin.js";

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
}

/**
 * The AI providers an install can seed a key for (memql#3473).
 *
 * DERIVED FROM WHAT THE SCRIPT ACCEPTS, not from what the wizard felt like
 * offering: `scripts/install/verify-provider-key.sh` supports exactly these
 * two and exits 2 on anything else, which is a fault in MemQL rather than in
 * the operator's answer -- so the field is a CHOICE and this list is what it
 * chooses from.
 */
export const SUPPORTED_PROVIDERS = ["anthropic", "openai"] as const;

export type SupportedProvider = (typeof SUPPORTED_PROVIDERS)[number];

/** What an install asks for. Collected once, before anything runs. */
export interface Inputs {
  domain: string;
  ownerFirstName: string;
  ownerLastName: string;
  ownerEmail: string;
  /**
   * Which vendor the key below belongs to.
   *
   * COLLECTED, not pinned. It was hardcoded `anthropic` in the panel AND in
   * `install.json`, where graph params win -- so an operator holding an OpenAI
   * key had no route through this wizard at all, while every test enumerated
   * the other five fields and the criterion read as satisfied at a glance.
   * Which vendor a key belongs to is a fact about the OPERATOR'S KEY, which is
   * run input like the path beside it, not policy the graph pins.
   */
  provider: string;
  /** A PATH. The key itself never enters this module -- see SessionOptions. */
  providerKeyFile: string;
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
 * THE OTHER FOUR ARE DELIBERATELY BLANK. A person's name, their email and
 * where they keep an API key are facts about them, and a wizard that guessed
 * would either be wrong or would prefill somebody else's details on a shared
 * machine. `seed-bootstrap.sh` agrees -- it defaults every owner field to the
 * empty string and exits 2 naming what is missing.
 */
export const DEFAULT_INPUTS: Inputs = {
  // The constant, not a copy of it (memql#3590): the form's offer and the
  // overlay's committed default are the same fact, and a literal here is how
  // they drift.
  domain: DEFAULT_LOCAL_DOMAIN,
  ownerFirstName: "",
  ownerLastName: "",
  ownerEmail: "",
  // The provider DOES get a default, unlike the four personal fields: it is a
  // choice from a closed set rather than a fact about the operator, so a
  // pre-selection is an answer they can accept rather than a guess about them.
  provider: "anthropic",
  providerKeyFile: "",
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

/** What the host found, in the only two shapes a form can render. */
export type ConnectProbeVerdict = { ok: true } | { ok: false; reason: string };

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
  | { state: "passed"; endpoint: string }
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
 * NO AI PROVIDER KEY IS REQUIRED BY ANY ACTION (epic memql#4440). This list is
 * where that requirement lived, and it is the ONLY place it ever lived -- the
 * engine has always booted keyless (a provider whose key does not resolve
 * registers as unavailable and is skipped at selection; nothing refuses boot
 * over it), and `seed-bootstrap.sh` has always returned cleanly when handed
 * neither `--provider` nor `--provider-key-file`. So a wizard that demanded
 * one was demanding it on its own authority, for a cluster that did not need
 * it, and the answer to "why does installing MemQL need an LLM key" was
 * "it does not".
 *
 * The fields did not go away -- see `optionalFields`. Supplied, they behave
 * exactly as before: verified by the `providerKey` step, then seeded. Left
 * empty, nothing is verified and nothing is seeded, and provider
 * configuration happens in the portal at Settings -> AI providers.
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
        // NO PROVIDER, NO KEY FILE (epic memql#4440). They are collected --
        // see optionalFields -- but nothing waits on them: installing a
        // cluster spends no inference, so a vendor key is not a thing the
        // machine needs before it can be built. See the block comment above.
        // Asked for LAST, and pre-filled: it is the one field with a house
        // answer, so it reads as a confirmation rather than a question.
        // A REPAIR does not collect it -- the receipt replays the version the
        // cluster was installed at (memql#3605), and asking would invite an
        // operator to silently upgrade a cluster they meant to repair.
        "version",
      ];
    case "repair":
      // THE KEY FIELDS ARE COLLECTED, and the receipt supplies their DEFAULTS
      // (memql#3544). This used to be `["domain"]` alone, on the reasoning that
      // a repair re-runs a graph over a machine that has already answered these
      // questions -- and memql#3512 made it read the answers back off the
      // receipt so wave 2 could pass.
      //
      // That reasoning inverts the moment the RECORDED answer is the thing that
      // is wrong. A repair then re-runs with the same bad value, fails at the
      // same step, and offers no field in which to correct it -- which is the
      // state an operator who fumbled the key file is left in permanently, with
      // Uninstall reporting nothing to remove because nothing was installed.
      //
      // Nobody retypes a good path: the panel pre-fills both from the receipt.
      // What changes is that the value is now in a box that can be edited.
      //
      // THE OWNER FIELDS ARE HERE FOR THE SAME REASON, and their absence was a
      // hard failure rather than a missing convenience (znasllc-io#3888).
      // `seedBootstrap` refuses a partial bootstrap set on purpose -- a partial
      // seed writes a Secret that looks healthy and leaves the operator at a
      // login page for an account that was never created -- so a repair that
      // collected none of them and pre-filled none of them died at `exit 2`
      // naming three values the wizard offered no way to supply. Every other
      // param on that step reached it: `domain` was collected, and
      // `registration-mode` has a default. Only the owner had neither.
      //
      // THE KEY FIELDS ARE NO LONGER AMONG THEM (epic memql#4440), and that is
      // the one part of the paragraph above that has changed rather than been
      // abandoned. memql#3544's reasoning was about an answer the machine
      // ALREADY HAS and got wrong; it never contemplated the case where the
      // recorded answer is that there was never a key -- which is now the
      // ordinary case, because an install no longer asks for one. A repair
      // that demanded a key would then be demanding a value that never
      // existed, on a cluster that is working, and leaving the operator with
      // no way past a form. The fields are still COLLECTED (optionalFields)
      // and still PRE-FILLED from the receipt, so the editable-box remedy
      // memql#3544 built is intact; what is gone is the requirement.
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
  }
}

/**
 * What each action COLLECTS but never waits for (epic memql#4440).
 *
 * A separate list rather than a flag on the fields, because the two questions
 * a screen asks are genuinely different: `requiredFields` answers "may this
 * run start", and this answers "what else is worth offering while we are
 * here". Merging them into one annotated list is what would let a future edit
 * make an optional field block a run by touching one character.
 *
 * The collect screen renders these in a collapsed disclosure, below the
 * required ones. An empty value is not an error; a MALFORMED one still is --
 * `validate()` shape-checks these exactly as it does the required fields, so
 * the paste-the-key-into-the-path-box refusal (memql#3545) survives the
 * demotion. That is the trap in making a required field optional: the
 * validation that protected it usually hung off the requirement.
 */
export function optionalFields(action: AddClusterAction): InputField[] {
  switch (action) {
    case "install":
    case "installGuided":
    case "repair":
      return ["provider", "providerKeyFile"];
    case "uninstall":
    case "connect":
    case "reconnect":
      return [];
  }
}

const LABELS: Record<InputField, string> = {
  domain: "domain",
  ownerFirstName: "first name",
  ownerLastName: "last name",
  ownerEmail: "email address",
  provider: "AI provider",
  providerKeyFile: "provider key file",
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
  private progress: StepProgress[] = [];
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
    return this.progress.map((p) => ({ ...p }));
  }
  get failed(): StepProgress | undefined {
    const found = this.progress.find((p) => p.id === this.failedId);
    return found === undefined ? undefined : { ...found };
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
    return this.progress.filter((p) => p.state === "failed").map((p) => ({ ...p }));
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

  /**
   * Where to send an operator whose cluster was built without an AI provider
   * (epic memql#4440), or "" when there is nothing to say.
   *
   * OFFERED ONLY WHEN NOTHING WAS SEEDED. An install that supplied a key
   * configured its providers as part of the run, and a link inviting the
   * operator to go and configure them again would read as though the key had
   * not taken. The empty string means "render no line", which is what the
   * whole keyed path gets.
   *
   * A GETTER HERE, not an HTML string in the panel, for the same reason
   * `primaryHandoffAction` is (memql#3884): `addClusterPanel.ts` imports
   * `vscode`, so nothing under `node --test` can render it, and a decision
   * written into a template is a decision no test can reach.
   *
   * The address is composed from the domain the operator gave the installer,
   * by the same single-label-under-the-domain convention every other host
   * follows -- `portal.<domain>` is the platform's own site, front-door rule
   * #1. Requires a successful hand-off, because a run that did not register a
   * cluster has no domain worth linking into.
   */
  get providerSetupUrl(): string {
    const handoff = this.handoffResult;
    if (handoff === undefined || !handoff.ok) return "";
    if (this.values.providerKeyFile.trim() !== "") return "";
    const domain = handoff.cluster.domain?.trim() ?? "";
    if (domain === "") return "";
    return `https://portal.${domain}/settings/providers`;
  }

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
    this.revealedKey = "";
    this.recoveryState = "none";
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
   * TWO PASSES, because the two lists fail differently (epic memql#4440). A
   * required field that is empty is an error and its shape is checked when it
   * is not; an optional field that is empty is the ordinary case and only its
   * SHAPE is ever checked.
   *
   * The optional pass is not a nicety. Making `providerKeyFile` optional moved
   * it out of the only loop that ran `problemWith` over it -- so the
   * paste-the-key refusal (memql#3545), which exists because the value would
   * otherwise reach a command line every process on the machine can read and
   * then be written verbatim into the install receipt, would have silently
   * stopped running. `problemWith` already returns undefined for an empty
   * value, so the pass costs nothing when the disclosure was never opened.
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
    for (const field of optionalFields(action)) {
      const problem = this.problemWith(field, this.values[field]);
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
    // The refusal is HERE rather than at the script, which would report exit 2
    // -- correctly worded as a fault in MemQL rather than in the operator's
    // answer, and therefore the wrong sentence for a value they chose.
    if (field === "provider" && !SUPPORTED_PROVIDERS.some((p) => p === trimmed)) {
      return `MemQL can verify a key for ${SUPPORTED_PROVIDERS.join(" or ")}.`;
    }
    // THE KEY ITSELF, PASTED WHERE THE PATH GOES (memql#3545).
    //
    // The field's hint has always said "A PATH to a file holding the key, never
    // the key itself", and for a long time the hint was the whole enforcement.
    // What an operator who pasted the key actually got: the value handed to the
    // capability script as `--key-file=sk-ant-...`, where `ps` shows it to every
    // process on the machine, and then written verbatim into the install
    // receipt, where it stayed.
    //
    // The message must not quote the value back. It is a secret, it is rendered
    // into HTML, and a validation message is exactly the sort of thing that ends
    // up in a screenshot attached to a bug report.
    if (field === "providerKeyFile" && looksLikeProviderKey(trimmed)) {
      return (
        "That is the key itself. This field takes the PATH to a file holding it -- " +
        "a command line is readable by every process on this machine, so the key " +
        "must never be one. Save it to a file (e.g. ~/.memql/key) and give that path."
      );
    }
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
    // A NEW RUN STARTS CLOSED. The disclosure being open is a fact about the
    // failure the operator was just reading, not a preference that should
    // outlive it -- and carrying it forward would open a pane onto the previous
    // run's output while this one has produced none.
    this.logsShown = false;
    this.logsFollowTail = true;
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
      this.connectProbeStatus = { state: "passed", endpoint: targets.endpoint };
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
        // upsert, so a RE-RUN (Retry, or a repair) keeps what the previous
        // attempt established. The steps that already passed re-report as
        // skipped; showing them blank again in between would be a display of
        // this event rather than of the machine.
        for (const step of event.steps) this.upsert(step.id, step.description);
        return;
      }
      case "stepStarted": {
        const entry = this.upsert(event.step.id, event.step.description);
        entry.state = "running";
        return;
      }
      case "stepLog": {
        const entry = this.upsert(event.step.id, event.step.description);
        entry.log = entry.log === "" ? event.line : `${entry.log}\n${event.line}`;
        return;
      }
      case "stepFinished": {
        const entry = this.upsert(event.step.id, event.step.description);
        entry.state = STATUS_TO_STATE[event.outcome.status] ?? "done";
        entry.reason = event.outcome.reason ?? "";
        entry.exitCode = event.outcome.exitCode;
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

  private upsert(id: string, description: string): StepProgress {
    const existing = this.progress.find((p) => p.id === id);
    if (existing !== undefined) {
      if (description !== "" && existing.description === "") existing.description = description;
      return existing;
    }
    const fresh: StepProgress = {
      id,
      description,
      state: "pending",
      reason: "",
      exitCode: null,
      log: "",
      guided: false,
      remedy: "",
    };
    this.progress.push(fresh);
    return fresh;
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
    const failed = this.progress.filter((p) => p.state === "failed");
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
    const failed = this.progress.filter((p) => p.state === "failed");
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
    entry.state = "pending";
    entry.reason = "";
    entry.exitCode = null;
    entry.log = "";
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

const STATUS_TO_STATE: Record<string, StepState> = {
  ok: "done",
  failed: "failed",
  skipped: "skipped",
  preserved: "preserved",
};
