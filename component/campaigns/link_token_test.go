package campaigns

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// link_token_test.go -- the sealed tokens behind the three email-link pages:
// confirm a subscription, change preferences, view a message in a browser
// (memql#6033).
//
// What these tests protect, in order of how badly it hurts to lose them:
//
//   - a token is opaque. It holds the recipient, the organization, a digest of
//     the address and the names the message showed, and links like these are
//     forwarded and posted, so none of it may be readable from the token. The
//     payload is sealed (AES-256-GCM), not just signed;
//   - a token minted for one page must not open another. The page's context is
//     authenticated data, not part of the token, so the only thing that can
//     refuse a cross-page token is the seal. TestALinkTokenForOnePageIsRefusedByAnother
//     uses a payload that is valid on EVERY page, which is what makes the seal
//     the only possible reason for the refusal;
//   - nothing can be changed without the secret: one flipped byte anywhere in a
//     token is refused;
//   - rotating the secret strands no link already in a mailbox, and a key this
//     node does not hold is told apart from a forgery (the log line differs);
//   - only a confirm link expires. A preferences link is where a person goes to
//     opt out, so an expiry there would be a compliance failure.
//
// The wire format is also checked against sealLinkByHand and openLinkByHand
// below, which build and open a token straight from the written design with
// nothing but crypto primitives and literal labels (HKDF included, written out
// from RFC 5869 rather than called). The implementation's own constants are not
// trusted to check themselves.

const (
	linkSecretA = "link-signing-secret-a-for-tests"
	linkSecretB = "link-signing-secret-b-for-tests"
	linkSecretC = "link-signing-secret-c-that-nobody-holds"
)

var (
	linkTestNow    = time.Date(2026, time.October, 12, 9, 30, 0, 0, time.UTC)
	linkTestDigest = EmailDigest("reader@example.test")
	linkKinds      = []LinkKind{LinkConfirm, LinkPreferences, LinkView}

	// linkOracleContexts are the three page contexts, spelled out here and not
	// read from the implementation.
	linkOracleContexts = map[LinkKind]string{
		LinkConfirm:     "memql/email/confirm/v1",
		LinkPreferences: "memql/email/preferences/v1",
		LinkView:        "memql/email/view/v1",
	}

	linkSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// fullLinkPayload is valid on every page: it carries what confirm needs (an
// expiry) and what view needs (a template and its revision). That is the point
// of it -- see TestALinkTokenForOnePageIsRefusedByAnother.
func fullLinkPayload() LinkPayload {
	return LinkPayload{
		OwnerUserID:      "owner-1",
		RecipientID:      "recipient-1",
		AccountID:        "account-1",
		EmailDigest:      linkTestDigest,
		CampaignID:       "campaign-1",
		TemplateID:       "template-1",
		TemplateRevision: "2026-10-11T09:30:00.123456789Z",
		SenderName:       "Acme News",
		CampaignName:     "October newsletter",
		ExpiresAt:        linkTestNow.Add(confirmLinkLifetime).Unix(),
	}
}

// minimalLinkPayload carries only what a page requires: no organization, no
// campaign, nothing optional.
func minimalLinkPayload(kind LinkKind) LinkPayload {
	p := LinkPayload{OwnerUserID: "owner-1", RecipientID: "recipient-1", EmailDigest: linkTestDigest}
	switch kind {
	case LinkConfirm:
		p.ExpiresAt = linkTestNow.Add(confirmLinkLifetime).Unix()
	case LinkView:
		p.TemplateID = "template-1"
		p.TemplateRevision = "2026-10-11T09:30:00.123456789Z"
	}
	return p
}

func mustMintLink(t *testing.T, secret string, kind LinkKind, p LinkPayload) string {
	t.Helper()
	token, err := MintLinkToken(secret, kind, p)
	if err != nil {
		t.Fatalf("MintLinkToken(%s): %v", kind, err)
	}
	return token
}

// splitLinkToken returns the three segments of a token: the version, the key id
// and the sealed payload.
func splitLinkToken(t *testing.T, token string) []string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token %q has %d segments, want 3", token, len(parts))
	}
	return parts
}

func decodeLinkSegment(t *testing.T, segment string) []byte {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		t.Fatalf("segment %q is not base64url: %v", segment, err)
	}
	return raw
}

// otherPage is a page that is not kind.
func otherPage(kind LinkKind) LinkKind {
	if kind == LinkView {
		return LinkPreferences
	}
	return LinkView
}

// rejoinLinkToken puts a token back together around a different sealed segment.
func rejoinLinkToken(parts []string, sealed []byte) string {
	return strings.Join([]string{parts[0], parts[1], base64.RawURLEncoding.EncodeToString(sealed)}, ".")
}

// hkdfSHA256ByHand is HKDF-SHA256 (RFC 5869) with no salt, for one 32-byte
// block, written from HMAC alone so that a mistake in how the implementation
// calls the standard library's HKDF is not shared by the check on it.
//
//	PRK  = HMAC-SHA256(salt = 32 zero bytes, IKM)     (an absent salt is HashLen zeros, section 2.2)
//	T(1) = HMAC-SHA256(PRK, info || 0x01)             (the first block is the whole 32-byte key)
func hkdfSHA256ByHand(secret, info string) []byte {
	extract := hmac.New(sha256.New, make([]byte, sha256.Size))
	extract.Write([]byte(secret))
	expand := hmac.New(sha256.New, extract.Sum(nil))
	expand.Write([]byte(info))
	expand.Write([]byte{0x01})
	return expand.Sum(nil)
}

// linkKeyIDByHand: hex(HMAC-SHA256(secret, "memql/email/link/key-id")[:4]).
func linkKeyIDByHand(secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("memql/email/link/key-id"))
	return hex.EncodeToString(mac.Sum(nil)[:4])
}

func linkAEADByHand(secret string) cipher.AEAD {
	block, err := aes.NewCipher(hkdfSHA256ByHand(secret, "memql/email/link/v1"))
	if err != nil {
		panic(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return gcm
}

// sealLinkByHand builds a token from the written format using only crypto
// primitives and literal strings:
//
//	l1.<keyId>.<base64url(nonce || ciphertext || tag)>
//	keyId = hex(HMAC-SHA256(secret, "memql/email/link/key-id")[:4])
//	key   = HKDF-SHA256(secret, no salt, info "memql/email/link/v1") -> 32 bytes
//	seal  = AES-256-GCM(key, 12-byte nonce, plaintext, aad = context + "\x00" + "l1.<keyId>")
//
// It takes the plaintext as raw bytes so a test can seal something MintLinkToken
// would refuse, which is how the parser's own checks are exercised past the seal.
func sealLinkByHand(secret, pageContext string, nonce, plaintext []byte) string {
	header := "l1." + linkKeyIDByHand(secret)
	sealed := linkAEADByHand(secret).Seal(append([]byte(nil), nonce...), nonce, plaintext, []byte(pageContext+"\x00"+header))
	return header + "." + base64.RawURLEncoding.EncodeToString(sealed)
}

func sealPayloadByHand(t *testing.T, secret string, kind LinkKind, nonce []byte, p LinkPayload) string {
	t.Helper()
	return sealLinkByHand(secret, linkOracleContexts[kind], nonce, mustJSON(t, p))
}

// openLinkByHand opens a token the way the written format says, with the
// context of the page the test names. It is the oracle for what MintLinkToken
// produced.
func openLinkByHand(secret, pageContext, token string) ([]byte, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("not three segments")
	}
	sealed, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, err
	}
	if len(sealed) < 12+16 {
		return nil, errors.New("shorter than a nonce and a tag")
	}
	return linkAEADByHand(secret).Open(nil, sealed[:12], sealed[12:], []byte(pageContext+"\x00"+parts[0]+"."+parts[1]))
}

