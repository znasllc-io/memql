package recoverykey

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

// recoverykey_test.go -- memql#3964.
//
// The assertions here are deliberately about the SHAPE and the STATE MACHINE
// rather than about the engine round trip, which the ceremony tests cover. The
// two properties worth pinning at this level are the ones whose failure is
// silent: a key whose body is the wrong length still looks like a key, and a
// state machine that classifies a spent key as valid still returns a row.

func TestMintShape(t *testing.T) {
	plain, hash, err := Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !strings.HasPrefix(plain, TokenPrefix) {
		t.Errorf("plaintext %q does not carry the %q prefix -- the gitleaks rule, the auth-scheme "+
			"dispatch and every operator grep key off it", plain, TokenPrefix)
	}
	body := strings.TrimPrefix(plain, TokenPrefix)
	if len(body) != TokenBodyChars {
		t.Errorf("body is %d chars, want %d (32 CSPRNG bytes in base64url-no-pad). The acceptance "+
			"criterion names mql_rec_<43>, and .gitleaks.toml's memql-credential-token rule matches "+
			"exactly 43 body characters -- a shorter body is a credential the secret scanner cannot see",
			len(body), TokenBodyChars)
	}
	raw, decErr := base64.RawURLEncoding.DecodeString(body)
	if decErr != nil {
		t.Errorf("body is not base64url-no-pad: %v", decErr)
	} else if len(raw) != tokenRandomBytes {
		t.Errorf("body decodes to %d bytes, want %d", len(raw), tokenRandomBytes)
	}
	if hash != Hash(plain) {
		t.Error("Mint's returned hash is not Hash(plain)")
	}
	if len(hash) != 64 {
		t.Errorf("hash is %d chars, want 64 (SHA-256 hex) -- the convention every sibling "+
			"credential package persists", len(hash))
	}
	if strings.Contains(hash, body) {
		t.Error("the hash contains the plaintext body, which would make the stored form recoverable")
	}
}

func TestMintIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 256; i++ {
		plain, _, err := Mint()
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		if seen[plain] {
			t.Fatalf("Mint produced a duplicate on iteration %d -- the entropy source is not what "+
				"this package believes it is", i)
		}
		seen[plain] = true
	}
}

