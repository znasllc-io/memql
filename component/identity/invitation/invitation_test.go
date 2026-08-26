package invitation

import (
	"strings"
	"testing"
	"time"
)

// The package had NO test file before memql#4601, which is part of how the
// redemption path shipped broken: the token half was never exercised either.

func TestMintProducesThePrefixedShapeAndOnlyTheDigestIsDerivable(t *testing.T) {
	plain, hash, err := Mint()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !strings.HasPrefix(plain, TokenPrefix) {
		t.Errorf("token %q does not carry the %q prefix", plain, TokenPrefix)
	}
	if body := strings.TrimPrefix(plain, TokenPrefix); len(body) != TokenBodyChars {
		t.Errorf("token body is %d chars, want %d", len(body), TokenBodyChars)
	}
	if hash != Hash(plain) {
		t.Error("the returned digest is not the digest of the returned plaintext")
	}
	if strings.Contains(hash, plain) {
		t.Error("the digest contains the plaintext, which would make the stored form a credential")
	}
}

// The redeem path hashes what it is given and looks the row up by that digest.
// If Hash were not stable across calls, a token would stop matching its own row
// -- which would look exactly like an invalid invitation and be diagnosed as
// one.
func TestHashIsStableAndTrimsSurroundingSpace(t *testing.T) {
	plain, _, err := Mint()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if Hash(plain) != Hash(plain) {
		t.Error("Hash is not deterministic")
	}
	if Hash(" "+plain+"\n") != Hash(plain) {
		t.Error("a token pasted with surrounding whitespace does not resolve to its own row")
	}
	if Hash("") != "" {
		t.Error("the empty token hashes to something, which would let an empty submission match a row")
	}
}

func TestMintDoesNotRepeat(t *testing.T) {
	seen := make(map[string]bool, 64)
	for i := 0; i < 64; i++ {
		plain, _, err := Mint()
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if seen[plain] {
			t.Fatalf("Mint returned a duplicate token on iteration %d", i)
		}
		seen[plain] = true
	}
}

// The TTLs came down in memql#4601 (7d -> 72h default, 30d -> 7d ceiling)
// because the invitation is a forwardable bearer and every extra hour is an
// hour in which a forward stays live. Asserted as concrete durations rather
// than against the constants themselves: a test that reads
// `DefaultTTL == DefaultTTL` would keep passing through an accidental edit,
// which is the only way this value is ever likely to change.
func TestTheTTLsAreTheTightenedOnes(t *testing.T) {
	if DefaultTTL != 72*time.Hour {
		t.Errorf("DefaultTTL = %v, want 72h", DefaultTTL)
	}
	if MaxTTL != 7*24*time.Hour {
		t.Errorf("MaxTTL = %v, want 7 days", MaxTTL)
	}
	if DefaultTTL > MaxTTL {
		t.Error("the default is above the ceiling, so every unqualified issue would be clamped")
	}
}

func TestClampTTL(t *testing.T) {
	if got := ClampTTL(0); got != DefaultTTL {
		t.Errorf("a zero request = %v, want the default %v", got, DefaultTTL)
	}
	if got := ClampTTL(-time.Hour); got != DefaultTTL {
		t.Errorf("a negative request = %v, want the default %v", got, DefaultTTL)
	}
	if got := ClampTTL(MaxTTL * 10); got != MaxTTL {
		t.Errorf("an over-ceiling request = %v, want it clamped to %v", got, MaxTTL)
	}
	if got := ClampTTL(time.Hour); got != time.Hour {
		t.Errorf("an in-range request = %v, want it preserved", got)
	}
}

// The step-up threshold decides which invitations cost a mailbox round trip.
// Both directions matter: a privileged role that slips through pays nothing,
// and an ordinary role that trips it makes every normal invitation slow.
func TestRequiresStepUp(t *testing.T) {
	for _, role := range []string{"owner", "admin", "OWNER", "  Admin  "} {
		if !RequiresStepUp(role) {
			t.Errorf("RequiresStepUp(%q) = false, want true -- a privileged invitation would skip the mailbox check", role)
		}
	}
	for _, role := range []string{"developer", "writer", "reader", "", "   "} {
		if RequiresStepUp(role) {
			t.Errorf("RequiresStepUp(%q) = true, want false -- an ordinary invitation would be sent the slow way", role)
		}
	}
}

// An unrecognised role must not step up. It cannot have been granted by
// IssueUserInvitation, which refuses any role outside its own table, so a value
// arriving here that is neither empty nor known is a row this build does not
// understand rather than a privileged one. Treating it as privileged would make
// an unknown value quietly more powerful than a known one.
func TestRequiresStepUpIgnoresUnknownRoles(t *testing.T) {
	for _, role := range []string{"superuser", "root", "owner-ish", "administrator"} {
		if RequiresStepUp(role) {
			t.Errorf("RequiresStepUp(%q) = true, want false", role)
		}
	}
}

func TestIsInvitationToken(t *testing.T) {
	plain, _, err := Mint()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !IsInvitationToken(plain) {
		t.Error("a freshly minted token is not recognised as one")
	}
	if !IsInvitationToken("  " + plain + "  ") {
		t.Error("a token pasted with surrounding whitespace is not recognised")
	}
	// mql_enr_ is the enrolment prefix. The two are deliberately distinct so a
	// support conversation about "the token in my email" can be settled by
	// looking at it, and so the redeem paths cannot be crossed.
	if IsInvitationToken("mql_enr_abc") {
		t.Error("an enrolment token is being treated as an invitation token")
	}
	if IsInvitationToken("") {
		t.Error("the empty string is being treated as an invitation token")
	}
}
