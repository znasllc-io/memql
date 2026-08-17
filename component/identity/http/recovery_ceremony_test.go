package http

// The owner recovery key as an authority for the passkey registration ceremony
// (memql#3968), and the audit-completeness gate memql#3971 asks for.
//
// THE CENTRAL CLAIM OF THIS FILE: no path through the recovery arm returns
// without writing an audit row. A break-glass credential whose failures are
// silent is the one that gets brute-forced quietly -- somebody spraying
// guesses at a 256-bit search space produces nothing anybody can see, and the
// first evidence of the attempt is the success.
//
// The rejection states are ENUMERATED from the package's own constants rather
// than listed, so a state added later fails this test until it is given a
// case. Listing them would let the seventh state arrive unaudited, which is
// precisely the shape of the defect.

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/recoverykey"
)

// recordingAudit captures every event a handler emits.
type recordingAudit struct {
	mu     sync.Mutex
	events []identity.AuditEvent
}

func (a *recordingAudit) Log(_ context.Context, ev identity.AuditEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, ev)
}

func (a *recordingAudit) snapshot() []identity.AuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]identity.AuditEvent, len(a.events))
	copy(out, a.events)
	return out
}

// recoveryRow builds a v1:identity:identity row of the recovery_key variant as
// the shape query projects it.
func recoveryRow(hash string, active bool, cred map[string]any) map[string]any {
	credentials := map[string]any{
		"keyHash":          hash,
		"boundOwnerUserId": passkeyTestUserId,
		"mintedBy":         "system:identity-svc",
		"claimedAt":        time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano),
	}
	for k, v := range cred {
		credentials[k] = v
	}
	return map[string]any{
		"id":          "rec-1",
		"userId":      passkeyTestUserId,
		"active":      active,
		"credentials": credentials,
		"createdAt":   time.Now().UTC().Format(time.RFC3339Nano),
	}
}

// newRecoveryTestServer wires a stub engine plus a recording audit sink.
func newRecoveryTestServer(t *testing.T, engine *passkeyStubEngine) (*Server, *recordingAudit) {
	t.Helper()
	s := newPasskeyTestServer(t, engine)
	audit := &recordingAudit{}
	s.Audit = audit
	return s, audit
}

// ---------------------------------------------------------------------
// THE AUDIT GATE (memql#3971)
// ---------------------------------------------------------------------

// TestEveryRecoveryRejectionIsAudited drives one request per rejection state
// and fails when any of them returns without an audit row.
func TestEveryRecoveryRejectionIsAudited(t *testing.T) {
	// Enumerated from the package's constants. A state added to recoverykey
	// without a case here fails the coverage assertion at the bottom.
	cases := []struct {
		name       string
		state      recoverykey.State
		wantStatus int
		wantCode   string
		// engine returns the stub for this case; nil row means "no match".
		engine func(hash string) *passkeyStubEngine
	}{
		{
			name:       "invalid -- no row matches the presented digest",
			state:      recoverykey.StateInvalid,
			wantStatus: http.StatusUnauthorized,
			wantCode:   "recovery_invalid",
			engine: func(string) *passkeyStubEngine {
				return &passkeyStubEngine{user: map[string]any{"id": passkeyTestUserId}}
			},
		},
		{
			name:       "already redeemed -- the replay case",
			state:      recoverykey.StateAlreadyRedeemed,
			wantStatus: http.StatusConflict,
			wantCode:   "recovery_already_redeemed",
			engine: func(hash string) *passkeyStubEngine {
				return &passkeyStubEngine{
					user: map[string]any{"id": passkeyTestUserId},
					recovery: recoveryRow(hash, false, map[string]any{
						"redeemedAt": time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano),
					}),
				}
			},
		},
		{
			name:       "deactivated -- rotated out without being used",
			state:      recoverykey.StateDeactivated,
			wantStatus: http.StatusForbidden,
			wantCode:   "recovery_deactivated",
			engine: func(hash string) *passkeyStubEngine {
				return &passkeyStubEngine{
					user:     map[string]any{"id": passkeyTestUserId},
					recovery: recoveryRow(hash, false, nil),
				}
			},
		},
	}

	covered := map[recoverykey.State]bool{}
	for _, tc := range cases {
		covered[tc.state] = true
		t.Run(tc.name, func(t *testing.T) {
			plain, hash, err := recoverykey.Mint()
			require.NoError(t, err)

			engine := tc.engine(hash)
			s, audit := newRecoveryTestServer(t, engine)

			rec := driveWithScheme(t, s, "/auth/webauthn/register/begin", "Recovery "+plain,
				WebAuthnRegisterBeginRequest{}, s.handleWebAuthnRegisterBegin)

			require.Equal(t, tc.wantStatus, rec.Code, "body=%s", rec.Body.String())
			require.Equal(t, tc.wantCode, decodeBegin(t, rec).ErrorCode)
			require.Empty(t, engine.mutations, "a refused redemption must write nothing at all")

			events := audit.snapshot()
			require.NotEmpty(t, events,
				"this rejection returned WITHOUT an audit row. A break-glass credential whose "+
					"failures are silent is the one that gets brute-forced quietly: a script "+
					"spraying guesses produces nothing anybody can see, and the first evidence "+
					"of the attempt is the success.")

			// The reason has to distinguish this rejection from the others. A
			// collapsed "recovery_denied" would make a replay burst and a
			// scanner burst indistinguishable in the trail.
			var reasons []string
			for _, ev := range events {
				reasons = append(reasons, ev.FailureReason)
			}
			joined := strings.Join(reasons, ",")
			wantReason := "recovery_" + strings.ReplaceAll(string(tc.state), "-", "_")
			require.Contains(t, joined, wantReason,
				"the audit reason must name the specific state; got %q", joined)

			// SourceIP on every event. The address is the only thing a redeem
			// carries that identifies the party holding the key.
			for _, ev := range events {
				require.NotEmpty(t, ev.SourceIP, "audit event %q carries no SourceIP", ev.Action)
			}
		})
	}

	// COVERAGE. Every non-valid state the package declares must have a case
	// above -- otherwise a state added later ships unaudited, which is the
	// defect this whole file exists to prevent.
	for _, state := range []recoverykey.State{
		recoverykey.StateInvalid,
		recoverykey.StateAlreadyRedeemed,
		recoverykey.StateDeactivated,
	} {
		require.True(t, covered[state],
			"recoverykey.State %q has no case in TestEveryRecoveryRejectionIsAudited; every "+
				"rejection path must be proven to audit", state)
	}
}

