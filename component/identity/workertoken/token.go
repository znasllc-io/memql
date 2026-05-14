// Package workertoken implements credentials for the workers
// (computer-use) feature.
//
// Worker tokens are long-lived bearer credentials, owned by a
// v1:identity:user, stored as v1:identity:identity rows of
// identityType="worker_token". Plaintext is shown to the user
// ONCE at generation; only its SHA-256 hex hash lives in the
// database.
//
// Wire format:
//
//	mql_wkr_<43 base64url chars>      // 32 random bytes, b64url-no-pad
//
// The mql_wkr_ prefix lets the gRPC interceptor distinguish worker
// tokens from PATs (mql_pat_) and JWTs without parsing the body.
// The interceptor admits worker tokens ONLY on WorkerService paths
// and rejects them on every other RPC.
package workertoken

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
)

// TokenPrefix is the literal prefix every worker-token plaintext
// carries.
const TokenPrefix = "mql_wkr_"

const tokenRandomBytes = 32

// ErrEmptyToken is returned by Hash when the input is empty.
var ErrEmptyToken = errors.New("workertoken: empty token")

// Mint generates a fresh worker token. Returns:
//
//	plain  -- the user-facing token, "mql_wkr_<base64url>".
//	         Show this exactly ONCE; it cannot be recovered.
//	hash   -- the SHA-256 hex digest of plain. Persist this on
//	         v1:identity:identity.credentials.keyHash.
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

// Hash returns the SHA-256 hex digest of an existing token.
func Hash(plain string) string {
	if plain == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// IsWorkerToken returns true when the token starts with TokenPrefix.
func IsWorkerToken(token string) bool {
	return strings.HasPrefix(strings.TrimSpace(token), TokenPrefix)
}
