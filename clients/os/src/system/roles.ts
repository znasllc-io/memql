// ONE role predicate for the whole shell (spec D8/E). The launcher grid,
// the dock, open-by-id, widget placement and section nav all call
// roleAdmits -- there is deliberately no second spelling of "can this actor
// see this". Presentation gating only: the engine's row admission stays the
// authority on every read; hiding an app here is UX, not a security
// boundary.

export const ROLE_LADDER = ["reader", "writer", "developer", "admin", "owner"] as const;
export type ClusterRole = (typeof ROLE_LADDER)[number];

/** Absent = every signed-in user. */
export interface RoleRequirement {
  min: ClusterRole;
}

export function roleRank(role: string): number {
  return ROLE_LADDER.indexOf(role as ClusterRole);
}

/**
 * An unknown or empty actor role admits only requirement-free surfaces:
 * a role we cannot rank must not unlock anything gated.
 */
export function roleAdmits(actorRole: string, requirement?: RoleRequirement): boolean {
  if (!requirement) return true;
  const actor = roleRank(actorRole);
  if (actor < 0) return false;
  return actor >= roleRank(requirement.min);
}
