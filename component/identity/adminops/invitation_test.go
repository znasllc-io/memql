package adminops

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/identity/invitation"
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
