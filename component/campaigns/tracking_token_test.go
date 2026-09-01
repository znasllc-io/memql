package campaigns

import (
	"errors"
	"strings"
	"testing"
)

// tracking_token_test.go -- mirrors token_rotation_test.go, because the two
// token families share a key ring and the properties that matter are the same
// ones (memql#4823).
//
// The tests unique to this family are the CROSS-CONTEXT pair. A tracking
// token that verified as an unsubscribe token would be an opt-out anybody
// could trigger by loading an image; an unsubscribe token that verified as a
// tracking token would let a click be attributed to a delivery it did not
// belong to. Both are asserted, in both directions, because "we use a
// different label" is the kind of separation a later refactor removes without
// noticing.

const (
	trackSecretA = "tracking-secret-a-not-a-credential"
	trackSecretB = "tracking-secret-b-not-a-credential"
)

func trackingPayload() TrackingPayload {
	return TrackingPayload{
		DeliveryID: "v1:campaigns:delivery:d-1",
		CampaignID: "camp-1",
		Kind:       EngagementClick,
		URL:        "https://acme.test/spring?utm=1&x=2",
	}
}

func TestTrackingTokenRoundTrips(t *testing.T) {
	token, err := MintTrackingToken(trackSecretA, trackingPayload())
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	got, err := ParseTrackingToken([]string{trackSecretA}, token)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got != trackingPayload() {
		t.Errorf("round trip lost data.\n got %+v\nwant %+v", got, trackingPayload())
	}
}

// TestTrackingTokenIsOnePathSegment is not a style check. The
// self-authenticated bypass that lets an unauthenticated mail client reach
// the handler is bounded to ONE segment under the mount, so a token
// containing a slash is 401'd before the handler runs -- which reaches the
// recipient as a broken image, with nothing on our side saying why.
func TestTrackingTokenIsOnePathSegment(t *testing.T) {
	// Both kinds, and a URL stuffed with the characters that would break the
	// encoding if any part of the token were raw rather than base64url. An
	// open token's url segment is EMPTY, which is a different encoding path
	// from a click's and is worth covering separately.
	cases := map[string]TrackingPayload{
		"a click carrying a slash-heavy url": {
			DeliveryID: "v1:campaigns:delivery:d-1", CampaignID: "camp-1", Kind: EngagementClick,
			URL: "https://acme.test/a/b/c?q=/slashes/everywhere#frag",
		},
		"an open with no url at all": {
			DeliveryID: "v1:campaigns:delivery:d-1", CampaignID: "camp-1", Kind: EngagementOpen,
		},
		"ids that are canonical and contain colons": {
			DeliveryID: "v1:campaigns:delivery:d/1", CampaignID: "v1:campaigns:campaign:c/1",
			Kind: EngagementOpen,
		},
	}
	for name, p := range cases {
		token, err := MintTrackingToken(trackSecretA, p)
		if err != nil {
			t.Fatalf("%s: mint: %v", name, err)
		}
		if strings.ContainsAny(token, "/?#") {
			t.Errorf("%s: the token contains a character that ends a path segment: %q\n"+
				"The bff verifier's self-authenticated exemption for these two mounts is bounded to ONE "+
				"further segment, and Go's mux splits on / as well -- so such a token is not mis-parsed, "+
				"it is 401'd before the handler runs. Every pixel would answer 401, every campaign would "+
				"report zero opens, and zero is a completely plausible number, which is why nothing would "+
				"report it. Standard base64 emits /; base64url does not.", name, token)
		}
		if _, err := ParseTrackingToken([]string{trackSecretA}, token); err != nil {
			t.Errorf("%s: the minted token does not verify: %v", name, err)
		}
	}
}

func TestTamperedTrackingTokenIsRefused(t *testing.T) {
	token, err := MintTrackingToken(trackSecretA, trackingPayload())
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	parts := strings.Split(token, ".")

	cases := map[string]func([]string) []string{
		"the redirect target": func(p []string) []string {
			// THE case this signature exists for: swap the URL and the
			// endpoint becomes an open redirect.
			out := append([]string(nil), p...)
			out[6] = "aHR0cHM6Ly9ldmlsLnRlc3Q"
			return out
		},
		"the delivery id": func(p []string) []string {
			out := append([]string(nil), p...)
			out[3] = "dGFtcGVyZWQ"
			return out
		},
		"the kind": func(p []string) []string {
			out := append([]string(nil), p...)
			out[5] = "b3Blbg" // "open"
			return out
		},
		"the key id": func(p []string) []string {
			out := append([]string(nil), p...)
			out[1] = "deadbeef"
			return out
		},
		"the tag": func(p []string) []string {
			out := append([]string(nil), p...)
			out[7] = "AAAAAAAAAAAAAAAAAAAAAA"
			return out
		},
	}
	for name, mutate := range cases {
		if _, err := ParseTrackingToken([]string{trackSecretA}, strings.Join(mutate(parts), ".")); err == nil {
			t.Errorf("a token with %s altered was ACCEPTED", name)
		}
	}
}

