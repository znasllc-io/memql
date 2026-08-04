package inbound

import (
	"net/http"
	"testing"
)

// identity_test.go -- memql#2957 landing review.
//
// The row id used to be derived from (source, dedupeKey), where dedupeKey came
// from a configured request HEADER. The vendor signs the BODY -- and the
// timestamp, where one is configured -- but it does not sign that header,
// because the header is our configuration and not part of its scheme.
//
// So one captured, still-valid request minted unbounded distinct rows: the
// signature stays valid because the body is unchanged, while varying the
// unsigned header varies the id. Storage amplification off a single replay,
// with no forged signature required.
//
// Identity is now the digest of exactly what was signed, and nothing else.

const identityProbeBody = `{"order":1}`

func headerWith(k, v string) http.Header {
	h := http.Header{}
	if k != "" {
		h.Set(k, v)
	}
	return h
}

// The attack, directly: same signed bytes, attacker-varied unsigned header.
func TestVaryingTheUnsignedDedupeHeaderCannotMintRows(t *testing.T) {
	src := SourceConfig{
		Scheme:       SchemeHMACSHA256Hex,
		DedupeHeader: "X-Delivery-Id",
	}
	body := []byte(identityProbeBody)

	first := requestIDFor("shopify", identityKeyFor(src, headerWith("X-Delivery-Id", "a"), body))
	for _, forged := range []string{"b", "c", "", "zzzzzzzz", "../../etc"} {
		got := requestIDFor("shopify", identityKeyFor(src, headerWith("X-Delivery-Id", forged), body))
		if got != first {
			t.Fatalf("replaying one captured request with dedupe header %q produced a DIFFERENT row id "+
				"(%s vs %s).\n\n"+
				"The header is not covered by the HMAC, so an attacker who captures a single valid "+
				"delivery can vary it freely and mint a row per value while the signature stays "+
				"valid. Identity must come from signed material only -- if the header contributes "+
				"to the id at all, this attack works.", forged, got, first)
		}
	}
}

// The property that makes the above safe rather than merely blunt: genuinely
// different signed bytes still separate.
func TestDifferentSignedBytesStillSeparate(t *testing.T) {
	src := SourceConfig{Scheme: SchemeHMACSHA256Hex}

	a := requestIDFor("shopify", identityKeyFor(src, http.Header{}, []byte(`{"order":1}`)))
	b := requestIDFor("shopify", identityKeyFor(src, http.Header{}, []byte(`{"order":2}`)))
	if a == b {
		t.Error("two different bodies collapsed onto one row id -- redelivery collapse has become " +
			"event collapse, which loses deliveries rather than deduplicating them")
	}

	// And the same body from two different sources must not collide either.
	if requestIDFor("shopify", identityKeyFor(src, http.Header{}, []byte(identityProbeBody))) ==
		requestIDFor("stripe", identityKeyFor(src, http.Header{}, []byte(identityProbeBody))) {
		t.Error("the same body from two different sources produced one row id")
	}
}

// Where a timestamp header IS configured it is part of the signed payload, so
// it both separates genuinely-distinct deliveries and remains unforgeable.
// This is the escape hatch for a sender that legitimately emits byte-identical
// events, and it is the reason dropping the unsigned header costs little.
func TestSignedTimestampSeparatesIdenticalBodies(t *testing.T) {
	src := SourceConfig{Scheme: SchemeHMACSHA256Hex, TimestampHeader: "X-Ts"}
	body := []byte(identityProbeBody)

	a := requestIDFor("shopify", identityKeyFor(src, headerWith("X-Ts", "1000"), body))
	b := requestIDFor("shopify", identityKeyFor(src, headerWith("X-Ts", "1001"), body))
	if a == b {
		t.Error("two deliveries with different SIGNED timestamps collapsed onto one row. That is " +
			"the configuration a sender uses to distinguish byte-identical events, and it is what " +
			"makes identity-from-signed-material affordable.")
	}

	// A redelivery reuses the signed timestamp, so it must still collapse --
	// otherwise the feature's central promise is broken in the other direction.
	if a != requestIDFor("shopify", identityKeyFor(src, headerWith("X-Ts", "1000"), body)) {
		t.Error("a byte-identical redelivery did not land on the same row")
	}
}

// An unverified source has no signed material and no signature to replay, so
// identity falls back to content. Pinned so the fallback cannot quietly become
// something weaker.
func TestUnverifiedSourceIdentityIsContent(t *testing.T) {
	src := SourceConfig{Scheme: SchemeNone, TimestampHeader: "X-Ts"}
	body := []byte(identityProbeBody)

	// The timestamp header is NOT folded in for scheme=none: nothing signs it,
	// so including it would reintroduce exactly the defect this file pins.
	a := identityKeyFor(src, headerWith("X-Ts", "1000"), body)
	b := identityKeyFor(src, headerWith("X-Ts", "9999"), body)
	if a != b {
		t.Error("an unsigned timestamp changed the identity of an unverified source, which is the " +
			"attacker-controlled-identity defect wearing a different header name")
	}
}
