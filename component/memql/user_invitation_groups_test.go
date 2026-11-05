package memql

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// An invitation names the groups its invitee joins on accepting, and
// component/identity/adminops sends them to createUserInvitation as a LIST
// (`groupIds: ["…"]`, renderStringList). The OS's Invite wizard always asks
// for them. The mutation declared the argument `object`, and the argument
// validator refuses a list for an object -- "argument "groupIds": expected
// object, got array" -- so every invitation that named a group was refused
// before a row was written. Only an invitation naming no groups got through.
func TestCreateUserInvitation_TakesTheGroupsAsAList(t *testing.T) {
	validate := profileFn(t, "createUserInvitation").SnapshotArgumentValidator()
	args := func(extra map[string]any) map[string]any {
		out := map[string]any{
			"invitationId": "inv-1",
			"email":        "moderator@example.com",
			"tokenHash":    "hash",
			"expiresAt":    "2026-11-12T00:00:00Z",
			"inviterId":    "u-operator",
			"inviterName":  "operator@example.com",
			"role":         "org-moderator",
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}

	require.NoError(t, validate(args(nil)), "an invitation that names no groups")
	require.NoError(t, validate(args(map[string]any{"groupIds": []any{"g-org"}, "accountId": "acct-org"})),
		"an invitation naming the group it joins, the way adminops sends it")
	require.NoError(t, validate(args(map[string]any{"groupIds": []any{"g-org", "g-other"}})),
		"an invitation naming two groups")
}
