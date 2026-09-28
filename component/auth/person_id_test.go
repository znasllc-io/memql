package auth

import "testing"

// An actor id names a PERSON only when the identity service minted it. Every
// actor the engine synthesizes carries a prefix that says so, and a caller
// holding only the string -- a router request, a ledger row -- must be able to
// tell the two apart without the context it came from.
func TestNamesNoPersonTellsSynthesizedActorsFromPeople(t *testing.T) {
	for _, tc := range []struct {
		id     string
		person bool
	}{
		{"", false},
		{"   ", false},
		{SystemActor("edge").UserId, false},
		{"system:automation:workerAppSessionStaleSweep", false},
		{MaintenanceActor("auditEventRetentionSweep").UserId, false},
		{ConnectorActor("shopify").UserId, false},
		{AnonymousUserId, false},
		{"a248d524-1791-45a7-a537-4a65b5aabdb4", true},
		{"v1:identity:user:a248d524-1791-45a7-a537-4a65b5aabdb4", true},
	} {
		if got := NamesNoPerson(tc.id); got == tc.person {
			t.Errorf("NamesNoPerson(%q) = %v, want %v", tc.id, got, !tc.person)
		}
	}
}
