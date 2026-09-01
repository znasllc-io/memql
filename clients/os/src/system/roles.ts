// ONE role predicate for the whole shell (spec D8/E). The launcher grid,
// the dock, open-by-id, widget placement and section nav all call
// roleAdmits -- there is deliberately no second spelling of "can this actor
// see this". Presentation gating only: the engine's row admission stays the
// authority on every read; hiding an app here is UX, not a security
// boundary.
//
// THE ORDERING IS CLUSTER STATE, NOT A LITERAL IN THIS FILE (epic
// memql#4832, D1).
//
// It used to be `["reader", "writer", "developer", "admin", "owner"]`, and
// that array disagreed with the engine: component/auth/rbac_model.go ranks
// developer 300 ABOVE admin 200, this file ranked admin above developer.
// Two hand-maintained ladders, and nothing noticed for the life of either.
//
// While the only consumer was a launcher the symptom was cosmetic -- a
// developer could not see an app the engine considered them MORE privileged
// for. Under rank-based row visibility it stops being cosmetic: the same
// request gets opposite answers depending on which side answers it.
//
// THE LITERAL IS DELETED RATHER THAN CORRECTED, and that is the decision.
// Reordering it would fix today's disagreement and leave the mechanism that
// produced it -- two lists, maintained by hand, in different languages.
// Keeping it as a FALLBACK would be worse still: a fallback preserves the
// divergence under another name and hides it behind a condition nobody
// exercises. The ladder is read from `activeRoles` and set once, and a
// build-time gate fails if a client ever ships an ordering of its own.
//
// WHAT THAT BUYS BEYOND THE FIX: customer-defined roles (D5). The ranks are
// spaced 50/100/200/300/400 precisely so a custom role can slot between two
// base ones, and a shell whose ladder is a five-item literal could never
// render one. This file now has no opinion about how many rungs exist.

/** One rung of the cluster's role ladder, as the engine defines it. */
export interface RoleRung {
  slug: string;
  name: string;
  /** HIGHER is more privileged. Spaced, so custom roles slot between. */
  rank: number;
  /**
   * Other slugs that resolve to this rung. The five values on a user row
   * (owner/admin/developer/writer/reader) are not the five the catalog seeds
   * (owner/developer/admin/user/viewer): `writer` is an alias of `user` and
   * `reader` of `viewer`. Carrying that as DATA is what lets this file hold
   * no translation table -- which is the other half of how the two ladders
   * drifted apart.
   */
  aliases: string[];
}

/**
 * A surface's role requirement. `min` is a role SLUG rather than a member of
 * a closed union, because the set of roles is now cluster state and a union
 * would put the closed set back one type further out. A slug that names no
 * rung admits NOBODY (see roleAdmits), and `rolesContract.test.ts` fails the
 * build if any shipped manifest names one.
 *
 * Absent = every signed-in user.
 */
export interface RoleRequirement {
  min: string;
}

/**
 * Kept as an alias so the many `ClusterRole` annotations across the shell
 * keep compiling. It is a plain string now: the closed union was the literal
 * ladder wearing a type.
 */
export type ClusterRole = string;

let ladder: RoleRung[] = [];

/**
 * Install the ladder the cluster reports. Called once, from the session
 * scope's read of `activeRoles`.
 *
 * Idempotent and last-write-wins: a re-read after a reconnect replaces the
 * rungs rather than merging, so a role DELETED in the cluster disappears
 * here too. Merging would leave a rung nothing backs, which is the shape of
 * bug this whole change exists to stop.
 */
export function setRoleLadder(rungs: RoleRung[]): void {
  ladder = [...rungs].sort((a, b) => a.rank - b.rank);
}

/**
 * The ladder, weakest rung first. Empty until the cluster read lands.
 *
 * Callers that render a role PICKER read this; callers that ask "may this
 * actor see that" call roleAdmits and never touch the list.
 */
export function roleLadder(): RoleRung[] {
  return ladder;
}

/** Whether the cluster read has landed. Every gated surface stays hidden until it has. */
export function roleLadderLoaded(): boolean {
  return ladder.length > 0;
}

/** The rung a role slug names, resolving aliases. Null when unknown. */
export function roleRungOf(role: string): RoleRung | null {
  const slug = role.trim().toLowerCase();
  if (!slug) return null;
  for (const rung of ladder) {
    if (rung.slug.toLowerCase() === slug) return rung;
  }
  for (const rung of ladder) {
    if (rung.aliases.some((a) => a.trim().toLowerCase() === slug)) return rung;
  }
  return null;
}

/**
 * A role's numeric rank, or -1 when it names no rung.
 *
 * -1 rather than 0, deliberately. 0 is a legal rank in a ladder whose ranks
 * are cluster data, so "unknown" and "the weakest rung" have to be different
 * values or an unrecognised role would silently clear a floor of 0.
 */
export function roleRank(role: string): number {
  return roleRungOf(role)?.rank ?? -1;
}

/**
 * An unknown or empty actor role admits only requirement-free surfaces:
 * a role we cannot rank must not unlock anything gated. The same is true
 * before the ladder loads, and true of a requirement naming a role the
 * cluster does not have -- a floor that cannot be resolved is not a floor
 * that admits everyone, which is exactly the fail-OPEN the engine's own
 * rankFloorAdmits exists to avoid.
 */
export function roleAdmits(actorRole: string, requirement?: RoleRequirement): boolean {
  if (!requirement) return true;
  const actor = roleRank(actorRole);
  if (actor < 0) return false;
  const floor = roleRank(requirement.min);
  if (floor < 0) return false;
  return actor >= floor;
}
