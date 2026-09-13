package auth

// WHO MAY GIVE WHOM WHICH ROLE (epic memql#5166, decision D4).
//
// Two seams assign a role -- SetUserRole and invitation issue -- and until this
// existed they applied DIFFERENT rules. SetUserRole applied none at all beyond
// "the caller is an admin", which is the "uncapped SetUserRole" three comments
// elsewhere in the tree name as the second move in a path to owner. Invitation
// issue applied a rank cap plus the people-authority clause, correctly, in a
// paragraph of its own.
//
// One function now, for the reason the ladder is one model: two implementations
// of a rule this shape do not stay in step, and the failure is silent in both
// directions -- a seam that is stricter refuses work somebody may legitimately
// do, and a seam that is looser is an escalation nobody is looking at.
//
// It composes the three predicates that already existed rather than restating
// them: GovernPrincipal for the target's current rung, CanCreatePrincipal for
// the new one, GrantsPrincipalAuthorityBeyond for the pair rank cannot judge.
// The only rule added here is D10's scope check, which is new with this epic.

import "strings"

// AssignRefusal names why an assignment was refused. The empty value is
// "allowed", so a caller reads `if refusal != AssignAllowed`.
//
// TYPED RATHER THAN A BOOL because both call sites write an audit row, and a
// trail that cannot tell "you do not manage users" from "that role is above
// you" cannot answer the question an auditor asks of a refusal. The values are
// the reason strings the audit rows carry.
type AssignRefusal string

const (
	// AssignAllowed is the zero value: no refusal.
	AssignAllowed AssignRefusal = ""
	// AssignUnknownRole -- the slug names no role this cluster can assign.
	// Covers a typo, a role that never existed, and a DEACTIVATED one (D8:
	// a retired role stays as history and cannot be handed out).
	AssignUnknownRole AssignRefusal = "unknown_role"
	// AssignNotAUserManager -- the caller's role holds no update-on-principal.
	AssignNotAUserManager AssignRefusal = "role_cannot_manage_principals"
	// AssignTargetOutranks -- the caller does not outrank the person they are
	// trying to re-role, or the target is an owner and the caller is not.
	AssignTargetOutranks AssignRefusal = "target_outranks_caller"
	// AssignAboveCaller -- the NEW role is at or above the caller's own rung.
	AssignAboveCaller AssignRefusal = "role_above_caller"
	// AssignAuthorityBeyond -- the new role holds a principal verb the caller
	// does not, whatever the ranks say.
	AssignAuthorityBeyond AssignRefusal = "role_grants_authority_beyond_caller"
	// AssignNotAMember -- the role is scoped to an account (D10) and the target
	// is not a member of it, or membership could not be resolved.
	AssignNotAMember AssignRefusal = "target_not_a_member_of_the_scope"
)

// AssignKind names WHICH SEAM is assigning, and the seam says so rather than
// being inferred (memql#5236).
//
// This was read off `targetCurrentSlug == ""`, and that test does not mean what
// it was being used for. It means THE TARGET HAS NO CURRENT RUNG, which is a
// different fact from "this caller is issuing an invitation" -- and the two
// come apart, because `v1:identity:user.role` is declared `@default("reader")`
// with no `!` and a concept-field default is never applied on insert, so a
// blank role is reachable and SetUserRole passes `user.Role` straight through.
//
// It was harmless while the inference decided only which CAPABILITY to require
// (SetUserRole's outer gate is AtLeastAdmin == create-on-principal, so only
// owner and admin reach the re-role path and both hold every principal verb).
// It stops being harmless the moment it also decides whether a SECURITY clause
// runs, which is exactly what the exemption below does. A rule that infers its
// own caller is a rule that stops meaning what its comment says.
type AssignKind int

const (
	// AssignOnInvitation -- naming the role on an invitation. No principal
	// exists yet; one is created only if the recipient redeems it.
	AssignOnInvitation AssignKind = iota
	// AssignOnReRole -- moving an existing principal to a different rung.
	AssignOnReRole
)

