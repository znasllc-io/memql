// Package accounttoken implements the credential an operator issues
// for one of the customers they manage (v1:identity:account).
//
// # What an account token IS, stated before anything else
//
// It is a credential ISSUED TO A USER, ON BEHALF OF AN ACCOUNT.
//
// The subject is the operator's v1:identity:user. The account is a
// BINDING carried on the credential row, not a subject and not a
// scope. That is not a simplification -- it is forced, twice over, by
// docs/internal/design/account-isolation-model.md:
//
//   - Section 3.3: an account holds no credential, mints no session and
//     is never a subject. Every gate in the tree resolves authority
//     through actor.userId, whose closed field set is
//     userId / role / identityId / isClusterOwner. There is nowhere for
//     an account to BE a principal.
//   - Section 5.2: the actor envelope carries no tenancy dimension, so
//     an account predicate can only compare a payload field against a
//     CALLER-SUPPLIED arg. A credential "scoped to Acme" would be
//     scoped by the honour system, which is not a scope.
//
// # What it therefore authorizes: nothing, today, on purpose
//
// Those two facts rule out both ways of making this a live bearer:
//
//   - It cannot authenticate AS the account (3.3), and
//   - it must not authenticate AS the operator carrying the operator's
//     whole authority under a customer's name (5.2 means the binding
//     cannot narrow it). A credential whose name says "Acme" and whose
//     blast radius is "everything this operator can do" is strictly
//     worse than a PAT, because the name lies about the radius.
//
// So no verifier resolves mql_acct_, no interceptor admits it, and
// dsl/identity/queries.memql declares no by-keyHash lookup for it. The
// digest here is a CUSTODY RECORD -- proof the plaintext was not kept
// -- rather than an authentication index.
//
// What ships now is the half that is honest now: mint good secret
// material, show it once, persist only its digest, list it, revoke it,
// and audit both ends. The authorization half lands with section 6(b)
// (a resolved tenancy dimension on AccessContext), at which point the
// accountId already stored on each credential row is exactly the value
// actor.accountIds would be populated from.
//
// Wire format:
//
//	mql_acct_<43 base64url chars>      // 32 random bytes, b64url-no-pad
//
// The mql_acct_ prefix is registered with the gRPC interceptors'
// reserved-prefix check (component/grpc/voice_agent_stream_interceptor.go)
// so an account token presented as a Bearer is short-circuited rather
// than fed to the JWT parser -- a reservation, not an admission.
package accounttoken

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
)

// TokenPrefix is the literal prefix every account-token plaintext
// carries. Distinct from mql_pat_ / mql_wkr_ so a credential's family
// is readable off the string without a lookup -- which matters most in
// the case this package is designed around: an operator pasting a
// secret somewhere and needing to know what they are holding.
const TokenPrefix = "mql_acct_"

// tokenRandomBytes matches pat / workertoken: 32 bytes of crypto/rand,
// 256 bits of entropy, base64url-no-pad to 43 characters.
const tokenRandomBytes = 32

// ErrEmptyToken is returned by Hash when the input is empty.
var ErrEmptyToken = errors.New("accounttoken: empty token")

// Mint generates a fresh account token. Returns:
//
//	plain  -- the operator-facing token, "mql_acct_<base64url>".
//	          Show this exactly ONCE; it cannot be recovered.
//	hash   -- the SHA-256 hex digest of plain. Persist this on
//	          v1:identity:identity.credentials.keyHash.
//
// The plaintext must not be logged, must not be written to a concept
// payload, and must not outlive the reply it is returned in.
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

// Hash returns the SHA-256 hex digest of an existing token. Empty in,
// empty out -- a caller that hashes "" must not get a digest that
// would match a stored row for the empty string.
func Hash(plain string) string {
	if plain == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// IsAccountToken reports whether the token carries TokenPrefix.
//
// Used by the interceptors' reserved-prefix check to RECOGNISE the
// family, never to admit it. There is no ResolveAccountToken anywhere
// in the tree, and adding one is a decision, not a detail -- see the
// package comment.
func IsAccountToken(token string) bool {
	return strings.HasPrefix(strings.TrimSpace(token), TokenPrefix)
}
