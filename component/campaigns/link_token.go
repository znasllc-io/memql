package campaigns

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// link_token.go -- the capability behind the three email-link pages
// (memql#6033): confirm a subscription, change preferences, view a message in
// a browser.
//
// It is token.go's and tracking_token.go's sibling and is built the same way:
// a keyed MAC over a self-describing body, no stored row, a key id derived from
// the secret, hmac.Equal throughout. What token.go argues about statelessness
// and about rotating the secret applies here unchanged and is not repeated.
// What is different, and why:
//
// # One key ring, three pages, and a context per page
//
// These tokens are signed by MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET and its
// _PREVIOUS, like the other two families: one process mints them, one message
// carries them and one origin serves them, so a second secret would only be a
// second thing to rotate. Sharing a key makes CROSS-USE the risk. The three
// pages do different things with it -- confirm records consent, preferences can
// lift an opt-out, view only reads -- and only confirm links expire. If a token
// for one page opened another, the weakest page's rules would govern the
// strongest page's action.
//
// So each page has its own context string (memql/email/<page>/v1), mixed into
// the MAC and NOT carried in the token. The route a request arrives at chooses
// the context. A token minted for one page is therefore a bad MAC on another:
// there is no field to edit and no comparison to skip. The families are kept
// apart from each other the same way: this one has four parts (u2/u3 have six
// or eight, t1 has eight), a different version prefix, and a key-id label that
// produces a different id for the same secret, so no parser recognises
// another's tokens.
//
// # The payload is signed whole
//
// The pages take the owner, recipient, organization and mailbox digest from the
// token and from nothing a request says; no query parameter selects a row. That
// is the rule unsubscribe.go follows, for the same reason: the token is the
// only source of WHOSE row to read that the server itself vouched for. The
// payload holds ids and a digest, never an address.
//
// # Only a confirm link expires
//
// A confirmation is a request to start mailing someone, and it only means
// something soon after it was made, so a confirm payload carries ExpiresAt
// (confirmLinkLifetime) and ParseLinkToken refuses it afterwards. Preferences
// and view links are the opposite. They sit in a mailbox as long as the message
// does, and the preferences page is where a person goes to opt out; an expiry
// there would be the stale-unsubscribe failure token.go refuses to build. They
// carry no expiry, and an ExpiresAt on one is ignored.
//
// The expiry is checked AFTER the MAC, so a caller without the secret cannot
// tell "expired" from "forged". The verified payload is returned with
// errExpiredLinkToken, so the page that says the link has expired can still
// name the organization the unconfirmed subscription was for.
//
// # Size
//
// A view token carries the snapshot of what was sent (template revision, sender
// and campaign names), so it is the longest of the three. Tokens over
// maxLinkTokenBytes are refused before anything is decoded, and refused at
// minting too: a link the parser would reject must never go out in a message.

// LinkKind names the email-link page a token is for. Which kind is checked is
// decided by the page a request arrives at, never by anything in the token.
type LinkKind string

const (
	// LinkConfirm is the page that confirms a subscription. These links, and
	// only these, expire.
	LinkConfirm LinkKind = "confirm"

	// LinkPreferences is the page that shows an address's state for an
	// organization and lets the person unsubscribe or subscribe again.
	LinkPreferences LinkKind = "preferences"

	// LinkView is the page that shows a sent message as it was sent.
	LinkView LinkKind = "view"
)

const (
	// confirmLinkLifetime is how long a confirm link works. Whoever mints one
	// sets ExpiresAt to the time of minting plus this.
	confirmLinkLifetime = 30 * 24 * time.Hour

	// linkTokenVersion prefixes every token, so a later format is
	// distinguishable from a corrupt one.
	linkTokenVersion = "l1"

	// linkTagBytes is how much of the HMAC-SHA256 output rides in the token:
	// the standard 128-bit truncation floor, short enough to survive a mail
	// client wrapping the line.
	linkTagBytes = 16

	// linkKeyIDBytes is the length of the key label. Four bytes picks one of at
	// most two keys; a collision costs a second constant-time comparison, not
	// an admission.
	linkKeyIDBytes = 4

	// linkKeyIDLabel is DIFFERENT from the unsubscribe and tracking labels on
	// purpose, so one secret names itself differently in each family.
	linkKeyIDLabel = "memql/email/link/key-id"

	// linkTokenParts is the part count of a well-formed token: version, key id,
	// payload, tag.
	linkTokenParts = 4

	// maxLinkTokenBytes is the longest token either side will handle.
	maxLinkTokenBytes = 4096
)

