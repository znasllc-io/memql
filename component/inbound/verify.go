package inbound

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// errUnverified is returned for every signature failure, whatever the cause.
//
// The caller of this endpoint is unauthenticated by definition, so the reason a
// check failed is not theirs to learn: "wrong digest", "missing header" and
// "stale timestamp" are three different pieces of information about the
// deployment's configuration. The operator gets the detail through the wrapped
// error in the node's log; the caller gets 401.
var errUnverified = errors.New("signature verification failed")

// verify checks the request's signature against the source's policy and
// reports whether a signature was actually checked.
//
// The bool exists because SchemeNone is a real, deliberate configuration and
// the staged row has to say so -- signatureVerified=false on a row means "this
// source runs unverified", never "this request failed the check", because a
// request that fails is refused here and no row is ever staged.
func verify(src SourceConfig, header http.Header, body []byte, now time.Time, tolerance time.Duration) (bool, error) {
	if src.Scheme == SchemeNone {
		return false, nil
	}

	raw := strings.TrimSpace(header.Get(src.SignatureHeader))
	if raw == "" {
		return false, fmt.Errorf("%w: header %q is absent or empty", errUnverified, src.SignatureHeader)
	}

	provided := raw
	// A composite header is a `k=v` list rather than a single value. Select the
	// digest element before anything else touches it -- the prefix strip below
	// then applies to the ELEMENT's value, which is where a sender that both
	// packs elements and prefixes its digest would put it.
	var elements map[string]string
	if src.ElementSeparator != "" {
		elements = parseElements(raw, src.ElementSeparator)
		v, ok := elements[src.SignatureElement]
		if !ok || v == "" {
			return false, fmt.Errorf("%w: header %q carries no %q element",
				errUnverified, src.SignatureHeader, src.SignatureElement)
		}
		provided = v
	}

	if src.SignaturePrefix != "" {
		if !strings.HasPrefix(provided, src.SignaturePrefix) {
			return false, fmt.Errorf("%w: header %q does not carry the configured prefix %q",
				errUnverified, src.SignatureHeader, src.SignaturePrefix)
		}
		provided = strings.TrimPrefix(provided, src.SignaturePrefix)
	}

	signed := body
	if ts, where, ok := signedTimestamp(src, header, elements); ok {
		if ts == "" {
			return false, fmt.Errorf("%w: timestamp %s is absent or empty", errUnverified, where)
		}
		if err := checkTimestamp(where, ts, now, tolerance); err != nil {
			return false, err
		}
		// "<timestamp>.<body>" -- the shape every timestamped webhook scheme
		// in the wild uses, and the reason the timestamp cannot be swapped
		// after signing.
		signed = append(append([]byte(ts), '.'), body...)
	}

	want := hmacSHA256(src.Secret, signed)
	got, err := decodeSignature(src.Scheme, provided)
	if err != nil {
		return false, fmt.Errorf("%w: %v", errUnverified, err)
	}
	// Constant time, and hmac.Equal already handles the length mismatch that a
	// naive compare would leak through timing.
	if !hmac.Equal(want, got) {
		return false, fmt.Errorf("%w: digest mismatch", errUnverified)
	}
	return true, nil
}

// parseElements splits a composite signature header into its `key=value`
// elements.
//
// Deliberately lenient about SPACING and strict about nothing else: an element
// with no `=` is dropped rather than treated as a key with an empty value,
// because a header that does not parse must fail as "no such element" (which
// names the configuration) and never as "the element was empty" (which reads
// like the sender's problem).
//
// LAST WINS on a repeated key. That is the safe direction here in a way it is
// not elsewhere: whichever value this returns is then compared against a digest
// the secret produces, so a duplicate element cannot smuggle anything past the
// HMAC -- at worst it selects a value that fails to verify.
func parseElements(raw, sep string) map[string]string {
	out := map[string]string{}
	for part := range strings.SplitSeq(raw, sep) {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out
}

// signedTimestamp resolves the timestamp that participates in the signed
// payload, and reports where it came from so an error can name it.
//
// Two sources, and only one of them can be in play for a given source config
// (loadSource refuses a config where both could be): an element inside the
// composite signature header, or a header of its own. The third return value is
// false when the sender signs the body alone -- which is not an error, it is
// what a source with no timestamp configured means, and it leaves the replay
// window off by design (dedupeKey is the answer there).
func signedTimestamp(src SourceConfig, header http.Header, elements map[string]string) (ts, where string, ok bool) {
	if src.TimestampElement != "" {
		// Present-but-empty and absent are the same failure to the caller: the
		// element the configuration names did not arrive with a value.
		return elements[src.TimestampElement],
			fmt.Sprintf("element %q of header %q", src.TimestampElement, src.SignatureHeader),
			true
	}
	if src.TimestampHeader != "" {
		return strings.TrimSpace(header.Get(src.TimestampHeader)),
			fmt.Sprintf("header %q", src.TimestampHeader),
			true
	}
	return "", "", false
}

func hmacSHA256(secret string, payload []byte) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return mac.Sum(nil)
}

func decodeSignature(scheme, provided string) ([]byte, error) {
	switch scheme {
	case SchemeHMACSHA256Hex:
		b, err := hex.DecodeString(provided)
		if err != nil {
			return nil, fmt.Errorf("signature is not valid hex")
		}
		return b, nil
	case SchemeHMACSHA256Base64:
		// Both alphabets, because senders differ and the padding rule is not
		// worth a support ticket.
		if b, err := base64.StdEncoding.DecodeString(provided); err == nil {
			return b, nil
		}
		b, err := base64.RawStdEncoding.DecodeString(provided)
		if err != nil {
			return nil, fmt.Errorf("signature is not valid base64")
		}
		return b, nil
	default:
		return nil, fmt.Errorf("unknown scheme %q", scheme)
	}
}

// checkTimestamp enforces the replay window. Both directions are checked: a
// future timestamp beyond the tolerance is as much a red flag as a stale one,
// and accepting it would let a captured request be replayed for as long as the
// attacker cared to backdate it.
//
// Unix seconds and RFC3339 are both accepted because senders use both.
//
// `where` names WHERE the timestamp came from -- a header, or an element inside
// a composite signature header -- and `raw` is its value. The value never
// reaches the error text: it is attacker-controlled on an unauthenticated
// endpoint, and these errors are logged, so quoting it writes caller-supplied
// bytes into the log (CodeQL go/clear-text-logging). The location and the
// measured delta are enough to diagnose a real sender -- an operator who needs
// the offending value can read it from the sender's own logs.
//
// `where` is composed by signedTimestamp rather than being a bare header name,
// because with a composite header the useful thing to say is which ELEMENT was
// unparseable: "header \"Stripe-Signature\"" would be true and would not
// distinguish the timestamp element from the digest one.
func checkTimestamp(where, raw string, now time.Time, tolerance time.Duration) error {
	var ts time.Time
	if secs, err := strconv.ParseInt(raw, 10, 64); err == nil {
		ts = time.Unix(secs, 0)
	} else if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		ts = parsed
	} else {
		return fmt.Errorf("%w: timestamp %s is neither unix seconds nor RFC3339",
			errUnverified, where)
	}
	if delta := now.Sub(ts); delta > tolerance || delta < -tolerance {
		// Names the source for the same reason the parse branch does: with two
		// timestamp-bearing locations configured, "outside the window" alone
		// does not say which one. The delta is derived, not caller text, so it
		// is safe to log.
		return fmt.Errorf("%w: timestamp %s is %s outside the %s replay window",
			errUnverified, where, delta.Round(time.Second), tolerance)
	}
	return nil
}
