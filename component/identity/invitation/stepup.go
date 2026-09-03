package invitation

// The step-up threshold (memql#4601).
//
// # What this decides
//
// An invitation is a credential that arrived by email. Redeeming one normally
// costs a single click, because the whole point of the redesign was that a
// person who was invited should not have to prove who they are twice: the token
// reached their mailbox, which is already evidence they can read it.
//
// That argument gets weaker as the role gets stronger. An invitation granting
// `admin` or `owner` can take the cluster, and "whoever opened the email first"
// is a poor thing to hand that to. For those roles the accept issues a magic
// link to the invited address instead of provisioning immediately, so the
// holder must demonstrate ongoing access to the mailbox rather than one-time
// possession of a message that may have been forwarded.
//
// # Why a threshold rather than a flag on the row
//
// Because it is POLICY, and policy that lives on the row is policy that was
// decided when the row was written. An invitation issued last week under one
// threshold would then keep its old answer after an operator tightened it,
// which is exactly backwards -- a tightening should apply to the credentials
// already in flight, since those are the ones nobody has re-examined.
//
// # Why this is a set and not a rank threshold
//
// It once said "why not reuse adminops.roleRank", and that map is gone: the
// admin surface now calls auth.RoleRank, the cluster's one ordering (epic
// memql#4832, D1). So the coupling argument no longer applies -- auth is
// exported, sits below both packages, and this file could call it.
//
// The reason to keep a set is that this is NOT a ranking question. The admin
// surface asks whether an inviter OUTRANKS the role they are granting, which
// is an ordering. This asks which invitations are worth an extra mailbox round
// trip, which is a policy choice about a handful of named roles -- and under
// the one ladder that list is not even a contiguous top of it. Expressing it as
// a threshold would silently enrol whatever rung a future release adds above
// the cut, which is the opposite of a deliberate list.

import "strings"

// stepUpRoles are the cluster roles whose invitations require the extra
// mailbox round trip before an account is created.
//
// Deliberately a small explicit set rather than a rank comparison. The set is
// the policy, and reading it is how somebody answers "which invitations are
// slower and why" without first reconstructing an ordering. Adding a role here
// is a one-line, reviewable change; the failure mode of a rank threshold is a
// role silently crossing it when the ordering is edited for another reason.
var stepUpRoles = map[string]bool{
	"owner": true,
	"admin": true,
}

// RequiresStepUp reports whether an invitation granting this role must confirm
// the mailbox before the account is provisioned.
//
// An EMPTY role does not step up. Empty means "the cluster's default role for a
// new user", which is the least-privileged case the cluster has chosen, and
// treating the absence of a role as though it were the presence of a powerful
// one would put every ordinary invitation through the slow path. An unknown
// role does not step up either, for the same reason and one more: it cannot
// have been granted by IssueUserInvitation, which refuses any role
// auth.IsValidRole does not name, so a value arriving here that is neither
// empty nor known is a row this build does not understand rather than a
// privileged one.
func RequiresStepUp(role string) bool {
	return stepUpRoles[strings.ToLower(strings.TrimSpace(role))]
}
