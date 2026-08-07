package memql

import (
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// forwarded_authority_source_test.go -- memql#3205.
//
// THE SOURCE is what the previous attempt got wrong, and it is what this file
// pins. Its test asserted "the packed claims map contains these two strings"
// without ever calling the function that produced them, so it passed against
// the defect: `forwardedAuthClaims` read `s.stream.Context()`, and a gRPC
// stream context is fixed at STREAM-OPEN while badge grants arrive MID-STREAM
// via RotateAuth.
//
// So these drive a REAL rotation -- real KeyManager, real JWTIssuer, real JWKS
// server, real verifier, real handleRotateAuth -- and then ask what the
// producer would actually put on the wire.

// After a real badge rotation the forwarded assertion must carry the CLAMPED
// role plus the class, ceiling and expiry that let the receiver prove it.
func TestForwardedPrincipalAfterARealBadgeRotationIsClamped(t *testing.T) {
	_, issueBadge, v := newRotateAuthFixtureWithBadge(t)
	svc := &service{verifier: v}
	sess, cs := newSessionForRotate(svc)

	const operator = "v1:identity:user:operator-1"
	badge := issueBadge(operator, "reader", 5*time.Minute)

	if err := sess.handleRotateAuth(
		&memqlv1.MemqlClientMessage{MessageId: "rot-1"},
		&memqlv1.RotateAuthMsg{AccessToken: badge},
	); err != nil {
		t.Fatalf("handleRotateAuth: %v", err)
	}
	if res := cs.lastSent().GetRotateAuthResult(); res == nil || !res.GetOk() {
		t.Fatalf("badge rotation was rejected: %+v", res)
	}

	// THE CONTROL THAT MAKES THIS TEST ABOUT THE SOURCE. The stream context is
	// the pre-rotate one and cannot have been updated by the rotation -- so if
	// the assertion below carries a ceiling, it did NOT come from here. Without
	// this, the test cannot distinguish the two sources, which is exactly how
	// the previous attempt's test passed against the defect.
	streamClaims, _ := auth.ClaimsFromContext(sess.stream.Context())
	if got := claimString(streamClaims, "role_ceiling"); got != "" {
		t.Fatalf("control broken: the stream context carries role_ceiling=%q, so this test can no "+
			"longer prove the assertion was sourced from the SESSION rather than the context", got)
	}

	principal, err := sess.forwardedPrincipal()
	if err != nil {
		t.Fatalf("forwardedPrincipal after a badge rotation: %v", err)
	}
	a := principal.Authority

	if a.CredentialClass != auth.ForwardedClassBadge {
		t.Errorf("credential class = %q, want %q.\n\n"+
			"A mid-stream badge grant that forwards no class is indistinguishable from 'not a "+
			"badge session', and the receiver resolves the operator's UNCLAMPED stored role. "+
			"That is memql#2876's measured escalation.", a.CredentialClass, auth.ForwardedClassBadge)
	}
	if a.RoleCeiling != auth.RoleReader {
		t.Errorf("role ceiling = %q, want reader -- sourced from the rotation, which the stream "+
			"context above proves it could not have come from", a.RoleCeiling)
	}
	if a.ExpiresAt.IsZero() {
		t.Error("no expiry on the assertion. The direct path gates every envelope on the grant's " +
			"exp; without it on the wire the worker cannot enforce expiry even if it tries")
	}
	if a.Role != auth.RoleReader {
		t.Errorf("role = %q, want the CLAMPED reader", a.Role)
	}

	// And the end the receiver sees.
	access, err := auth.VerifyForwardedAuthority(a, time.Now())
	if err != nil {
		t.Fatalf("the receiver refused a well-formed badge assertion: %v", err)
	}
	if access.IsClusterOwner() {
		t.Error("the forwarded operator arrives as CLUSTER OWNER.\n\n" +
			"This is the regression the previous attempt shipped: an operator on a kiosk with a " +
			"reader ceiling, chatting BFF -> agent, binding an owner actor for the whole turn.")
	}
	if access.Role != auth.RoleReader {
		t.Errorf("bound role = %q, want reader", access.Role)
	}
}

// Expiry parity: when the direct path's gate says a grant is expired, the mesh
// verifier must say so too. Two gates over one credential that disagree is how
// a walked-away kiosk keeps working on one path and not the other.
func TestForwardedAuthorityExpiresWithTheDirectPathGate(t *testing.T) {
	_, issueBadge, v := newRotateAuthFixtureWithBadge(t)
	svc := &service{verifier: v}
	sess, cs := newSessionForRotate(svc)

	badge := issueBadge("v1:identity:user:operator-2", "reader", 5*time.Minute)
	if err := sess.handleRotateAuth(
		&memqlv1.MemqlClientMessage{MessageId: "rot-2"},
		&memqlv1.RotateAuthMsg{AccessToken: badge},
	); err != nil {
		t.Fatalf("handleRotateAuth: %v", err)
	}
	if res := cs.lastSent().GetRotateAuthResult(); res == nil || !res.GetOk() {
		t.Fatalf("badge rotation was rejected: %+v", res)
	}

	probe := &memqlv1.MemqlClientMessage{
		MessageId: "probe",
		Payload:   &memqlv1.MemqlClientMessage_ExecuteQuery{ExecuteQuery: &memqlv1.ExecuteQueryMsg{}},
	}

	// Both gates are asked about the same credential at the same instant
	// ("now"), once while the grant is live and once after it has lapsed. The
	// session's stamp is what both read from, so this compares the two
	// implementations rather than two different facts.
	for _, tc := range []struct {
		name        string
		expiry      time.Time
		wantAllowed bool
	}{
		{"a live grant", time.Now().Add(5 * time.Minute), true},
		{"a lapsed grant", time.Now().Add(-time.Second), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess.accessMu.Lock()
			sess.stampAuthorityLocked("badge", "reader", tc.expiry)
			sess.accessMu.Unlock()

			principal, err := sess.forwardedPrincipal()
			if err != nil {
				t.Fatalf("forwardedPrincipal: %v", err)
			}

			directAllowed := sess.badgeGate(probe) == badgeGateAllow
			_, meshErr := auth.VerifyForwardedAuthority(principal.Authority, time.Now())
			meshAllowed := meshErr == nil

			if directAllowed != tc.wantAllowed {
				t.Fatalf("control: the DIRECT gate allowed=%v, want %v", directAllowed, tc.wantAllowed)
			}
			if meshAllowed != directAllowed {
				t.Errorf("the mesh verifier allowed=%v while the direct gate allowed=%v (err=%v).\n\n"+
					"Two gates over one credential that disagree is how a walked-away kiosk's "+
					"expired grant gets rejected on the client's own stream and honoured on every "+
					"forwarded AiChat / CallTool -- the asymmetry memql#3205 exists to remove.",
					meshAllowed, directAllowed, meshErr)
			}
		})
	}
}

