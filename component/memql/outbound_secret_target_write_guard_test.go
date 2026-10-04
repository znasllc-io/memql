package memql

import (
	"context"
	"maps"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
)

// TestOutboundSecretTargetWriteGuardDecisions is the guard's rule as a table
// (memql#5480). outbound_secret_target_db_test.go proves the engine consults it
// on every write path; this states what it answers, including the cases a
// careless rule would get wrong -- an empty stamp is not a secret, a legacy row
// with no targetSecret at all is the same unset value, and the worker's
// lifecycle stamps on a secret row stay open.
func TestOutboundSecretTargetWriteGuardDecisions(t *testing.T) {
	client := auth.ContextWithClientOrigin(context.Background())
	internal := auth.ContextWithInternalOrigin(context.Background())
	secretRow := map[string]any{
		"medium": "webhook", "target": "secret:DISCORD_X", "targetSecret": "DISCORD_X",
		"body": "b", "dedupeKey": "k", "requestedBy": "pipelines:notify:r1", "status": "pending", "attempts": float64(0),
	}
	plainRow := map[string]any{
		"medium": "webhook", "target": "https://hooks.example/x", "body": "b", "status": "pending", "attempts": float64(0),
	}
	with := func(row map[string]any, kv ...any) map[string]any {
		out := maps.Clone(row)
		for i := 0; i < len(kv); i += 2 {
			out[kv[i].(string)] = kv[i+1]
		}
		return out
	}

	for _, tc := range []struct {
		name         string
		ctx          context.Context
		prior, final map[string]any
		refuses      string // the field named in the refusal; empty when the write is admitted
	}{
		{"a client creating a secret row", client, nil, secretRow, "targetSecret"},
		{"internal origin creating a secret row", internal, nil, secretRow, ""},
		{"a client creating a plain row", client, nil, plainRow, ""},
		{"a plain row carrying the empty stamp", client, nil, with(plainRow, "targetSecret", ""), ""},
		{"a legacy plain row re-staged under the empty stamp", client, plainRow, with(plainRow, "targetSecret", "", "body", "b2"), ""},
		{"a client re-staging a plain row's body", client, plainRow, with(plainRow, "body", "b2"), ""},
		{"the worker stamping a secret row's lifecycle", client, secretRow,
			with(secretRow, "status", "sent", "attempts", float64(1), "lastError", "", "nextAttemptAt", "2026-10-04T12:00:30Z", "sentAt", "2026-10-04T12:00:31Z"), ""},
		{"a client replacing a secret row's body", client, secretRow, with(secretRow, "body", "b2"), "body"},
		{"a client retargeting a secret row's medium", client, secretRow, with(secretRow, "medium", "email"), "medium"},
		{"a client rewriting a secret row's provenance", client, secretRow, with(secretRow, "requestedBy", "someone"), "requestedBy"},
		{"a client clearing the secret", client, secretRow, with(secretRow, "targetSecret", ""), "targetSecret"},
		{"a client pointing a row at another secret", client, secretRow, with(secretRow, "targetSecret", "OTHER"), "targetSecret"},
		{"a client turning a plain row into a secret row", client, plainRow, with(plainRow, "targetSecret", "DISCORD_X"), "targetSecret"},
		{"internal origin replacing a secret row's body", internal, secretRow, with(secretRow, "body", "b2"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateOutboundSecretTargetWrite(tc.ctx, tc.prior, tc.final)
			if tc.refuses == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), "`"+tc.refuses+"`")
		})
	}
}
