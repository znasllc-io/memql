package auth

// AccountRole is authority in one organization. It is never a cluster Role:
// a client's Owner or Developer must not acquire cluster administration.
type AccountRole string

const (
	AccountOwner     AccountRole = "owner"
	AccountDeveloper AccountRole = "developer"
	AccountAdmin     AccountRole = "admin"
	AccountMember    AccountRole = "member"
	AccountViewer    AccountRole = "viewer"
)

func AccountRoleRank(role AccountRole) int {
	switch role {
	case AccountOwner:
		return 400
	case AccountDeveloper:
		return 300
	case AccountAdmin:
		return 200
	case AccountMember:
		return 100
	case AccountViewer:
		return 50
	default:
		return 0
	}
}

func ValidAccountRole(role AccountRole) bool { return AccountRoleRank(role) > 0 }

// Account managers can change only lower roles. Ownership is changed through
// an atomic transfer, never by adding another Owner or removing the last one.
func MayManageAccountMember(actor, target, next AccountRole) bool {
	if AccountRoleRank(actor) < AccountRoleRank(AccountAdmin) || !ValidAccountRole(actor) {
		return false
	}
	if target == AccountOwner || next == AccountOwner {
		return false
	}
	if target != "" && (!ValidAccountRole(target) || AccountRoleRank(target) >= AccountRoleRank(actor)) {
		return false
	}
	return next == "" || ValidAccountRole(next) && AccountRoleRank(next) < AccountRoleRank(actor)
}
