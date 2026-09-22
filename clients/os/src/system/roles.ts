// WHO MAY SEE WHAT, for the whole shell -- and since epic memql#5289 the
// answer is a CAPABILITY, not a rung.
//
// The launcher grid, the dock, open-by-id, widget placement and section nav
// all ask `holds("read", "app:<id>")` of the effective capability set the
// cluster reported for this person (`effectiveCapabilitiesForActor`), and
// there is deliberately no second spelling of "can this actor see this".
// `roleAdmits` below stays for the questions that are genuinely about the
// LADDER -- "is this the cluster owner", "may this rung be assigned" -- and
// for nothing that decides whether a surface is drawn. Presentation gating
// only, in both cases: the engine's row admission and its capability gates
// stay the authority on every read and write; hiding an app here is UX, not
// a security boundary.
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
    // RUNG identity, not rank equality. Both sides resolve through
    // roleRungOf, so a legacy slug on the actor (`writer`) and the catalog
    // slug in the set (`user`) name the same rung and match -- while two
    // DIFFERENT roles that happen to share a rank do not. A cluster may
    // define one: the ranks are spaced for exactly that, and "these roles"
    // has to mean these roles.
    const actorRung = roleRungOf(actorRole);
    if (actorRung === null) return false;
    return requirement.any.some((role) => roleRungOf(role)?.slug === actorRung.slug);
  }
  const floor = roleRank(requirement.min);
  if (floor < 0) return false;
  return actor >= floor;
}

/**
 * The slug to GRANT for a rung -- the spelling a `v1:identity:user.role` row
 * actually carries, which is not always the catalog's own slug.
 *
 * THE TWO VOCABULARIES ARE NOT THE SAME SET. The catalog seeds
 * owner/developer/admin/user/viewer; a user row carries
 * owner/admin/developer/writer/reader.
 *
 * THE ENGINE ACCEPTS EITHER NOW (epic memql#5166): `auth.IsValidRole` resolves
 * an active catalog slug OR one of its aliases, so `user` is no longer a write
 * the engine rejects. This function stays anyway, and the reason is
 * consistency rather than validity: every ordinary principal's row already
 * spells the member tier `writer`, and a picker that granted `user` would leave
 * two spellings of one rung in the user table -- both correct, both resolving
 * to the same rank, and neither matching the other in a list somebody scans.
 *
 * The alias is the bridge, and it is the FIRST one deliberately: a rung
 * carries its legacy user-row spelling there, and a custom role -- which has
 * no legacy spelling -- carries none and grants under its own slug.
 */
export function roleGrantSlug(rung: RoleRung): string {
  return rung.aliases[0] ?? rung.slug;
}

// ===========================================================================
// THE EFFECTIVE CAPABILITY SET (epic memql#5289, design D10 and D11)
// ===========================================================================
// One read replaces every hand-written floor. At sign-in the shell calls
// `effectiveCapabilitiesForActor()` and holds the answer HERE, beside the
// ladder, for the same reason the ladder lives here: the registry selectors
// and the shell's actions ask the question out of band, and threading a set
// through every one of their call sites would be a second spelling of "can
// this actor see this".
//
// WHAT IS HELD IS THE ENGINE'S ANSWER AND NOTHING DERIVED. The engine resolves
// the role catalog, the person's group grants and their own grants -- most
// specific wins, deny wins within a level -- and reports one decision per
// (verb, resource) pair with its provenance. This file never infers a
// capability from a role: a shell that did would disagree with the engine the
// first time somebody was granted an app their role lacks, or denied one it
// holds, which is the whole feature.
//
// FAIL-CLOSED UNTIL THE READ LANDS, as the ladder is. `holds` answers false
// for everything while the set is empty, so nothing gated renders before the
// cluster has said what this person may open. The `accessEpoch` is the
// reactivity signal: it increments on every install, and every memo that
// filters by capability names it in its deps (the memql#4857 lesson, which
// bit the ladder first).
//
// FRESHNESS IS BY RE-READ (D11): on sign-in, on window focus, and after any
// grant written from this browser (`notifyGrantWritten`). Grant rows are
// cluster-owner tier and never broadcast, so a grant somebody else writes for
// this person appears here on the next focus. The engine honours it on the
// next request regardless, so the lag is a hidden control appearing late --
// never one that works when it should not.

/** The five verbs, as the engine spells them. */
export type CapabilityVerb = "read" | "create" | "update" | "delete" | "execute";

/** Where an effective answer came from, as the engine reports it. */
export type CapabilitySource = "role" | "group" | "user";

/**
 * One decision out of the effective set: the engine's answer for one
 * (verb, resource) pair, and which level gave it.
 */
export interface EffectiveCapability {
  verb: string;
  resource: string;
  /** `allow` is held; `deny` is a bar the set keeps visible so it can be lifted. */
  effect: "allow" | "deny";
  source: CapabilitySource;
}

/** A target-specific decision, kept separate from the global permission view. */
export interface OrganizationCapability {
  accountId: string;
  verb: string;
  resource: string;
  effect: "allow" | "deny";
}

