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
  dryRun: "Dry-run",
  tryInSession: "Try in session",
  promote: "Promote",
  demote: "Demote",
};

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
        headline: `${VERB[outcome.action]} failed: ${outcome.diagnostics.length} construct(s) did not compile. See Problems.`,
        body: [
          `${VERB[outcome.action]} of "${outcome.request.name}" stopped at validation. Nothing reached the cluster.`,
          "",
          ...outcome.diagnostics.map((d) => `  ${d.message}`),
        ].join("\n"),
      };

    case "error":
      return {
        severity: "error",
        headline:
          outcome.errorId === ""
            ? `${VERB[outcome.action]} failed: ${outcome.message}`
            : `${VERB[outcome.action]} failed (${outcome.errorId}): ${outcome.message}`,
        body: [
          `${VERB[outcome.action]} of "${outcome.request.name}" failed.`,
          "",
          outcome.message,
          ...(outcome.errorId === ""
            ? []
            : ["", `Engine error id: ${outcome.errorId} -- quote it to find the server-side log entry.`]),
        ].join("\n"),
      };

    case "breaking":
      // NOT an error, and rendered as the classification rather than as the
      // engine's prose. What the developer needs is the field, the rows and the
      // constructs that reference it -- a refusal IS a diff, and the diff is the
      // part they can act on.
      return {
        severity: "warning",
        headline: `Promote refused: a breaking schema change. Review the diff, then override deliberately if it is meant.`,
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
        headline: `Dry-run clean: ${outcome.result.constructs} construct(s) compile and bind against ${where}.`,
        body: [
          `Dry-run of "${outcome.request.name}" against ${where}.`,
          "",
          "Every construct in the bundle compiled and bound. NOTHING WAS CHANGED: a dry-run is a compile against a read-only clone of the registry, so the cluster is exactly as it was.",
        ].join("\n"),
      };

    case "tryInSession":
      return {
        severity: "info",
        headline: `Defined for this session only: ${outcome.result.defined.length} construct(s) on ${where}. Not promoted.`,
        body: [
          `"${outcome.request.name}" is callable by name on ${where}, on this connection and nowhere else.`,
          "",
          constructList(outcome.result.defined),
          "",
          "TEMPORARY. Nothing is persisted, no other caller can see it, and every definition is dropped when the connection drops or you switch cluster -- silently, because the engine does not announce it. Promote is what makes a construct outlive the session.",
        ].join("\n"),
      };

    case "promote":
      return {
        severity: outcome.result.overridden ? "warning" : "info",
        headline: outcome.result.overridden
          ? `Promoted ${outcome.result.promoted.length} construct(s) to ${where} WITH a breaking-change override.`
          : `Promoted ${outcome.result.promoted.length} construct(s) to ${where}.`,
        body: [
          `Promote of "${outcome.request.name}" to ${where} succeeded.`,
          "",
          constructList(outcome.result.promoted),
          "",
          "Persisted, registered into the shared registry, and broadcast -- every node serves them within seconds and a restart replays them.",
          ...(outcome.result.overridden
            ? [
                "",
                "A BREAKING SCHEMA CHANGE WAS OVERRIDDEN. The engine has audited it on v1:identity:auditEvent, naming the concept and the fields.",
              ]
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
    return `Demoted ${outcomes.length} construct(s) from ${where}. The name(s) are free again.`;
  }
  const rows = retired.reduce((total, o) => total + o.rowCount, 0);
  return `Demoted from ${where}: ${retired.length} retired (${rows} row(s) keep the name claimed), ${outcomes.length - retired.length} removed.`;
}
