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
 * What a surface asks of the actor's cluster role. Absent = every signed-in
 * user.
 *
 * TWO FORMS, AND THE LADDER IS STILL THE DEFAULT. `{ min }` is a floor on the
 * cluster's ladder, and it is the right shape for almost everything here:
 * this cluster's authority accumulates, so a surface an admin may use is one
 * an owner may use as well, and a floor keeps a rung added later from having
 * to be pasted into every list that already existed.
 *
 * `{ any }` is the escape from that monotonicity, and it exists because ONE
 * requirement genuinely is not monotonic. Integration credentials (P6 of the
 * email-campaigns program) are gated owner-or-developer and explicitly NOT
 * admin: wiring up what the cluster talks to is a developer's concern, while
 * an admin's is USER ADMINISTRATION. `{ min: "developer" }` is the nearest
 * the ladder can come and it admits admin -- so approximating it would show
 * an admin a section of forms the engine refuses one by one, which is a worse
 * answer than not offering the section.
 *
 * Reach for the set form only when a rung in the MIDDLE is deliberately left
 * out. A set that is really a contiguous top of the ladder is a `min` written
 * the long way, and it silently stops admitting whatever rung is added above
 * it.
 *
 * BOTH FORMS NAME SLUGS RATHER THAN A CLOSED UNION (epic memql#4832), because
 * the set of roles is cluster state now and a union would put the closed set
 * back one type further out. A slug that names no rung admits NOBODY -- see
 * roleAdmits -- and `rankLadder.test.tsx` fails the build if any shipped
 * manifest names one.
 */
export type RoleRequirement = { min: string } | { any: readonly string[] };

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

/**
 * The rung a role slug names, resolving aliases. Null when unknown.
 *
 * CASE-SENSITIVE, deliberately. Every slug in play is a lowercase value the
 * cluster wrote -- the catalog's `slug`, the user row's `role` enum -- so
 * folding case buys nothing real and costs the property `roles_any.test.ts`
 * pins by name: `"Owner"` must be unrankable. A role string that differs from
 * what the cluster reported is a string the cluster did not report, and
 * normalising it is how a typo quietly becomes a permission.
 */
export function roleRungOf(role: string): RoleRung | null {
  const slug = role.trim();
  if (!slug) return null;
  for (const rung of ladder) {
    if (rung.slug === slug) return rung;
  }
  for (const rung of ladder) {
    if (rung.aliases.some((a) => a.trim() === slug)) return rung;
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
 * a role we cannot rank must not unlock anything gated. That holds for both
 * forms -- and an EMPTY `any` set admits nobody at all, which is the
 * fail-closed reading of "these roles, and this is none of them".
 *
 * The same is true before the ladder loads, and true of a requirement naming
 * a role the cluster does not have: a floor that cannot be resolved is not a
 * floor that admits everyone, which is exactly the fail-OPEN the engine's own
 * rankFloorAdmits exists to avoid.
 */
export function roleAdmits(actorRole: string, requirement?: RoleRequirement): boolean {
  if (!requirement) return true;
  const actor = roleRank(actorRole);
  if (actor < 0) return false;
  if ("any" in requirement) {
    // Rank EQUALITY rather than slug equality, so a legacy slug on the actor
    // (`writer`) and the catalog slug in the set (`user`) name the same rung
    // and match -- the alias resolution both sides already go through.
    return requirement.any.some((role) => {
      const named = roleRank(role);
      return named >= 0 && named === actor;
    });
  }
  const floor = roleRank(requirement.min);
  if (floor < 0) return false;
  return actor >= floor;
}

/**
 * The floor a requirement states, for a surface that has to NAME it -- the
 * refused-surface panel is the one caller.
 *
 * The set form has no single floor, so it reports its weakest member: that is
 * the honest thing to show somebody who was refused, since it is the least
 * they could hold and still be admitted. Returns "" when there is nothing to
 * name.
 */
export function requirementFloor(requirement?: RoleRequirement): string {
  if (!requirement) return "";
  if ("any" in requirement) {
    const ranked = requirement.any
      .map((slug) => ({ slug, rank: roleRank(slug) }))
      .filter((r) => r.rank >= 0)
      .sort((a, b) => a.rank - b.rank);
    return ranked[0]?.slug ?? "";
  }
  return requirement.min;
}
