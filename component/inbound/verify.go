package inbound

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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
	// A token, not a digest: its own reading of the header, and none of the
	// composite or timestamp handling below, which loadSource refuses for it.
	if src.Scheme == SchemeJWTHS256BodySHA256 {
		return verifyJWTSource(src, header, body, now, tolerance)
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

// verifyJWTSource reads a jwt-hs256-body-sha256 source's token from its header and
// checks it.
//
// THE HEADER'S WHOLE VALUE IS THE TOKEN, less the source's prefix when it configures
// one. LiveKit's notifier, the scheme's first sender, sets the bare token as
// Authorization with no "Bearer " (webhook/url_notifier.go send), and its own receiver
// parses the header as it stands (webhook/verifier.go Receive). A sender that writes a
// prefix names it in SIGNATURE_PREFIX, as an HMAC sender does. loadSource trims that
// value, so "Bearer " arrives as "Bearer", and the space after it is trimmed off the
// token here: a token holds no space, so trimming cannot change one that verifies.
func verifyJWTSource(src SourceConfig, header http.Header, body []byte, now time.Time, tolerance time.Duration) (bool, error) {
	token := strings.TrimSpace(header.Get(src.SignatureHeader))
	if token == "" {
		return false, fmt.Errorf("%w: header %q is absent or empty", errUnverified, src.SignatureHeader)
	}
	if src.SignaturePrefix != "" {
		if !strings.HasPrefix(token, src.SignaturePrefix) {
			return false, fmt.Errorf("%w: header %q does not carry the configured prefix %q",
				errUnverified, src.SignatureHeader, src.SignaturePrefix)
		}
		token = strings.TrimSpace(strings.TrimPrefix(token, src.SignaturePrefix))
	}
	if err := verifyJWTBodySHA256(token, body, []byte(src.Secret), src.JWTIssuer, now, tolerance); err != nil {
		return false, err
	}
	return true, nil
}

// jwtAlgorithm is the one algorithm the scheme accepts.
const jwtAlgorithm = "HS256"

// jwtSegment is how each of a token's three parts is encoded: base64url with no padding
// (RFC 7515 section 2), strictly, so one value has one spelling.
var jwtSegment = base64.RawURLEncoding.Strict()

// verifyJWTBodySHA256 checks a compact JWS whose claims sign a request body:
// authorization is the token itself (the header's value less any configured prefix),
// body the request body as received, secret the HS256 key, and issuer the `iss` the
// token must name. It answers nil only when every check passes, and otherwise an error
// wrapping errUnverified, whose words name the check and never repeat what the caller
// sent.
//
// THE ORDER IS THE SECURITY. Nothing is believed before the signature is checked, and
// nothing about the signature before the algorithm is:
//
//  1. Three parts, and a key to check them with. An empty secret refuses everything: a
//     source with no key verifies nothing, rather than an HMAC under the empty key.
//  2. The header names HS256, exactly, and no critical extension. The algorithm is the
//     SCHEME's, never the token's: "none", RS256 (the algorithm-confusion shape, where a
//     shared secret is passed off as a public key) and every other name are refused
//     before the signature is looked at. A `crit` header lists extensions the token
//     says must be understood, and none is, so RFC 7515 section 4.1.11 makes it invalid.
//  3. The signature, HMAC-SHA256 over "<header>.<claims>" under the secret, compared in
//     constant time.
//  4. The issuer, exactly: two keys sharing a secret are still two senders, and an empty
//     configured issuer refuses everything rather than matching a token with none.
//  5. exp, then nbf, each widened by tolerance for the two clocks' skew. exp is
//     REQUIRED: a token without one is valid for ever, and so is a captured request
//     carrying it. nbf is optional (RFC 7519 section 4.1.5). A time that is not a
//     number is refused rather than read as zero.
//  6. The `sha256` claim: the body's SHA-256 in standard base64, with or without its
//     padding (the SDK writes it padded), compared in constant time. Without it a token
//     would verify ANY body for as long as it lived.
//
// A token whose exp is far ahead verifies until it expires; only the secret's holder can
// mint one. What bounds the replay of a CAPTURED request is the sender's own validity
// (five minutes, for LiveKit) plus tolerance, and the row's identity being the body's
// digest: a replay inside that window lands on the row the delivery made (identityKeyFor).
func verifyJWTBodySHA256(authorization string, body, secret []byte, issuer string, now time.Time, tolerance time.Duration) error {
	parts := strings.Split(authorization, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return fmt.Errorf("%w: the token is not three non-empty parts", errUnverified)
	}
	if len(secret) == 0 {
		return fmt.Errorf("%w: the source has no secret, so no token can be checked", errUnverified)
	}

	header, err := jwtObject(parts[0])
	if err != nil {
		return fmt.Errorf("%w: the token's header %v", errUnverified, err)
	}
	if alg, ok := jwtString(header["alg"]); !ok || alg != jwtAlgorithm {
		return fmt.Errorf("%w: the token is not signed with %s", errUnverified, jwtAlgorithm)
	}
	if _, ok := header["crit"]; ok {
		return fmt.Errorf("%w: the token's header lists critical extensions, and none is understood", errUnverified)
	}

	got, err := jwtSegment.DecodeString(parts[2])
	if err != nil {
		return fmt.Errorf("%w: the token's signature is not unpadded base64url", errUnverified)
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(mac.Sum(nil), got) {
		return fmt.Errorf("%w: the token's signature does not match", errUnverified)
	}

	claims, err := jwtObject(parts[1])
	if err != nil {
		return fmt.Errorf("%w: the token's claims %v", errUnverified, err)
	}
	if issuer == "" {
		return fmt.Errorf("%w: the source names no issuer, so no token can match it", errUnverified)
	}
	if iss, ok := jwtString(claims["iss"]); !ok || iss != issuer {
		return fmt.Errorf("%w: the token does not name the source's issuer", errUnverified)
	}

	nowSecs := float64(now.UnixNano()) / float64(time.Second)
	skew := tolerance.Seconds()
	exp, ok := jwtNumericDate(claims["exp"])
	if !ok {
		return fmt.Errorf("%w: the token carries no exp that is a number, so it would never expire", errUnverified)
	}
	if past := nowSecs - exp; past > skew {
		return fmt.Errorf("%w: the token expired %.0fs ago, outside the %s tolerance", errUnverified, past, tolerance)
	}
	if raw, present := claims["nbf"]; present {
		nbf, ok := jwtNumericDate(raw)
		if !ok {
			return fmt.Errorf("%w: the token's nbf is not a number", errUnverified)
		}
		if ahead := nbf - nowSecs; ahead > skew {
			return fmt.Errorf("%w: the token is valid only from %.0fs ahead, outside the %s tolerance", errUnverified, ahead, tolerance)
		}
	}

	claimed, ok := jwtString(claims["sha256"])
	if !ok || claimed == "" {
		return fmt.Errorf("%w: the token carries no sha256 claim to bind it to the body", errUnverified)
	}
	digest, err := base64.StdEncoding.DecodeString(claimed)
	if err != nil {
		digest, err = base64.RawStdEncoding.DecodeString(claimed)
	}
	if err != nil {
		return fmt.Errorf("%w: the token's sha256 claim is not standard base64", errUnverified)
	}
	sum := sha256.Sum256(body)
	if !hmac.Equal(sum[:], digest) {
		return fmt.Errorf("%w: the token's sha256 claim does not match the body", errUnverified)
	}
	return nil
}

// jwtObject decodes one of a token's first two parts into its JSON object's members.
// Its error says which way the part was malformed and never repeats it.
func jwtObject(segment string) (map[string]json.RawMessage, error) {
	raw, err := jwtSegment.DecodeString(segment)
	if err != nil {
		return nil, errors.New("is not unpadded base64url")
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil || members == nil {
		return nil, errors.New("is not a JSON object")
	}
	return members, nil
}

// jwtString reads a member that must be a JSON string. An absent one, null, or a value
// of any other type is no string.
func jwtString(raw json.RawMessage) (string, bool) {
	var s *string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil || s == nil {
		return "", false
	}
	return *s, true
}

// jwtNumericDate reads a NumericDate member (RFC 7519 section 2): a JSON number of
// seconds since the epoch, which may carry a fraction. It stays a float64 -- every
// comparison it takes part in is in seconds -- so nothing here narrows it to an integer.
func jwtNumericDate(raw json.RawMessage) (float64, bool) {
	var secs *float64
	if len(raw) == 0 || json.Unmarshal(raw, &secs) != nil || secs == nil {
		return 0, false
	}
	return *secs, true
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