// testNonce is a 12-byte nonce for the hand-sealed tokens; n makes them differ.
func testNonce(n byte) []byte {
	nonce := make([]byte, 12)
	for i := range nonce {
		nonce[i] = n + byte(i)
	}
	return nonce
}

func TestLinkTokensRoundTripPerKind(t *testing.T) {
	keys := []string{linkSecretA}
	for _, kind := range linkKinds {
		shapes := map[string]LinkPayload{
			"every field":              fullLinkPayload(),
			"only what the page needs": minimalLinkPayload(kind),
		}
		for shape, want := range shapes {
			token := mustMintLink(t, linkSecretA, kind, want)

			parts := splitLinkToken(t, token)
			if parts[0] != "l1" || parts[1] != LinkKeyID(linkSecretA) {
				t.Errorf("%s/%s: token %q does not start l1.<keyId of the signing secret>", kind, shape, token)
			}

			got, err := ParseLinkToken(keys, kind, token, linkTestNow)
			if err != nil {
				t.Fatalf("%s/%s: a token we just minted does not verify: %v", kind, shape, err)
			}
			if got != want {
				t.Errorf("%s/%s: round trip changed the payload.\n got %+v\nwant %+v", kind, shape, got, want)
			}
		}
	}
}

// TestLinkTokenFormatMatchesTheSpecification pins the exact labels, contexts,
// key derivation and layout. A token that round-trips with ITSELF proves nothing
// about these: swap a label for another and every other test here still passes.
// So the token MintLinkToken makes is opened by the oracle, and the token the
// oracle seals is the very token MintLinkToken makes from the same nonce.
func TestLinkTokenFormatMatchesTheSpecification(t *testing.T) {
	keys := []string{linkSecretA}
	for _, kind := range linkKinds {
		for _, p := range []LinkPayload{fullLinkPayload(), minimalLinkPayload(kind)} {
			plaintext := mustJSON(t, p)

			// What MintLinkToken makes opens, by hand, to the payload and to nothing else.
			minted := mustMintLink(t, linkSecretA, kind, p)
			opened, err := openLinkByHand(linkSecretA, linkOracleContexts[kind], minted)
			if err != nil {
				t.Fatalf("%s: the oracle cannot open the token MintLinkToken made: %v", kind, err)
			}
			if !bytes.Equal(opened, plaintext) {
				t.Errorf("%s: the sealed payload is %s, want %s", kind, opened, plaintext)
			}
			parts := splitLinkToken(t, minted)
			if parts[0] != "l1" || parts[1] != linkKeyIDByHand(linkSecretA) {
				t.Errorf("%s: the header is %q, want l1.%s", kind, parts[0]+"."+parts[1], linkKeyIDByHand(linkSecretA))
			}
			if sealed := decodeLinkSegment(t, parts[2]); len(sealed) != 12+len(plaintext)+16 {
				t.Errorf("%s: the sealed segment is %d bytes, want a 12-byte nonce, the %d payload bytes and a 16-byte tag", kind, len(sealed), len(plaintext))
			}
			// The page's context is authenticated, so no other page opens it.
			for _, other := range linkKinds {
				if other == kind {
					continue
				}
				if _, err := openLinkByHand(linkSecretA, linkOracleContexts[other], minted); err == nil {
					t.Errorf("%s: the oracle opened a %s token with the %s page's context", kind, kind, other)
				}
			}
			// And only its own secret does.
			if _, err := openLinkByHand(linkSecretB, linkOracleContexts[kind], minted); err == nil {
				t.Errorf("%s: the oracle opened the token with another secret", kind)
			}

			// From one nonce, the implementation and the oracle make the same token.
			nonce := testNonce(7)
			byHand := sealLinkByHand(linkSecretA, linkOracleContexts[kind], nonce, plaintext)
			fromNonce, err := mintLinkToken(bytes.NewReader(nonce), linkSecretA, kind, p)
			if err != nil {
				t.Fatalf("%s: mintLinkToken: %v", kind, err)
			}
			if fromNonce != byHand {
				t.Errorf("%s: the token differs from the one sealed by hand from the specification.\n got %s\nwant %s", kind, fromNonce, byHand)
			}
			// ...and the verifier takes the oracle's token.
			got, err := ParseLinkToken(keys, kind, byHand, linkTestNow)
			if err != nil || got != p {
				t.Errorf("%s: the token sealed by hand was refused or changed: %+v, %v", kind, got, err)
			}
		}
	}
}

func TestLinkKeyIDIsDerivedFromTheSecretAndLabelledApart(t *testing.T) {
	idMAC := hmac.New(sha256.New, []byte(linkSecretA))
	idMAC.Write([]byte("memql/email/link/key-id"))
	if want := hex.EncodeToString(idMAC.Sum(nil)[:4]); LinkKeyID(linkSecretA) != want {
		t.Errorf("LinkKeyID = %q, want %q (first 4 bytes of HMAC(secret, label), hex)", LinkKeyID(linkSecretA), want)
	}
	if LinkKeyID(linkSecretA) == LinkKeyID(linkSecretB) {
		t.Fatal("two different secrets share a key id")
	}
	if strings.Contains(LinkKeyID(linkSecretA), linkSecretA) {
		t.Fatal("the key id echoes the secret")
	}
	// One secret keys all three families. Each must name it differently, or
	// each parser would recognise the others' tokens as naming a key it holds
	// and go on to check the signature (or try to open the seal) instead of
	// stopping at the label.
	if LinkKeyID(linkSecretA) == UnsubscribeKeyID(linkSecretA) || LinkKeyID(linkSecretA) == TrackingKeyID(linkSecretA) {
		t.Error("the link key-id label produces the same id as another token family's for one secret")
	}
}

// TestALinkTokenForOnePageIsRefusedByAnother -- the page a request arrives at
// chooses the associated data the seal is opened with, so a token for one page
// does not open on another. The payload is valid on every page, so no payload
// check can be what refuses it.
func TestALinkTokenForOnePageIsRefusedByAnother(t *testing.T) {
	keys := []string{linkSecretA}
	for _, minted := range linkKinds {
		token := mustMintLink(t, linkSecretA, minted, fullLinkPayload())
		for _, presented := range linkKinds {
			got, err := ParseLinkToken(keys, presented, token, linkTestNow)
			if presented == minted {
				if err != nil {
					t.Errorf("a %s token was refused at the %s page it was minted for: %v", minted, presented, err)
				}
				continue
			}
			if !errors.Is(err, errBadLinkToken) {
				t.Errorf("a %s token presented at the %s page: err = %v, want errBadLinkToken. Under a shared key "+
					"that would let the weakest page's rules govern the strongest page's action", minted, presented, err)
			}
			if got != (LinkPayload{}) {
				t.Errorf("a %s token presented at the %s page still returned a payload: %+v", minted, presented, got)
			}
		}
	}
}

