package campaigns

import (
	"bytes"
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

// link_token_test.go -- the signed tokens behind the three email-link pages:
// confirm a subscription, change preferences, view a message in a browser
// (memql#6033).
//
// What these tests protect, in order of how badly it hurts to lose them:
//
//   - a token minted for one page must not open another. The context is part
//     of the MAC and is not in the token, so the only thing that can refuse a
//     cross-page token is the MAC. TestALinkTokenForOnePageIsRefusedByAnother
//     uses a payload that is valid on EVERY page, which is what makes the MAC
//     the only possible reason for the refusal;
//   - nothing in the payload can be changed without the secret;
//   - rotating the secret strands no link already in a mailbox, and a key this
//     node does not hold is told apart from a forgery (the log line differs);
//   - only a confirm link expires. A preferences link is where a person goes to
//     opt out, so an expiry there would be a compliance failure.
//
// The wire format is also checked against signLinkByHand below, which builds a
// token straight from the written design with nothing but crypto primitives.
// The implementation's own constants are not trusted to check themselves.

const (
	linkSecretA = "link-signing-secret-a-for-tests"
	linkSecretB = "link-signing-secret-b-for-tests"
	linkSecretC = "link-signing-secret-c-that-nobody-holds"
)

var (
	linkTestNow    = time.Date(2026, time.October, 12, 9, 30, 0, 0, time.UTC)
	linkTestDigest = EmailDigest("reader@example.test")
	linkKinds      = []LinkKind{LinkConfirm, LinkPreferences, LinkView}

	// linkOracleContexts are the three MAC contexts, spelled out here and not
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

func splitLinkToken(t *testing.T, token string) []string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 4 {
		t.Fatalf("token %q has %d segments, want 4", token, len(parts))
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

// signLinkByHand builds a token from the written format using only crypto
// primitives and literal strings:
//
//	l1.<keyId>.<base64url(payload)>.<base64url(tag)>
//	keyId = hex(HMAC-SHA256(secret, "memql/email/link/key-id")[:4])
//	tag   = HMAC-SHA256(secret, context + "\x00" + "l1.<keyId>.<payload>")[:16]
//
// It takes the payload as raw bytes so a test can sign something MintLinkToken
// would refuse, which is how the parser's own checks are exercised past the MAC.
func signLinkByHand(secret, macContext string, rawPayload []byte) string {
	idMAC := hmac.New(sha256.New, []byte(secret))
	idMAC.Write([]byte("memql/email/link/key-id"))
	keyID := hex.EncodeToString(idMAC.Sum(nil)[:4])

	body := "l1." + keyID + "." + base64.RawURLEncoding.EncodeToString(rawPayload)

	tagMAC := hmac.New(sha256.New, []byte(secret))
	tagMAC.Write([]byte(macContext + "\x00" + body))
	return body + "." + base64.RawURLEncoding.EncodeToString(tagMAC.Sum(nil)[:16])
}

func signPayloadByHand(t *testing.T, secret string, kind LinkKind, p LinkPayload) string {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return signLinkByHand(secret, linkOracleContexts[kind], raw)
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

// TestLinkTokenFormatMatchesTheSpecification pins the exact labels, contexts
// and tag length. A token that round-trips with ITSELF proves nothing about
// these: swap a label for another and every other test here still passes.
func TestLinkTokenFormatMatchesTheSpecification(t *testing.T) {
	for _, kind := range linkKinds {
		for _, p := range []LinkPayload{fullLinkPayload(), minimalLinkPayload(kind)} {
			got := mustMintLink(t, linkSecretA, kind, p)
			want := signPayloadByHand(t, linkSecretA, kind, p)
			if got != want {
				t.Errorf("%s: minted token differs from the one built by hand from the specification.\n got %s\nwant %s", kind, got, want)
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
	// One secret signs all three families. Each must name it differently, or
	// each parser would recognise the others' tokens as naming a key it holds
	// and fall through to the MAC instead of stopping at the label.
	if LinkKeyID(linkSecretA) == UnsubscribeKeyID(linkSecretA) || LinkKeyID(linkSecretA) == TrackingKeyID(linkSecretA) {
		t.Error("the link key-id label produces the same id as another token family's for one secret")
	}
}

// TestALinkTokenForOnePageIsRefusedByAnother -- the page a request arrives at
// chooses the MAC context, so a token for one page is a bad MAC on another.
// The payload is valid on every page, so no payload check can be what refuses it.
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

// TestAForgedLinkTokenIsRefused -- every way of editing a token without the
// secret ends in errBadLinkToken, and never returns any of the payload.
func TestAForgedLinkTokenIsRefused(t *testing.T) {
	keys := []string{linkSecretA, linkSecretB}
	for _, kind := range linkKinds {
		good := mustMintLink(t, linkSecretA, kind, fullLinkPayload())
		parts := splitLinkToken(t, good)
		goodPayload := decodeLinkSegment(t, parts[2])
		goodTag := decodeLinkSegment(t, parts[3])

		// keepTag re-encodes an edited payload and leaves the original tag, which
		// is exactly what an attacker without the secret can do.
		keepTag := func(edit func(payload []byte) []byte) string {
			edited := edit(append([]byte(nil), goodPayload...))
			return strings.Join([]string{parts[0], parts[1], base64.RawURLEncoding.EncodeToString(edited), parts[3]}, ".")
		}
		replace := func(from, to string) func([]byte) []byte {
			return func(payload []byte) []byte {
				if !strings.Contains(string(payload), from) {
					t.Fatalf("%s: precondition: payload %s does not contain %q", kind, payload, from)
				}
				return []byte(strings.Replace(string(payload), from, to, 1))
			}
		}
		withTag := func(tag []byte) string {
			return strings.Join([]string{parts[0], parts[1], parts[2], base64.RawURLEncoding.EncodeToString(tag)}, ".")
		}
		flippedTag := append([]byte(nil), goodTag...)
		flippedTag[0] ^= 0x01
		other := fullLinkPayload()
		other.OwnerUserID = "owner-2"
		otherToken := mustMintLink(t, linkSecretA, kind, other)

		forgeries := map[string]string{
			"one payload byte flipped": keepTag(func(p []byte) []byte { p[len(p)/2] ^= 0x01; return p }),
			// The shapes below are VALID payloads: only the MAC stands in the way.
			"the owner swapped (aiming the impersonation)": keepTag(replace("owner-1", "owner-2")),
			"the recipient swapped":                        keepTag(replace("recipient-1", "recipient-2")),
			"the organization swapped":                     keepTag(replace("account-1", "account-2")),
			"the digest swapped for another valid one":     keepTag(replace(linkTestDigest, EmailDigest("someone-else@example.test"))),
			"the expiry pushed out":                        keepTag(replace(`"x":`, `"x":9`)),
			"another token's payload under this tag":       strings.Join([]string{parts[0], parts[1], splitLinkToken(t, otherToken)[2], parts[3]}, "."),
			"one tag bit flipped":                          withTag(flippedTag),
			"the tag zeroed":                               withTag(make([]byte, len(goodTag))),
			"the tag truncated":                            strings.TrimSuffix(good, good[len(good)-1:]),
			"the tag removed":                              strings.Join(parts[:3], ".") + ".",
			// The ring holds both keys, so naming the OTHER one is "known key,
			// wrong MAC" and must be a forgery, not an unknown key.
			"the key id of the other held key": strings.Join([]string{parts[0], LinkKeyID(linkSecretB), parts[2], parts[3]}, "."),
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
		"empty":                       "",
		"whitespace":                  "   ",
		"the version only":            "l1",
		"dots only":                   "...",
		"empty segments":              "l1...",
		"three segments":              strings.Join(parts[:3], "."),
		"five segments":               good + ".extra",
		"another version":             "l2." + strings.Join(parts[1:], "."),
		"no version":                  strings.Join(parts[1:], "."),
		"a payload that is no base64": strings.Join([]string{parts[0], parts[1], "!!!!", parts[3]}, "."),
		"an unsubscribe token":        unsub,
		"a tracking token":            track,
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

// TestParseChecksTheLinkPayloadItselfPastTheMAC -- a validly signed token that
// this code would never have minted is refused anyway. It cannot come from
// MintLinkToken, so the fixtures are signed by hand.
func TestParseChecksTheLinkPayloadItselfPastTheMAC(t *testing.T) {
	keys := []string{linkSecretA}
	sign := func(kind LinkKind, p LinkPayload) string { return signPayloadByHand(t, linkSecretA, kind, p) }

	cases := map[string]struct {
		kind  LinkKind
		token string
	}{
		"no owner":     {LinkPreferences, sign(LinkPreferences, LinkPayload{RecipientID: "r", EmailDigest: linkTestDigest})},
		"no recipient": {LinkPreferences, sign(LinkPreferences, LinkPayload{OwnerUserID: "o", EmailDigest: linkTestDigest})},
		"no digest":    {LinkPreferences, sign(LinkPreferences, LinkPayload{OwnerUserID: "o", RecipientID: "r"})},
		"an uppercase digest": {LinkPreferences, sign(LinkPreferences,
			LinkPayload{OwnerUserID: "o", RecipientID: "r", EmailDigest: strings.ToUpper(linkTestDigest)})},
		"a confirm link with no expiry": {LinkConfirm, sign(LinkConfirm, minimalLinkPayloadWithout(LinkConfirm, func(p *LinkPayload) { p.ExpiresAt = 0 }))},
		"a view link with no template":  {LinkView, sign(LinkView, minimalLinkPayloadWithout(LinkView, func(p *LinkPayload) { p.TemplateID = "" }))},
		"a view link with no revision":  {LinkView, sign(LinkView, minimalLinkPayloadWithout(LinkView, func(p *LinkPayload) { p.TemplateRevision = "" }))},
		"a payload that is not JSON":    {LinkPreferences, signLinkByHand(linkSecretA, linkOracleContexts[LinkPreferences], []byte("not json"))},
		"a JSON null":                   {LinkPreferences, signLinkByHand(linkSecretA, linkOracleContexts[LinkPreferences], []byte("null"))},
		"an empty payload":              {LinkPreferences, signLinkByHand(linkSecretA, linkOracleContexts[LinkPreferences], nil)},
		"a payload with trailing bytes": {LinkPreferences, signLinkByHand(linkSecretA, linkOracleContexts[LinkPreferences], append(mustJSON(t, minimalLinkPayload(LinkPreferences)), []byte(`{}`)...))},
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

	// Control: the same hand-signed route accepts a good payload, so the cases
	// above fail because of what they carry and not because of how they were signed.
	for _, kind := range linkKinds {
		if _, err := ParseLinkToken(keys, kind, sign(kind, minimalLinkPayload(kind)), linkTestNow); err != nil {
			t.Errorf("control: a good %s payload signed by hand was refused: %v", kind, err)
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

func TestALinkSignedByThePreviousSecretVerifies(t *testing.T) {
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
			t.Errorf("%s: a token signed by a key this node does not hold: err = %v, want errUnknownLinkKey", kind, err)
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
		forged := strings.Join([]string{parts[0], parts[1], base64.RawURLEncoding.EncodeToString(flipped), parts[3]}, ".")
		if _, err := ParseLinkToken(keys, kind, forged, linkTestNow); errors.Is(err, errUnknownLinkKey) {
			t.Errorf("%s: a forgery under a held key was reported as an unknown key", kind)
		}
	}

	// The key id goes into a log line, so it must never echo attacker-chosen bytes.
	good := mustMintLink(t, linkSecretA, LinkView, fullLinkPayload())
	for name, token := range map[string]string{
		"not a token":                  "not-a-token",
		"empty":                        "",
		"a key id that is not hex":     "l1.zzzzzzzz.a.b",
		"a key id of the wrong length": "l1.deadbeef00.a.b",
		"too many segments":            good + ".x",
		"another version":              "l2." + strings.Join(splitLinkToken(t, good)[1:], "."),
		"oversized":                    "l1." + LinkKeyID(linkSecretA) + "." + strings.Repeat("a", 5000) + ".b",
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

// TestAnExpiredLinkIsNotReportedForAForgery -- the MAC is checked before the
// expiry, so a forged token never reaches the expiry check: only a token this
// code signed can be reported as expired.
func TestAnExpiredLinkIsNotReportedForAForgery(t *testing.T) {
	keys := []string{linkSecretA}
	good := mustMintLink(t, linkSecretA, LinkConfirm, fullLinkPayload())
	parts := splitLinkToken(t, good)
	edited := strings.Replace(string(decodeLinkSegment(t, parts[2])), "owner-1", "owner-2", 1)
	forged := strings.Join([]string{parts[0], parts[1], base64.RawURLEncoding.EncodeToString([]byte(edited)), parts[3]}, ".")
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

	for n := 0; n < 9; n++ {
		p := fullLinkPayload()
		// Runs of "?" and "~" are the bytes that standard base64 turns into "/" and
		// "+" at the right alignment; the other names push the length through every
		// remainder so padding ("=") appears too.
		p.SenderName = strings.Repeat("?", n) + strings.Repeat("~", n+1) + " & caf\u00e9 / 100% +1 =x \u2013 news"
		p.CampaignName = strings.Repeat("\u00ff", n) + "?" + strings.Repeat("a", n)

		standard := base64.StdEncoding.EncodeToString(mustJSON(t, p))
		for _, special := range []string{"/", "+", "="} {
			if strings.Contains(standard, special) {
				sawStandardEncoding[special] = true
			}
		}

		for _, kind := range linkKinds {
			token := mustMintLink(t, linkSecretA, kind, p)
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
			t.Errorf("none of the test payloads would have put %q in standard base64, so this test proves nothing about it", special)
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
// The parser refuses an oversized token whatever its MAC says (the fixtures are
// signed by hand and are valid in every other respect), and the minter never
// issues a token the parser would refuse, so a message cannot ship with a link
// that can never work.
func TestLinkTokensOverTheSizeLimitAreRefusedBothWays(t *testing.T) {
	keys := []string{linkSecretA}
	withPad := func(pad int) LinkPayload {
		p := fullLinkPayload()
		p.CampaignName = strings.Repeat("n", pad)
		return p
	}

	// Find where the limit falls, then look at both sides of it.
	firstOver := 0
	for pad := 0; ; pad++ {
		if len(signPayloadByHand(t, linkSecretA, LinkView, withPad(pad))) > 4096 {
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
		byHand := signPayloadByHand(t, linkSecretA, LinkView, p)
		minted, mintErr := MintLinkToken(linkSecretA, LinkView, p)
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
			t.Errorf("pad %d (%d bytes): a validly signed oversized token: err = %v, want errBadLinkToken", pad, len(byHand), parseErr)
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
		t.Error("a node holding no signing keys verified a token")
	}
	if _, err := ParseLinkToken([]string{linkSecretA}, LinkKind("admin"), good, linkTestNow); err == nil {
		t.Error("a page that does not exist verified a token")
	}
	if _, err := ParseLinkToken([]string{linkSecretA}, LinkKind(""), good, linkTestNow); err == nil {
		t.Error("no page at all verified a token")
	}
}

// --- fix round 1: the exported verifier fails closed ---------------------------

// TestABlankKeyInTheLinkRingVerifiesNothing -- HMAC accepts an empty key, so a
// ring that held one would verify any token its forger signed under that key.
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
			// Signed under the blank key by hand: MintLinkToken refuses to sign with one.
			forged := signPayloadByHand(t, blank, kind, fullLinkPayload())
			for name, ring := range rings {
				got, err := ParseLinkToken(ring, kind, forged, linkTestNow)
				if err == nil {
					t.Errorf("%s link signed under the blank key %q verified against %s", kind, blank, name)
				}
				if got != (LinkPayload{}) {
					t.Errorf("%s link signed under the blank key %q, %s: returned payload %+v", kind, blank, name, got)
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

// TestARespelledLinkTagIsRefused -- the tag is compared as decoded bytes, and
// Go's base64 decoder is lenient: it ignores CR and LF anywhere in its input and
// does not insist that the unused bits of the last character are zero. Without
// a check on the spelling, several different strings carry the same tag. Exactly
// one spelling of a token verifies.
func TestARespelledLinkTagIsRefused(t *testing.T) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	keys := []string{linkSecretA}
	for _, kind := range linkKinds {
		good := mustMintLink(t, linkSecretA, kind, fullLinkPayload())
		parts := splitLinkToken(t, good)
		tag := parts[3]
		goodTagBytes := decodeLinkSegment(t, tag)
		if len(tag) != 22 {
			t.Fatalf("precondition: tag %q is %d characters, want 22", tag, len(tag))
		}
		// 16 bytes are 128 bits: 21 full characters and a 22nd carrying 2 bits and
		// 4 bits of padding, which a minted tag leaves zero.
		last := strings.IndexByte(alphabet, tag[21])
		if last < 0 || last&0x0f != 0 {
			t.Fatalf("precondition: the last character of %q does not have zero padding bits", tag)
		}

		respellings := map[string]string{
			"one padding bit set":                    tag[:21] + string(alphabet[last|0x01]),
			"every padding bit set":                  tag[:21] + string(alphabet[last|0x0f]),
			"a line feed inside the tag":             tag[:10] + "\n" + tag[10:],
			"a carriage return and line feed inside": tag[:10] + "\r\n" + tag[10:],
			"padding characters added":               tag + "==",
		}
		for name, respelled := range respellings {
			// The point of the test: Go's decoder reads these as the very same tag.
			// (The padded one it already rejects.)
			if name != "padding characters added" {
				decoded, err := base64.RawURLEncoding.DecodeString(respelled)
				if err != nil || !bytes.Equal(decoded, goodTagBytes) {
					t.Fatalf("precondition: %s no longer decodes to the same tag (err %v)", name, err)
				}
			}
			token := strings.Join([]string{parts[0], parts[1], parts[2], respelled}, ".")
			got, err := ParseLinkToken(keys, kind, token, linkTestNow)
			if !errors.Is(err, errBadLinkToken) {
				t.Errorf("%s link with %s: err = %v, want errBadLinkToken", kind, name, err)
			}
			if got != (LinkPayload{}) {
				t.Errorf("%s link with %s: returned payload %+v", kind, name, got)
			}
		}

		// A line feed inside the PAYLOAD was never accepted: the MAC covers its text.
		payload := parts[2]
		inPayload := strings.Join([]string{parts[0], parts[1], payload[:10] + "\n" + payload[10:], parts[3]}, ".")
		if _, err := ParseLinkToken(keys, kind, inPayload, linkTestNow); !errors.Is(err, errBadLinkToken) {
			t.Errorf("%s link with a line feed inside the payload: err = %v, want errBadLinkToken", kind, err)
		}

		// Control: the spelling we mint still verifies.
		if _, err := ParseLinkToken(keys, kind, good, linkTestNow); err != nil {
			t.Errorf("%s link in its canonical spelling was refused: %v", kind, err)
		}
	}
}

// TestALinkCarriesTheBareOrganizationId -- as the u3 unsubscribe token does,
// the organization id is signed in its bare form. What ParseLinkToken returns is
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
		if raw := string(decodeLinkSegment(t, splitLinkToken(t, token)[2])); strings.Contains(raw, "v1:common") {
			t.Errorf("%s: the token carries the canonical id: %s", kind, raw)
		}
		if p.AccountID != canonical {
			t.Errorf("%s: minting changed the caller's payload to %q", kind, p.AccountID)
		}
	}
}
