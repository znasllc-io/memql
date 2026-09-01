// ONE role predicate for the whole shell (spec D8/E). The launcher grid,
// the dock, open-by-id, widget placement and section nav all call
// roleAdmits -- there is deliberately no second spelling of "can this actor
// see this". Presentation gating only: the engine's row admission stays the
// authority on every read; hiding an app here is UX, not a security
// boundary.

export const ROLE_LADDER = ["reader", "writer", "developer", "admin", "owner"] as const;
export type ClusterRole = (typeof ROLE_LADDER)[number];

/**
 * What a surface asks of the actor's cluster role. Absent = every signed-in
 * user.
 *
 * TWO FORMS, AND THE LADDER IS STILL THE DEFAULT. `{ min }` is a floor on
 * ROLE_LADDER, and it is the right shape for almost everything here: this
 * cluster's authority accumulates, so a surface an admin may use is one an
 * owner may use as well, and a floor keeps a rung added later from having to
 * be pasted into every list that already existed.
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
 */
export type RoleRequirement = { min: ClusterRole } | { any: readonly ClusterRole[] };

export function roleRank(role: string): number {
  return ROLE_LADDER.indexOf(role as ClusterRole);
}

/**
 * An unknown or empty actor role admits only requirement-free surfaces:
 * a role we cannot rank must not unlock anything gated. That holds for both
 * forms -- and an EMPTY `any` set admits nobody at all, which is the
 * fail-closed reading of "these roles, and this is none of them".
 */
export function roleAdmits(actorRole: string, requirement?: RoleRequirement): boolean {
  if (!requirement) return true;
  const actor = roleRank(actorRole);
  if (actor < 0) return false;
  if ("any" in requirement) return requirement.any.some((role) => roleRank(role) === actor);
  return actor >= roleRank(requirement.min);
}
