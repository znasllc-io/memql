// Package recoverykey implements the owner recovery key -- the break-glass
// credential that makes a cluster whose owner has lost every sign-in route
// recoverable without a second bearer that also decrypts config (memql#3964).
//
// It is what remains after the sealed genesis envelope is deleted (epic
// memql#3958). The envelope's tooling wrote `export MEMQL_MASTER_KEY=` into a
// world-readable ~/.bashrc, and while that key doubled as an authenticator
// that was the defect memql#3519 named. Splitting decrypt from authenticate
// left a real gap behind it: nothing else could get an owner back in. This is
// that thing, and it authenticates ONLY -- it decrypts nothing.
//
// Wire format:
//
//	mql_rec_<43 base64url chars>      // 32 random bytes, b64url-no-pad
//
// Byte for byte the shape of mql_enr_ / mql_pat_ / mql_wkr_, and the package
// is a deliberate copy of component/identity/enrolment rather than a
// generalisation of it. Two credential families that look alike today have
// diverged before; a shared abstraction would have to be right about both
// futures at once, and the duplicated code here is forty lines.
//
// THE HASHING CONVENTION IS NOT NEW. SHA-256 hex of the plaintext, exactly
// what pat.Hash, workertoken.Hash, enrolment.Hash and the invitation /
// magic-link paths already do. The plaintext is returned to the minter once
// and revealed to an operator once, at claim time; it is never persisted,
// never logged, and never re-derivable.
//
// # What makes this different from an enrolment token
//
// An enrolment token EXPIRES -- fifteen minutes by default, twenty-four hours
// at most -- because it is issued during a conversation and is expected to be
// used immediately. A recovery key has NO EXPIRY, deliberately: it is minted
// when the cluster is claimed and used, if ever, on the worst day of the
// operator's year. A key that had quietly expired in the interim would be
// indistinguishable from one that never worked.
//
// What bounds it instead is that it is SINGLE-USE and REVOCABLE. Redemption
// spends it and deactivates the row in one write, and the standing mint
// invariant (memql#3965) then produces a fresh unclaimed successor -- so a
// leaked key is worth exactly one passkey registration, and the cluster is
// never left with no break-glass route.
package recoverykey

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

// TokenPrefix is the literal prefix every recovery-key plaintext carries.
const TokenPrefix = "mql_rec_"

// tokenRandomBytes is the entropy size of the random body (excluding the
// prefix). 32 bytes = 256 bits, encoded as 43 base64url-no-pad characters.
const tokenRandomBytes = 32

// TokenBodyChars is the exact length of the encoded body. Pinned as a constant
// because the acceptance criterion names it (`mql_rec_<43>`) and a test
// asserts against it rather than against a recomputation.
const TokenBodyChars = 43

// AuthScheme is the HTTP Authorization scheme a holder presents the key under.
//
// It sits beside `Enrolment` on the SAME passkey registration ceremony rather
// than getting a ceremony of its own (memql#3968): what a recovery key
// authorizes is exactly what an enrolment token authorizes -- register a
// passkey as one named user -- so the only thing that differs is how the
// server decides which user that is, and whether it is willing.
const AuthScheme = "Recovery"

// Mint generates a fresh recovery key. Returns:
//
//	plain  -- the operator-facing key, "mql_rec_<base64url>". It is revealed
//	          once at claim time and cannot be recovered afterwards.
//	hash   -- the SHA-256 hex digest of plain. This is the ONLY form that is
//	          ever persisted.
//
// Returns the underlying crypto/rand error on entropy failure -- callers
// should treat that as fatal-class. A recovery key minted from a degraded
// entropy source is worse than no recovery key, because it looks like one.
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

// Hash returns the SHA-256 hex digest of a recovery-key plaintext.
// Used by the redeem path (hash inbound -> look up by hash).
func Hash(plain string) string {
	plain = strings.TrimSpace(plain)
	if plain == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// IsRecoveryKey reports whether a string carries the recovery-key prefix.
func IsRecoveryKey(token string) bool {
	return strings.HasPrefix(strings.TrimSpace(token), TokenPrefix)
}
