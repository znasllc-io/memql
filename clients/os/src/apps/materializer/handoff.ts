// handoff.ts -- the one file in this app that names another one.
//
// A materialization IS a goal (design D6), so "go and look at the run
// that made this" is a real act on every composition. The app that draws
// a goal is not this one, and which app that is has changed once already
// during this epic -- the Work app is being subsumed by Nexus in the
// sibling sub-project.
//
// So the id lives HERE, as one constant, and nothing else in this app
// imports from that app's tree. Re-pointing it is a one-line change, and
// the alternative -- an import of `apps/nexus/` from `apps/materializer/`
// -- would make one app's section list a compile dependency of another's.

/** The app that draws a goal and its run. */
export const GOAL_APP_ID = "nexus";

/** The section of that app a goal opens in. */
export const GOAL_APP_SECTION = "goals";

/**
 * The intent payload key that app consumes for a goal.
 *
 * A BARE ID, per the identifiers contract: clients never compose, parse
 * or compare canonical ids, and the engine resolves a bare one inbound.
 */
export function goalIntent(goalId: string): Record<string, unknown> {
  return { goalId };
}