let organizationEffective: readonly OrganizationCapability[] = [];

let effective: Map<string, EffectiveCapability> = new Map();
let effectiveLoaded = false;
let epoch = 0;
const grantListeners = new Set<() => void>();

function keyOf(verb: string, resource: string): string {
  return `${verb.trim()}\u0000${resource.trim()}`;
}

/**
 * Install the set the cluster reported. Called from the session scope's read
 * of `effectiveCapabilitiesForActor`, and again on every re-read.
 *
 * LAST-WRITE-WINS, never merged: a grant revoked since the last read has to
 * disappear here, and merging would keep a capability nothing backs -- the
 * ladder's own reason for replacing rather than merging.
 */
export function setEffectiveCapabilities(entries: readonly EffectiveCapability[], organizationEntries: readonly OrganizationCapability[] = []): void {
  const next = new Map<string, EffectiveCapability>();
  for (const entry of entries) {
    if (entry.verb.trim() === "" || entry.resource.trim() === "") continue;
    next.set(keyOf(entry.verb, entry.resource), entry);
  }
  effective = next;
  organizationEffective = organizationEntries;
  effectiveLoaded = true;
  epoch += 1;
}

/**
 * Forget the set. The session scope calls this when the identity is cleared
 * -- a signed-out or reconnecting shell must not go on drawing a person's
 * apps from a set that belonged to a connection that is gone.
 */
export function clearEffectiveCapabilities(): void {
  effective = new Map();
  organizationEffective = [];
  effectiveLoaded = false;
  epoch += 1;
}

/** Whether the cluster read has landed. Every gated surface stays hidden until it has. */
export function effectiveAccessLoaded(): boolean {
  return effectiveLoaded;
}

/**
 * A counter that advances on every install or clear, so a memo keyed on it
 * recomputes the moment the set changes. Read it through `useOs().accessEpoch`
 * or `useSession().accessEpoch` rather than here: a component that reads the
 * bare counter during render is not re-rendered when it moves.
 */
export function effectiveAccessEpoch(): number {
  return epoch;
}

/**
 * THE ONE ACCESS PREDICATE: does the effective set hold `verb` on `resource`?
 *
 * Fail-closed on every silence -- an unloaded set, an unknown pair, a blank
 * argument -- and a `deny` entry is a held bar, not a held capability. Opening
 * an app is `holds("read", "app:<id>")`; a named part of one is
 * `holds("execute", "app:<id>/<part>")`.
 */
export function holds(verb: string, resource: string): boolean {
  const entry = effective.get(keyOf(verb, resource));
  return entry !== undefined && entry.effect === "allow";
}

/** Discovery only: one organization's allow never changes a global action. */
export function availableInAnyOrganization(verb: string, resource: string): boolean {
  return organizationEffective.some(entry => entry.verb === verb && entry.resource === resource && entry.effect === "allow");
}

/** An explicit organization is authoritative when the server reports scoped
 * decisions. Operators have no scoped entries and retain their global set. */
export function holdsForOrganization(accountId: string, verb: string, resource: string): boolean {
  if (organizationEffective.length === 0) return holds(verb, resource);
  return organizationEffective.some(entry => entry.accountId === accountId && entry.verb === verb && entry.resource === resource && entry.effect === "allow");
}

export function hasOrganizationDecisions(): boolean {
  return organizationEffective.length > 0;
}

/** The whole set, resource then verb, for a surface that RENDERS it (the permissions self-view). */
export function effectiveCapabilities(): EffectiveCapability[] {
  return [...effective.values()].sort(
    (a, b) => a.resource.localeCompare(b.resource) || a.verb.localeCompare(b.verb),
  );
}

/**
 * Tell the session scope a grant was written from this browser, so it re-reads
 * the set (D11). Called by the Access screen after `grantSet` / `grantRevoke`
 * -- the one place in the shell that writes one -- and by nothing that merely
 * reads. A grant written for SOMEBODY ELSE changes this person's set only if
 * they share a group, which the re-read answers correctly and a local edit
 * could not.
 */
export function notifyGrantWritten(): void {
  for (const listener of [...grantListeners]) listener();
}

/** Subscribe to grant writes made in this browser. Returns the unsubscribe. */
export function onGrantWritten(listener: () => void): () => void {
  grantListeners.add(listener);
  return () => {
    grantListeners.delete(listener);
  };
}

/**
 * What the shell asks of a resource name for somebody who has to READ the
 * answer -- the refused-window panel and the permissions self-view.
 *
 * "read on app:users" is the honest sentence now: what admits a person is a
 * capability their role, a group or a grant gave them, and the old
 * "admin or owner" would name a role that may be exactly wrong for a person
 * granted the app on their own name. The resource is printed as the engine
 * spells it, because that is the string an operator will look for in the
 * Access screen and in the seeds.
 */
export function describeResource(resource?: string): string {
  if (resource === undefined || resource.trim() === "") return "nothing beyond signing in";
  return `read on ${resource.trim()}`;
}