func TestHashAndPrefix(t *testing.T) {
	if Hash("") != "" {
		t.Error("Hash(\"\") must be empty, so an absent bearer cannot resolve a row by hashing to a constant")
	}
	if Hash("   ") != "" {
		t.Error("Hash of whitespace must be empty for the same reason")
	}
	plain, hash, _ := Mint()
	if Hash("  "+plain+"  ") != hash {
		t.Error("Hash must trim surrounding whitespace -- a header value routinely carries it")
	}

	for _, tc := range []struct {
		in   string
		want bool
	}{
		{plain, true},
		{"  " + plain, true},
		{"mql_rec_", true}, // prefix-only: the shape check is not a validity check
		{"mql_enr_abc", false},
		{"mql_pat_abc", false},
		{"", false},
		{"Recovery " + plain, false}, // the scheme is stripped before this is called
	} {
		if got := IsRecoveryKey(tc.in); got != tc.want {
			t.Errorf("IsRecoveryKey(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestStateMachine pins the classification, including the ordering rule that
// makes a spent key report "already redeemed" rather than "deactivated".
func TestStateMachine(t *testing.T) {
	now := time.Now().UTC()

	for _, tc := range []struct {
		name string
		row  Row
		want State
	}{
		{"fresh", Row{Active: true}, StateValid},
		{"claimed but unspent", Row{Active: true, ClaimedAt: now}, StateValid},
		{"rotated out, never used", Row{Active: false}, StateDeactivated},
		{
			// Redemption deactivates in the same write, so this is what a
			// spent row ACTUALLY looks like -- both conditions true at once.
			// "You already used this" is the true and useful answer; reporting
			// "this was retired" would be technically true and would misdescribe
			// what happened.
			name: "redeemed (and therefore also inactive)",
			row:  Row{Active: false, RedeemedAt: now},
			want: StateAlreadyRedeemed,
		},
		{
			// Should not occur -- but if the two writes are ever split apart,
			// the classification must still lead with the redemption.
			name: "redeemed but somehow still active",
			row:  Row{Active: true, RedeemedAt: now},
			want: StateAlreadyRedeemed,
		},
	} {
		if got := tc.row.State(); got != tc.want {
			t.Errorf("%s: State() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestClaimedAndRedeemedPredicates(t *testing.T) {
	var zero Row
	if zero.IsClaimed() {
		t.Error("a row with no claimedAt must not read as claimed -- an unclaimed key is re-mintable, " +
			"and mistaking one for claimed strands a key nobody holds")
	}
	if zero.IsRedeemed() {
		t.Error("a row with no redeemedAt must not read as redeemed")
	}
	stamped := Row{ClaimedAt: time.Now(), RedeemedAt: time.Now()}
	if !stamped.IsClaimed() || !stamped.IsRedeemed() {
		t.Error("stamped timestamps must read as claimed/redeemed")
	}
}

// TestNoExpirySurface is a design pin, not a behaviour test.
//
// A recovery key deliberately has no expiry: it is minted when the cluster is
// claimed and used, if ever, on the worst day of the operator's year, so a key
// that had quietly expired in the interim would be indistinguishable from one
// that never worked. If somebody adds an ExpiresAt to Row, this test is where
// they should have to argue for it -- and StateExpired would have to appear
// alongside it, because a row that expired silently is the failure this
// package exists to avoid.
func TestNoExpirySurface(t *testing.T) {
	for _, s := range []State{StateValid, StateInvalid, StateAlreadyRedeemed, StateDeactivated} {
		if strings.Contains(string(s), "expire") {
			t.Errorf("state %q suggests an expiry concept; see the package comment for why a "+
				"recovery key does not expire, and change that reasoning deliberately if it is wrong", s)
		}
	}
}

func TestCanonicalIdRoundTrip(t *testing.T) {
	const slug = "abc123"
	full := CanonicalId(slug)
	if !strings.HasPrefix(full, canonicalIdPrefix) {
		t.Fatalf("CanonicalId(%q) = %q, missing the %q prefix", slug, full, canonicalIdPrefix)
	}
	if CanonicalId(full) != full {
		t.Error("CanonicalId must be idempotent -- double-prefixing produces an id that resolves nothing")
	}
	if BareSlug(full) != slug {
		t.Errorf("BareSlug(%q) = %q, want %q", full, BareSlug(full), slug)
	}
	if BareSlug(slug) != slug {
		t.Error("BareSlug must be idempotent on an already-bare slug")
	}
}

// TestStoreRefusesUnwiredEngine covers the nil paths, so a misconfigured store
// reports itself instead of panicking inside a recovery ceremony.
func TestStoreRefusesUnwiredEngine(t *testing.T) {
	var s *Store
	if err := s.Create(t.Context(), "i", "u", "h", "m", "", ""); err == nil {
		t.Error("Create on a nil store must error")
	}
	if err := s.Claim(t.Context(), "i", "ip", time.Now()); err == nil {
		t.Error("Claim on a nil store must error")
	}
	if err := s.Redeem(t.Context(), "i", "ip", time.Now()); err == nil {
		t.Error("Redeem on a nil store must error")
	}
	if err := s.Deactivate(t.Context(), "i"); err == nil {
		t.Error("Deactivate on a nil store must error")
	}
	if _, err := s.LookupByHash(t.Context(), "h"); err == nil {
		t.Error("LookupByHash on a nil store must error")
	}
	if _, err := s.ActiveForUser(t.Context(), "u"); err == nil {
		t.Error("ActiveForUser on a nil store must error")
	}
}

// TestResolveShortCircuitsMalformedInput asserts the redeem path never reaches
// the engine for input that cannot be a recovery key. A scanner spraying
// garbage at the endpoint should cost no database round trips.
func TestResolveShortCircuitsMalformedInput(t *testing.T) {
	// Engine deliberately nil: reaching it would panic, which is the assertion.
	s := &Store{}
	for _, in := range []string{"", "   ", "mql_enr_" + strings.Repeat("a", 43), "not-a-token"} {
		row, state, err := s.Resolve(t.Context(), in)
		if err != nil {
			t.Errorf("Resolve(%q) errored: %v -- malformed input is not a server fault", in, err)
		}
		if row != nil {
			t.Errorf("Resolve(%q) returned a row", in)
		}
		if state != StateInvalid {
			t.Errorf("Resolve(%q) = %q, want %q", in, state, StateInvalid)
		}
	}
}
