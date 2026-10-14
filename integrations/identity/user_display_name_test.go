package identity

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
)

// Even direct integration invocation cannot borrow the internal origin used
// by the underlying user mutation. Only the audit sink reaches the engine.
func TestDisplayNameCapabilityRefusesUnprivilegedCallersAndAudits(t *testing.T) {
	for _, role := range []auth.Role{"", auth.RoleReader, auth.RoleWriter, auth.RoleDeveloper} {
		t.Run(string(role), func(t *testing.T) {
			engine := &beginFakeEngine{}
			integration := NewIdentityIntegrationWithEngine(engine, nil, nil)
			ctx := context.Background()
			if role != "" {
				ctx = appCtx(role, "caller")
			}
			_, err := integration.handleSetUserDisplayName(ctx, map[string]any{"userId": "target", "displayName": "Name"}, 0)
			require.Error(t, err)
			require.Len(t, engine.statements, 1)
			require.True(t, strings.HasPrefix(engine.statements[0], "mutation createAuditEvent("))
			require.Contains(t, engine.statements[0], "admin_auth_forbidden")
		})
	}
}

func TestDisplayNameCapabilityRefusesExtraPayloadAndMissingEngine(t *testing.T) {
	engine := &beginFakeEngine{}
	integration := NewIdentityIntegrationWithEngine(engine, nil, nil)
	_, err := integration.handleSetUserDisplayName(appCtx(auth.RoleOwner, "caller"), map[string]any{
		"userId": "target", "displayName": "Name", "payload": map[string]any{"role": "owner"},
	}, 0)
	require.ErrorContains(t, err, "unsupported argument")
	require.Empty(t, engine.statements)
	_, err = NewIdentityIntegration().handleSetUserDisplayName(appCtx(auth.RoleOwner, "caller"), map[string]any{"userId": "target", "displayName": "Name"}, 0)
	require.ErrorContains(t, err, "engine unavailable")
}
