// Package invitation owns the user-invitation credential: minting it, hashing
// it, and resolving a presented one back to the row that authorizes it.
//
// # Two halves that only make sense together
//
// v1:identity:invitation has carried kind="user" since it was written, and the
// login page has redeemed one the whole time -- stage needs_invite posts
// form=invite and the value reaches the magic-link issuer. But nothing ever
// ISSUED one (memql#4270), so there were no real tokens for the redeem side to
// compare against, and it compared against nothing: `strings.TrimSpace(x) != ""`
// was the entire check (memql#4282).
//
// The two defects were each other's cover. An issuer with no validator mints
// credentials nobody checks; a validator with no issuer has nothing to check.
// This package is both halves, which is why they live in one place.
//
// # The convention this follows
//
// Identical to enrolment tokens, worker tokens, PATs and magic links: 32
// CSPRNG bytes, base64url, a `mql_inv_` prefix, and only the SHA-256 hex digest
// persisted. The plaintext is returned to the issuer exactly once and is never
// logged, never re-derivable, and never written to the audit trail -- an
// append-only log is the wrong place for a live credential.
package invitation

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"time"
)

// TokenPrefix marks a user-invitation token. Distinct from mql_enr_ (enrolment)
// and mql_rec_ (recovery) so a support conversation about "the token in my
// email" can be settled by looking at it.
const TokenPrefix = "mql_inv_"

const tokenRandomBytes = 32

// TokenBodyChars is the exact length of the encoded body: `mql_inv_<43>`.
const TokenBodyChars = 43

const (
	// DefaultTTL is the lifetime of a freshly-issued invitation when the issuer
	// names none.
	//
	// Seven days rather than the enrolment token's fifteen minutes, and the
	// difference is the conversation around it: an enrolment link is normally
	// followed while the admin is still talking to the person, whereas an
	// invitation is emailed to somebody who may be asleep, on holiday, or not
	// yet expecting it. A credential that expires before it is read is a
	// support ticket.
	DefaultTTL = 7 * 24 * time.Hour

	// MaxTTL is the ceiling on a per-issue override. An invitation is still a
	// standing permission to enter the cluster, and one that outlives anybody's
	// memory of issuing it is how a closed cluster quietly stops being closed.
	// Requests above this are CLAMPED, not refused, for the reason
	// enrolment.MaxTTL gives.
	MaxTTL = 30 * 24 * time.Hour
)

// Mint generates a fresh invitation token. Returns the plaintext (which goes
// into the invitation link and nowhere else) and its SHA-256 hex digest (the
// only form ever persisted).
func Mint() (plain, hash string, err error) {
	buf := make([]byte, tokenRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	plain = TokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
	return plain, Hash(plain), nil
}

// Hash returns the SHA-256 hex digest of an invitation-token plaintext. The
// redeem path hashes what it was given and looks the row up by this digest, so
// a database snapshot can never be replayed into a registration.
func Hash(plain string) string {
	plain = strings.TrimSpace(plain)
	if plain == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// IsInvitationToken reports whether a string carries the invitation prefix.
//
// Used to tell a pasted TOKEN from a pasted row id on the redeem path. It is a
// hint for choosing the lookup, never an authorization: the hash comparison is
// what decides.
func IsInvitationToken(token string) bool {
	return strings.HasPrefix(strings.TrimSpace(token), TokenPrefix)
}

// ClampTTL resolves a requested lifetime against the default and the ceiling.
func ClampTTL(requested time.Duration) time.Duration {
	if requested <= 0 {
		return DefaultTTL
	}
	if requested > MaxTTL {
		return MaxTTL
	}
	return requested
}
