import { roleHolds, type GrantRow, type RoleRow } from "./rows";

// WHICH RUNGS THIS CALLER IS OFFERED FOR THIS PERSON.
//
// ===========================================================================
// A MIRROR OF auth.MayAssignRole, AND ONLY A MIRROR
// ===========================================================================
// component/auth/rbac_assignment.go is the authority: both seams that assign a
// role -- SetUserRole and invitation issue -- run it against the role the
// stream interceptor verified, and nothing decided in this browser changes its
// answer. What this decides is which rungs are OFFERED, which is presentation
// (design record, section E) and matters for one reason: a ladder that offers
// every rung teaches an operator that they may hand out any of them, and they
// find out otherwise by being refused.
//
// So the unoffered rungs are DRAWN, dashed, with the sentence -- rather than
// dropped. A rung that vanished would leave somebody comparing two people's
// pages and seeing two different ladders with nothing to explain the
// difference.
//
// THE RULES, in the Go's own order, and each one is here because leaving it
// out would offer something the server refuses:
//
//  1. the role must be one this cluster can assign: active, and known.
//  2. the caller must hold the people-authority grant at all -- `update` on
//     `principal` to re-role somebody, `create` on `admission` or `principal`
//     to name a role on an INVITATION. That split is memql#4917's: a role
//     may invite people without re-roling them (the developer role did until
//     2026-11-25, when it was given both). Which one is asked is decided
//     by `kind`, NOT by whether `targetRole` is empty -- see AssignKind.
//  3. the caller must govern the person as they stand today (strictly above
//     them, or an owner).
//  4. the new rung must sit strictly below the caller's own -- unless the
//     caller is an owner, the carve-out that makes a second owner possible.
//  5. RE-ROLING ONLY (memql#5236): the new role must hold no `principal` verb
//     the caller does not, whatever the ranks say. A re-role hands authority
//     over immediately; an invitation opens a door the recipient must still
//     walk through, and the inviter holds no verb on the principal that
//     results. So a role holding admission alone MAY invite an admin and may
//     not re-role anybody into one. The escalation that buys -- inviting an address you control --
//     is an accepted, recorded trade-off; see auth.MayAssignRole. DO NOT
//     "restore" this to the invitation kind as a fix: that is the bug this
//     rule caused, and it disables the rung the engine would accept.
//  6. an account-scoped role is offered only for somebody already in that
//     account's group. An unanswerable membership question is a NO, exactly as
//     a nil `targetIsMember` refuses server-side.

/** Why a rung is not offered, or "" when it is. */
export type RungRefusal =
  | ""
  | "inactive"
  | "notAUserManager"
  | "targetOutranks"
  | "aboveCaller"
  | "authorityBeyond"
  | "notAMember";

/**
 * WHICH SEAM is assigning, mirroring `auth.AssignKind`.
 *
 * The Go read this off `targetCurrentSlug == ""` and that is not what the test
 * means: it means THE TARGET HAS NO RUNG, which is a different fact from "this
 * is an invitation". They come apart here too -- PersonPage passes
 * `person?.role ?? ""`, so a person whose role is blank would take the
 * invitation branch on the RE-ROLE page. Harmless while the test only chose
 * which capability to require; not harmless once it also decides whether rule 5
 * runs. Each surface says which it is.
 */
export type AssignKind = "invitation" | "reRole";

export interface AssignContext {
  /** Which seam is asking. Decides rules 2 and 5. */
  kind: AssignKind;
  /** The caller's own role slug. */
  callerRole: string;
  /** The caller's rank, resolved through the ladder. */
  callerRank: number;
  /** True when the caller is on the owner rung. */
  callerIsOwner: boolean;
  /** The whole grant catalog. */
  grants: readonly GrantRow[];
  /** The rung the person holds today. "" for an invitation: nobody yet. */
  targetRole: string;
  targetRank: number;
  targetIsOwner: boolean;
  /** The accounts the person is a member of, through their groups. */
  targetAccountIds: readonly string[];
}