// An ordinary (non-badge) session must state a class rather than omitting one,
// and must not carry a ceiling nothing will enforce.
func TestForwardedPrincipalStatesAClassForAnOrdinarySession(t *testing.T) {
	issue, v := newRotateAuthFixture(t)
	svc := &service{verifier: v}
	sess, cs := newSessionForRotate(svc)

	tok := issue("v1:identity:user:plain-1", "plain@example.com", "writer")
	if err := sess.handleRotateAuth(
		&memqlv1.MemqlClientMessage{MessageId: "rot-3"},
		&memqlv1.RotateAuthMsg{AccessToken: tok},
	); err != nil {
		t.Fatalf("handleRotateAuth: %v", err)
	}
	if res := cs.lastSent().GetRotateAuthResult(); res == nil || !res.GetOk() {
		t.Fatalf("rotation was rejected: %+v", res)
	}

	principal, err := sess.forwardedPrincipal()
	if err != nil {
		t.Fatalf("forwardedPrincipal: %v", err)
	}
	if principal.Authority.CredentialClass == "" {
		t.Error("credential class is empty for an ordinary session. 'Not a badge session' must be " +
			"a VALUE -- an absent class is the exact ambiguity that makes a lost ceiling " +
			"undetectable on the receiver")
	}
	if principal.Authority.RoleCeiling != "" {
		t.Errorf("a non-badge session carries ceiling %q. A ceiling its class will not enforce "+
			"lets an assertion LOOK clamped", principal.Authority.RoleCeiling)
	}
	if _, err := auth.VerifyForwardedAuthority(principal.Authority, time.Now()); err != nil {
		t.Errorf("the receiver refused an ordinary session's assertion: %v", err)
	}
}
