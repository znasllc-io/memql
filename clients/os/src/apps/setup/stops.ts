import { nextOpen, type StopState } from "../../kit/Rail";
import { stateWords } from "../../kit/ReadinessStates";
import type { Readiness } from "../../live/readiness";
import { MODULE_DESCRIPTIONS, MODULE_NAMES, READINESS_MODULES, type ModuleId } from "../../system/modules";
import type { Verdict } from "../../system/readinessFold";

// WHAT THE FIRST-RUN WIZARD KNOWS, as a function over four facts (design
// record 2026-09-06-first-run-wizard, D2 and D6).
//
// ===========================================================================
// PURE, BECAUSE THE CLAIMS ARE ABOUT THE READING AND NOT THE PICTURE
// ===========================================================================
// "The passkey comes first", "a module nothing reported is not a module that
// is done", "a loaded feed with nothing core in it is not a configured
// cluster" and "the widget retires only when every stop is settled" are
// statements about this file. Asserting them through a rendered widget would
// make each one an assertion about a rail as well, and the rail has its own
// tests.
//
// ===========================================================================
// THREE SILENCES, AND THEY ARE NOT THE SAME SILENCE
// ===========================================================================
//   NO STOPS AT ALL   the feed has not loaded, or it has and names no core
//                     module. Both mean "we cannot say", and the widget draws
//                     nothing. The second is the one that is easy to get
//                     wrong: the rows arrive after the subscription seeds, so
//                     for a moment a live feed holds nothing, and reading
//                     that gap as "the core is done" retires the wizard on
//                     the very boot that needed it.
//   A STOP IS UNKNOWN a read that has not landed. The widget draws nothing
//                     rather than a rail that will rearrange itself under
//                     somebody's cursor.
//   EVERY STOP DONE   there is nothing left to do, and the widget goes.
//
// Only the third retires the widget. `coreIsConfigured` is false for the
// other two, which is what keeps "we did not ask" from reading as "you are
// finished".

/** The one stop that is not a readiness module. */
export const PASSKEY_STOP = "passkey";

export type StopId = typeof PASSKEY_STOP | ModuleId;

/**
 * What is known about this person's passkeys.
 *
 * A FAILED READ IS `unknown`, NEVER `none` (D6). "We could not ask whether
 * you have a passkey" and "you have no passkey" lead to opposite acts, and
 * the second one told over the first sends somebody to register a credential
 * they already hold.
 */
export type PasskeyReading = "unknown" | "none" | "held";

export interface StopFacts {
  readiness: Readiness | undefined;
  passkeys: PasskeyReading;
  /** `MEMQL_IDENTITY_ENABLED`, as the runtime config carries it. */
  authEnabled: boolean;
}

export interface SetupStop {
  id: StopId;
  /** What the stop is called: the shell's own name for the module. */
  name: string;
  state: StopState;
  /** What this stop is for -- the manifest's own sentence, for a module. */
  sentence: string;
  /** The state in words, on the collapsed line. */
  answer: string;
}

const PASSKEY_LAW =
  "A sign-in link is the only way back into an account without one, so this comes first.";

/**
 * The stops, in the one order that is a law followed by the order the
 * manifest declares.
 *
 * THE PASSKEY IS FIRST AND EVERYTHING ELSE IS NOT ORDERED (D2). That is the
 * whole ordering claim this surface makes: a passkey before anything else,
 * because a sign-in link is the only way back into an account without one.
 * The modules that follow may be opened in any order and the rail says so by
 * having no Next.
 */
export function stopsFor(facts: StopFacts): SetupStop[] {
  const feed = facts.readiness;
  if (!feed || !feed.loaded) return [];

  const modules = READINESS_MODULES.filter((id) => feed.of(id)?.core === true);
  if (modules.length === 0) return [];

  return [passkeyStop(facts), ...modules.map((id) => moduleStop(id, feed.of(id)))];
}

