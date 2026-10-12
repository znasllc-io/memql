package campaigns

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// link_token.go -- the capability behind the three email-link pages
// (memql#6033): confirm a subscription, change preferences, view a message in
// a browser.
//
// It is token.go's and tracking_token.go's sibling in what it needs to be: no
// stored row, a key id derived from the secret, one secret and its previous one
// for rotation, constant-time checks. What token.go argues about statelessness
// and about rotating the secret applies here unchanged and is not repeated. What
// is different, and why:
//
// # The payload is sealed, not just signed
//
// The other two families are signed and readable: the u3 unsubscribe token
// carries the owner, the recipient, the organization and an unkeyed SHA-256 of
// the address in base64, because a link in a mailbox has to keep working for
// years and its format cannot change under it. This family has no such history,
// so it does not inherit the habit. Its payload holds the same things and more
// (the template revision, the sender's name, the campaign's name), and these
// links are the ones that get forwarded and posted: preferences, view. A holder
// of a readable token could read who it is for, and could confirm a guessed
// address offline against the digest.
//
// So the payload is encrypted and authenticated in one step with AES-256-GCM:
//
//	l1.<keyId>.<base64url(nonce || ciphertext || tag)>
//
// The key is derived from the secret with HKDF-SHA256 (info "memql/email/link/v1"),
// the nonce is 96 random bits per token, and the associated data is the page's
// context and the header, "memql/email/<page>/v1" + "\x00" + "l1.<keyId>". Two
// tokens for one payload are unrelated strings, nothing in a token reads as
// anything, and a flipped byte anywhere is refused. The key id is derived as it
// always was (HMAC-SHA256 of a label under the secret) and only names which
// secret to try.
//
// With random 96-bit nonces a key is good for about 2^32 tokens before the odds
// of a repeated nonce stop being negligible (NIST SP 800-38D). A message mints
// two or three, and the secret is rotated by operators, so that is billions of
// messages per secret; if that ever stops being true, this is the place to move
// to a nonce with a counter or to XChaCha20-Poly1305.
//
// # One key ring, three pages, and a context per page
//
// These tokens are sealed under MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET and its
// _PREVIOUS, like the other two families: one process mints them, one message
// carries them and one origin serves them, so a second secret would only be a
// second thing to rotate. Sharing a key makes CROSS-USE the risk. The three
// pages do different things with it -- confirm records consent, preferences can
// lift an opt-out, view only reads -- and only confirm links expire. If a token
// for one page opened another, the weakest page's rules would govern the
// strongest page's action.
//
// So each page has its own context string (memql/email/<page>/v1), authenticated
// and NOT carried in the token. The route a request arrives at chooses the
// context. A token minted for one page therefore fails to open on another: there
// is no field to edit and no comparison to skip. The families are kept apart
// from each other the same way: this one has three parts (u2/u3 have six or
// eight, t1 has eight), a different version prefix, a key-id label that produces
// a different id for the same secret, and a different key derivation, so no
// parser recognises another's tokens.
//
// # The payload is opened whole
//
// The pages take the owner, recipient, organization and mailbox digest from the
// token and from nothing a request says; no query parameter selects a row. That
// is the rule unsubscribe.go follows, for the same reason: the token is the
// only source of WHOSE row to read that the server itself vouched for. The
// payload holds ids and a digest, never an address, and the organization id is
// sealed in its bare form, as the u3 token signs it.
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
// The expiry is checked AFTER the token is opened, so a forged token never
// reaches the expiry check: only a token this code sealed can be reported as
// expired. The opened payload is returned with errExpiredLinkToken, so the page
// that says the link has expired can still name the organization the
// unconfirmed subscription was for. It is for that and nothing else; see
// errExpiredLinkToken.
//
// # The verifier fails closed
//
// ParseLinkToken is exported and takes its ring and its clock as arguments, so
// it trusts neither. A blank key is never part of the ring (an empty secret
// derives a key like any other, so a token sealed under it would open). A
// confirm link is not checked against a zero clock (the zero time is before
// every expiry). And the sealed segment has exactly one spelling: Go's base64
// decoder also accepts CR and LF anywhere and non-zero padding bits in the last
// character, and each such respelling would decode to the same bytes and open.
//
// # Size
//
// A view token carries the snapshot of what was sent (template revision, the
// sender the footer named and the campaign's name), so it is the longest of the
// three. Tokens over maxLinkTokenBytes are refused before anything is decoded,
// and refused at minting too: a link the parser would reject must never go out
// in a message. The campaign's name is capped where it is saved
// (maxCampaignNameRunes) and the sender's name is cut to maxSenderNameRunes
// when the link is built, so that cannot happen to a message that was valid
// when it was queued.

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

	// linkKeyIDBytes is the length of the key label. Four bytes picks one of at
	// most two keys; a collision costs a second attempt to open, not an
	// admission.
	linkKeyIDBytes = 4

	// linkKeyIDLabel is DIFFERENT from the unsubscribe and tracking labels on
	// purpose, so one secret names itself differently in each family.
	linkKeyIDLabel = "memql/email/link/key-id"

	// linkSealInfo is the HKDF info the sealing key is derived under. It names
	// the family; the page is told apart by the associated data, not here.
	linkSealInfo = "memql/email/link/v1"

	// linkSealKeyBytes makes the cipher AES-256.
	linkSealKeyBytes = 32

	// linkMinSealedBytes is the least a sealed segment can be: the 12-byte
	// nonce and the 16-byte tag of AES-GCM around an empty payload. A segment
	// shorter than that is not a token of this family, whatever key it names.
	linkMinSealedBytes = 12 + 16

	// linkTokenParts is the part count of a well-formed token: version, key id,
	// sealed payload.
	linkTokenParts = 3

	// maxLinkTokenBytes is the longest token either side will handle.
	maxLinkTokenBytes = 4096
)

