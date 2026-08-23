package http

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"testing"
)

// s256Challenge derives the PKCE S256 code_challenge for a verifier:
// base64url(sha256(verifier)) with no padding (RFC 7636 §4.2).
func s256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func TestVerifyPKCE(t *testing.T) {
	// A realistic 43-char (minimum) high-entropy verifier.
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	s256 := s256Challenge(verifier)

	tests := []struct {
		name      string
		challenge string
		method    string
		verifier  string
		wantErr   bool
	}{
		{
			name:      "S256 happy path",
			challenge: s256,
			method:    "S256",
			verifier:  verifier,
			wantErr:   false,
		},
		{
			name:      "empty method defaults to S256",
			challenge: s256,
			method:    "",
			verifier:  verifier,
			wantErr:   false,
		},
		{
			name:      "S256 missing verifier",
			challenge: s256,
			method:    "S256",
			verifier:  "",
			wantErr:   true,
		},
		{
			name:      "S256 wrong verifier",
			challenge: s256,
			method:    "S256",
			verifier:  "this-is-not-the-right-verifier-value-000000",
			wantErr:   true,
		},
		{
			// RETIRED (memql#4303). `plain` used to pass here. It puts the
			// verifier in the challenge, so anyone who can read the
			// authorization request can redeem the code -- PKCE that is off
			// while looking, in a log or a config, exactly like PKCE that is
			// on. RFC 7636 SS7.2 says reject it; no MemQL client used it.
			name:      "plain is refused even when it matches",
			challenge: verifier,
			method:    "plain",
			verifier:  verifier,
			wantErr:   true,
		},
		{
			name:      "plain mismatch",
			challenge: verifier,
			method:    "plain",
			verifier:  "some-other-verifier",
			wantErr:   true,
		},
		{
			name:      "plain missing verifier",
			challenge: verifier,
			method:    "plain",
			verifier:  "",
			wantErr:   true,
		},
		{
			name:      "unsupported method",
			challenge: s256,
			method:    "S512",
			verifier:  verifier,
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verifyPKCE(tt.challenge, tt.method, tt.verifier)
			if tt.wantErr && err == nil {
				t.Fatalf("verifyPKCE(%q, %q, %q) = nil, want error", tt.challenge, tt.method, tt.verifier)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("verifyPKCE(%q, %q, %q) = %v, want nil", tt.challenge, tt.method, tt.verifier, err)
			}
		})
	}
}

// TestPlainPKCEIsRefusedWithItsOwnReason pins BOTH halves of the memql#4303
// retirement: that `plain` is refused at all, and that the refusal is
// distinguishable from an ordinary verifier mismatch.
//
// The second half is not decoration. /oauth/token audits the refusal, and an
// operator reading `pkce_failed` learns that somebody got the verifier
// wrong, while `plain_not_allowed` tells them a client is still trying to
// use a method that was removed. Folding the two together would hide a
// client that needs migrating inside noise that needs nothing.
func TestPlainPKCEIsRefusedWithItsOwnReason(t *testing.T) {
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"

	err := verifyPKCE(verifier, "plain", verifier)
	if err == nil {
		t.Fatal("verifyPKCE accepted code_challenge_method=plain.\n" +
			"plain means the challenge IS the verifier, so a code is redeemable by anyone who saw " +
			"the authorization request -- PKCE that is structurally off while every log and config " +
			"says it is on. This must not come back.")
	}
	if !errors.Is(err, errPKCEPlainNotAllowed) {
		t.Errorf("plain refusal returned %v, want errPKCEPlainNotAllowed -- /oauth/token audits "+
			"this refusal as plain_not_allowed, and folding it into a generic mismatch hides a "+
			"client that needs migrating inside noise that needs nothing", err)
	}

	// And S256 still works, so the test above is not passing because
	// verifyPKCE refuses everything.
	if err := verifyPKCE(s256Challenge(verifier), "S256", verifier); err != nil {
		t.Fatalf("S256 verification broke: %v", err)
	}
}
