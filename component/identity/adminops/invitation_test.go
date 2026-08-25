package adminops

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/invitation"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// The issuing half of user invitations (memql#4270), and the policy it has to
// honour. The redeem half is tested in component/identity/registration.

func TestInvitationTokenIsHashedAndShapedLikeItsSiblings(t *testing.T) {
	plain, hash, err := invitation.Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !strings.HasPrefix(plain, invitation.TokenPrefix) {
		t.Errorf("token %q does not carry the %q prefix", plain, invitation.TokenPrefix)
	}
	if got := len(strings.TrimPrefix(plain, invitation.TokenPrefix)); got != invitation.TokenBodyChars {
		t.Errorf("token body is %d chars, want %d", got, invitation.TokenBodyChars)
	}
	// The hash is what persists; the plaintext must not be derivable from it.
	if hash == plain || len(hash) != 64 {
		t.Errorf("hash %q is not a sha256 hex digest of the plaintext", hash)
	}
	if invitation.Hash(plain) != hash {
		t.Error("Hash(plain) does not reproduce the digest Mint returned")
	}
	// Two mints must not collide -- 32 CSPRNG bytes, but assert it rather than
	// assume it, because a broken entropy source is silent.
	other, _, _ := invitation.Mint()
	if other == plain {
		t.Error("two mints produced the same token")
	}
}

func TestInvitationTTLIsClampedNotRefused(t *testing.T) {
	if got := invitation.ClampTTL(0); got != invitation.DefaultTTL {
		t.Errorf("zero TTL = %v, want the default %v", got, invitation.DefaultTTL)
	}
	if got := invitation.ClampTTL(invitation.MaxTTL * 10); got != invitation.MaxTTL {
		t.Errorf("an over-ceiling request = %v, want it clamped to %v", got, invitation.MaxTTL)
	}
	// A caller asking for too much still wants a link. Silently issuing one
	// that OUTLIVES the ceiling is the only outcome that would be wrong.
	if invitation.ClampTTL(invitation.MaxTTL*10) > invitation.MaxTTL {
		t.Error("clamping produced a lifetime above the ceiling")
	}
}

// The domain allowlist an invitation must satisfy under domain_restricted.
// Issuing a link the recipient cannot redeem is worse than refusing: they only
// find out after clicking.
func TestDomainAllowedMatchesTheAddressHost(t *testing.T) {
	domains := []string{"example.com", "@second.test"}
	for _, ok := range []string{"a@example.com", "A@EXAMPLE.COM", "b@second.test"} {
		if !domainAllowed(strings.ToLower(ok), domains) {
			t.Errorf("%q should be allowed by %v", ok, domains)
		}
	}
	for _, bad := range []string{"a@elsewhere.test", "a@notexample.com", "no-at-sign", "trailing@"} {
		if domainAllowed(strings.ToLower(bad), domains) {
			t.Errorf("%q should NOT be allowed by %v", bad, domains)
		}
	}
}

// An unset policy seam must read as `open`, never as a restriction. A node that
// cannot resolve the policy inventing invite_only would refuse invitations on a
// cluster that never asked for that.
func TestUnsetRegistrationPolicyDegradesToOpen(t *testing.T) {
	s := &Service{}
	mode, domains := s.registrationPolicy(context.Background())
	if mode != "open" {
		t.Errorf("mode = %q, want %q", mode, "open")
	}
	if len(domains) != 0 {
		t.Errorf("domains = %v, want none", domains)
	}
}

// An inviter cannot grant above their own role. Without this an admin could
// mint an owner invitation and hold the cluster through the account it creates.
func TestRoleRankOrdersEveryClusterRole(t *testing.T) {
	for _, role := range []string{"reader", "writer", "developer", "admin", "owner"} {
		if roleRank[role] == 0 {
			t.Errorf("%q has no rank, so it would compare as lowest and could be used to escalate", role)
		}
	}
	if roleRank["owner"] <= roleRank["admin"] {
		t.Error("owner must outrank admin")
	}
	if roleRank["admin"] <= roleRank["reader"] {
		t.Error("admin must outrank reader")
	}
	// An unknown role ranks zero, which is BELOW every real role -- so it can
	// never pass the "not above your own" comparison.
	if roleRank["superuser"] != 0 {
		t.Error("an unknown role must rank lowest")
	}
}

