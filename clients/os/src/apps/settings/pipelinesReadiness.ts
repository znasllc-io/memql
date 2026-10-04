import { stateWords } from "../../kit/ReadinessStates";
import type { Verdict } from "../../system/readinessFold";

// Settings -> Pipelines, READ (epic memql#5479, design record D15).
//
// ===========================================================================
// THREE FACTS, AND THE ENGINE SAYS WHICH OF THEM HOLD
// ===========================================================================
// The pipelines readiness item has three sub-steps: the cluster's GitHub App,
// a repository whose pipeline is connected, and compute to run the steps on.
// The agent nodes evaluate them as the pipelines integration report's three
// settings, and every row carries that report as ONE lane named `report`, a
// slot per setting (component/memql/readiness/integration.go,
// IntegrationLane). The fold passes the worst live reporter's lanes through,
// so a slot here is what the least set-up agent said.
//
// ===========================================================================
// NO LANE IS NOT "NOT DONE"
// ===========================================================================
// A row written by an engine older than the lane, or a verdict nobody has
// reported, says nothing about WHICH facts hold. A configured verdict still
// means all three do -- the report is configured only when every setting is
// -- but any other verdict without the lane leaves each sub-step not known.
// Inventing a "not done" there would send an owner to set up a GitHub App
// their cluster already has.
//
// PURE, beside the section that draws it, for the reason the setup wizard's
// stops are: the claims are about the reading, not the picture.

/** The lane an integration-evaluated module's row carries. Wire contract:
 *  component/memql/readiness/integration.go, IntegrationLaneName. */
export const REPORT_LANE = "report";

export type SubStepId = "githubApp" | "repository" | "compute";

/** Done, not done, or not known: three answers, never two. */
export type SubStepReading = "done" | "notDone" | "notKnown";

export interface SubStep {
  id: SubStepId;
  /** What a person calls it. */
  name: string;
  /** The integration report's setting this sub-step reads. */
  slot: "githubApp" | "repository" | "runner";
  reading: SubStepReading;
}

/** The item as a whole: the four words its bar can say. */
export type ItemState = "setUp" | "partlySetUp" | "notSetUp" | "notReported";

export interface PipelinesReading {
  githubApp: SubStep;
  repository: SubStep;
  compute: SubStep;
  /** The three, in the order they are set up. */
  steps: SubStep[];
  state: ItemState;
  /** The state in words, for the bar. */
  words: string;
  /** How many sub-steps are done, or null while any one is not known. */
  done: number | null;
  /** "2 of 3 done", or "" while any sub-step is not known. */
  detail: string;
}

/** What each sub-step is called, and which of the report's settings it reads. */
const STEPS: Record<SubStepId, Omit<SubStep, "reading">> = {
  githubApp: { id: "githubApp", name: "GitHub App", slot: "githubApp" },
  repository: { id: "repository", name: "A repository", slot: "repository" },
  compute: { id: "compute", name: "Compute", slot: "runner" },
};

const WORDS: Record<Exclude<ItemState, "notReported">, string> = {
  setUp: "Set up",
  partlySetUp: "Partly set up",
  notSetUp: "Not set up",
};

/** The pipelines verdict, read as its three sub-steps and the item's state. */
export function readPipelines(verdict: Verdict | null): PipelinesReading {
  const lane = verdict?.lanes.find((l) => l.name === REPORT_LANE) ?? null;
  const read = (id: SubStepId): SubStep => ({ ...STEPS[id], reading: subStepReading(verdict, lane, STEPS[id].slot) });
  // In the order the item is set up: an app, then a repository it reaches,
  // then somewhere for that repository's steps to run.
  const githubApp = read("githubApp");
  const repository = read("repository");
  const compute = read("compute");
  const steps = [githubApp, repository, compute];
  const known = steps.every((s) => s.reading !== "notKnown");
  const done = steps.filter((s) => s.reading === "done").length;
  const state = itemState(verdict, known, done, steps.length);
  return {
    githubApp,
    repository,
    compute,
    steps,
    state,
    // THE KIT'S WORD for a verdict nobody voted on, so "Not reported" and
    // "Could not check" read here exactly as they do in every Set up group.
    words: state === "notReported" ? stateWords(verdict) : WORDS[state],
    done: known ? done : null,
    detail: known ? `${done} of ${steps.length} done` : "",
  };
}

function subStepReading(
  verdict: Verdict | null,
  lane: Verdict["lanes"][number] | null,
  slot: SubStep["slot"],
): SubStepReading {
  if (verdict === null || verdict.state === "unreported") return "notKnown";
  const found = lane?.slots.find((s) => s.name === slot);
  if (found !== undefined) return found.present ? "done" : "notDone";
  // No lane, or a lane from a report that names other settings: the verdict
  // can say "all of them" and nothing finer.
  return verdict.state === "configured" ? "done" : "notKnown";
}

function itemState(verdict: Verdict | null, known: boolean, done: number, of: number): ItemState {
  if (known) return done === of ? "setUp" : done === 0 ? "notSetUp" : "partlySetUp";
  if (verdict === null || verdict.state === "unreported") return "notReported";
  // Reported, but not which sub-steps hold: the verdict's own state is the
  // whole of what is known.
  if (verdict.state === "configured") return "setUp";
  return verdict.state === "partial" ? "partlySetUp" : "notSetUp";
}

// ---------------------------------------------------------------------------
// The fleet line
// ---------------------------------------------------------------------------

/** The fleet line under a cluster that can run steps. */
export function fleetSentence(count: number): string {
  if (count === 0) {
    return "None of your machines allows pipelines yet, so a step that needs one cannot run.";
  }
  return count === 1 ? "1 of your machines allows pipelines." : `${count} of your machines allow pipelines.`;
}

// ---------------------------------------------------------------------------
// Installations whose permissions lag the app's
// ---------------------------------------------------------------------------

/**
 * A missing permission in words: "checks:write" -> "write checks",
 * "merge_queues:read" -> "read merge queues". GitHub's own names, read aloud;
 * a value with no level keeps its name alone rather than a guessed verb.
 */
export function permissionWords(permission: string): string {
  const [name = "", level = ""] = permission.split(":");
  const subject = name.trim().replaceAll("_", " ");
  return level.trim() === "" ? subject : `${level.trim()} ${subject}`;
}

/** Every missing permission, in words, in the order GitHub gave them. */
export function missingPermissionsWords(permissions: readonly string[]): string {
  return permissions.filter((p) => p.trim() !== "").map(permissionWords).join(", ");
}
