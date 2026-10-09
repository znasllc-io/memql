package memql

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
)

// Exercise the actual embedded query and executor predicate. Recovery must see
// revoked/OIDC credentials, while existing callers retain the active-route
// semantics that protect recovery-key redemption.
func TestOwnerCredentialHistoryPreservesActiveRouteDefault(t *testing.T) {
	engine := selfScopeEngine(t)
	ctx := auth.ContextWithInternalOrigin(context.Background())
	for _, option := range []string{"", ", includeHistory: false", ", includeHistory: true"} {
		t.Run(option, func(t *testing.T) {
			history := option == ", includeHistory: true"
			filter := evaluableFilter(t, engine, ctx, `query signInIdentitiesForUser(userId: "`+selfScopeAlice+`"`+option+`)`)
			for _, kind := range []string{"passkey", "magic_link", "oidc", "recovery_key", "api_key", "worker_token", "node_token", "badge"} {
				for _, active := range []bool{false, true} {
					row := identityRowNode(t, "v1:identity:identity:history", selfScopeAlice, kind)
					row.Payload, _ = json.Marshal(map[string]any{"userId": selfScopeAlice, "identityType": kind, "active": active})
					expected := (kind == "passkey" || kind == "magic_link") && (active || history) || kind == "oidc" && history
					require.Equal(t, expected, matchesFilter(t, row, filter), "kind=%s active=%v history=%v", kind, active, history)
					row.Payload, _ = json.Marshal(map[string]any{"userId": selfScopeBob, "identityType": kind, "active": active})
					require.False(t, matchesFilter(t, row, filter), "another user's credential must not count")
				}
			}
		})
	}
}