// The link carries a plaintext bearer, so it must be https or not exist.
func TestInvitationURLPutsTheTokenWhereTheLoginFormReadsIt(t *testing.T) {
	got := invitationURL("https://identity.example.com", "mql_inv_abc-123")
	if !strings.HasPrefix(got, "https://identity.example.com/login?invitation=") {
		t.Errorf("link %q does not land on the login page's invitation field", got)
	}
	if !strings.Contains(got, "mql_inv_abc-123") {
		t.Errorf("link %q does not carry the token", got)
	}
}

// ===========================================================================
// DELIVERY (memql#4584)
// ===========================================================================
// Issuing an invitation used to send nothing at all. The row was written, the
// link was returned, the portal's button said "Send the invitation" and no
// code path in the process could have produced an email. These tests pin the
// three properties that fix has to keep, and the second one is the property
// most likely to regress: it is the one a well-meaning refactor breaks by
// treating a delivery error like every other error in the function.

// issuingEngine accepts every mutation and records it, so an invitation can be
// issued end-to-end. The opposite of gate_test.go's recordingEngine, which
// refuses everything in order to prove the gate is reached; here the gate is
// not what is under test and the call has to get past it.
type issuingEngine struct{ queries []string }

func (e *issuingEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	e.queries = append(e.queries, q)
	return nil, nil
}

// capturedInvite is one call into the mail seam.
type capturedInvite struct {
	in    InvitationEmail
	calls int
}

