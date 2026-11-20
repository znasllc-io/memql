package calls

import (
	"crypto/sha256"
	"encoding/hex"
)

// CredentialDigest is the persisted 256-bit fingerprint of a public WebAuthn
// credential ID. It is not the credential row identity or an access token; its
// exact encoding must remain stable so already enrolled credentials still match.
func CredentialDigest(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}