// MayAssignRole decides D4: may `actor` put `newSlug` on the principal named by
// `targetUserId`, who currently holds `targetCurrentSlug`?
//
// `targetCurrentSlug` is EMPTY for an invitation -- there is no principal yet,
// so there is no current rung to outrank, and the target half of the rule
// passes trivially. What stops an admin inviting an owner is the NEW-rank
// bound, which applies identically to an invitation and to a re-role.
//
// `kind` selects which CAPABILITY is required and whether the people-authority
// clause runs; see both below.
//
// `targetIsMember` answers "is this person in that account's groups", and is
// consulted only for a scoped role. A NIL function REFUSES a scoped role: a
// caller that cannot answer the question is not a caller whose answer is yes.
func MayAssignRole(
	actor UserContext,
	kind AssignKind,
	targetUserId, targetCurrentSlug, newSlug string,
	targetIsMember func(accountId string) bool,
) AssignRefusal {
	newSlug = normalizeSlug(newSlug)
	if !IsValidRole(Role(newSlug)) {
		return AssignUnknownRole
	}

	// THE CAPABILITY, FIRST. Everything below is relational narrowing on top of
	// a grant the caller must hold at all; asking the relational questions of a
	// caller who holds no people-authority would let the refusal name a rank
	// when the real answer is "this is not your job".
	//
	// WHICH GRANT DEPENDS ON WHICH SEAM IS ASKING, and that is the model's own
	// create-versus-update split rather than a convenience. Re-roling somebody
	// is `update` on `principal` (D4). Naming the role on an INVITATION is not:
	// there is no principal yet, and a developer holds create-on-admission and
	// no update-on-principal precisely so it can invite people and cannot
	// re-role them (memql#4917). Requiring update here would take invitations
	// away from every developer in every cluster, through the one function whose
	// job is deciding which ROLE they may name.
	//
	// ROLE-LEVEL, DELIBERATELY (epic memql#5296). This is a statement about
	// which ROLE may name which role -- the assignment seams hand in a
	// UserContext, not a request -- and it is not one of the request-path
	// capability gates CapableFor replaced. A person granted `update` on
	// `principal` by a v1:rbac:grant does not thereby become a user manager
	// here; widening that is a governance decision the grants record does not
	// make, and it would have to be made at both seams together.
	if kind == AssignOnInvitation {
		if !roleHasCapability(actor.Role, VerbCreate, ResourceAdmission) &&
			!roleHasCapability(actor.Role, VerbCreate, ResourcePrincipal) {
			return AssignNotAUserManager
		}
	} else if !roleHasCapability(actor.Role, VerbUpdate, ResourcePrincipal) {
		return AssignNotAUserManager
	}

	actorP := Principal{
		UserId:  actor.ID,
		Rank:    roleRank(actor.Role),
		IsOwner: isOwnerRung(string(actor.Role)),
	}
	targetP := Principal{
		UserId:  targetUserId,
		Rank:    roleRank(Role(targetCurrentSlug)),
		IsOwner: isOwnerRung(targetCurrentSlug),
	}
	if !GovernPrincipal(actorP, targetP, GovernUpdate) {
		return AssignTargetOutranks
	}

	// THE NEW-RANK BOUND, with the owner carve-out.
	//
	// CanCreatePrincipal is `newRank < actorRank`, which refuses owner -> owner
	// (400 is not below 400) and would make a second owner unmakeable through
	// every path in the product: a cluster with one owner and no way to name
	// another, including no way for that owner to hand the cluster on.
	// GovernPrincipal carries the identical carve-out on the target half
	// ("an owner manages everyone"), for the identical reason -- two owners
	// share a rank, so a strict comparison cannot express them.
	if !actorP.IsOwner && !CanCreatePrincipal(actorP, roleRank(Role(newSlug))) {
		return AssignAboveCaller
	}

	// RANK IS NOT AUTHORITY, and this is the clause that says so. developer
	// ranks 300 above admin's 200 and holds strictly fewer principal verbs, so
	// every "must strictly outrank" rule admits developer -> admin -- while
	// admin can then do the user management the developer cannot, including
	// calling this function to make anybody an owner.
	//
	// RE-ROLING ONLY (memql#5236). Admitting somebody is not wielding their
	// powers: a re-role hands over authority immediately, while an invitation
	// opens a door the recipient must still walk through, and the inviter still
	// holds no verb on the principal that results. The seeds grant developer
	// create-on-admission for exactly this -- "a developer standing a cluster up
	// alongside the owner can get colleagues in" (dsl/rbac/seeds.memql) -- and
	// this clause took most of it back, since `admin` is the rung such a person
	// most often needs to hand out.
	//
	// THE TRADE-OFF IS ACCEPTED AND DEFERRED, NOT OVERLOOKED. A developer can
	// invite an address they control as admin, redeem it, and act through that
	// account to do the people-management developers are denied. The cluster
	// owner accepted this knowingly; the open question -- a second sign-off from
	// somebody who holds the authority, an owner notification, or an explicit
	// per-role admission ceiling -- is recorded for a later epic. DO NOT
	// "restore" this clause to the invitation seam as a fix: that silently takes
	// invitations away from every developer in every cluster, which is the bug
	// this issue exists to close.
	if kind == AssignOnReRole && GrantsPrincipalAuthorityBeyond(actor.Role, Role(newSlug)) {
		return AssignAuthorityBeyond
	}

	// D10: a scoped role is holdable only by a member of its account.
	if scope := roleScope(newSlug); scope != "" {
		if targetIsMember == nil || !targetIsMember(scope) {
			return AssignNotAMember
		}
	}

	return AssignAllowed
}

