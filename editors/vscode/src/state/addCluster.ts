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
// Refs: #3470 #3469 #3463

import type { ExecEvent } from "../install/executor.js";
import type { AddClusterAction } from "../clusters/presence.js";

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

const EMPTY_INPUTS: Inputs = {
  domain: "",
  ownerFirstName: "",
  ownerLastName: "",
  ownerEmail: "",
  providerKeyFile: "",
};

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
  private values: Inputs = { ...EMPTY_INPUTS };
  private fieldErrors: FieldError[] = [];
  private progress: StepProgress[] = [];
  private failedId: string | undefined;
  private wasCancelled = false;
  private didSucceed = false;

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
  }

  back(): void {
    this.chosen = undefined;
    this.guidedRun = false;
    this.currentScreen = "landing";
    this.fieldErrors = [];
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