var (
	// errBadLinkToken is every failure that is not a rotation problem or an
	// expiry: the wrong shape, the wrong page, an altered payload, a tag that
	// does not match, a payload this code would not have minted.
	errBadLinkToken = errors.New("campaigns: email link token is not valid")

	// errUnknownLinkKey is the one failure an operator can fix, and the only
	// reason it is told apart from errBadLinkToken: it means a rotation dropped
	// a secret that had already signed live links. The recipient sees the same
	// page either way; the distinction is for the log line, as for unsubscribe.
	errUnknownLinkKey = errors.New("campaigns: email link token names a signing key this node does not hold")

	// errExpiredLinkToken is a genuine confirm link used after its expiry.
	errExpiredLinkToken = errors.New("campaigns: email link token has expired")
)

// LinkPayload is signed whole; nothing a request says can change it.
//
// OwnerUserID, RecipientID and EmailDigest are required on every page. The
// rest is optional, except that a view link needs TemplateID and
// TemplateRevision and a confirm link needs ExpiresAt.
type LinkPayload struct {
	OwnerUserID      string `json:"o"`
	RecipientID      string `json:"r"`
	AccountID        string `json:"a,omitempty"`
	EmailDigest      string `json:"d"`
	CampaignID       string `json:"c,omitempty"` // view: the campaign the message belonged to
	TemplateID       string `json:"t,omitempty"` // view
	TemplateRevision string `json:"v,omitempty"` // view: the template createdAt that was sent (RFC 3339 nano)
	SenderName       string `json:"s,omitempty"` // view: the footer's sender as sent
	CampaignName     string `json:"n,omitempty"` // view: {{campaignName}} as sent
	ExpiresAt        int64  `json:"x,omitempty"` // confirm: unix seconds
}

// LinkKeyID is the short public id of a signing secret under the link label:
// the first bytes of HMAC-SHA256(secret, label), hex-encoded. Stable for a
// given secret and independent of which slot the secret occupies.
func LinkKeyID(secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(linkKeyIDLabel))
	return hex.EncodeToString(mac.Sum(nil)[:linkKeyIDBytes])
}

// linkContext is the string mixed into the MAC for a page. It is a closed
// table, not built from the kind, so no string a caller invents can name a
// context.
func linkContext(kind LinkKind) (string, bool) {
	switch kind {
	case LinkConfirm:
		return "memql/email/confirm/v1", true
	case LinkPreferences:
		return "memql/email/preferences/v1", true
	case LinkView:
		return "memql/email/view/v1", true
	}
	return "", false
}

// MintLinkToken signs p for one page: l1.<keyId>.<base64url(json)>.<base64url(tag)>.
//
// The tag covers the page's context, a NUL, and everything before the tag, so
// the token cannot be moved to another page or edited. Minting uses the
// current secret only; the previous one only verifies.
func MintLinkToken(secret string, kind LinkKind, p LinkPayload) (string, error) {
	if strings.TrimSpace(secret) == "" {
		return "", errors.New("campaigns: no unsubscribe secret configured, so no email link can be signed")
	}
	macContext, ok := linkContext(kind)
	if !ok {
		return "", fmt.Errorf("campaigns: %q is not an email link page", kind)
	}
	if err := validateLinkPayload(kind, p); err != nil {
		return "", err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("campaigns: cannot encode the %s link: %w", kind, err)
	}
	body := linkTokenVersion + "." + LinkKeyID(secret) + "." + base64.RawURLEncoding.EncodeToString(raw)
	token := body + "." + base64.RawURLEncoding.EncodeToString(linkTag(secret, macContext, body))
	if len(token) > maxLinkTokenBytes {
		return "", fmt.Errorf("campaigns: the %s link would be %d bytes, over the %d byte limit", kind, len(token), maxLinkTokenBytes)
	}
	return token, nil
}