/** The `principal` verbs, which are the ones rule 5 compares. */
const PRINCIPAL_VERBS = ["read", "create", "update", "delete"] as const;

export function rungRefusal(rung: RoleRow, ctx: AssignContext): RungRefusal {
  if (!rung.active) return "inactive";

  // Rule 2. An invitation names a role on a person who does not exist yet, so
  // it asks for the CREATE grant; a re-role asks for update-on-principal.
  const managing =
    ctx.kind === "invitation"
      ? roleHolds(ctx.grants, ctx.callerRole, "create", "admission") ||
        roleHolds(ctx.grants, ctx.callerRole, "create", "principal")
      : roleHolds(ctx.grants, ctx.callerRole, "update", "principal");
  if (!managing) return "notAUserManager";

  // Rule 3. An owner governs everyone; otherwise the caller must rank strictly
  // above the person as they stand, and nobody but an owner governs an owner.
  if (!ctx.callerIsOwner && (ctx.targetIsOwner || ctx.targetRank >= ctx.callerRank)) {
    return "targetOutranks";
  }

  // Rule 4, with the owner carve-out: two owners share a rank, so a strict
  // comparison cannot express owner -> owner and a cluster could never name a
  // second owner or hand itself on.
  if (!ctx.callerIsOwner && rung.rank >= ctx.callerRank) return "aboveCaller";

  // Rule 5 -- RE-ROLING ONLY. See the rule list above for why, and for why
  // putting it back on the invitation kind is the bug rather than the fix.
  for (const verb of ctx.kind === "reRole" ? PRINCIPAL_VERBS : []) {
    if (
      roleHolds(ctx.grants, rung.slug, verb, "principal") &&
      !roleHolds(ctx.grants, ctx.callerRole, verb, "principal")
    ) {
      return "authorityBeyond";
    }
  }

  // Rule 6.
  if (rung.accountId !== "" && !ctx.targetAccountIds.includes(rung.accountId)) {
    return "notAMember";
  }

  return "";
}

/** Whether this caller may put this rung on this person. */
export function mayAssign(rung: RoleRow, ctx: AssignContext): boolean {
  return rungRefusal(rung, ctx) === "";
}

/**
 * The sentence beneath a ladder whose rungs are not all offered.
 *
 * ONE sentence for the whole ladder rather than one per dashed rung: the
 * common case by far is the rank bound, and a reason repeated against five
 * rungs is chrome. The per-rung reason rides as the rung's `title`, where
 * somebody asking about one specific rung will look.
 */
export function ladderSentence(refusals: readonly RungRefusal[]): string {
  const set = new Set(refusals.filter((r) => r !== ""));
  if (set.size === 0) return "";
  if (set.has("notAUserManager")) {
    return "Your role does not carry changing what somebody is. An owner or an admin does that.";
  }
  if (set.has("targetOutranks")) {
    return "This person ranks at or above you, so their role is not yours to change.";
  }
  if (set.size === 1 && set.has("notAMember")) {
    return "A role scoped to a client is offered once the person is in that client's group.";
  }
  return "Only rungs below your own are offered.";
}

/** The per-rung reason, for the rung's own title. */
export function rungSentence(refusal: RungRefusal): string {
  switch (refusal) {
    case "inactive":
      return "This role is retired. People who hold it keep it; nobody new gets it.";
    case "notAUserManager":
      return "Your role does not carry changing what somebody is.";
    case "targetOutranks":
      return "This person ranks at or above you.";
    case "aboveCaller":
      return "This rung is at or above your own.";
    case "authorityBeyond":
      return "This role manages people in ways your own does not, so you cannot hand it on.";
    case "notAMember":
      return "This role is scoped to a client. Add the person to that client's group first.";
    default:
      return "";
  }
}