// TestAnUnsubscribeTokenNeverVerifiesAsTracking and its twin are the whole
// reason for the separate key-id label and the context string in the body.
func TestAnUnsubscribeTokenNeverVerifiesAsTracking(t *testing.T) {
	unsub, err := MintUnsubscribeToken(trackSecretA, "u-1", "r-1", "camp-1")
	if err != nil {
		t.Fatalf("mint unsubscribe: %v", err)
	}
	if _, err := ParseTrackingToken([]string{trackSecretA}, unsub); err == nil {
		t.Fatal("an UNSUBSCRIBE token verified as a tracking token. Under a shared signing key that " +
			"would let a click be attributed to a delivery it did not belong to, and it is exactly what " +
			"the separate key-id label and the in-body context string exist to prevent")
	}
}

func TestATrackingTokenNeverVerifiesAsUnsubscribe(t *testing.T) {
	token, err := MintTrackingToken(trackSecretA, trackingPayload())
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, _, _, err := ParseUnsubscribeToken([]string{trackSecretA}, token); err == nil {
		t.Fatal("a TRACKING token verified as an unsubscribe token. That is an opt-out anybody can " +
			"trigger by loading an image in a message we sent them")
	}
}

// TestTheTwoFamiliesProduceDifferentKeyIdsForOneSecret is the mechanism
// behind the pair above, asserted directly so a refactor that unified the
// labels fails here with the reason rather than only in the two
// cross-verification tests.
func TestTheTwoFamiliesProduceDifferentKeyIdsForOneSecret(t *testing.T) {
	if TrackingKeyID(trackSecretA) == UnsubscribeKeyID(trackSecretA) {
		t.Error("the tracking and unsubscribe key-id labels produce the same id for one secret. The " +
			"labels are domain separation, not decoration: with them equal, each parser recognises the " +
			"other's tokens as naming a key it holds and falls through to the MAC")
	}
}

// TestPreviousKeyStillVerifies is the rotation half, and it matters as much
// here as for unsubscribe: a pixel in a mailbox is fetched long after the
// secret that signed it stopped being current.
func TestPreviousKeyStillVerifies(t *testing.T) {
	token, err := MintTrackingToken(trackSecretA, trackingPayload())
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// A has been rotated out: B is current, A is previous.
	if _, err := ParseTrackingToken([]string{trackSecretB, trackSecretA}, token); err != nil {
		t.Errorf("a token signed by the now-previous secret was refused after a rotation: %v", err)
	}
}

func TestAnUnknownKeyIsDistinguishable(t *testing.T) {
	token, err := MintTrackingToken(trackSecretA, trackingPayload())
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	_, err = ParseTrackingToken([]string{trackSecretB}, token)
	if !errors.Is(err, errUnknownTrackingKey) {
		t.Errorf("a token naming a key this node does not hold reported %v, want errUnknownTrackingKey. "+
			"The distinction is for the LOG -- it is the one failure an operator can fix, and it means a "+
			"rotation dropped a secret that had already signed live links", err)
	}
	if id := trackingKeyIDOf(token); id != TrackingKeyID(trackSecretA) {
		t.Errorf("trackingKeyIDOf = %q, want the key id in the token", id)
	}
	if id := trackingKeyIDOf("not-a-token"); id != "" {
		t.Errorf("trackingKeyIDOf echoed attacker-chosen bytes into a log field: %q", id)
	}
}

func TestMintRefusesAnIncompleteOrUnknownPayload(t *testing.T) {
	cases := map[string]TrackingPayload{
		"no delivery": {CampaignID: "c1", Kind: EngagementOpen},
		"no campaign": {DeliveryID: "d1", Kind: EngagementOpen},
		"bad kind":    {DeliveryID: "d1", CampaignID: "c1", Kind: "hover"},
	}
	for name, p := range cases {
		if _, err := MintTrackingToken(trackSecretA, p); err == nil {
			t.Errorf("%s: minted a token anyway", name)
		}
	}
	if _, err := MintTrackingToken("", trackingPayload()); err == nil {
		t.Error("minted a token with no signing secret configured")
	}
}

func TestParseRefusesShapesThatAreNotTokens(t *testing.T) {
	keys := []string{trackSecretA}
	for _, bad := range []string{"", "t1", "t1.a.b.c", strings.Repeat("x.", 8), "u2.a.b.c.d.e"} {
		if _, err := ParseTrackingToken(keys, bad); err == nil {
			t.Errorf("%q was accepted as a tracking token", bad)
		}
	}
	if _, err := ParseTrackingToken(nil, "anything"); err == nil {
		t.Error("a node holding no signing keys verified a token")
	}
}