var (
	// errBadLinkToken is every failure that is not a rotation problem or an
	// expiry: the wrong shape, the wrong page, an altered token, one that does
	// not open, a payload this code would not have minted.
	errBadLinkToken = errors.New("campaigns: email link token is not valid")

	// errUnknownLinkKey is the one failure an operator can fix, and the only
	// reason it is told apart from errBadLinkToken: it means a rotation dropped
	// a secret that had already sealed live links. The recipient sees the same
	// page either way; the distinction is for the log line, as for unsubscribe.
	errUnknownLinkKey = errors.New("campaigns: email link token names a signing key this node does not hold")

	// errExpiredLinkToken is a genuine confirm link used after its expiry.
	//
	// ParseLinkToken returns the OPENED payload with it, and only so that a
	// page can name the organization the unconfirmed subscription was for. The
	// payload must not be acted on: an expired link authorizes no write, no
	// consent and no state change.
	errExpiredLinkToken = errors.New("campaigns: email link token has expired")

	// errNoLinkClock is a caller bug and not something a recipient did, which is
	// why it is neither errBadLinkToken nor errExpiredLinkToken (the two a page
	// shows): a confirm link can only be checked against the current time, and a
	// zero time would call every confirm link live.
	errNoLinkClock = errors.New("campaigns: a confirm link cannot be checked without the current time")
)

