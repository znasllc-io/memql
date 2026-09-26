package githubconnect

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

// NewVerifier returns 256 bits of entropy in RFC 7636's URL-safe alphabet.
func NewVerifier() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Challenge implements S256, the only PKCE method GitHub accepts.
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