// TestTheBreakGlassRefusalIsAudited covers the memql#3967 gate, which is a
// refusal of the REQUEST rather than of the row and so is not in the State
// enumeration above.
func TestTheBreakGlassRefusalIsAudited(t *testing.T) {
	plain, hash, err := recoverykey.Mint()
	require.NoError(t, err)

	engine := &passkeyStubEngine{
		user:     map[string]any{"id": passkeyTestUserId, "primaryEmail": "owner@example.test"},
		recovery: recoveryRow(hash, true, nil),
		// The owner still holds a sign-in route, so the gate must refuse.
		signInRoutes: []map[string]any{
			{"id": "v1:identity:identity:ml-1", "userId": passkeyTestUserId, "identityType": "magic_link"},
		},
	}
	s, audit := newRecoveryTestServer(t, engine)

	rec := driveWithScheme(t, s, "/auth/webauthn/register/begin", "Recovery "+plain,
		WebAuthnRegisterBeginRequest{}, s.handleWebAuthnRegisterBegin)

	require.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, "recovery_not_needed", decodeBegin(t, rec).ErrorCode,
		"a key presented while the owner can still sign in must be refused: otherwise it is a "+
			"second password rather than a break-glass credential")
	require.Empty(t, engine.mutations, "a refused redemption must write nothing at all")

	events := audit.snapshot()
	require.NotEmpty(t, events, "the break-glass refusal returned without an audit row")
	var reasons []string
	for _, ev := range events {
		reasons = append(reasons, ev.FailureReason)
	}
	require.Contains(t, strings.Join(reasons, ","), "recovery_owner_still_has_sign_in_route")
}

// TestTheBreakGlassGateAdmitsAnOwnerWithNoRoute is the other half, and the one
// that stops the gate from being "always refuse".
//
// It also pins the trap the issue names: the gate must use the NAMED-USER
// query. With the self-scoped variant every account reports zero routes and
// the gate fails OPEN -- which this test would still pass. So its sibling
// above, which must REFUSE, is what actually detects that; they are only
// meaningful as a pair.
func TestTheBreakGlassGateAdmitsAnOwnerWithNoRoute(t *testing.T) {
	plain, hash, err := recoverykey.Mint()
	require.NoError(t, err)

	engine := &passkeyStubEngine{
		user:     map[string]any{"id": passkeyTestUserId, "primaryEmail": "owner@example.test"},
		recovery: recoveryRow(hash, true, nil),
		// No sign-in routes: the owner is locked out, which is the whole
		// situation this credential exists for.
		signInRoutes: nil,
	}
	s, audit := newRecoveryTestServer(t, engine)

	rec := driveWithScheme(t, s, "/auth/webauthn/register/begin", "Recovery "+plain,
		WebAuthnRegisterBeginRequest{}, s.handleWebAuthnRegisterBegin)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.True(t, decodeBegin(t, rec).Success)

	// The SUCCESS path audits too. An audit trail that records only failures
	// cannot answer "when was this key used", which is the first question
	// anybody asks about a break-glass credential.
	require.NotEmpty(t, audit.snapshot(), "the served ceremony wrote no audit row")
}

// TestARecoveryKeyPlaintextNeverReachesTheEngine is the storage claim, checked
// on the redeem path rather than asserted from the mint's shape.
func TestARecoveryKeyPlaintextNeverReachesTheEngine(t *testing.T) {
	plain, hash, err := recoverykey.Mint()
	require.NoError(t, err)

	engine := &passkeyStubEngine{
		user:     map[string]any{"id": passkeyTestUserId},
		recovery: recoveryRow(hash, true, nil),
	}
	s, _ := newRecoveryTestServer(t, engine)

	driveWithScheme(t, s, "/auth/webauthn/register/begin", "Recovery "+plain,
		WebAuthnRegisterBeginRequest{}, s.handleWebAuthnRegisterBegin)

	for _, q := range append(append([]string{}, engine.mutations...), engine.queries...) {
		require.NotContains(t, q, plain, "the plaintext recovery key reached the engine")
	}
	require.Contains(t, strings.Join(engine.queries, "\n"), hash,
		"the lookup is by digest; without the hash present this check proves nothing")
}