// LinkPayload is sealed whole; nothing a request says can change it, and nothing
// in it can be read without the secret.
//
// OwnerUserID, RecipientID and EmailDigest are required on every page. The
// rest is optional, except that a view link needs TemplateID and
// TemplateRevision and a confirm link needs ExpiresAt. MintLinkToken seals
// AccountID in its bare form (as the u3 token does), so ParseLinkToken returns a
// bare organization id whichever form the caller passed.
type LinkPayload struct {
	OwnerUserID      string `json:"o"`
	RecipientID      string `json:"r"`
	AccountID        string `json:"a,omitempty"`
	EmailDigest      string `json:"d"`
	CampaignID       string `json:"c,omitempty"` // view: the campaign the message belonged to
	TemplateID       string `json:"t,omitempty"` // view
	TemplateRevision string `json:"v,omitempty"` // view: the template createdAt that was sent (RFC 3339 nano)
	SenderName       string `json:"s,omitempty"` // view: the sender the footer named (footerSender), cut to maxSenderNameRunes
	CampaignName     string `json:"n,omitempty"` // view: what {{campaignName}} showed
	ExpiresAt        int64  `json:"x,omitempty"` // confirm: unix seconds
}

// LinkKeyID is the short public id of a secret under the link label: the first
// bytes of HMAC-SHA256(secret, label), hex-encoded. Stable for a given secret
// and independent of which slot the secret occupies. It only says which secret
// to try; it is not the key, and the key is not derived from it.
func LinkKeyID(secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(linkKeyIDLabel))
	return hex.EncodeToString(mac.Sum(nil)[:linkKeyIDBytes])
}

// linkContext is the string a page's tokens are authenticated under. It is a
// closed table, not built from the kind, so no string a caller invents can name
// a context.
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

// MintLinkToken seals p for one page: l1.<keyId>.<base64url(nonce || ciphertext || tag)>.
//
// The associated data is the page's context, a NUL, and the header, so the token
// cannot be moved to another page or have its key id or version edited. Minting
// uses the current secret only; the previous one only opens.
func MintLinkToken(secret string, kind LinkKind, p LinkPayload) (string, error) {
	return mintLinkToken(rand.Reader, secret, kind, p)
}