function passkeyStop(facts: StopFacts): SetupStop {
  const base = { id: PASSKEY_STOP as StopId, name: "Your passkey", sentence: PASSKEY_LAW };
  // AUTH OFF IS SKIPPED, NOT DONE AND NOT WAITING. The rail draws a skipped
  // stop with its reason beside it, which is the honest picture: there is no
  // passkey to register on a cluster that is not checking who anybody is, and
  // saying so is more useful than leaving a gap where a stop was.
  if (!facts.authEnabled) {
    return {
      ...base,
      state: "skipped",
      answer: "Not applicable",
      sentence: "This cluster runs with authentication turned off, so there is no account to protect.",
    };
  }
  if (facts.passkeys === "unknown") return { ...base, state: "unknown", answer: "" };
  if (facts.passkeys === "held") return { ...base, state: "done", answer: "Set up" };
  return { ...base, state: "waiting", answer: "Not set up" };
}

function moduleStop(id: ModuleId, verdict: Verdict | null): SetupStop {
  const base = { id: id as StopId, name: MODULE_NAMES[id], sentence: MODULE_DESCRIPTIONS[id] };
  const state = verdict?.state ?? "unreported";
  if (state === "configured") return { ...base, state: "done", answer: "Set up" };
  if (state === "notApplicable") return { ...base, state: "skipped", answer: "Not applicable" };
  // THE KIT'S WORDS for the two states that have two causes each -- "Set up
  // on some nodes" or "Partly set up", "Could not check" or "Not reported" --
  // so the rail and the Set up group read one vocabulary (rule 7).
  if (state === "partial") return { ...base, state: "waiting", answer: stateWords(verdict) };
  // `unreported` STAYS WAITING rather than becoming unknown. A module whose
  // hosting node is down has told us nothing, and the wizard says exactly
  // that -- but it keeps drawing, because silencing the whole rail over one
  // quiet node would take the other three stops away from somebody who can
  // act on them. The same holds when every node answered and none could
  // finish the check: the rail says "Could not check" and keeps the stop's
  // act, because the check failing says nothing about whether the setup was
  // ever done.
  if (state === "unreported") return { ...base, state: "waiting", answer: stateWords(verdict) };
  return { ...base, state: "waiting", answer: "Not set up" };
}

/** Whether every stop has been read. Nothing is drawn before this. */
export function stopsAreKnown(stops: readonly SetupStop[]): boolean {
  return stops.length > 0 && stops.every((s) => s.state !== "unknown");
}

const SETTLED: readonly StopState[] = ["done", "complete", "skipped"];

/**
 * Whether there is nothing left to do -- the one condition that takes the
 * widget off the desk.
 *
 * FALSE FOR AN EMPTY RAIL, which is the guard that matters: `[]` means we
 * could not say, and an `every` over an empty array is true.
 */
export function coreIsConfigured(stops: readonly SetupStop[]): boolean {
  return stopsAreKnown(stops) && stops.every((s) => SETTLED.includes(s.state));
}

/**
 * The state a stop DRAWS: the one the rail has disclosed wears the open ring.
 *
 * The rail's `openStop` decides which body is shown; this decides which mark
 * says "this one is yours now". They are the same stop, and keeping the
 * promotion here rather than inside `stopsFor` is what lets a person open a
 * later stop and have the ring follow them.
 */
export function drawnState(stop: SetupStop, openStop: string): StopState {
  return stop.id === openStop && stop.state === "waiting" ? "open" : stop.state;
}

/**
 * Which stop the rail has open.
 *
 * THREE ANSWERS, AND THE MIDDLE ONE IS WHY THIS IS NOT ONE LINE:
 *
 *   null   nobody has chosen, so the law decides -- the first stop still
 *          outstanding, and it MOVES as stops settle. That is what makes
 *          finishing one advance the rail with no Next button anywhere.
 *   ""     somebody closed the open stop. Honoured: a person who collapsed
 *          the rail did not ask for it to spring back open on the next
 *          reading, and the marks still carry every state change.
 *   an id  somebody opened that stop. Honoured while it is still
 *          outstanding, and then the law takes over again -- so setting up
 *          the stop you chose hands you the next one rather than leaving
 *          you on a finished disclosure.
 */
export function openStopFor(stops: readonly SetupStop[], override: string | null): string {
  if (override === null) return nextOpen(stops);
  if (override === "") return "";
  const held = stops.find((s) => s.id === override);
  return held !== undefined && held.state === "waiting" ? override : nextOpen(stops);
}
