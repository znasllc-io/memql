package campaigns

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// token.go -- the unsubscribe capability (memql#3348).
//
// # Why a signed token and not a stored one
//
// Every other credential in this tree is a random secret whose SHA-256
// hash is stored on a row (magic links, PATs, worker tokens, enrolment
// tokens). This one is not, and the reason is volume: a stored token
// means one row per (recipient, campaign) MINTED AT SEND TIME, which
// doubles the write load of a send and leaves a table that grows without
// bound and can never be pruned, because an unsubscribe link in somebody's
// inbox has to keep working years later.
//
// A keyed MAC gives the same unforgeability with no storage. The token
// carries the three ids the endpoint needs and a tag over them; the
// endpoint recomputes the tag and refuses anything that does not match.
// There is nothing to look up, nothing to expire, and nothing to grow.
//
// # What the token is allowed to do, and why that is safe to hand out
//
// Exactly one thing: suppress the address on the recipient row it names.
// It is not a session, it grants no read, and it names no address -- the
// endpoint resolves the address from the recipient row, under the owner
// the token names. So the worst a leaked token does is unsubscribe the
// person it was already going to let unsubscribe.
//
// It is deliberately NOT expiring. An unsubscribe link that has gone
// stale is a compliance failure dressed as hygiene: the recipient clicks,
// nothing happens, and the next campaign arrives anyway.
//
// # Why the owner id is in the token
//
// The endpoint has to read an OWNED row (the recipient) to learn the
// address, and owned-tier reads inject `ownerUserId==actor.userId`. An
// unauthenticated HTTP request has no actor, so the endpoint must
// impersonate one -- and the only safe source for WHICH one is a value
// the server itself signed. Taking it from a query parameter would let a
// caller aim the impersonation; taking it from the token means it can
// only ever name the owner that minted the link.
//
// # Rotating the secret (memql#3458)
//
// The token names the KEY it was signed with, and the verifier holds a
// ring of two: MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET and the optional
// MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET_PREVIOUS. Minting only ever uses the
// first; verification uses whichever the key id names. So a rotation is
// "copy current to previous, set current to the new value, roll" and
// every link already in a mailbox keeps working.
//
// The key id is DERIVED FROM THE SECRET (a truncated HMAC of a fixed
// label under it), not a position or a counter. That is the load-bearing
// choice: a token minted today is verified by a node on which that same
// secret has since become the PREVIOUS one, so a positional label
// ("current" / "previous") would be wrong exactly when it is needed, and
// a counter would be one more value an operator can set inconsistently
// across replicas. A digest of the key is true wherever the key is.
//
// It discloses nothing an attacker does not already hold: anyone with a
// token already has a 128-bit MAC over known plaintext under the same
// secret, which is a strictly better oracle than 32 bits of a fixed-label
// digest.
//
// HOW LONG AN OLD LINK KEEPS WORKING: forever, until a SECOND rotation.
// The window is counted in rotations, not days -- there is no time-based
// expiry anywhere in this file, deliberately (see above). The operator
// procedure and the reasoning are in
// docs/public/operate/campaign-sending.md.

const (
	// unsubscribeTokenVersion prefixes every token so a future format
	// change is distinguishable from a corrupt one -- the failure a
	// version-less token format produces is "invalid link", which tells
	// a recipient nothing and an operator less.
	//
	// u2 added the key-id segment (memql#3458). u1 is not accepted: it
	// carries no key id, so a verifier could only try every key it holds
	// and hope, and the format existed for one day between memql#3348 and
	// this change -- before any deployment had sent campaign mail.
	unsubscribeTokenVersion = "u2"

	// unsubscribeTagBytes is how much of the HMAC-SHA256 output rides in
	// the token. 16 bytes / 128 bits is the standard truncation floor for
	// a MAC and keeps the URL short enough to survive a mail client's
	// line wrapping, which matters because a wrapped List-Unsubscribe URI
	// is a broken one.
	unsubscribeTagBytes = 16

	// unsubscribeKeyIDBytes is how much of the key-id digest rides in the
	// token. 4 bytes is a label, not a secret: its only job is to pick
	// one of at most two keys, and a collision between the two costs a
	// second constant-time comparison rather than an admission (the
	// verifier checks the MAC under every key whose id matches).
	unsubscribeKeyIDBytes = 4

	// unsubscribeKeyIDLabel domain-separates the key-id digest from the
	// token tag, so the id can never be mistaken for a MAC prefix over a
	// body an attacker chose.
	unsubscribeKeyIDLabel = "memql/campaigns/unsubscribe/key-id"
)

var (
	errBadToken = errors.New("campaigns: unsubscribe token is not valid")

	// errUnknownUnsubscribeKey is the ONE failure an operator can fix,
	// and the only reason it is distinguishable from errBadToken: it
	// means a rotation dropped a secret that had already signed live
	// links. The recipient still gets the same "not valid" page -- the
	// distinction exists for the log line, not for the wire.
	errUnknownUnsubscribeKey = errors.New("campaigns: unsubscribe token names a signing key this node does not hold")
)

