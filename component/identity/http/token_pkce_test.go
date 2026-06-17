package http

import (
	"crypto/sha256"
	"encoding/base64"
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
			name:      "plain happy path",
			challenge: verifier,
			method:    "plain",
			verifier:  verifier,
			wantErr:   false,
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
