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

	provided := strings.TrimSpace(header.Get(src.SignatureHeader))
	if provided == "" {
		return false, fmt.Errorf("%w: header %q is absent or empty", errUnverified, src.SignatureHeader)
	}
	if src.SignaturePrefix != "" {
		if !strings.HasPrefix(provided, src.SignaturePrefix) {
			return false, fmt.Errorf("%w: header %q does not carry the configured prefix %q",
				errUnverified, src.SignatureHeader, src.SignaturePrefix)
		}
		provided = strings.TrimPrefix(provided, src.SignaturePrefix)
	}

	signed := body
	if src.TimestampHeader != "" {
		ts := strings.TrimSpace(header.Get(src.TimestampHeader))
		if ts == "" {
			return false, fmt.Errorf("%w: timestamp header %q is absent or empty",
				errUnverified, src.TimestampHeader)
		}
		if err := checkTimestamp(src.TimestampHeader, ts, now, tolerance); err != nil {
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
// `header` is the header NAME and `raw` is its value. The value never reaches
// the error text: it is attacker-controlled on an unauthenticated endpoint, and
// these errors are logged, so quoting it writes caller-supplied bytes into the
// log (CodeQL go/clear-text-logging on this PR). The header name and the
// measured delta are enough to diagnose a real sender -- an operator who needs
// the offending value can read it from the sender's own logs.
func checkTimestamp(header, raw string, now time.Time, tolerance time.Duration) error {
	var ts time.Time
	if secs, err := strconv.ParseInt(raw, 10, 64); err == nil {
		ts = time.Unix(secs, 0)
	} else if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		ts = parsed
	} else {
		return fmt.Errorf("%w: timestamp header %q is neither unix seconds nor RFC3339",
			errUnverified, header)
	}
	if delta := now.Sub(ts); delta > tolerance || delta < -tolerance {
		// Names the header for the same reason the parse branch does: with two
		// timestamp-bearing headers configured, "outside the window" alone does
		// not say which one. The delta is derived, not caller text, so it is
		// safe to log.
		return fmt.Errorf("%w: timestamp header %q is %s outside the %s replay window",
			errUnverified, header, delta.Round(time.Second), tolerance)
	}
	return nil
}