// newIssuingService builds a Service that can actually issue, with a mail seam
// that records what it was asked to send and fails when failWith is non-nil.
func newIssuingService(t *testing.T, failWith error) (*Service, *issuingEngine, *capturingAudit, *capturedInvite) {
	t.Helper()
	eng := &issuingEngine{}
	audit := &capturingAudit{}
	sent := &capturedInvite{}
	svc, err := New(&Service{
		Engine: eng,
		Audit:  audit,
		IdentityBaseURL: func(context.Context) string {
			return "https://identity.example.test"
		},
		SendInvitationEmail: func(_ context.Context, in InvitationEmail) error {
			sent.calls++
			sent.in = in
			return failWith
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc, eng, audit, sent
}

// The whole point of memql#4584: issuing an invitation must actually try to
// tell the person who was invited.
func TestIssuingAnInvitationSendsItToTheInvitee(t *testing.T) {
	svc, _, _, sent := newIssuingService(t, nil)

	res := svc.IssueUserInvitation(ctxAs(auth.RoleOwner), UserInvitation{
		Email: "Invitee@Example.test",
		Role:  "admin",
	})

	if !res.OK {
		t.Fatalf("issue failed: code=%d %s", res.Code, res.ErrorMessage)
	}
	if sent.calls != 1 {
		t.Fatalf("mail seam called %d times, want exactly 1 -- an invitation that sends nothing is the defect this closes", sent.calls)
	}
	if sent.in.To != "invitee@example.test" {
		t.Errorf("sent to %q, want the normalized invitee address", sent.in.To)
	}
	// The message has to be able to say who invited them, to what, and until
	// when. All three already exist on the call; none needs new storage.
	if sent.in.InviterName == "" {
		t.Error("no inviter on the email input, so the message cannot say who invited them")
	}
	if sent.in.Role != "admin" {
		t.Errorf("role on the email input = %q, want %q", sent.in.Role, "admin")
	}
	if sent.in.ExpiresAt.IsZero() {
		t.Error("no expiry on the email input, so the message cannot say when the link dies")
	}
	// The link must reach the message -- an invitation email with no link is
	// worse than none, because the recipient waits for a second one.
	if sent.in.LinkURL != res.InvitationURL {
		t.Errorf("emailed link %q is not the link returned to the caller %q", sent.in.LinkURL, res.InvitationURL)
	}
	if !res.InvitationEmailSent {
		t.Error("InvitationEmailSent is false after a successful send")
	}
	if res.InvitationEmailError != "" {
		t.Errorf("InvitationEmailError = %q on a successful send", res.InvitationEmailError)
	}
}

// THE PROPERTY MOST LIKELY TO REGRESS.
//
// A send failure must not fail the invitation. The row is already committed
// and the LINK is what actually admits somebody -- it is returned on this
// Result and can never be fetched again, because only its digest was stored.
// Dropping it because Graph was briefly unreachable would destroy a perfectly
// good credential and leave a pending row nobody can use.
func TestAFailingSenderStillMintsTheRowAndStillReturnsTheLink(t *testing.T) {
	svc, eng, audit, sent := newIssuingService(t, errors.New("graph: 503 service unavailable"))

	res := svc.IssueUserInvitation(ctxAs(auth.RoleOwner), UserInvitation{
		Email: "invitee@example.test",
		Role:  "writer",
	})

	if !res.OK {
		t.Fatalf("a delivery failure failed the whole invitation: code=%d %s", res.Code, res.ErrorMessage)
	}
	if res.InvitationURL == "" {
		t.Fatal("the link was withheld after a send failure -- it can never be fetched again, so it is now lost")
	}
	if sent.calls != 1 {
		t.Fatalf("mail seam called %d times, want 1", sent.calls)
	}

	// The row still has to have been written, or the link redeems nothing.
	var minted bool
	for _, q := range eng.queries {
		if strings.Contains(q, "createUserInvitation") {
			minted = true
		}
	}
	if !minted {
		t.Error("no createUserInvitation mutation ran, so the returned link cannot be redeemed")
	}

	// And the failure must be RETRIEVABLE, not just logged. This is what lets
	// the portal tell the operator to deliver the link by hand.
	if res.InvitationEmailSent {
		t.Error("InvitationEmailSent is true after the sender returned an error")
	}
	if !strings.Contains(res.InvitationEmailError, "503") {
		t.Errorf("InvitationEmailError = %q, want the sender's own reason", res.InvitationEmailError)
	}
	if !strings.Contains(res.Message, "could not be delivered") {
		t.Errorf("Message = %q, want it to say delivery failed so an operator does not walk away believing the invitee was told", res.Message)
	}

	// The trail records the delivery verdict, because this event is what an
	// operator greps when somebody says an invitation never arrived.
	ev := lastInvitationEvent(t, audit)
	if ev.Detail["emailDelivered"] != false {
		t.Errorf("audit detail emailDelivered = %v, want false", ev.Detail["emailDelivered"])
	}
	if ev.Detail["emailError"] == nil {
		t.Error("audit detail carries no emailError, so the trail cannot say why nothing arrived")
	}
	// The invitation itself succeeded, and the trail must not claim otherwise:
	// pendingUserInvitations will list this row either way.
	if ev.Outcome != identity.AuditOutcomeSuccess {
		t.Errorf("audit outcome = %v, want success -- the invitation WAS issued", ev.Outcome)
	}
}

// The link is a bearer credential and v1:identity:auditEvent is append-only.
// Anything written there cannot be redacted later, so the link must never
// reach it -- not in the detail map, not in the target fields, not in the
// failure reason. The function's own comment promises this; the promise is
// worth nothing unless something checks it.
func TestTheInvitationLinkNeverEntersTheAuditTrail(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure error
	}{
		{"delivered", nil},
		// The failing path is checked too because it is the one that adds a
		// reason string to the event, and a reason string is exactly where a
		// careless change would interpolate the link.
		{"delivery failed", errors.New("graph: 401 unauthorized")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, audit, _ := newIssuingService(t, tc.failure)

			res := svc.IssueUserInvitation(ctxAs(auth.RoleOwner), UserInvitation{
				Email: "invitee@example.test",
			})
			if res.InvitationURL == "" {
				t.Fatal("no link was issued, so this test would pass vacuously")
			}
			// The token, not merely the whole URL: a detail field holding just
			// the plaintext token would leak exactly as badly.
			token := strings.TrimPrefix(res.InvitationURL, "https://identity.example.test/login?invitation=")
			if token == "" || token == res.InvitationURL {
				t.Fatalf("could not isolate the token from %q", res.InvitationURL)
			}

			for _, ev := range audit.events {
				blob, err := json.Marshal(ev)
				if err != nil {
					t.Fatalf("marshal audit event: %v", err)
				}
				if strings.Contains(string(blob), token) {
					t.Errorf("the invitation token reached the audit trail, which is append-only and cannot be redacted: %s", blob)
				}
			}
		})
	}
}

// lastInvitationEvent returns the user_invitation_issued event, failing the
// test when none was written.
func lastInvitationEvent(t *testing.T, audit *capturingAudit) identity.AuditEvent {
	t.Helper()
	for i := len(audit.events) - 1; i >= 0; i-- {
		if audit.events[i].Action == "user_invitation_issued" {
			return audit.events[i]
		}
	}
	t.Fatal("no user_invitation_issued audit event was written")
	return identity.AuditEvent{}
}
