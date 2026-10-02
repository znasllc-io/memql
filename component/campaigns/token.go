package campaigns

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
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
// carries the identities and mailbox digest the endpoint needs and a tag over them; the
// endpoint recomputes the tag and refuses anything that does not match.
// There is nothing to look up, nothing to expire, and nothing to grow.
//
// # What the token is allowed to do, and why that is safe to hand out
//
// Exactly one thing: suppress the original mailbox digest for its signed
// organization. It is not a session, grants no read, and contains no plaintext
// address. The recipient row only supplies optional display/audit metadata. So the worst a leaked token does is unsubscribe the
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
	// u3 binds organization and mailbox digest. Already-mailed u2 links
	// remain valid; u2 added the key-id segment (memql#3458). u1 is not accepted: it
	// carries no key id, so a verifier could only try every key it holds
	// and hope, and the format existed for one day between memql#3348 and
	// this change -- before any deployment had sent campaign mail.
	unsubscribeTokenVersion = "u3"

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

// UnsubscribePayload binds a sent message to its organization and mailbox
// digest. Moving or deleting a recipient later cannot redirect that opt-out.
// Legacy u2 links have no bound scope; only those resolve it from the recipient.
type UnsubscribePayload struct {
	OwnerUserID       string
	RecipientID       string
	CampaignID        string
	AccountID         string
	EmailDigest       string
	organizationBound bool
}

func MintUnsubscribeToken(secret string, payload UnsubscribePayload) (string, error) {
	if strings.TrimSpace(secret) == "" {
		return "", errors.New("campaigns: no unsubscribe secret configured")
	}
	if payload.OwnerUserID == "" || payload.RecipientID == "" {
		return "", errors.New("campaigns: unsubscribe token needs both an owner and a recipient")
	}
	if payload.AccountID != "" && !validEmailDigest(payload.EmailDigest) {
		return "", errors.New("campaigns: organization unsubscribe token requires a mailbox digest")
	}
	values := []string{unsubscribeTokenVersion, UnsubscribeKeyID(secret)}
	for _, value := range []string{payload.OwnerUserID, payload.RecipientID, payload.CampaignID, bare(payload.AccountID), payload.EmailDigest} {
		values = append(values, base64.RawURLEncoding.EncodeToString([]byte(value)))
	}
	body := strings.Join(values, ".")
	return body + "." + base64.RawURLEncoding.EncodeToString(unsubscribeTag(secret, body)), nil
}

// ParseUnsubscribeToken accepts already-mailed u2 links and organization-bound
// u3 links. Both use the same current/previous key ring and constant-time MAC.
func ParseUnsubscribeToken(keys []string, token string) (UnsubscribePayload, error) {
	empty := UnsubscribePayload{}
	if len(keys) == 0 {
		return empty, errors.New("campaigns: no unsubscribe secret configured")
	}
	if len(token) > 4096 {
		return empty, errBadToken
	}
	parts := strings.Split(strings.TrimSpace(token), ".")
	legacy := len(parts) == 6 && parts[0] == "u2"
	if !legacy && (len(parts) != 8 || parts[0] != unsubscribeTokenVersion) {
		return empty, errBadToken
	}
	body := strings.Join(parts[:len(parts)-1], ".")
	presented, err := base64.RawURLEncoding.DecodeString(parts[len(parts)-1])
	if err != nil || len(presented) != unsubscribeTagBytes {
		return empty, errBadToken
	}
	named, verified := false, false
	for _, key := range keys {
		if !hmac.Equal([]byte(UnsubscribeKeyID(key)), []byte(parts[1])) {
			continue
		}
		named = true
		if hmac.Equal(presented, unsubscribeTag(key, body)) {
			verified = true
		}
	}
	if !named {
		return empty, errUnknownUnsubscribeKey
	}
	if !verified {
		return empty, errBadToken
	}
	values := make([]string, 5)
	for n, value := range parts[2 : len(parts)-1] {
		decoded, err := base64.RawURLEncoding.DecodeString(value)
		if err != nil {
			return empty, errBadToken
		}
		values[n] = string(decoded)
	}
	payload := UnsubscribePayload{OwnerUserID: values[0], RecipientID: values[1], CampaignID: values[2], AccountID: values[3], EmailDigest: values[4], organizationBound: !legacy}
	if payload.OwnerUserID == "" || payload.RecipientID == "" || (payload.AccountID != "" && !validEmailDigest(payload.EmailDigest)) || (payload.EmailDigest != "" && !validEmailDigest(payload.EmailDigest)) {
		return empty, errBadToken
	}
	return payload, nil
}

func validEmailDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && value == strings.ToLower(value)
}

func unsubscribeTag(secret, body string) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return mac.Sum(nil)[:unsubscribeTagBytes]
}
