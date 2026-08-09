// Package enrolment implements single-use enrolment tokens -- the
// credential that removes email from the critical path (memql#3408).
//
// An enrolment token authorizes exactly ONE action: register a passkey as a
// named user. That narrowness is what makes it safe to hand out as a plain
// bearer in a URL, and it is what makes a FIRST credential obtainable with no
// mailbox: `/setup` cannot host enrolment (handleSetupGet 404s once any user
// exists, and the install wizard's unattended bootstrap completes setup from
// env before `/setup` would ever render), so something else has to authorize
// the very first passkey.
//
// Wire format:
//
//	mql_enr_<43 base64url chars>      // 32 random bytes, b64url-no-pad
//
// The same shape as mql_pat_ / mql_wkr_, for the same reason: a memorable
// prefix makes a leaked token greppable, and it lets a reader fan out on the
// prefix without speculatively parsing the body.
//
// THE HASHING CONVENTION IS NOT NEW. SHA-256 hex of the plaintext, byte for
// byte what pat.Hash, workertoken.Hash, workerpairing.Hash and the invitation
// / magic-link paths already do. The plaintext is returned to the issuer once,
// travels in the enrolment link, and is never persisted or logged.
package enrolment

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"time"
)

// TokenPrefix is the literal prefix every enrolment-token plaintext carries.
const TokenPrefix = "mql_enr_"

// tokenRandomBytes is the entropy size of the random body (excluding the
// prefix). 32 bytes = 256 bits, encoded as 43 base64url-no-pad characters.
const tokenRandomBytes = 32

// TokenBodyChars is the exact length of the encoded body. Pinned as a
// constant because the acceptance criterion names it (`mql_enr_<43>`) and a
// test asserts against it rather than against a recomputation.
const TokenBodyChars = 43

const (
	// DefaultTTL is the lifetime of a freshly-issued enrolment token when the
	// issuer names none. Deliberately short: the link is expected to be
	// followed immediately -- the wizard opens it in the operator's own
	// browser, and an admin issuing one for someone else is normally talking
	// to them at the time.
	DefaultTTL = 15 * time.Minute

	// MaxTTL is the ceiling on a per-issue override. An issuer may ask for
	// longer than the default (a link for a colleague in another timezone),
	// but not for a credential that outlives the conversation that produced
	// it. Requests above this are CLAMPED, not refused: a caller asking for
	// too much still wants a link, and silently issuing a longer-lived
	// credential than the ceiling is the only outcome that would be wrong.
	MaxTTL = 24 * time.Hour
)

// Mint generates a fresh enrolment token. Returns:
//
//	plain  -- the user-facing token, "mql_enr_<base64url>". It goes into the
//	          enrolment link and nowhere else; it cannot be recovered.
//	hash   -- the SHA-256 hex digest of plain. This is the ONLY form that is
//	          ever persisted.
//
// Returns the underlying crypto/rand error on entropy failure -- callers
// should treat that as fatal-class.
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

// Hash returns the SHA-256 hex digest of an enrolment-token plaintext.
// Used by the redeem path (hash inbound -> look up by hash).
func Hash(plain string) string {
	plain = strings.TrimSpace(plain)
	if plain == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// IsEnrolmentToken reports whether a string carries the enrolment prefix.
func IsEnrolmentToken(token string) bool {
	return strings.HasPrefix(strings.TrimSpace(token), TokenPrefix)
}

// ClampTTL resolves a requested lifetime against the default and the ceiling.
// Zero or negative means "use the default"; anything above MaxTTL is clamped
// down to it.
func ClampTTL(requested time.Duration) time.Duration {
	if requested <= 0 {
		return DefaultTTL
	}
	if requested > MaxTTL {
		return MaxTTL
	}
	return requested
}
