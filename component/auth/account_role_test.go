package auth

import "testing"

func TestAccountHierarchyDoesNotCreatePeerOrOwnerAuthority(t *testing.T) {
	for _, tc := range []struct {
		actor, target, next AccountRole
		allowed             bool
	}{
		{AccountOwner, "", AccountDeveloper, true},
		{AccountOwner, AccountDeveloper, AccountAdmin, true},
		{AccountOwner, "", AccountOwner, false},
		{AccountOwner, AccountOwner, AccountDeveloper, false},
		{AccountDeveloper, "", AccountDeveloper, false},
		{AccountDeveloper, AccountDeveloper, AccountViewer, false},
		{AccountDeveloper, AccountAdmin, AccountMember, true},
		{AccountAdmin, AccountMember, AccountViewer, true},
		{AccountAdmin, AccountMember, AccountDeveloper, false},
		{AccountMember, AccountViewer, "", false},
		{"unknown", AccountViewer, AccountMember, false},
		{AccountOwner, "unknown", AccountViewer, false},
	} {
		if got := MayManageAccountMember(tc.actor, tc.target, tc.next); got != tc.allowed {
			t.Errorf("%s changes %s to %s: got %v", tc.actor, tc.target, tc.next, got)
		}
	}
}
