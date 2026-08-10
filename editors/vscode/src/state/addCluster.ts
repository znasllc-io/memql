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
import type { AddClusterAction } from "../clusters/presence.js";
import type { ClustersFile } from "../clusters/model.js";
import { composeEndpointFromDomain, normalizeDomain, webSocketUrlFor } from "../connection/endpoint.js";

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
}

/** What an install asks for. Collected once, before anything runs. */
export interface Inputs {
  domain: string;
  ownerFirstName: string;
  ownerLastName: string;
  ownerEmail: string;
  /** A PATH. The key itself never enters this module -- see SessionOptions. */
  providerKeyFile: string;
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
 * `local.znas.io` is not invented here. It is the installer's OWN default:
 * `scripts/install/hosts-entries.sh` writes
 * `cockpit.local.znas.io,identity.local.znas.io,local.znas.io` when given no
 * `--hostnames`, and `verify-frontdoor.sh` checks the same two hosts. Picking
 * any other value here would make the form disagree with the scripts it is
 * about to run.
 *
 * THE OTHER FOUR ARE DELIBERATELY BLANK. A person's name, their email and
 * where they keep an API key are facts about them, and a wizard that guessed
 * would either be wrong or would prefill somebody else's details on a shared
 * machine. `seed-bootstrap.sh` agrees -- it defaults every owner field to the
 * empty string and exits 2 naming what is missing.
 */
export const DEFAULT_INPUTS: Inputs = {
  domain: "local.znas.io",
  ownerFirstName: "",
  ownerLastName: "",
  ownerEmail: "",
  providerKeyFile: "",
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
 *     `cockpit.<domain>`, which a hand-registered cluster need not be;
 *   - the token is the paste-a-credential path, which "memQL: Sign In" has
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
 * skips when satisfied -- so demanding the owner's name and a provider key
 * again would be asking the operator for what the machine can already see. The
 * domain stays because it is how the cluster is addressed, and a repair
 * pointed at the wrong one is not a repair.
 */
export function requiredFields(action: AddClusterAction): InputField[] {
  switch (action) {
    case "install":
    case "installGuided":
      return ["domain", "ownerFirstName", "ownerLastName", "ownerEmail", "providerKeyFile"];
    case "repair":
      return ["domain"];
    case "uninstall":
    case "connect":
      return [];
  }
}

const LABELS: Record<InputField, string> = {
  domain: "domain",
  ownerFirstName: "first name",
  ownerLastName: "last name",
  ownerEmail: "email address",
  providerKeyFile: "provider key file",
};

/** The screen each action needs first. */
function screenFor(action: AddClusterAction): Screen {
  switch (action) {
    case "uninstall":
      return "uninstallPreview";
    case "connect":
      return "connect";
    default:
      return "collect";
  }
}

export class AddClusterState {
  private currentScreen: Screen = "landing";
  private chosen: AddClusterAction | undefined;
  private guidedRun = false;
  private values: Inputs = { ...DEFAULT_INPUTS };
  private fieldErrors: FieldError[] = [];
  private progress: StepProgress[] = [];
  private failedId: string | undefined;
  private wasCancelled = false;
  private didSucceed = false;
  private connectValues: ConnectInputs = { ...EMPTY_CONNECT };
  private connectErrorList: ConnectFieldError[] = [];
  private connectFailureMessage = "";
  private registry: ClustersFile | undefined;

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
    this.values[field] = value;
    this.fieldErrors = this.fieldErrors.filter((e) => e.field !== field);
    const problem = this.problemWith(field, value);
    if (problem !== undefined) this.fieldErrors.push({ field, message: problem });
  }

  /** Every problem with what has been entered so far, for the action chosen. */
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
    return true;
  }

  // ---------------------------------------------------------------------------
  // registering an existing cluster (memql#3475)
  // ---------------------------------------------------------------------------

  /**
   * Hands over the registry the duplicate-name check reads.
   *
   * A SNAPSHOT, and knowingly one. clusters.yaml is shared with the memQL
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

    if (domain !== "" && /\s/.test(domain)) {
      errors.push({ field: "domain", message: "A domain cannot contain spaces." });
    } else if (domain.includes("://")) {
      errors.push({
        field: "domain",
        message: "A domain is a hostname, not a URL -- drop the scheme.",
      });
    }

    // The endpoint is DERIVABLE from the domain, so an empty box is only a
    // problem when nothing else names the front door. This is the same
    // `cockpit.<domain>:443` convention identityBaseUrlFor reads back off a
    // registered endpoint, called rather than copied.
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
          "That is a Personal Access Token, and the mesh cannot verify one: it checks bearers against the identity service's JWKS feed, so a PAT fails before any lookup. Paste the `access_token` from POST <identity>/oauth/token, or leave this empty and run \"memQL: Sign In\".",
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
        if (entry.state === "failed") {
          this.failedId = entry.id;
          this.currentScreen = "failedStep";
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
    };
    this.progress.push(fresh);
    return fresh;
  }

  // ---------------------------------------------------------------------------
  // recovery
  // ---------------------------------------------------------------------------

  /** Puts the failed step back to pending and returns to the run. */
  retry(): void {
    const entry = this.progress.find((p) => p.id === this.failedId);
    if (entry === undefined) return;
    entry.state = "pending";
    entry.reason = "";
    entry.exitCode = null;
    this.failedId = undefined;
    this.currentScreen = "running";
  }

  /**
   * Marks the failed step guided, and only that step.
   *
   * PER STEP, deliberately. An operator who would rather run the one command
   * that needs sudo by hand should not be dropped into a fully manual install
   * for the other eleven.
   */
  switchToGuided(): void {
    const entry = this.progress.find((p) => p.id === this.failedId);
    if (entry === undefined) return;
    entry.guided = true;
    entry.state = "pending";
    entry.reason = "";
    entry.exitCode = null;
    this.failedId = undefined;
    this.currentScreen = "running";
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