// TestAFlippedByteAnywhereInALinkTokenIsRefused -- the seal covers the nonce,
// the ciphertext and the tag, and the header is authenticated data, so there is
// no byte of a token that can be changed. Every refusal is errBadLinkToken, and
// none returns any of the payload; the one exception is a change to the key id,
// which can also name a key this node does not hold.
func TestAFlippedByteAnywhereInALinkTokenIsRefused(t *testing.T) {
	keys := []string{linkSecretA, linkSecretB}
	for _, kind := range linkKinds {
		good := mustMintLink(t, linkSecretA, kind, fullLinkPayload())
		parts := splitLinkToken(t, good)
		sealed := decodeLinkSegment(t, parts[2])

		// Every byte of nonce || ciphertext || tag, three bits of each.
		for i := range sealed {
			for _, bit := range []byte{0x01, 0x10, 0x80} {
				edited := append([]byte(nil), sealed...)
				edited[i] ^= bit
				got, err := ParseLinkToken(keys, kind, rejoinLinkToken(parts, edited), linkTestNow)
				if !errors.Is(err, errBadLinkToken) {
					t.Errorf("%s: byte %d of the sealed segment with bit %#x flipped: err = %v, want errBadLinkToken", kind, i, bit, err)
				}
				if got != (LinkPayload{}) {
					t.Errorf("%s: byte %d flipped: returned payload %+v", kind, i, got)
				}
			}
		}

		// Every character of the token as it is written, replaced by another.
		headerLen := len(parts[0]) + 1 + len(parts[1]) + 1
		for i := 0; i < len(good); i++ {
			replacement := byte('A')
			if good[i] == 'A' {
				replacement = 'B'
			}
			edited := good[:i] + string(replacement) + good[i+1:]
			got, err := ParseLinkToken(keys, kind, edited, linkTestNow)
			if err == nil {
				t.Errorf("%s: character %d of the token replaced: still verified", kind, i)
				continue
			}
			if i >= headerLen && !errors.Is(err, errBadLinkToken) {
				t.Errorf("%s: character %d of the sealed segment replaced: err = %v, want errBadLinkToken", kind, i, err)
			}
			if got != (LinkPayload{}) {
				t.Errorf("%s: character %d replaced: returned payload %+v", kind, i, got)
			}
		}
	}
}

// TestAForgedLinkTokenIsRefused -- the shapes of forgery that are not a single
// flipped bit: parts of one genuine token put together with parts of another.
func TestAForgedLinkTokenIsRefused(t *testing.T) {
	keys := []string{linkSecretA, linkSecretB}
	for _, kind := range linkKinds {
		good := mustMintLink(t, linkSecretA, kind, fullLinkPayload())
		parts := splitLinkToken(t, good)
		sealed := decodeLinkSegment(t, parts[2])

		other := fullLinkPayload()
		other.OwnerUserID = "owner-2" // aiming the impersonation
		otherToken := mustMintLink(t, linkSecretA, kind, other)
		otherSealed := decodeLinkSegment(t, splitLinkToken(t, otherToken)[2])

		withNonceOf := func(donor []byte) []byte {
			return append(append([]byte(nil), donor[:12]...), sealed[12:]...)
		}
		forgeries := map[string]string{
			// (Another token's whole sealed segment is not a forgery: under the same
			// header it IS that genuine token. What a holder of one token cannot do is
			// move it to another page, or mix two.)
			"another page's sealed segment":            rejoinLinkToken(parts, decodeLinkSegment(t, splitLinkToken(t, mustMintLink(t, linkSecretA, otherPage(kind), fullLinkPayload()))[2])),
			"another token's nonce on this ciphertext": rejoinLinkToken(parts, withNonceOf(otherSealed)),
			"this nonce on another token's ciphertext": rejoinLinkToken(parts, append(append([]byte(nil), sealed[:12]...), otherSealed[12:]...)),
			"the nonce zeroed":                         rejoinLinkToken(parts, append(make([]byte, 12), sealed[12:]...)),
			"the tag zeroed":                           rejoinLinkToken(parts, append(append([]byte(nil), sealed[:len(sealed)-16]...), make([]byte, 16)...)),
			"the tag removed":                          rejoinLinkToken(parts, sealed[:len(sealed)-16]),
			"one byte short":                           rejoinLinkToken(parts, sealed[:len(sealed)-1]),
			"one byte long":                            rejoinLinkToken(parts, append(append([]byte(nil), sealed...), 0)),
			"the ciphertext doubled":                   rejoinLinkToken(parts, append(append([]byte(nil), sealed...), sealed[12:]...)),
			"shorter than a nonce and a tag":           rejoinLinkToken(parts, sealed[:27]),
			"no sealed bytes at all":                   parts[0] + "." + parts[1] + ".",
			// The ring holds both keys, so naming the OTHER one is "known key, wrong
			// key" and must be a forgery, not an unknown key. The header is
			// authenticated data, so it fails the seal even before the key is wrong.
			"the key id of the other held key": strings.Join([]string{parts[0], LinkKeyID(linkSecretB), parts[2]}, "."),
			"another version":                  strings.Join([]string{"l2", parts[1], parts[2]}, "."),
		}
		for name, forged := range forgeries {
			got, err := ParseLinkToken(keys, kind, forged, linkTestNow)
			if !errors.Is(err, errBadLinkToken) {
				t.Errorf("%s link with %s: err = %v, want errBadLinkToken", kind, name, err)
			}
			if got != (LinkPayload{}) {
				t.Errorf("%s link with %s: returned payload %+v", kind, name, got)
			}
		}
	}
}

func TestParseRefusesShapesThatAreNotLinkTokens(t *testing.T) {
	keys := []string{linkSecretA}
	good := mustMintLink(t, linkSecretA, LinkView, fullLinkPayload())
	parts := splitLinkToken(t, good)
	unsub, err := MintUnsubscribeToken(linkSecretA, UnsubscribePayload{OwnerUserID: "owner-1", RecipientID: "recipient-1", CampaignID: "campaign-1"})
	if err != nil {
		t.Fatalf("mint unsubscribe: %v", err)
	}
	track, err := MintTrackingToken(linkSecretA, TrackingPayload{DeliveryID: "delivery-1", CampaignID: "campaign-1", Kind: EngagementOpen})
	if err != nil {
		t.Fatalf("mint tracking: %v", err)
	}

	cases := map[string]string{
		"empty":                              "",
		"whitespace":                         "   ",
		"the version only":                   "l1",
		"dots only":                          "...",
		"empty segments":                     "l1..",
		"two segments":                       strings.Join(parts[:2], "."),
		"four segments":                      good + ".extra",
		"the old four-segment shape":         good + "." + parts[2][:22],
		"another version":                    "l2." + strings.Join(parts[1:], "."),
		"no version":                         strings.Join(parts[1:], "."),
		"a sealed segment that is no base64": strings.Join([]string{parts[0], parts[1], "!!!!"}, "."),
		"a sealed segment of padding":        strings.Join([]string{parts[0], parts[1], parts[2] + "=="}, "."),
		"an unsubscribe token":               unsub,
		"a tracking token":                   track,
	}
	for name, token := range cases {
		got, err := ParseLinkToken(keys, LinkView, token, linkTestNow)
		if !errors.Is(err, errBadLinkToken) {
			t.Errorf("%s: err = %v, want errBadLinkToken", name, err)
		}
		if got != (LinkPayload{}) {
			t.Errorf("%s: returned payload %+v", name, got)
		}
	}

	// And the other direction: neither older family may accept a link token.
	for _, kind := range linkKinds {
		link := mustMintLink(t, linkSecretA, kind, fullLinkPayload())
		if _, err := ParseUnsubscribeToken(keys, link); err == nil {
			t.Errorf("a %s link token verified as an UNSUBSCRIBE token", kind)
		}
		if _, err := ParseTrackingToken(keys, link); err == nil {
			t.Errorf("a %s link token verified as a TRACKING token", kind)
		}
	}

	// A link pasted with a stray newline still works, as an unsubscribe link does.
	if _, err := ParseLinkToken(keys, LinkView, " "+good+"\n", linkTestNow); err != nil {
		t.Errorf("surrounding whitespace made a good token fail: %v", err)
	}
}