// AssignRefusalSentence renders a refusal for the person who hit it.
//
// It names the requirement and the caller's own role, and never names who
// COULD do it: that is a directory disclosure on a refusal path, and the same
// rule refuseBelowRequiredRank follows.
func AssignRefusalSentence(refusal AssignRefusal, actorRole Role, newSlug string) string {
	switch refusal {
	case AssignAllowed:
		return ""
	case AssignUnknownRole:
		return newSlug + " is not a role this cluster can assign"
	case AssignNotAUserManager:
		return "your role (" + string(actorRole) + ") does not manage people"
	case AssignTargetOutranks:
		return "you do not outrank that person"
	case AssignAboveCaller:
		return "you cannot grant " + newSlug + " -- it is at or above your own role"
	case AssignAuthorityBeyond:
		return "you cannot grant " + newSlug + " -- it holds user-management authority yours does not"
	case AssignNotAMember:
		return newSlug + " belongs to one account, and that person is not a member of it"
	default:
		return "that role change is not permitted"
	}
}

// isOwnerRung reports whether a slug names the owner rung.
//
// THE SLUG, NOT THE RANK, matching the rbac integration's ownerFromArgs: a
// custom role authored at 400 is not an owner, and treating it as one would
// hand it the carve-outs GovernPrincipal reserves for the cluster's owner.
func isOwnerRung(slug string) bool {
	return normalizeSlug(slug) == string(RoleOwner)
}

// roleScope is the account a role is scoped to, or "" for a global one.
//
// A cluster with NO catalog installed has no scoped roles -- nothing but a role
// row can declare a scope -- so an absent catalog answers "" rather than
// declining, and the membership check is skipped because there is nothing to
// check against. That is not a fail-open: a scoped role cannot exist without
// the rows that define it.
func roleScope(slug string) string {
	cat := InstalledCapabilityCatalog()
	if cat == nil {
		return ""
	}
	return strings.TrimSpace(cat.Scope(normalizeSlug(slug)))
}
