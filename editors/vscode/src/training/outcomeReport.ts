// Turning an outcome into what the developer reads.
//
// Split from report.ts so nothing in the module graph points both ways: report.ts
// supplies the words the ACTIONS ask with, actions.ts produces the outcomes, and
// this reads both. The two halves are otherwise the same job and the same rules.
//
// TWO SURFACES, AND THE SPLIT IS THE POINT. A `headline` goes to a toast, which
// is a line somebody reads in passing; a `body` goes to the output channel,
// which is where the structure lives -- the per-change diff, the row counts, the
// per-construct outcomes. Squeezing a classified schema diff into a toast would
// lose exactly the fields that make it actionable, and putting the headline only
// in a channel nobody has open would lose the fact that anything happened.
//
// A REFUSAL IS RENDERED, NOT SWALLOWED. The engine's owner-only message, its
// core-shadow message and its breaking-change classification each arrive as
// their own outcome shape and each get their own rendering. Nothing here
// paraphrases an engine refusal: the editor explains what kind of thing happened
// and then hands over the engine's own words.
//
// Refs: #3763 #3745

import type { DemoteOutcome } from "@znasllc-io/memql-sdk-core/authoring";
import { DemoteOutcomeRetired } from "@znasllc-io/memql-sdk-core/authoring";

import type { TrainingActionKind, TrainingOutcome } from "./actions.js";
import {
  conceptDiffReport,
  constructList,
  demoteOutcomeReport,
} from "./report.js";

export interface TrainingReport {
  severity: "info" | "warning" | "error";
  /** One line, for a toast. */
  headline: string;
  /** The full record, for the output channel. Never empty. */
  body: string;
}

/** The verb each action reports in, so a headline reads as a sentence. */
const VERB: Record<TrainingActionKind, string> = {
  dryRun: "Dry run",
  tryInSession: "Try in session",
  stage: "Stage",
  promote: "Promote",
  demote: "Demote",
};

/** "3 constructs": a count and its noun, without "construct(s)". */
function constructs(n: number): string {
  return `${n} construct${n === 1 ? "" : "s"}`;
}

/**
 * outcomeReport renders an outcome.
 *
 * Returns undefined for `superseded` and `declined` -- the two outcomes with
 * nothing to say. A superseded action was overtaken by a newer one, whose report
 * is the one that should be on screen; a declined one is the developer's own
 * answer being honoured, and telling someone their Cancel worked is noise.
 */
export function outcomeReport(outcome: TrainingOutcome): TrainingReport | undefined {
  switch (outcome.status) {
    case "superseded":
    case "declined":
      return undefined;

    case "invalid":
      return {
        severity: "error",
        headline: `MemQL: ${VERB[outcome.action]} stopped: ${constructs(outcome.diagnostics.length)} didn't compile.`,
        body: [
          `${VERB[outcome.action]} of "${outcome.request.name}" stopped at validation. Nothing reached the cluster.`,
          "",
          ...outcome.diagnostics.map((d) => `  ${d.message}`),
        ].join("\n"),
      };

    case "error":
      return {
        severity: "error",
        // The id is lifted out of the message (training/actions.ts), so the
        // message already carries it; naming it again would say it twice.
        headline:
          outcome.errorId === "" || outcome.message.includes(outcome.errorId)
            ? `MemQL: ${VERB[outcome.action]} failed: ${outcome.message}`
            : `MemQL: ${VERB[outcome.action]} failed (${outcome.errorId}): ${outcome.message}`,
        body: [
          `${VERB[outcome.action]} of "${outcome.request.name}" failed.`,
          "",
          outcome.message,
          ...(outcome.errorId === ""
            ? []
            : ["", `Error ID: ${outcome.errorId}. Quote it to find the cluster's log entry.`]),
        ].join("\n"),
      };

    case "breaking":
      // NOT an error, and rendered as the classification rather than as the
      // engine's prose. What the developer needs is the field, the rows and the
      // constructs that reference it -- a refusal IS a diff, and the diff is the
      // part they can act on.
      return {
        severity: "warning",
        headline: "MemQL: Promote blocked by a breaking schema change. Override only if you mean it.",
        body: [
          `Promote of "${outcome.request.name}" to ${outcome.cluster.label} was refused. Nothing was promoted.`,
          "",
          conceptDiffReport(outcome.diffs),
          ...(outcome.error === "" ? [] : ["", "The engine's refusal:", `  ${outcome.error}`]),
        ].join("\n"),
      };

    case "ok":
      return okReport(outcome);
  }
}