// TestParseChecksTheLinkPayloadItselfPastTheSeal -- a validly sealed token that
// this code would never have minted is refused anyway. It cannot come from
// MintLinkToken, so the fixtures are sealed by hand.
func TestParseChecksTheLinkPayloadItselfPastTheSeal(t *testing.T) {
	keys := []string{linkSecretA}
	nonce := testNonce(1)
	seal := func(kind LinkKind, p LinkPayload) string { return sealPayloadByHand(t, linkSecretA, kind, nonce, p) }
	sealRaw := func(kind LinkKind, raw []byte) string {
		return sealLinkByHand(linkSecretA, linkOracleContexts[kind], nonce, raw)
	}

	cases := map[string]struct {
		kind  LinkKind
		token string
	}{
		"no owner":     {LinkPreferences, seal(LinkPreferences, LinkPayload{RecipientID: "r", EmailDigest: linkTestDigest})},
		"no recipient": {LinkPreferences, seal(LinkPreferences, LinkPayload{OwnerUserID: "o", EmailDigest: linkTestDigest})},
		"no digest":    {LinkPreferences, seal(LinkPreferences, LinkPayload{OwnerUserID: "o", RecipientID: "r"})},
		"an uppercase digest": {LinkPreferences, seal(LinkPreferences,
			LinkPayload{OwnerUserID: "o", RecipientID: "r", EmailDigest: strings.ToUpper(linkTestDigest)})},
		"a confirm link with no expiry": {LinkConfirm, seal(LinkConfirm, minimalLinkPayloadWithout(LinkConfirm, func(p *LinkPayload) { p.ExpiresAt = 0 }))},
		"a view link with no template":  {LinkView, seal(LinkView, minimalLinkPayloadWithout(LinkView, func(p *LinkPayload) { p.TemplateID = "" }))},
		"a view link with no revision":  {LinkView, seal(LinkView, minimalLinkPayloadWithout(LinkView, func(p *LinkPayload) { p.TemplateRevision = "" }))},
		"a payload that is not JSON":    {LinkPreferences, sealRaw(LinkPreferences, []byte("not json"))},
		"a JSON null":                   {LinkPreferences, sealRaw(LinkPreferences, []byte("null"))},
		"an empty payload":              {LinkPreferences, sealRaw(LinkPreferences, nil)},
		"a payload with trailing bytes": {LinkPreferences, sealRaw(LinkPreferences, append(mustJSON(t, minimalLinkPayload(LinkPreferences)), []byte(`{}`)...))},
	}
	for name, tc := range cases {
		got, err := ParseLinkToken(keys, tc.kind, tc.token, linkTestNow)
		if !errors.Is(err, errBadLinkToken) {
			t.Errorf("%s: err = %v, want errBadLinkToken", name, err)
		}
		if got != (LinkPayload{}) {
			t.Errorf("%s: returned payload %+v", name, got)
		}
	}

	// Control: the same hand-sealed route accepts a good payload, so the cases
	// above fail because of what they carry and not because of how they were sealed.
	for _, kind := range linkKinds {
		if _, err := ParseLinkToken(keys, kind, seal(kind, minimalLinkPayload(kind)), linkTestNow); err != nil {
			t.Errorf("control: a good %s payload sealed by hand was refused: %v", kind, err)
		}
	}
}

func minimalLinkPayloadWithout(kind LinkKind, edit func(*LinkPayload)) LinkPayload {
	p := minimalLinkPayload(kind)
	edit(&p)
	return p
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return raw
}

func TestALinkSealedByThePreviousSecretVerifies(t *testing.T) {
	// A rotation in progress: B is the new current secret and A is the previous one.
	rotated := Config{UnsubscribeSecret: linkSecretB, UnsubscribeSecretPrevious: linkSecretA}
	for _, kind := range linkKinds {
		want := fullLinkPayload()

		// Minted before the rotation, sitting in a mailbox.
		before := mustMintLink(t, linkSecretA, kind, want)
		got, err := ParseLinkToken(rotated.UnsubscribeKeys(), kind, before, linkTestNow)
		if err != nil {
			t.Fatalf("%s: a link minted before the rotation stopped verifying after it: %v", kind, err)
		}
		if got != want {
			t.Errorf("%s: the rotation changed the payload: %+v", kind, got)
		}

		// Minting after the rotation uses the current secret only.
		after := mustMintLink(t, rotated.UnsubscribeSecret, kind, want)
		if id := splitLinkToken(t, after)[1]; id != LinkKeyID(linkSecretB) || id == LinkKeyID(linkSecretA) {
			t.Errorf("%s: a token minted after the rotation names key %q, want the CURRENT secret's %q", kind, id, LinkKeyID(linkSecretB))
		}

		// A node that has finished rotating (previous dropped) still verifies
		// what it mints, and has stopped verifying the oldest links: that is the
		// second rotation, the one that cannot be survived.
		finished := Config{UnsubscribeSecret: linkSecretB}
		if _, err := ParseLinkToken(finished.UnsubscribeKeys(), kind, after, linkTestNow); err != nil {
			t.Errorf("%s: a node on the new secret alone refused a token it just minted: %v", kind, err)
		}
		if _, err := ParseLinkToken(finished.UnsubscribeKeys(), kind, before, linkTestNow); !errors.Is(err, errUnknownLinkKey) {
			t.Errorf("%s: after the second rotation err = %v, want errUnknownLinkKey", kind, err)
		}
	}
}