// ParseLinkToken verifies a token for one page against the key ring and returns
// what it carries. keys is Config.UnsubscribeKeys(): current first, then
// previous. now is only consulted for a confirm link's expiry.
//
// On any error the payload is the zero value, with one exception:
// errExpiredLinkToken comes with the verified payload (see the file comment).
func ParseLinkToken(keys []string, kind LinkKind, token string, now time.Time) (LinkPayload, error) {
	empty := LinkPayload{}
	macContext, ok := linkContext(kind)
	if !ok {
		return empty, fmt.Errorf("campaigns: %q is not an email link page", kind)
	}
	if len(keys) == 0 {
		return empty, errors.New("campaigns: no unsubscribe secret configured, so no email link can be verified")
	}
	// Before anything is split or decoded: the size of what a request sent is
	// not something to spend work on.
	if len(token) > maxLinkTokenBytes {
		return empty, errBadLinkToken
	}
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != linkTokenParts || parts[0] != linkTokenVersion {
		return empty, errBadLinkToken
	}
	presented, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil || len(presented) != linkTagBytes {
		return empty, errBadLinkToken
	}

	// Narrow to the key the token names, as the unsubscribe parser does: without
	// it every probe costs a MAC per held key, and "a rotation dropped a key"
	// could not be told from "somebody is guessing".
	body := strings.Join(parts[:linkTokenParts-1], ".")
	named, verified := false, false
	for _, key := range keys {
		if !hmac.Equal([]byte(LinkKeyID(key)), []byte(parts[1])) {
			continue
		}
		named = true
		if hmac.Equal(presented, linkTag(key, macContext, body)) {
			verified = true
		}
	}
	if !named {
		return empty, errUnknownLinkKey
	}
	if !verified {
		return empty, errBadLinkToken
	}

	// Past this point the bytes are ours. They are still checked against the
	// rules minting applies: a validly signed token this code would never have
	// issued is not one to act on.
	raw, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return empty, errBadLinkToken
	}
	var p LinkPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return empty, errBadLinkToken
	}
	if validateLinkPayload(kind, p) != nil {
		return empty, errBadLinkToken
	}
	if kind == LinkConfirm && now.Unix() > p.ExpiresAt {
		return p, errExpiredLinkToken
	}
	return p, nil
}

// validateLinkPayload is the one statement of what each page requires, applied
// when minting and again when parsing.
func validateLinkPayload(kind LinkKind, p LinkPayload) error {
	if p.OwnerUserID == "" || p.RecipientID == "" {
		return errors.New("campaigns: an email link needs both an owner and a recipient")
	}
	if !validEmailDigest(p.EmailDigest) {
		return errors.New("campaigns: an email link needs the mailbox digest as 64 lowercase hex characters")
	}
	switch kind {
	case LinkConfirm:
		if p.ExpiresAt <= 0 {
			return errors.New("campaigns: a confirm link needs an expiry")
		}
	case LinkView:
		if p.TemplateID == "" || p.TemplateRevision == "" {
			return errors.New("campaigns: a view link needs the template and the revision that was sent")
		}
	}
	return nil
}

func linkTag(secret, macContext, body string) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(macContext))
	mac.Write([]byte{0})
	mac.Write([]byte(body))
	return mac.Sum(nil)[:linkTagBytes]
}

// linkKeyIDOf pulls the (public, non-secret) key id out of a token for the log
// line that reports errUnknownLinkKey, so an operator can tell one dropped key
// from another. It returns "" for anything not shaped like a token: a log must
// never echo attacker-chosen bytes.
func linkKeyIDOf(token string) string {
	if len(token) > maxLinkTokenBytes {
		return ""
	}
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != linkTokenParts || parts[0] != linkTokenVersion {
		return ""
	}
	if _, err := hex.DecodeString(parts[1]); err != nil || len(parts[1]) != linkKeyIDBytes*2 {
		return ""
	}
	return parts[1]
}