// mintLinkToken is MintLinkToken with its source of randomness named, so a test
// can seal under a nonce it chose and compare the result with a token built by
// hand. It reads exactly one nonce from random and refuses to seal if it cannot:
// a token sealed under a nonce it did not get would be sealed under a repeat.
func mintLinkToken(random io.Reader, secret string, kind LinkKind, p LinkPayload) (string, error) {
	if strings.TrimSpace(secret) == "" {
		return "", errors.New("campaigns: no unsubscribe secret configured, so no email link can be sealed")
	}
	pageContext, ok := linkContext(kind)
	if !ok {
		return "", fmt.Errorf("campaigns: %q is not an email link page", kind)
	}
	if err := validateLinkPayload(kind, p); err != nil {
		return "", err
	}
	// Sealed in its bare form, as MintUnsubscribeToken signs it: a canonical id
	// off a row would lengthen every URL, and the pages compare organizations
	// bare. p is a copy, so the caller's payload is unchanged.
	p.AccountID = bare(p.AccountID)
	raw, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("campaigns: cannot encode the %s link: %w", kind, err)
	}
	aead, err := linkSealer(secret)
	if err != nil {
		return "", fmt.Errorf("campaigns: cannot seal the %s link: %w", kind, err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(random, nonce); err != nil {
		return "", fmt.Errorf("campaigns: cannot seal the %s link, no nonce: %w", kind, err)
	}
	header := linkTokenVersion + "." + LinkKeyID(secret)
	sealed := aead.Seal(nonce, nonce, raw, linkAAD(pageContext, header))
	token := header + "." + base64.RawURLEncoding.EncodeToString(sealed)
	if len(token) > maxLinkTokenBytes {
		return "", fmt.Errorf("campaigns: the %s link would be %d bytes, over the %d byte limit", kind, len(token), maxLinkTokenBytes)
	}
	return token, nil
}

// linkSealer is the AEAD one secret seals and opens link tokens with: AES-256-GCM
// under a key derived from the secret with HKDF-SHA256. Deriving it, rather than
// using the secret as a key, keeps this family's key apart from the HMAC keys the
// same secret is used as in the other two families and for the key id.
func linkSealer(secret string) (cipher.AEAD, error) {
	key, err := hkdf.Key(sha256.New, []byte(secret), nil, linkSealInfo, linkSealKeyBytes)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// linkAAD is the associated data a token is authenticated under: the page's
// context, a NUL, and the header it is written under ("l1.<keyId>").
func linkAAD(pageContext, header string) []byte {
	return []byte(pageContext + "\x00" + header)
}

// ParseLinkToken opens a token for one page against the key ring and returns
// what it carries.
//
// keys is Config.UnsubscribeKeys(): current first, then previous. Blank entries
// are ignored, and a ring with nothing else in it refuses every token.
//
// now is the current time. It is only consulted for a confirm link's expiry, and
// for one it must not be the zero time: ParseLinkToken returns errNoLinkClock (a
// caller bug, deliberately neither errBadLinkToken nor errExpiredLinkToken, which
// are what a page shows the recipient) rather than guess that an expired link is
// live. The other pages ignore it.
//
// On any error the payload is the zero value, with one exception:
// errExpiredLinkToken comes with the opened payload (see the file comment), and
// that payload must not be acted on.
func ParseLinkToken(keys []string, kind LinkKind, token string, now time.Time) (LinkPayload, error) {
	empty := LinkPayload{}
	pageContext, ok := linkContext(kind)
	if !ok {
		return empty, fmt.Errorf("campaigns: %q is not an email link page", kind)
	}
	// A blank key is not a key. An empty secret derives a key like any other, so
	// a token sealed under it would open; Config.UnsubscribeKeys never yields
	// one, and this keeps a caller that builds its own ring from opening that
	// hole.
	ring := make([]string, 0, len(keys))
	for _, key := range keys {
		if strings.TrimSpace(key) != "" {
			ring = append(ring, key)
		}
	}
	if len(ring) == 0 {
		return empty, errors.New("campaigns: no unsubscribe secret configured, so no email link can be opened")
	}
	if kind == LinkConfirm && now.IsZero() {
		return empty, errNoLinkClock
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
	sealed, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return empty, errBadLinkToken
	}
	// The sealed bytes are opened as decoded bytes, and Go's decoder ignores CR
	// and LF anywhere in its input and does not require the unused bits of the
	// last character to be zero. Re-encoding and comparing leaves exactly one
	// spelling of the segment that opens.
	if base64.RawURLEncoding.EncodeToString(sealed) != parts[2] {
		return empty, errBadLinkToken
	}
	// A shape check, before the key is looked at, so that a string that is not
	// a token is a bad token and never an unknown key.
	if len(sealed) < linkMinSealedBytes {
		return empty, errBadLinkToken
	}

	// Narrow to the key the token names, as the unsubscribe parser does: without
	// it every probe costs an attempt to open per held key, and "a rotation
	// dropped a key" could not be told from "somebody is guessing".
	header := parts[0] + "." + parts[1]
	var raw []byte
	named, opened := false, false
	for _, key := range ring {
		if !hmac.Equal([]byte(LinkKeyID(key)), []byte(parts[1])) {
			continue
		}
		named = true
		aead, err := linkSealer(key)
		if err != nil {
			continue
		}
		nonce, ciphertext := sealed[:aead.NonceSize()], sealed[aead.NonceSize():]
		if plaintext, err := aead.Open(nil, nonce, ciphertext, linkAAD(pageContext, header)); err == nil {
			raw, opened = plaintext, true
			break
		}
	}
	if !named {
		return empty, errUnknownLinkKey
	}
	if !opened {
		return empty, errBadLinkToken
	}

	// Past this point the bytes are ours. They are still checked against the
	// rules minting applies: a validly sealed token this code would never have
	// issued is not one to act on.
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
//
// Links already sent are re-validated by these rules, so they are not free to
// change: tighten them only behind a new version prefix (l2), or preferences
// links, which never expire and are the opt-out path, stop working.
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