// TestAnUnknownLinkKeyIsReportedDistinctly -- the one failure an operator can
// act on. The recipient sees the same page as for a forgery; only the log differs.
func TestAnUnknownLinkKeyIsReportedDistinctly(t *testing.T) {
	keys := []string{linkSecretA, linkSecretB}
	for _, kind := range linkKinds {
		token := mustMintLink(t, linkSecretC, kind, fullLinkPayload())

		got, err := ParseLinkToken(keys, kind, token, linkTestNow)
		if !errors.Is(err, errUnknownLinkKey) {
			t.Errorf("%s: a token sealed under a key this node does not hold: err = %v, want errUnknownLinkKey", kind, err)
		}
		if errors.Is(err, errBadLinkToken) {
			t.Errorf("%s: an unknown key must not also read as a forgery", kind)
		}
		if got != (LinkPayload{}) {
			t.Errorf("%s: returned payload %+v", kind, got)
		}
		if id := linkKeyIDOf(token); id != LinkKeyID(linkSecretC) {
			t.Errorf("%s: linkKeyIDOf = %q, want the id in the token, %q", kind, id, LinkKeyID(linkSecretC))
		}

		// A forgery under a key this node DOES hold is not an unknown key.
		good := mustMintLink(t, linkSecretA, kind, fullLinkPayload())
		parts := splitLinkToken(t, good)
		flipped := decodeLinkSegment(t, parts[2])
		flipped[0] ^= 0x01
		forged := rejoinLinkToken(parts, flipped)
		if _, err := ParseLinkToken(keys, kind, forged, linkTestNow); errors.Is(err, errUnknownLinkKey) {
			t.Errorf("%s: a forgery under a held key was reported as an unknown key", kind)
		}
	}

	// The key id goes into a log line, so it must never echo attacker-chosen bytes.
	good := mustMintLink(t, linkSecretA, LinkView, fullLinkPayload())
	for name, token := range map[string]string{
		"not a token":                  "not-a-token",
		"empty":                        "",
		"a key id that is not hex":     "l1.zzzzzzzz.a",
		"a key id of the wrong length": "l1.deadbeef00.a",
		"too few segments":             "l1." + LinkKeyID(linkSecretA),
		"too many segments":            good + ".x",
		"another version":              "l2." + strings.Join(splitLinkToken(t, good)[1:], "."),
		"oversized":                    "l1." + LinkKeyID(linkSecretA) + "." + strings.Repeat("a", 5000),
	} {
		if id := linkKeyIDOf(token); id != "" {
			t.Errorf("linkKeyIDOf(%s) = %q, want empty", name, id)
		}
	}
}

