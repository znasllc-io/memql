package auth

// RBAC relational governance (Epic 1, E1.3 -- memql#2071).
//
// Governance answers "who can manage whom" over a (actor, target, verb)
// triple, using the rank model E1.2 (memql#2070) authored in the DSL
// (v1:rbac:role.rank, where HIGHER == more privileged). It is the rank-based
// successor to the legacy CanManageUser / CanViewUser / CanDeleteUser helpers
// in rbac.go; E1.6 (memql#2074) wires enforcement through GovernPrincipal so
// the scattered Go role-constant checks resolve through ONE relational
// predicate.
//
// The governed resource is the PRINCIPAL (a user/identity). Applying a verb to
// a principal IS user management -- there is no separate "manage" verb (the
// uniform-verb decision from E1.1). The locked rules:
//
//   - read   on a principal: a capability-level grant (does the actor's role
//     hold read-on-principal?), not a relational one. GovernPrincipal returns
//     true for read so the relational layer never narrows a view the
//     capability layer already granted; per-row visibility is handled by the
//     capability + the legacy CanViewUser tiering until E1.5 migrates it.
//   - update / delete on a principal: actor.rank > target.rank OR
//     actor == target (self-edit). This is the core relational rule.
//   - owner-principal (target holds the owner rank): editable ONLY by an
//     owner. An owner manages everyone, but a non-owner -- however high-ranked
//     -- can never edit an owner. (Mirrors the legacy "admin sees everyone
//     except owners" + "owner can't be managed by non-owner" carve-outs.)
//   - owner self-demotion lockout: an owner cannot delete/demote THEMSELVES
//     via this path (prevents the last owner locking the cluster out). Mirrors
//     CanDeleteUser's owner-self guard.
//   - create on a principal is NOT relational -- it is a pure capability grant
//     (does the role hold create-on-principal?). This is the create != update
//     split from E1.1: a role can hold create-on-principal of a rank WITHOUT
//     holding update, i.e. "mint users but not edit existing ones".
//     GovernPrincipal therefore does not gate create; CanCreatePrincipal is
//     the capability-only companion the enforcement layer consults.
//
// All inputs are plain values (ranks + ids + an owner flag) so the core is
// pure and DB-free -- the caller resolves ranks from the role catalog
// (roleBySlug) before calling. This keeps the security-critical arithmetic in
// one unit-tested place.

// Principal is the minimal projection of an actor or target the governance
// core needs: who they are (UserId), how privileged (Rank, higher == more),
// and whether they hold the owner rank (IsOwner -- the owner-only carve-out
// short-circuit, set by the caller when the resolved role slug is "owner").
type Principal struct {
	UserId  string
	Rank    int
	IsOwner bool
}

// GovernVerb is the verb being applied to the target principal. Mirrors the
// E1.1 primitive verbs; only the management verbs (update/delete) are
// relationally gated here. read is always allowed at this layer (capability
// gates it); create is gated by CanCreatePrincipal (capability-only), not by
// the relational rule.
type GovernVerb string

const (
	GovernRead   GovernVerb = "read"
	GovernCreate GovernVerb = "create"
	GovernUpdate GovernVerb = "update"
	GovernDelete GovernVerb = "delete"
)

// GovernPrincipal returns true when `actor` may apply `verb` to the `target`
// principal under the relational governance rules above. It assumes the
// capability layer has already confirmed the actor's role HOLDS the
// (verb, principal) capability; this function applies the relational
// narrowing (rank + self + owner-only) on top.
//
// See the package-level comment for the full rule set. The four acceptance
// scenarios (create-not-edit, edit-down-rank, self-edit, owner-only-by-owner)
// are pinned in rbac_governance_test.go.
func GovernPrincipal(actor, target Principal, verb GovernVerb) bool {
	switch verb {
	case GovernRead:
		// Relational layer does not narrow reads; the capability grant +
		// per-row visibility decide. Always pass here.
		return true

	case GovernCreate:
		// Create is a pure capability grant, not relational -- handled by
		// CanCreatePrincipal, not here. Treat create as not-relationally-gated
		// (a caller that routes create through GovernPrincipal gets the
		// capability-only semantics: allowed, with rank bounding applied by
		// CanCreatePrincipal at the call site).
		return true

	case GovernUpdate, GovernDelete:
		return canManagePrincipal(actor, target, verb)

	default:
		// Unknown verb: fail closed.
		return false
	}
}

// canManagePrincipal applies the update/delete relational rule:
//
//	actor.rank > target.rank  OR  actor == target (self)
//
// with the owner carve-outs:
//   - target is an owner: only an owner may manage it.
//   - delete/demote self as an owner: forbidden (lockout guard).
func canManagePrincipal(actor, target Principal, verb GovernVerb) bool {
	self := actor.UserId != "" && actor.UserId == target.UserId

	// Owner target is editable only by an owner. This wins over rank: even a
	// higher-ranked non-owner (there is none above owner in the base model,
	// but a custom rank could be authored above admin) can never edit an
	// owner.
	if target.IsOwner && !actor.IsOwner {
		return false
	}

	// Owner self-demotion / self-deletion lockout: an owner may not remove
	// themselves via this path (mirrors CanDeleteUser's owner-self guard).
	// Applies to delete; an owner editing their own non-rank fields still
	// flows through the self branch for update.
	if verb == GovernDelete && actor.IsOwner && self {
		return false
	}

	// An owner manages everyone (the owner-target carve-out above has already
	// confirmed a non-owner can't reach an owner target). The owner branch is
	// explicit because two owners share the same rank, so the strict-rank rule
	// below would otherwise refuse owner-manages-owner.
	if actor.IsOwner {
		return true
	}

	if self {
		// Self-edit (and self-delete for non-owners) is always permitted.
		return true
	}

	// Relational rank rule: strictly outrank the target.
	return actor.Rank > target.Rank
}

// CanCreatePrincipal expresses the create != update split: creating a
// principal AT a given rank is a pure capability grant bounded by the actor's
// own rank -- a creator may mint a principal only at a rank STRICTLY BELOW
// their own (they cannot mint a peer or a superior). This is the companion to
// GovernPrincipal for the create verb, kept separate precisely so a role can
// hold create-on-principal without update-on-principal.
//
// targetRank is the rank the new principal would be created at. An owner may
// create at any rank below owner; a non-owner may create strictly below their
// own rank.
func CanCreatePrincipal(actor Principal, targetRank int) bool {
	return targetRank < actor.Rank
}
