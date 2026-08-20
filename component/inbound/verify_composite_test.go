package inbound

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Composite-signature-header verification (memql#3854).
//
// # The defect this closes
//
// The receiver could read a signature from ONE header, strip a fixed leading
// PREFIX, and read a timestamp from a SEPARATE header. Stripe -- the sender the
// MemQL Cloud epic needs -- sends both in one header:
//
//	Stripe-Signature: t=1614556800,v1=5257a869e7ecebeda32affa62cdca3fa51cad7e77a0e56ff536d0ce8e108d8bd
//
// `v1=` is not a leading prefix, because `t=<unix>,` comes first. So
// SIGNATURE_PREFIX cannot reach the digest, TIMESTAMP_HEADER has no separate
// header to read, and a source configured as carefully as the documentation
// allowed still refused every delivery with a flat 401 -- the deliberately
// uninformative one, because why a check failed is a fact about our
// configuration and not the caller's to learn.
//
// The epic's architecture decision was "Stripe rides the existing inbound
// surface, no new HTTP endpoints". That decision was not implementable until
// this landed.
//
// # Why it is spelled as an encoding
//
// The scheme constants are encodings on purpose, so a new sender is a config
// change rather than a release. A composite header is the same KIND of fact --
// how the bytes are laid out, not who sent them -- so it is named the same way:
// a separator and two element keys. There is still no vendor table, and Stripe
// appears in this file only as the worked example.

// stripeSignature builds a genuine Stripe-shaped header for a body and secret:
// HMAC-SHA256 over "<timestamp>.<body>", hex, packed as `t=...,v1=...`.
//
// Constructed rather than pasted from a fixture, so the test proves the
// receiver agrees with the DOCUMENTED scheme rather than with one captured
// string somebody may have transcribed wrong.
func stripeSignature(t *testing.T, secret, body string, ts time.Time) string {
	t.Helper()
	unix := fmt.Sprintf("%d", ts.Unix())
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(unix + "." + body))
	return "t=" + unix + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// stripeSource is the configuration an operator writes for Stripe.
func stripeSource(secret string) SourceConfig {
	return SourceConfig{
		Name:             "stripe",
		Secret:           secret,
		Scheme:           SchemeHMACSHA256Hex,
		SignatureHeader:  "Stripe-Signature",
		ElementSeparator: ",",
		SignatureElement: "v1",
		TimestampElement: "t",
	}
}

func TestCompositeHeaderVerifiesAStripeSignature(t *testing.T) {
	const secret = "whsec_test_deadbeef"
	const body = `{"id":"evt_1","type":"checkout.session.completed"}`
	now := time.Unix(1614556800, 0)

	header := http.Header{}
	header.Set("Stripe-Signature", stripeSignature(t, secret, body, now))

	verified, err := verify(stripeSource(secret), header, []byte(body), now, 300*time.Second)
	if err != nil {
		t.Fatalf("a genuine Stripe signature was refused: %v", err)
	}
	if !verified {
		t.Fatal("verify reported the request as unverified without an error")
	}
}

// TestCompositeHeaderRefusesATamperedBody. The whole point of the digest: the
// signature covers "<timestamp>.<body>", so changing one byte of the body
// invalidates it even though the header is untouched.
func TestCompositeHeaderRefusesATamperedBody(t *testing.T) {
	const secret = "whsec_test_deadbeef"
	now := time.Unix(1614556800, 0)

	header := http.Header{}
	header.Set("Stripe-Signature", stripeSignature(t, secret, `{"amount":100}`, now))

	if _, err := verify(stripeSource(secret), header, []byte(`{"amount":100000}`), now, 300*time.Second); err == nil {
		t.Fatal("a body that does not match the signature was accepted")
	}
}

// TestCompositeHeaderRefusesASwappedTimestamp is the property the composite
// form exists to preserve.
//
// The timestamp is INSIDE the signed payload, so an attacker replaying a
// captured delivery cannot move it forward to escape the replay window -- doing
// so invalidates the digest. Reading the timestamp from the same header the
// signature came from is what keeps that true; reading it from a second,
// unsigned header would have handed the window's own input to the caller.
func TestCompositeHeaderRefusesASwappedTimestamp(t *testing.T) {
	const secret = "whsec_test_deadbeef"
	const body = `{"id":"evt_1"}`
	signedAt := time.Unix(1614556800, 0)

	original := stripeSignature(t, secret, body, signedAt)
	digest := original[strings.Index(original, "v1="):]

	// The captured digest, re-presented with a fresh timestamp.
	header := http.Header{}
	header.Set("Stripe-Signature", fmt.Sprintf("t=%d,%s", signedAt.Add(time.Hour).Unix(), digest))

	if _, err := verify(stripeSource(secret), header, []byte(body), signedAt.Add(time.Hour), 300*time.Second); err == nil {
		t.Fatal("a replayed digest with a forward-dated timestamp was accepted -- the replay window is defeatable")
	}
}

// TestCompositeHeaderEnforcesTheReplayWindow. A genuine, correctly-signed
// delivery that is simply too old is still refused.
func TestCompositeHeaderEnforcesTheReplayWindow(t *testing.T) {
	const secret = "whsec_test_deadbeef"
	const body = `{"id":"evt_1"}`
	signedAt := time.Unix(1614556800, 0)

	header := http.Header{}
	header.Set("Stripe-Signature", stripeSignature(t, secret, body, signedAt))

	if _, err := verify(stripeSource(secret), header, []byte(body), signedAt.Add(time.Hour), 300*time.Second); err == nil {
		t.Fatal("a correctly-signed delivery from an hour ago was accepted inside a 5-minute window")
	}
}

// TestCompositeHeaderReportsAMissingElement. The failure has to name the
// ELEMENT rather than the header: with two elements configured, "the header is
// wrong" does not tell an operator which half of their configuration to look at.
func TestCompositeHeaderReportsAMissingElement(t *testing.T) {
	const secret = "whsec_test_deadbeef"

	header := http.Header{}
	header.Set("Stripe-Signature", "t=1614556800")

	_, err := verify(stripeSource(secret), header, []byte(`{}`), time.Unix(1614556800, 0), 300*time.Second)
	if err == nil {
		t.Fatal("a header carrying no digest element was accepted")
	}
	if !strings.Contains(err.Error(), `"v1"`) {
		t.Errorf("the error does not name the missing element: %v", err)
	}
}

// TestCompositeHeaderToleratesSpacingAndExtraElements. Senders add elements
// (Stripe's own `v0` for test-mode) and vary the spacing. Neither is a security
// property: whatever value the digest element yields is compared against an
// HMAC, so a lenient parse cannot smuggle anything past it -- at worst it
// selects a value that fails.
func TestCompositeHeaderToleratesSpacingAndExtraElements(t *testing.T) {
	const secret = "whsec_test_deadbeef"
	const body = `{"id":"evt_1"}`
	now := time.Unix(1614556800, 0)

	sig := stripeSignature(t, secret, body, now)
	unix := fmt.Sprintf("%d", now.Unix())
	digest := strings.TrimPrefix(sig[strings.Index(sig, "v1="):], "v1=")

	for _, raw := range []string{
		"t=" + unix + ", v1=" + digest,
		" t=" + unix + " , v1=" + digest + " ",
		"v1=" + digest + ",t=" + unix,
		"t=" + unix + ",v0=ignored," + "v1=" + digest,
	} {
		header := http.Header{}
		header.Set("Stripe-Signature", raw)
		if _, err := verify(stripeSource(secret), header, []byte(body), now, 300*time.Second); err != nil {
			t.Errorf("refused a valid header variant %q: %v", raw, err)
		}
	}
}

// TestPlainHeaderSourcesAreUnaffected. The GitHub- and Shopify-shaped sources
// the receiver already served must behave exactly as before: ElementSeparator
// empty means the whole header is the value, prefix-stripped as it always was.
func TestPlainHeaderSourcesAreUnaffected(t *testing.T) {
	const secret = "s3cr3t"
	const body = `{"order":1}`
	now := time.Unix(1614556800, 0)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	digest := hex.EncodeToString(mac.Sum(nil))

	src := SourceConfig{
		Name:            "gh",
		Secret:          secret,
		Scheme:          SchemeHMACSHA256Hex,
		SignatureHeader: "X-Hub-Signature-256",
		SignaturePrefix: "sha256=",
	}
	header := http.Header{}
	header.Set("X-Hub-Signature-256", "sha256="+digest)

	verified, err := verify(src, header, []byte(body), now, 300*time.Second)
	if err != nil || !verified {
		t.Fatalf("a GitHub-shaped source stopped verifying: verified=%v err=%v", verified, err)
	}
}

// TestCompositeConfigIsAllOrNothing.
//
// Each half of a partial configuration fails DIFFERENTLY and neither failure
// names itself, which is why they are refused at load rather than at request
// time. A separator with no element key leaves nothing to select, so every
// delivery 401s against a digest that is really a whole header. An element key
// with no separator is silently ignored, so the source verifies against the raw
// header and 401s too -- the same flat status code, from a configuration that
// looks MORE complete than the one that would work.
func TestCompositeConfigIsAllOrNothing(t *testing.T) {
	base := map[string]string{
		"MEMQL_INBOUND_SOURCE_STRIPE_SECRET":           "whsec_x",
		"MEMQL_INBOUND_SOURCE_STRIPE_SIGNATURE_SCHEME": SchemeHMACSHA256Hex,
		"MEMQL_INBOUND_SOURCE_STRIPE_SIGNATURE_HEADER": "Stripe-Signature",
	}

	for name, extra := range map[string]map[string]string{
		"separator without an element key": {
			"MEMQL_INBOUND_SOURCE_STRIPE_ELEMENT_SEPARATOR": ",",
		},
		"signature element without a separator": {
			"MEMQL_INBOUND_SOURCE_STRIPE_SIGNATURE_ELEMENT": "v1",
		},
		"timestamp element without a separator": {
			"MEMQL_INBOUND_SOURCE_STRIPE_TIMESTAMP_ELEMENT": "t",
		},
		"timestamp element pointing at another header": {
			"MEMQL_INBOUND_SOURCE_STRIPE_ELEMENT_SEPARATOR": ",",
			"MEMQL_INBOUND_SOURCE_STRIPE_SIGNATURE_ELEMENT": "v1",
			"MEMQL_INBOUND_SOURCE_STRIPE_TIMESTAMP_ELEMENT": "t",
			"MEMQL_INBOUND_SOURCE_STRIPE_TIMESTAMP_HEADER":  "X-Some-Other-Header",
		},
	} {
		t.Run(name, func(t *testing.T) {
			for k, v := range base {
				t.Setenv(k, v)
			}
			for k, v := range extra {
				t.Setenv(k, v)
			}
			if _, err := loadSource("stripe"); err == nil {
				t.Error("the partial configuration was accepted; it would refuse every delivery with a flat 401")
			}
		})
	}
}

// TestCompleteCompositeConfigLoads is the reachable positive for the test
// above: the same env shape, complete, must produce a usable source. Without it
// the all-or-nothing test would still pass if loadSource had simply started
// rejecting everything.
func TestCompleteCompositeConfigLoads(t *testing.T) {
	for k, v := range map[string]string{
		"MEMQL_INBOUND_SOURCE_STRIPE_SECRET":            "whsec_x",
		"MEMQL_INBOUND_SOURCE_STRIPE_SIGNATURE_SCHEME":  SchemeHMACSHA256Hex,
		"MEMQL_INBOUND_SOURCE_STRIPE_SIGNATURE_HEADER":  "Stripe-Signature",
		"MEMQL_INBOUND_SOURCE_STRIPE_ELEMENT_SEPARATOR": ",",
		"MEMQL_INBOUND_SOURCE_STRIPE_SIGNATURE_ELEMENT": "v1",
		"MEMQL_INBOUND_SOURCE_STRIPE_TIMESTAMP_ELEMENT": "t",
	} {
		t.Setenv(k, v)
	}
	src, err := loadSource("stripe")
	if err != nil {
		t.Fatalf("the documented Stripe configuration was refused: %v", err)
	}
	if src.SignatureElement != "v1" || src.TimestampElement != "t" || src.ElementSeparator != "," {
		t.Fatalf("the composite fields did not survive loading: %+v", src)
	}

	// And it verifies a real signature end to end, which is the claim the
	// documentation makes.
	const body = `{"id":"evt_1"}`
	now := time.Unix(1614556800, 0)
	header := http.Header{}
	header.Set("Stripe-Signature", stripeSignature(t, "whsec_x", body, now))
	if _, err := verify(src, header, []byte(body), now, 300*time.Second); err != nil {
		t.Fatalf("the loaded configuration refused a genuine signature: %v", err)
	}
}
