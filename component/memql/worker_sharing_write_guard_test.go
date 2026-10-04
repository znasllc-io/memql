package memql

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// The guard's decisions without a database: what counts as a change to the
// consent and to the owner, and that internal origin is the only way past
// either (memql#5658). The db test proves the guard is on the write path;
// this one pins the cases a read-merge produces that a raw write never would.
func TestWorkerConsentGuardJudgesChangesNotSpellings(t *testing.T) {
	const ana = "v1:identity:user:ana"
	shared := map[string]any{"mode": "cluster", "userIds": []any{}, "groupIds": []any{}}
	stored := map[string]any{"ownerUserId": ana, "name": "anas-mac", "sharing": shared}
	client := context.Background()

	cases := []struct {
		name    string
		prior   map[string]any
		final   map[string]any
		refused bool
	}{
		{"a rename carries the stored block and owner through", stored,
			map[string]any{"ownerUserId": ana, "name": "Ana's Mac", "sharing": shared}, false},
		{"the same block decoded afresh is the same value", stored,
			map[string]any{"ownerUserId": ana, "sharing": map[string]any{"groupIds": []any{}, "mode": "cluster", "userIds": []any{}}}, false},
		{"a template stamping the owner bare is the stored canonical owner", stored,
			map[string]any{"ownerUserId": "ana", "sharing": shared}, false},
		{"a create that names no block", nil,
			map[string]any{"ownerUserId": ana, "name": "anas-mac"}, false},
		{"a create names its owner and is not judged on it", nil,
			map[string]any{"ownerUserId": "somebody-else"}, false},
		{"a changed mode", stored,
			map[string]any{"ownerUserId": ana, "sharing": map[string]any{"mode": "owner", "userIds": []any{}, "groupIds": []any{}}}, true},
		{"a block removed", stored, map[string]any{"ownerUserId": ana}, true},
		{"a create that sets a block", nil,
			map[string]any{"ownerUserId": ana, "sharing": shared}, true},
		{"an owner moved to somebody else", stored,
			map[string]any{"ownerUserId": "v1:identity:user:operator", "sharing": shared}, true},
		{"an owner blanked", stored, map[string]any{"ownerUserId": "", "sharing": shared}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateWorkerConsentServerOnly(client, tc.prior, tc.final)
			if tc.refused && err == nil {
				t.Fatal("refusal expected, the write was admitted")
			}
			if !tc.refused && err != nil {
				t.Fatalf("admission expected, got %v", err)
			}
			if err := validateWorkerConsentServerOnly(auth.ContextWithInternalOrigin(client), tc.prior, tc.final); err != nil {
				t.Fatalf("internal origin is the one way past the guard, and it was refused: %v", err)
			}
		})
	}
}
