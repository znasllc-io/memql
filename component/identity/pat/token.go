// Package pat implements Personal Access Tokens for CLI clients.
//
// PATs are long-lived bearer credentials, owned by a v1:identity:user,
// stored as v1:identity:identity rows of identityType="api_key". The
// plaintext token is shown to the user ONCE at generation; only its
// SHA-256 hex hash lives in the database.
//
// Wire format:
//
//	mql_pat_<43 base64url chars>      // 32 random bytes, b64url-no-pad
//
// The mql_pat_ prefix lets the gRPC interceptor distinguish PATs from
// JWTs without trying to parse the token body. Verifier.VerifyToken
// uses the same convention to fan out to verifyPAT vs verifyJWT.
package pat

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
)

// TokenPrefix is the literal prefix every PAT plaintext carries.
// Picking a memorable prefix makes leaked tokens trivially greppable.
const TokenPrefix = "mql_pat_"

// tokenRandomBytes is the entropy size of the random body (excluding
// the prefix). 32 bytes = 256 bits, encoded as 43 base64url-no-pad
// characters. Comfortably above the 128-bit security floor.
const tokenRandomBytes = 32

// ErrEmptyToken is returned by Hash when the input is empty.
var ErrEmptyToken = errors.New("pat: empty token")

// Mint generates a fresh PAT. Returns:
//
//	plain  -- the user-facing token, "mql_pat_<base64url>".
//	         Show this exactly ONCE to the user; it cannot be recovered.
//	hash   -- the SHA-256 hex digest of plain. Persist this on the
//	         v1:identity:identity row (credentials.api_key.keyHash).
//
// Returns the underlying crypto/rand error on entropy failure --
// callers should treat that as fatal-class.
func Mint() (plain, hash string, err error) {
	buf := make([]byte, tokenRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	body := base64.RawURLEncoding.EncodeToString(buf)
	plain = TokenPrefix + body
	hash = Hash(plain)
	return plain, hash, nil
}

// Hash returns the SHA-256 hex digest of an existing PAT plaintext.
// Used by the gRPC interceptor (hash inbound -> look up by hash) and
// by tests; production code that needs a fresh token uses Mint, which
// returns both plain and hash so the interceptor's hash-on-the-fly
// path is the only call site that needs Hash on its own.
func Hash(plain string) string {
	if plain == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// IsPATToken returns true when the token starts with TokenPrefix.
// Used by the verifier to fan out between the PAT path and the JWT
// path without parsing either format speculatively.
func IsPATToken(token string) bool {
	return strings.HasPrefix(strings.TrimSpace(token), TokenPrefix)
}