func TestAnExpiredConfirmLinkIsRefused(t *testing.T) {
	if confirmLinkLifetime != 30*24*time.Hour {
		t.Fatalf("confirmLinkLifetime = %s, want 30 days", confirmLinkLifetime)
	}
	keys := []string{linkSecretA}
	want := fullLinkPayload()
	want.ExpiresAt = linkTestNow.Add(confirmLinkLifetime).Unix()
	token := mustMintLink(t, linkSecretA, LinkConfirm, want)
	expiry := time.Unix(want.ExpiresAt, 0)

	cases := []struct {
		name    string
		now     time.Time
		expired bool
	}{
		{"when it was issued", linkTestNow, false},
		{"a day before it expires", expiry.Add(-24 * time.Hour), false},
		{"at the instant it expires", expiry, false},
		{"a second after", expiry.Add(time.Second), true},
		{"a year after", expiry.Add(365 * 24 * time.Hour), true},
	}
	for _, tc := range cases {
		got, err := ParseLinkToken(keys, LinkConfirm, token, tc.now)
		if tc.expired {
			if !errors.Is(err, errExpiredLinkToken) {
				t.Errorf("%s: err = %v, want errExpiredLinkToken", tc.name, err)
			}
			if errors.Is(err, errBadLinkToken) {
				t.Errorf("%s: an expired link must not read as a forgery", tc.name)
			}
			// The payload was verified before the expiry was looked at, and the
			// page that says "this link has expired" still has to name the
			// organization the subscription was for.
			if got != want {
				t.Errorf("%s: the verified payload was not returned with the expiry error: %+v", tc.name, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: a live confirm link was refused: %v", tc.name, err)
		}
		if got != want {
			t.Errorf("%s: payload = %+v", tc.name, got)
		}
	}
}

// TestOnlyConfirmLinksExpire -- preferences and view links sit in a mailbox as
// long as the message does, and the preferences page is where a person opts
// out. An expiry that took effect there would be a stale opt-out link.
func TestOnlyConfirmLinksExpire(t *testing.T) {
	keys := []string{linkSecretA}
	long := linkTestNow.Add(10 * 365 * 24 * time.Hour)
	for _, kind := range []LinkKind{LinkPreferences, LinkView} {
		p := fullLinkPayload()
		p.ExpiresAt = 1 // a long-past instant, in case a caller sets one by mistake
		token := mustMintLink(t, linkSecretA, kind, p)
		if _, err := ParseLinkToken(keys, kind, token, long); err != nil {
			t.Errorf("a %s link ten years on: %v. These links never expire", kind, err)
		}
		if _, err := ParseLinkToken(keys, kind, mustMintLink(t, linkSecretA, kind, minimalLinkPayload(kind)), long); err != nil {
			t.Errorf("a %s link with no expiry, ten years on: %v", kind, err)
		}
	}
}

// TestAnExpiredLinkIsNotReportedForAForgery -- the seal is opened before the
// expiry is looked at, so a forged token never reaches the expiry check: only a
// token this code sealed can be reported as expired.
func TestAnExpiredLinkIsNotReportedForAForgery(t *testing.T) {
	keys := []string{linkSecretA}
	good := mustMintLink(t, linkSecretA, LinkConfirm, fullLinkPayload())
	parts := splitLinkToken(t, good)
	edited := decodeLinkSegment(t, parts[2])
	edited[len(edited)/2] ^= 0x01
	forged := rejoinLinkToken(parts, edited)
	late := linkTestNow.Add(5 * 365 * 24 * time.Hour)

	got, err := ParseLinkToken(keys, LinkConfirm, forged, late)
	if !errors.Is(err, errBadLinkToken) || errors.Is(err, errExpiredLinkToken) {
		t.Errorf("a forged confirm link long after its expiry: err = %v, want errBadLinkToken", err)
	}
	if got != (LinkPayload{}) {
		t.Errorf("returned payload %+v", got)
	}

	// The same holds for a genuine confirm token shown to the wrong page.
	if _, err := ParseLinkToken(keys, LinkPreferences, good, late); !errors.Is(err, errBadLinkToken) {
		t.Errorf("an expired confirm token at the preferences page: err = %v, want errBadLinkToken", err)
	}
}

func TestLinkTokensAreURLSafe(t *testing.T) {
	keys := []string{linkSecretA}
	sawStandardEncoding := map[string]bool{}

	// Fixed nonces, so the bytes sealed are the same on every run, and names of
	// every length, so the sealed segment passes through every remainder mod 3
	// and padding ("=") would appear in standard base64.
	for n := 0; n < 240; n++ {
		p := fullLinkPayload()
		p.SenderName = strings.Repeat("?", n%5) + strings.Repeat("~", n%7) + " & café / 100% +1 =x – news"
		p.CampaignName = strings.Repeat("ÿ", n%3) + "?" + strings.Repeat("a", n%11)

		for _, kind := range linkKinds {
			token, err := mintLinkToken(bytes.NewReader(testNonce(byte(n))), linkSecretA, kind, p)
			if err != nil {
				t.Fatalf("%s (n=%d): %v", kind, n, err)
			}
			standard := base64.StdEncoding.EncodeToString(decodeLinkSegment(t, splitLinkToken(t, token)[2]))
			for _, special := range []string{"/", "+", "="} {
				if strings.Contains(standard, special) {
					sawStandardEncoding[special] = true
				}
			}
			if strings.ContainsAny(token, "/+=") {
				t.Errorf("%s (n=%d): token contains a character that is not URL-safe: %q", kind, n, token)
			}
			if escaped := url.QueryEscape(token); escaped != token {
				t.Errorf("%s (n=%d): the token needs escaping in a query string: %q -> %q", kind, n, token, escaped)
			}
			for _, segment := range strings.Split(token, ".") {
				if !linkSegmentPattern.MatchString(segment) {
					t.Errorf("%s (n=%d): segment %q is outside [A-Za-z0-9_-]", kind, n, segment)
				}
			}
			if got, err := ParseLinkToken(keys, kind, token, linkTestNow); err != nil || got != p {
				t.Errorf("%s (n=%d): did not round trip: %+v, %v", kind, n, got, err)
			}
		}
	}
	for _, special := range []string{"/", "+", "="} {
		if !sawStandardEncoding[special] {
			t.Errorf("none of the test tokens would have put %q in standard base64, so this test proves nothing about it", special)
		}
	}
}

func TestMintRefusesAnIncompletePayload(t *testing.T) {
	// Controls first: each page mints from exactly what it requires.
	for _, kind := range linkKinds {
		if _, err := MintLinkToken(linkSecretA, kind, minimalLinkPayload(kind)); err != nil {
			t.Fatalf("control: the %s page's minimal payload was refused: %v", kind, err)
		}
	}
	if strings.ToUpper(linkTestDigest) == linkTestDigest {
		t.Fatal("precondition: the test digest has no letters, so uppercasing it proves nothing")
	}

	cases := []struct {
		name   string
		secret string
		kind   LinkKind
		edit   func(*LinkPayload)
	}{
		{"no owner", linkSecretA, LinkPreferences, func(p *LinkPayload) { p.OwnerUserID = "" }},
		{"no recipient", linkSecretA, LinkPreferences, func(p *LinkPayload) { p.RecipientID = "" }},
		{"no digest", linkSecretA, LinkPreferences, func(p *LinkPayload) { p.EmailDigest = "" }},
		{"a digest that is not hex", linkSecretA, LinkPreferences, func(p *LinkPayload) { p.EmailDigest = strings.Repeat("z", 64) }},
		{"an uppercase digest", linkSecretA, LinkPreferences, func(p *LinkPayload) { p.EmailDigest = strings.ToUpper(linkTestDigest) }},
		{"a digest of the wrong length", linkSecretA, LinkPreferences, func(p *LinkPayload) { p.EmailDigest = linkTestDigest[:62] }},
		{"a confirm link with no expiry", linkSecretA, LinkConfirm, func(p *LinkPayload) { p.ExpiresAt = 0 }},
		{"a confirm link with a negative expiry", linkSecretA, LinkConfirm, func(p *LinkPayload) { p.ExpiresAt = -1 }},
		{"a view link with no template", linkSecretA, LinkView, func(p *LinkPayload) { p.TemplateID = "" }},
		{"a view link with no template revision", linkSecretA, LinkView, func(p *LinkPayload) { p.TemplateRevision = "" }},
		{"a page that does not exist", linkSecretA, LinkKind("admin"), func(*LinkPayload) {}},
		{"no page at all", linkSecretA, LinkKind(""), func(*LinkPayload) {}},
		{"no secret", "", LinkView, func(*LinkPayload) {}},
		{"a blank secret", "   ", LinkView, func(*LinkPayload) {}},
	}
	for _, tc := range cases {
		p := fullLinkPayload()
		tc.edit(&p)
		token, err := MintLinkToken(tc.secret, tc.kind, p)
		if err == nil {
			t.Errorf("%s: minted %q anyway", tc.name, token)
		}
		if token != "" {
			t.Errorf("%s: returned a token alongside the error: %q", tc.name, token)
		}
	}
}

// TestLinkTokensOverTheSizeLimitAreRefusedBothWays -- 4096 bytes is the limit.
// The parser refuses an oversized token whatever its seal says (the fixtures are
// sealed by hand and are valid in every other respect), and the minter never
// issues a token the parser would refuse, so a message cannot ship with a link
// that can never work.
func TestLinkTokensOverTheSizeLimitAreRefusedBothWays(t *testing.T) {
	keys := []string{linkSecretA}
	nonce := testNonce(3)
	withPad := func(pad int) LinkPayload {
		p := fullLinkPayload()
		p.CampaignName = strings.Repeat("n", pad)
		return p
	}

	// Find where the limit falls, then look at both sides of it.
	firstOver := 0
	for pad := 0; ; pad++ {
		if len(sealPayloadByHand(t, linkSecretA, LinkView, nonce, withPad(pad))) > 4096 {
			firstOver = pad
			break
		}
		if pad > 10000 {
			t.Fatal("precondition: no padding made the token exceed 4096 bytes")
		}
	}
	sawFits, sawOver := false, false
	for pad := firstOver - 4; pad <= firstOver+2; pad++ {
		p := withPad(pad)
		byHand := sealPayloadByHand(t, linkSecretA, LinkView, nonce, p)
		minted, mintErr := mintLinkToken(bytes.NewReader(nonce), linkSecretA, LinkView, p)
		parsed, parseErr := ParseLinkToken(keys, LinkView, byHand, linkTestNow)

		if len(byHand) <= 4096 {
			sawFits = true
			if mintErr != nil || minted != byHand {
				t.Errorf("pad %d (%d bytes): a token within the limit was not minted: %v", pad, len(byHand), mintErr)
			}
			if parseErr != nil || parsed != p {
				t.Errorf("pad %d (%d bytes): a token within the limit was refused: %v", pad, len(byHand), parseErr)
			}
			continue
		}
		sawOver = true
		if mintErr == nil {
			t.Errorf("pad %d (%d bytes): minted a token the parser would refuse", pad, len(byHand))
		}
		if !errors.Is(parseErr, errBadLinkToken) {
			t.Errorf("pad %d (%d bytes): a validly sealed oversized token: err = %v, want errBadLinkToken", pad, len(byHand), parseErr)
		}
		if parsed != (LinkPayload{}) {
			t.Errorf("pad %d: an oversized token returned a payload", pad)
		}
	}
	if !sawFits || !sawOver {
		t.Fatalf("the sizes tried did not straddle the limit (fits=%v over=%v)", sawFits, sawOver)
	}
}

func TestParseLinkNeedsAKeyAndAKnownPage(t *testing.T) {
	good := mustMintLink(t, linkSecretA, LinkView, fullLinkPayload())
	if _, err := ParseLinkToken(nil, LinkView, good, linkTestNow); err == nil {
		t.Error("a node holding no keys opened a token")
	}
	if _, err := ParseLinkToken([]string{linkSecretA}, LinkKind("admin"), good, linkTestNow); err == nil {
		t.Error("a page that does not exist verified a token")
	}
	if _, err := ParseLinkToken([]string{linkSecretA}, LinkKind(""), good, linkTestNow); err == nil {
		t.Error("no page at all verified a token")
	}
}

// --- fix round 1: the exported verifier fails closed ---------------------------

// TestABlankKeyInTheLinkRingVerifiesNothing -- an empty secret derives a key
// like any other (HMAC and HKDF both take an empty input), so a ring that held
// one would verify any token its forger sealed under that key.
// Config.UnsubscribeKeys never yields a blank, but ParseLinkToken is exported
// and the ring is its argument, so it fails closed on its own.
func TestABlankKeyInTheLinkRingVerifiesNothing(t *testing.T) {
	for _, blank := range []string{"", "   ", "\t\n"} {
		rings := map[string][]string{
			"only the blank key":            {blank},
			"two blank keys":                {blank, blank},
			"the blank key then a real one": {blank, linkSecretA},
			"a real key then the blank one": {linkSecretA, blank},
		}
		for _, kind := range linkKinds {
			// Sealed under the blank key by hand: MintLinkToken refuses to seal with one.
			forged := sealPayloadByHand(t, blank, kind, testNonce(5), fullLinkPayload())
			for name, ring := range rings {
				got, err := ParseLinkToken(ring, kind, forged, linkTestNow)
				if err == nil {
					t.Errorf("%s link sealed under the blank key %q verified against %s", kind, blank, name)
				}
				if got != (LinkPayload{}) {
					t.Errorf("%s link sealed under the blank key %q, %s: returned payload %+v", kind, blank, name, got)
				}
			}
		}
	}

	genuine := mustMintLink(t, linkSecretA, LinkView, fullLinkPayload())

	// A ring with nothing usable in it refuses every token, genuine ones included,
	// and reads as a missing secret: nothing was rotated, so it is not an unknown key.
	for _, ring := range [][]string{{""}, {"  "}, {"", ""}} {
		_, err := ParseLinkToken(ring, LinkView, genuine, linkTestNow)
		if err == nil {
			t.Errorf("ring %q verified a genuine token", ring)
			continue
		}
		if errors.Is(err, errUnknownLinkKey) || errors.Is(err, errBadLinkToken) {
			t.Errorf("ring %q: err = %v. An empty ring is a missing secret, not a rotation problem or a bad link", ring, err)
		}
	}

	// And a blank entry beside a real key does not disturb it.
	for _, ring := range [][]string{{"", linkSecretA}, {linkSecretA, ""}, {"  ", linkSecretA}} {
		if _, err := ParseLinkToken(ring, LinkView, genuine, linkTestNow); err != nil {
			t.Errorf("ring %q refused a genuine token: %v", ring, err)
		}
	}
}

// TestAConfirmLinkIsRefusedWithoutAClock -- the zero time sits before every
// expiry, so a caller that forgot to pass the time would see every confirm link
// as live, expired ones included. ParseLinkToken refuses to guess.
func TestAConfirmLinkIsRefusedWithoutAClock(t *testing.T) {
	keys := []string{linkSecretA}
	expired := fullLinkPayload()
	expired.ExpiresAt = 1 // long past
	live := fullLinkPayload()

	for name, p := range map[string]LinkPayload{"expired": expired, "live": live} {
		token := mustMintLink(t, linkSecretA, LinkConfirm, p)
		got, err := ParseLinkToken(keys, LinkConfirm, token, time.Time{})
		if !errors.Is(err, errNoLinkClock) {
			t.Errorf("%s confirm link, zero clock: err = %v, want errNoLinkClock", name, err)
		}
		// A caller bug, not something the recipient did: it must not be one of the
		// two errors a page turns into "this link is not valid" or "has expired".
		if errors.Is(err, errBadLinkToken) || errors.Is(err, errExpiredLinkToken) || errors.Is(err, errUnknownLinkKey) {
			t.Errorf("%s confirm link, zero clock: %v reads as a link problem", name, err)
		}
		if got != (LinkPayload{}) {
			t.Errorf("%s confirm link, zero clock: returned payload %+v", name, got)
		}
	}

	// With a clock the same expired token takes the normal path.
	if _, err := ParseLinkToken(keys, LinkConfirm, mustMintLink(t, linkSecretA, LinkConfirm, expired), linkTestNow); !errors.Is(err, errExpiredLinkToken) {
		t.Errorf("control: with a clock the expired link gave err = %v, want errExpiredLinkToken", err)
	}
}

// TestPreferencesAndViewLinksNeedNoClock -- the clock is only consulted for a
// confirm link's expiry, so the other two pages take no position on it.
func TestPreferencesAndViewLinksNeedNoClock(t *testing.T) {
	keys := []string{linkSecretA}
	for _, kind := range []LinkKind{LinkPreferences, LinkView} {
		token := mustMintLink(t, linkSecretA, kind, fullLinkPayload())
		if _, err := ParseLinkToken(keys, kind, token, time.Time{}); err != nil {
			t.Errorf("%s link with a zero clock: %v", kind, err)
		}
	}
}

// TestARespelledSealedSegmentIsRefused -- the sealed segment is opened as
// decoded bytes, and Go's base64 decoder is lenient: it ignores CR and LF
// anywhere in its input and does not insist that the unused bits of the last
// character are zero. Without a check on the spelling, several different
// strings open to the same bytes, and the seal itself cannot tell them apart.
// Exactly one spelling of a token verifies.
func TestARespelledSealedSegmentIsRefused(t *testing.T) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	keys := []string{linkSecretA}
	for _, kind := range linkKinds {
		for _, remainder := range []int{0, 1, 2} {
			// The sealed segment is the payload and 28 bytes; padding the campaign
			// name moves it through the three remainders mod 3. 1 leaves four unused
			// bits in the last character, 2 leaves two, and 0 leaves none.
			p := fullLinkPayload()
			for (len(mustJSON(t, p))+28)%3 != remainder {
				p.CampaignName += "x"
			}
			good := mustMintLink(t, linkSecretA, kind, p)
			parts := splitLinkToken(t, good)
			sealed := parts[2]
			goodBytes := decodeLinkSegment(t, sealed)
			if len(goodBytes)%3 != remainder {
				t.Fatalf("precondition: %d sealed bytes, want a remainder of %d", len(goodBytes), remainder)
			}

			respellings := map[string]string{
				"a line feed inside the segment":         sealed[:10] + "\n" + sealed[10:],
				"a carriage return and line feed inside": sealed[:10] + "\r\n" + sealed[10:],
				"padding characters added":               sealed + "==",
			}
			last := strings.IndexByte(alphabet, sealed[len(sealed)-1])
			switch remainder {
			case 1:
				respellings["one unused bit set"] = sealed[:len(sealed)-1] + string(alphabet[last|0x01])
				respellings["every unused bit set"] = sealed[:len(sealed)-1] + string(alphabet[last|0x0f])
			case 2:
				respellings["one unused bit set"] = sealed[:len(sealed)-1] + string(alphabet[last|0x01])
				respellings["every unused bit set"] = sealed[:len(sealed)-1] + string(alphabet[last|0x03])
			}
			for name, respelled := range respellings {
				// The point of the test: Go's decoder reads these as the very same
				// bytes (the padded one it already rejects), and the seal opens them.
				if name != "padding characters added" {
					decoded, err := base64.RawURLEncoding.DecodeString(respelled)
					if err != nil || !bytes.Equal(decoded, goodBytes) {
						t.Fatalf("precondition: %s no longer decodes to the same bytes (err %v)", name, err)
					}
					if _, err := openLinkByHand(linkSecretA, linkOracleContexts[kind], parts[0]+"."+parts[1]+"."+respelled); err != nil {
						t.Fatalf("precondition: the seal does not open %s: %v", name, err)
					}
				}
				token := strings.Join([]string{parts[0], parts[1], respelled}, ".")
				got, err := ParseLinkToken(keys, kind, token, linkTestNow)
				if !errors.Is(err, errBadLinkToken) {
					t.Errorf("%s link (remainder %d) with %s: err = %v, want errBadLinkToken", kind, remainder, name, err)
				}
				if got != (LinkPayload{}) {
					t.Errorf("%s link (remainder %d) with %s: returned payload %+v", kind, remainder, name, got)
				}
			}

			// Control: the spelling we mint still verifies.
			if _, err := ParseLinkToken(keys, kind, good, linkTestNow); err != nil {
				t.Errorf("%s link (remainder %d) in its canonical spelling was refused: %v", kind, remainder, err)
			}
		}
	}
}

// TestALinkCarriesTheBareOrganizationId -- as the u3 unsubscribe token does,
// the organization id is sealed in its bare form. What ParseLinkToken returns is
// then the id the pages compare against, and a canonical id never lengthens the URL.
func TestALinkCarriesTheBareOrganizationId(t *testing.T) {
	keys := []string{linkSecretA}
	const canonical = "v1:common:account:acct-42"
	for _, kind := range linkKinds {
		p := fullLinkPayload()
		p.AccountID = canonical
		token := mustMintLink(t, linkSecretA, kind, p)

		got, err := ParseLinkToken(keys, kind, token, linkTestNow)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		want := p
		want.AccountID = "acct-42"
		if got != want {
			t.Errorf("%s: payload = %+v, want the organization as %q", kind, got, "acct-42")
		}
		opened, err := openLinkByHand(linkSecretA, linkOracleContexts[kind], token)
		if err != nil {
			t.Fatalf("%s: the oracle cannot open the token: %v", kind, err)
		}
		if strings.Contains(string(opened), "v1:common") {
			t.Errorf("%s: the sealed payload carries the canonical id: %s", kind, opened)
		}
		if p.AccountID != canonical {
			t.Errorf("%s: minting changed the caller's payload to %q", kind, p.AccountID)
		}
	}
}

// TestNoPartOfALinkTokenReadsAsItsContents -- the payload names the recipient,
// the organization, a digest of the address and what the message showed, and
// these links are forwarded and posted. The digest in particular is an unkeyed
// hash: whoever holds it can confirm a guessed address offline. So nothing in a
// token, as written or as decoded, may contain any of it.
func TestNoPartOfALinkTokenReadsAsItsContents(t *testing.T) {
	p := fullLinkPayload()
	p.OwnerUserID = "owner-2f6c1d90a7"
	p.RecipientID = "recipient-8e3b44c5d1"
	p.AccountID = "account-17a9e0b6c2"
	p.CampaignID = "campaign-5d02f87e3a"
	p.TemplateID = "template-c4719b2e08"

	digest, err := hex.DecodeString(p.EmailDigest)
	if err != nil {
		t.Fatalf("precondition: the digest is not hex: %v", err)
	}
	secrets := map[string][]byte{
		"the digest, as hex":            []byte(p.EmailDigest),
		"the digest, as its 32 bytes":   digest,
		"the owner":                     []byte(p.OwnerUserID),
		"the recipient":                 []byte(p.RecipientID),
		"the organization":              []byte(p.AccountID),
		"the campaign":                  []byte(p.CampaignID),
		"the template":                  []byte(p.TemplateID),
		"the template revision":         []byte(p.TemplateRevision),
		"the sender's name":             []byte(p.SenderName),
		"the campaign's name":           []byte(p.CampaignName),
		"the JSON field names":          []byte(`"o":`),
		"the digest, base64 of its hex": []byte(base64.StdEncoding.EncodeToString([]byte(p.EmailDigest))[:40]),
		"the owner, base64":             []byte(base64.RawURLEncoding.EncodeToString([]byte(p.OwnerUserID))),
		"the recipient, base64":         []byte(base64.RawURLEncoding.EncodeToString([]byte(p.RecipientID))),
		"the organization, base64":      []byte(base64.RawURLEncoding.EncodeToString([]byte(p.AccountID))),
	}

	for _, kind := range linkKinds {
		// Many tokens, so that a leak that depends on the nonce cannot hide in one.
		for n := 0; n < 40; n++ {
			token, err := mintLinkToken(bytes.NewReader(testNonce(byte(n*5))), linkSecretA, kind, p)
			if err != nil {
				t.Fatalf("%s: %v", kind, err)
			}
			views := map[string][]byte{"the token as written": []byte(token)}
			for i, segment := range strings.Split(token, ".") {
				views[segment[:min(len(segment), 6)]+"... as written"] = []byte(segment)
				if raw, err := base64.RawURLEncoding.DecodeString(segment); err == nil {
					views[segment[:min(len(segment), 6)]+"... decoded from base64url"] = raw
				}
				if raw, err := hex.DecodeString(segment); err == nil {
					views["segment "+string(rune('0'+i))+" decoded from hex"] = raw
				}
			}
			for what, secret := range secrets {
				for view, data := range views {
					if bytes.Contains(data, secret) {
						t.Errorf("%s (n=%d): %s appears in %s of the token", kind, n, what, view)
					}
				}
			}
		}
	}
}

// TestTwoTokensForOnePayloadDiffer -- every token carries a fresh random nonce,
// so the same payload sealed twice is two unrelated strings: a link in one
// message says nothing about whether a link in another is for the same person.
func TestTwoTokensForOnePayloadDiffer(t *testing.T) {
	for _, kind := range linkKinds {
		first, second := mustMintLink(t, linkSecretA, kind, fullLinkPayload()), mustMintLink(t, linkSecretA, kind, fullLinkPayload())
		if first == second {
			t.Fatalf("%s: two tokens for one payload are the same string", kind)
		}
		a, b := decodeLinkSegment(t, splitLinkToken(t, first)[2]), decodeLinkSegment(t, splitLinkToken(t, second)[2])
		if bytes.Equal(a[:12], b[:12]) {
			t.Errorf("%s: two tokens share a nonce", kind)
		}
		if bytes.Equal(a[12:], b[12:]) || bytes.Equal(a[12:len(a)-16], b[12:len(b)-16]) {
			t.Errorf("%s: two tokens share ciphertext, so the nonce is not reaching the cipher", kind)
		}
	}

	// And across many: no nonce repeats.
	seen := map[string]bool{}
	for n := 0; n < 2000; n++ {
		nonce := string(decodeLinkSegment(t, splitLinkToken(t, mustMintLink(t, linkSecretA, LinkPreferences, minimalLinkPayload(LinkPreferences)))[2])[:12])
		if seen[nonce] {
			t.Fatalf("a nonce repeated after %d tokens", n)
		}
		seen[nonce] = true
	}
}

// TestTheNonceComesFromTheRandomSource -- MintLinkToken reads its nonce from the
// operating system's random source and refuses to mint if that fails, rather
// than sealing under a nonce it could not get.
func TestTheNonceComesFromTheRandomSource(t *testing.T) {
	if _, err := mintLinkToken(bytes.NewReader(nil), linkSecretA, LinkView, fullLinkPayload()); err == nil {
		t.Error("a token was minted although no nonce could be read")
	}
	if _, err := mintLinkToken(bytes.NewReader(make([]byte, 11)), linkSecretA, LinkView, fullLinkPayload()); err == nil {
		t.Error("a token was minted from a nonce one byte short")
	}
}
