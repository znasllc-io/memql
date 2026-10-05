package memql

import (
	"context"
	"maps"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
)

// TestProtectedOutboundWriteGuardDecisions is the guard's rule as a table
// (memql#5480). outbound_protected_row_db_test.go proves the engine consults it
// on every write path; this states what it answers, including the cases a
// careless rule would get wrong -- an empty stamp is not a secret, a legacy row
// with no targetSecret at all is the same unset value, a plain row's delivery
// state stays open, and a protected row's -- one naming a secret, or one server
// code staged -- is closed to everything but internal origin, the worker's
// stamps included unless they carry it.
func TestProtectedOutboundWriteGuardDecisions(t *testing.T) {
	client := auth.ContextWithClientOrigin(context.Background())
	internal := auth.ContextWithInternalOrigin(context.Background())
	secretRow := map[string]any{
		"medium": "webhook", "target": "secret:DISCORD_X", "targetSecret": "DISCORD_X", "serverStaged": true,
		"body": "b", "dedupeKey": "k", "requestedBy": "pipelines:notify:r1", "status": "pending", "attempts": float64(0),
	}
	// A secret row staged before rows were marked serverStaged: protected by
	// its secret alone.
	legacySecretRow := map[string]any{
		"medium": "webhook", "target": "secret:DISCORD_X", "targetSecret": "DISCORD_X",
		"body": "b", "status": "pending", "attempts": float64(0),
	}
	// The notify stage's email row: a plain target, staged by server code.
	stagedRow := map[string]any{
		"medium": "email", "target": "ops@example.test", "targetSecret": "", "serverStaged": true,
		"subject": "s", "body": "b", "dedupeKey": "k", "requestedBy": "pipelines:notify:r1", "status": "pending", "attempts": float64(0),
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
	without := func(row map[string]any, keys ...string) map[string]any {
		out := maps.Clone(row)
		for _, k := range keys {
			delete(out, k)
		}
		return out
	}

	lifecycle := []any{"status", "sent", "attempts", float64(1), "lastError", "", "nextAttemptAt", "2026-10-04T12:00:30Z", "sentAt", "2026-10-04T12:00:31Z"}
	for _, tc := range []struct {
		name         string
		ctx          context.Context
		prior, final map[string]any
		refuses      string // a substring of the refusal; empty when the write is admitted
	}{
		// Rows nobody protects.
		{"a client creating a plain row", client, nil, plainRow, ""},
		{"a plain row carrying the empty stamp", client, nil, with(plainRow, "targetSecret", ""), ""},
		{"a legacy plain row re-staged under the empty stamp", client, plainRow, with(plainRow, "targetSecret", "", "body", "b2"), ""},
		{"a client re-staging a plain row's body", client, plainRow, with(plainRow, "body", "b2"), ""},
		{"a client stamping a plain row's delivery state", client, plainRow, with(plainRow, lifecycle...), ""},
		{"a client requeueing a failed plain row", client, with(plainRow, "status", "failed"), plainRow, ""},
		{"a plain row given a blank secret name", client, plainRow, with(plainRow, "targetSecret", "  "), "`targetSecret`"},

		// Secret rows.
		{"a client creating a secret row", client, nil, secretRow, "`targetSecret`"},
		{"internal origin creating a secret row", internal, nil, secretRow, ""},
		{"the worker stamping a secret row's delivery state", internal, secretRow, with(secretRow, lifecycle...), ""},
		{"a client stamping a secret row's delivery state", client, secretRow, with(secretRow, lifecycle...), "`status`"},
		{"a client marking a secret row sent", client, secretRow, with(secretRow, "status", "sent", "sentAt", "2026-10-04T12:00:31Z"), "`status`"},
		{"a client requeueing a failed secret row", client, with(secretRow, "status", "failed"), secretRow, "`status`"},
		{"a client counting an attempt on a secret row", client, secretRow, with(secretRow, "attempts", float64(3)), "`attempts`"},
		{"a client rewriting a secret row unchanged", client, secretRow, secretRow, "write refused --"},
		{"a client replacing a secret row's body", client, secretRow, with(secretRow, "body", "b2"), "`body`"},
		{"a client retargeting a secret row's medium", client, secretRow, with(secretRow, "medium", "email"), "`medium`"},
		{"a client rewriting a secret row's provenance", client, secretRow, with(secretRow, "requestedBy", "someone"), "`requestedBy`"},
		{"a client clearing the secret", client, secretRow, with(secretRow, "targetSecret", ""), "`targetSecret`"},
		{"a client pointing a row at another secret", client, secretRow, with(secretRow, "targetSecret", "OTHER"), "`targetSecret`"},
		{"a client turning a plain row into a secret row", client, plainRow, with(plainRow, "targetSecret", "DISCORD_X"), "`targetSecret`"},
		{"internal origin replacing a secret row's body", internal, secretRow, with(secretRow, "body", "b2"), ""},
		{"a client marking a legacy secret row sent", client, legacySecretRow, with(legacySecretRow, "status", "sent"), "`status`"},

		// Server-staged rows: a plain target, protected by who staged it.
		{"a client raw-inserting a server-staged row", client, nil, stagedRow, "`serverStaged`"},
		{"internal origin staging a server-staged row", internal, nil, stagedRow, ""},
		{"the worker stamping a server-staged row's delivery state", internal, stagedRow, with(stagedRow, lifecycle...), ""},
		{"a client marking a server-staged row sent", client, stagedRow, with(stagedRow, "status", "sent", "sentAt", "2026-10-04T12:00:31Z"), "`status`"},
		{"a client requeueing a failed server-staged row", client, with(stagedRow, "status", "failed"), stagedRow, "`status`"},
		{"a client re-staging a server-staged row elsewhere", client, stagedRow, with(stagedRow, "target", "eve@example.test"), "`target`"},
		{"a client re-staging a server-staged row's body", client, stagedRow, with(stagedRow, "body", "b2"), "`body`"},
		{"a client rewriting a server-staged row unchanged", client, stagedRow, stagedRow, "write refused --"},
		{"a client unmarking a server-staged row", client, stagedRow, with(stagedRow, "serverStaged", false), "`serverStaged`"},
		{"a client dropping a server-staged row's mark", client, stagedRow, without(stagedRow, "serverStaged"), "`serverStaged`"},
		{"a client marking a plain row server-staged", client, plainRow, with(plainRow, "serverStaged", true), "`serverStaged`"},
		{"a client marking a plain row server-staged as text", client, plainRow, with(plainRow, "serverStaged", "true"), "`serverStaged`"},
		{"a client writing the mark false on a plain row", client, plainRow, with(plainRow, "serverStaged", false), "`serverStaged`"},
		{"a stored text mark protects the row", client, with(plainRow, "serverStaged", "TRUE"), with(plainRow, "serverStaged", "TRUE", "status", "sent"), "`status`"},
		{"internal origin re-staging a server-staged row", internal, stagedRow, with(stagedRow, "body", "b2"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateProtectedOutboundWrite(tc.ctx, tc.prior, tc.final)
			if tc.refuses == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.refuses)
		})
	}
}