function okReport(
  outcome: Extract<TrainingOutcome, { status: "ok" }>,
): TrainingReport {
  const where = outcome.cluster.label;
  switch (outcome.result.kind) {
    case "dryRun":
      return {
        severity: "info",
        headline: `MemQL: Dry run passed on ${where}: ${constructs(outcome.result.constructs)}, nothing changed.`,
        body: [
          `Dry run of "${outcome.request.name}" on ${where}.`,
          "",
          "Everything compiled and bound. Nothing was changed: a dry run compiles against a read-only copy of the cluster's registry.",
        ].join("\n"),
      };

    case "tryInSession":
      return {
        severity: "info",
        headline: `MemQL: "${outcome.request.name}" is live on ${where} for this session only.`,
        body: [
          `"${outcome.request.name}" is callable by name on ${where}, on this connection and nowhere else.`,
          "",
          constructList(outcome.result.defined),
          "",
          "Temporary: nothing is saved, nobody else can call it, and it is dropped without notice when the connection drops or you switch cluster. Promote keeps it.",
        ].join("\n"),
      };

    case "stage":
      return {
        severity: "info",
        headline: `MemQL: Staged "${outcome.request.name}" on ${where}. Only you can call it.`,
        body: [
          `"${outcome.request.name}" is durable on ${where} and callable by you and by nobody else.`,
          "",
          constructList(outcome.result.staged),
          "",
          // The sentence that distinguishes this from Try in session -- and the
          // one the whole tier exists to be able to say. Try in session's report
          // ends by naming Promote as what outlives the connection; this is the
          // other thing that does, without making the construct everyone's.
          "Saved, and replayed when the cluster restarts: unlike Try in session, it survives the connection. Nobody else can call it. Promote makes it live for everyone.",
        ].join("\n"),
      };

    case "promote":
      return {
        severity: outcome.result.overridden ? "warning" : "info",
        headline: outcome.result.overridden
          ? `MemQL: Promoted "${outcome.request.name}" to ${where}, with a breaking-change override.`
          : `MemQL: Promoted "${outcome.request.name}" to ${where}.`,
        body: [
          `Promote of "${outcome.request.name}" to ${where} succeeded.`,
          "",
          constructList(outcome.result.promoted),
          "",
          "Saved and live for everyone: every node serves them within seconds, and a restart replays them.",
          ...(outcome.result.overridden
            ? ["", "A breaking schema change was overridden. The cluster audited it, naming the concept and the fields."]
            : []),
          ...(outcome.result.conceptDiffs.length === 0
            ? []
            : ["", "Concept schema changes:", conceptDiffReport(outcome.result.conceptDiffs)]),
        ].join("\n"),
      };

    case "demote":
      return {
        severity: "info",
        // The headline says RETIRED vs REMOVED rather than "demoted", because
        // both are success and which one happened is the next thing the caller
        // needs to know: whether the name is claimable again.
        headline: demoteHeadline(outcome.result.outcomes, where),
        body: [
          `Demote of "${outcome.request.name}" from ${where} succeeded.`,
          "",
          demoteOutcomeReport(outcome.result.outcomes),
        ].join("\n"),
      };
  }
}

function demoteHeadline(outcomes: readonly DemoteOutcome[], where: string): string {
  // Compared against the SDK's constant, not a bare "retired". The two values
  // are the engine's vocabulary and the SDK is where this consumer gets its copy
  // of them; a literal here would be a third spelling nothing keeps in step.
  const retired = outcomes.filter((o) => o.outcome === DemoteOutcomeRetired);
  if (retired.length === 0) {
    return `MemQL: Demoted ${constructs(outcomes.length)} from ${where}. ${outcomes.length === 1 ? "The name is" : "The names are"} free again.`;
  }
  const rows = retired.reduce((total, o) => total + o.rowCount, 0);
  return `MemQL: Demoted from ${where}: ${retired.length} retired (${rows} row${rows === 1 ? "" : "s"} keep the name taken), ${outcomes.length - retired.length} removed.`;
}
