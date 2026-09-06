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

/**
 * The app that deploys a package source, and the section its list is on.
 *
 * The SECOND hand-off, and it carries no payload on purpose. Deployables'
 * compose flow takes an artifact at its Source stop; seeding it from here
 * would mean this app writing into another app's flow, and that app spent
 * two epics making that rail the one place a deploy is composed. So this
 * opens the list and the Materializer's own copy says the zip is in the
 * Library and Source is where it goes -- a control that claimed to carry
 * the artifact across and then did not would be worse than one that says
 * plainly what it does.
 */
export const DEPLOY_APP_ID = "deployables";
export const DEPLOY_APP_SECTION = "deployables";
