package campaigns

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
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
// # Rotating the secret
//
// Changing MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET invalidates every link
// already in a recipient's inbox. Treat it as a key with no rotation
// story rather than a password: set it once per deployment. (A key-id
// prefix and a verifier that accepts the previous secret is the standard
// fix and is a follow-up, not something to improvise during an incident.)

const (
	// unsubscribeTokenVersion prefixes every token so a future format
	// change is distinguishable from a corrupt one -- the failure a
	// version-less token format produces is "invalid link", which tells
	// a recipient nothing and an operator less.
	unsubscribeTokenVersion = "u1"

	// unsubscribeTagBytes is how much of the HMAC-SHA256 output rides in
	// the token. 16 bytes / 128 bits is the standard truncation floor for
	// a MAC and keeps the URL short enough to survive a mail client's
	// line wrapping, which matters because a wrapped List-Unsubscribe URI
	// is a broken one.
	unsubscribeTagBytes = 16
)

var errBadToken = errors.New("campaigns: unsubscribe token is not valid")

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
	body := encodeTokenBody(ownerUserId, recipientId, campaignId)
	tag := unsubscribeTag(secret, body)
	return body + "." + base64.RawURLEncoding.EncodeToString(tag), nil
}

// ParseUnsubscribeToken verifies the tag and returns the ids it covers.
//
// The comparison is hmac.Equal (constant time). That matters more here
// than it looks: this endpoint is unauthenticated and rate-limited only
// by the network, so a byte-at-a-time comparison is exactly the shape an
// attacker can afford to measure.
func ParseUnsubscribeToken(secret, token string) (ownerUserId, recipientId, campaignId string, err error) {
	if strings.TrimSpace(secret) == "" {
		return "", "", "", errors.New("campaigns: no unsubscribe secret configured")
	}
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 5 || parts[0] != unsubscribeTokenVersion {
		return "", "", "", errBadToken
	}
	body := strings.Join(parts[:4], ".")
	presented, decodeErr := base64.RawURLEncoding.DecodeString(parts[4])
	if decodeErr != nil {
		return "", "", "", errBadToken
	}
	if !hmac.Equal(presented, unsubscribeTag(secret, body)) {
		return "", "", "", errBadToken
	}
	owner, oerr := base64.RawURLEncoding.DecodeString(parts[1])
	recipient, rerr := base64.RawURLEncoding.DecodeString(parts[2])
	campaign, cerr := base64.RawURLEncoding.DecodeString(parts[3])
	if oerr != nil || rerr != nil || cerr != nil {
		return "", "", "", errBadToken
	}
	if len(owner) == 0 || len(recipient) == 0 {
		return "", "", "", errBadToken
	}
	return string(owner), string(recipient), string(campaign), nil
}

// encodeTokenBody joins the version and the three base64url-encoded ids.
// Each id is encoded rather than escaped, so the "." separator cannot
// occur inside a field and the split above is unambiguous -- the same
// injectivity argument the DSL's composite-id derivation makes about its
// separator (authoring rule 20).
func encodeTokenBody(ownerUserId, recipientId, campaignId string) string {
	return fmt.Sprintf("%s.%s.%s.%s",
		unsubscribeTokenVersion,
		base64.RawURLEncoding.EncodeToString([]byte(ownerUserId)),
		base64.RawURLEncoding.EncodeToString([]byte(recipientId)),
		base64.RawURLEncoding.EncodeToString([]byte(campaignId)))
}

func unsubscribeTag(secret, body string) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return mac.Sum(nil)[:unsubscribeTagBytes]
}
