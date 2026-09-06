import { rowNumber, rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

// The automations catalog, as rows.
//
// ===========================================================================
// WHAT THIS SECTION IS ACTUALLY ABOUT
// ===========================================================================
// The product's claim is that the system works a goal out ONCE and afterwards
// replays it without a model unless reasoning is genuinely needed. THIS is the
// list of things it can replay -- `v1:authoring:construct` rows of kind
// `automation` that have been promoted into the owner's catalog. Every other
// surface in the app is about one piece of work; this one is about what the
// instance has learned.

export interface AutomationRow {
  id: string;
  name: string;
  targetNamespace: string;
  status: string;
  /** The compiled goal's catalog key. Empty on a construct authored directly. */
  goalSignature: string;
  /**
   * The trust ladder, 0..1. ABSENT reads as 0 and 0 is drawn as "not yet
   * proven" rather than as "0%" -- a template nobody has run has earned
   * nothing, which is a different sentence from one that has failed.
   */
  reliability: number;
  reinforceCount: number;
  lastReinforced: string;
  catalogedAt: string;
  catalogedFromBundleId: string;
  source: string;
  createdAt: string;
}

export function automationFromRow(row: Row): AutomationRow {
  return {
    id: rowString(row, "id"),
    name: rowString(row, "name"),
    targetNamespace: rowString(row, "targetNamespace"),
    status: rowString(row, "status"),
    goalSignature: rowString(row, "goalSignature"),
    reliability: rowNumber(row, "reliability"),
    reinforceCount: rowNumber(row, "reinforceCount"),
    lastReinforced: rowString(row, "lastReinforced"),
    catalogedAt: rowString(row, "catalogedAt"),
    catalogedFromBundleId: rowString(row, "catalogedFromBundleId"),
    source: rowString(row, "source"),
    createdAt: rowString(row, "createdAt"),
  };
}

export function isAutomation(row: Row): boolean {
  return rowString(row, "kind") === "automation";
}

/**
 * Where a template stands on the trust ladder.
 *
 * FIVE RUNGS AND A WORD FOR EACH, never a percentage. `reliability` is not a
 * probability of success -- it climbs when a run whose fingerprints matched
 * succeeds and decays on mismatch and on disuse -- so printing "62%" would
 * invite a reader to treat it as one. The rung a person cares about is
 * whether this thing can be trusted to run without being watched, and that is
 * a word.
 *
 * `unproven` and `poor` are DIFFERENT and the distinction is the whole point:
 * a template nobody has run has earned nothing, and one that has been run and
 * kept missing has earned less than nothing. Collapsing them would let a
 * brand-new automation and a broken one read alike.
 */
export type Rung = "unproven" | "poor" | "fair" | "good" | "proven";

export function rung(automation: AutomationRow): Rung {
  if (automation.reinforceCount === 0 && automation.reliability === 0) return "unproven";
  if (automation.reliability >= 0.9) return "proven";
  if (automation.reliability >= 0.7) return "good";
  if (automation.reliability >= 0.4) return "fair";
  return "poor";
}

export function rungWord(value: Rung): string {
  switch (value) {
    case "proven":
      return "Proven";
    case "good":
      return "Good";
    case "fair":
      return "Mixed";
    case "poor":
      return "Struggling";
    default:
      return "Not yet proven";
  }
}

export function rungMeaning(automation: AutomationRow): string {
  const runs = automation.reinforceCount;
  switch (rung(automation)) {
    case "proven":
      return `Replayed without a model and succeeded ${runs} ${runs === 1 ? "time" : "times"}.`;
    case "good":
      return `Mostly succeeds. ${runs} successful ${runs === 1 ? "run" : "runs"} so far.`;
    case "fair":
      return `Succeeds about as often as not. ${runs} successful ${runs === 1 ? "run" : "runs"}.`;
    case "poor":
      return "Has been run and has been missing. Worth reading before it is armed again.";
    default:
      return "Nothing has run this yet, so it has earned nothing either way.";
  }
}

export function statusWord(status: string): string {
  switch (status) {
    case "active":
      return "Armed";
    case "staged":
      return "Staged";
    case "retired":
      return "Retired";
    case "draft":
      return "Draft";
    default:
      return status === "" ? "Unknown" : status;
  }
}

export function statusMeaning(status: string): string {
  switch (status) {
    case "active":
      return "Registered in the shared runtime. A goal that matches it replays it without a model.";
    case "staged":
      return "Registered for you alone. It resolves for your own work and is not broadcast.";
    case "retired":
      return "Withdrawn. Still readable, so a run that used it can be explained, and never selected again.";
    case "draft":
      return "Authored and not yet activated.";
    default:
      return "";
  }
}

/**
 * The fingerprint the arrival cue reads.
 *
 * NO LIVENESS FIELD, and here that rule bites differently than elsewhere: this
 * section is a READ rather than a feed, so there is no cue at all -- but the
 * fingerprint is still what a re-read compares to decide whether anything
 * moved, and naming `lastReinforced` would report a change every time a run
 * touched the ladder without changing where the template stands.
 */
export function automationFingerprint(automation: AutomationRow): string {
  return [automation.name, automation.status, rung(automation), automation.reinforceCount].join("|");
}

export function automationMatches(automation: AutomationRow, search: string): boolean {
  const needle = search.trim().toLowerCase();
  if (needle === "") return true;
  return (
    automation.name.toLowerCase().includes(needle) ||
    automation.targetNamespace.toLowerCase().includes(needle) ||
    automation.status.toLowerCase().includes(needle)
  );
}