// UnsubscribeKeyID is the short public id of a signing secret: the first
// bytes of HMAC-SHA256(secret, label), hex-encoded. Stable for a given
// secret and independent of which slot the secret currently occupies.
func UnsubscribeKeyID(secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(unsubscribeKeyIDLabel))
	return hex.EncodeToString(mac.Sum(nil)[:unsubscribeKeyIDBytes])
}

// MintUnsubscribeToken produces the token embedded in a campaign's
// List-Unsubscribe URL.
//
// All three ids are covered by the tag, including campaignId -- so the
// suppression row can record WHICH send drove the opt-out (a
// deliverability review's first question when complaints rise) without
// the campaign id being a caller-supplied claim.
func MintUnsubscribeToken(secret, ownerUserId, recipientId, campaignId string) (string, error) {
	if strings.TrimSpace(secret) == "" {
		return "", errors.New("campaigns: no unsubscribe secret configured")
	}
	if strings.TrimSpace(ownerUserId) == "" || strings.TrimSpace(recipientId) == "" {
		return "", errors.New("campaigns: unsubscribe token needs both an owner and a recipient")
	}
	body := encodeTokenBody(secret, ownerUserId, recipientId, campaignId)
	tag := unsubscribeTag(secret, body)
	return body + "." + base64.RawURLEncoding.EncodeToString(tag), nil
}

// ParseUnsubscribeToken verifies the tag against the key the token names
// and returns the ids it covers. `keys` is the verification ring in
// preference order (Config.UnsubscribeKeys: current, then previous).
//
// The comparison is hmac.Equal (constant time). That matters more here
// than it looks: this endpoint is unauthenticated and rate-limited only
// by the network, so a byte-at-a-time comparison is exactly the shape an
// attacker can afford to measure. The key-id match is constant time too
// -- not because the id is secret, but because a variable-time compare
// on a value derived from the secret is a habit worth not having.
func ParseUnsubscribeToken(keys []string, token string) (ownerUserId, recipientId, campaignId string, err error) {
	if len(keys) == 0 {
		return "", "", "", errors.New("campaigns: no unsubscribe secret configured")
	}
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 6 || parts[0] != unsubscribeTokenVersion {
		return "", "", "", errBadToken
	}
	body := strings.Join(parts[:5], ".")
	presented, decodeErr := base64.RawURLEncoding.DecodeString(parts[5])
	if decodeErr != nil || len(presented) != unsubscribeTagBytes {
		return "", "", "", errBadToken
	}

	// Narrow to the key the token names FIRST. Without this the endpoint
	// would compute a MAC per held key on every probe, and -- worse for
	// an operator -- an expired link and a forged one would be the same
	// event in the log.
	named := 0
	verified := false
	for _, key := range keys {
		if !hmac.Equal([]byte(UnsubscribeKeyID(key)), []byte(parts[1])) {
			continue
		}
		named++
		if hmac.Equal(presented, unsubscribeTag(key, body)) {
			verified = true
		}
	}
	if named == 0 {
		return "", "", "", errUnknownUnsubscribeKey
	}
	if !verified {
		return "", "", "", errBadToken
	}

	owner, oerr := base64.RawURLEncoding.DecodeString(parts[2])
	recipient, rerr := base64.RawURLEncoding.DecodeString(parts[3])
	campaign, cerr := base64.RawURLEncoding.DecodeString(parts[4])
	if oerr != nil || rerr != nil || cerr != nil {
		return "", "", "", errBadToken
	}
	if len(owner) == 0 || len(recipient) == 0 {
		return "", "", "", errBadToken
	}
	return string(owner), string(recipient), string(campaign), nil
}

// encodeTokenBody joins the version, the key id and the three
// base64url-encoded ids. Each id is encoded rather than escaped, so the
// "." separator cannot occur inside a field and the split above is
// unambiguous -- the same injectivity argument the DSL's composite-id
// derivation makes about its separator (authoring rule 20).
//
// The key id is INSIDE the body, so the tag covers it: a token whose key
// id was edited to name the other held key fails on the MAC rather than
// being quietly verified under a key that did not sign it.
func encodeTokenBody(secret, ownerUserId, recipientId, campaignId string) string {
	return fmt.Sprintf("%s.%s.%s.%s.%s",
		unsubscribeTokenVersion,
		UnsubscribeKeyID(secret),
		base64.RawURLEncoding.EncodeToString([]byte(ownerUserId)),
		base64.RawURLEncoding.EncodeToString([]byte(recipientId)),
		base64.RawURLEncoding.EncodeToString([]byte(campaignId)))
}

func unsubscribeTag(secret, body string) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return mac.Sum(nil)[:unsubscribeTagBytes]
}
