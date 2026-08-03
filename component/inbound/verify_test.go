package inbound

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testSecret = "topsecret"

func sign(secret string, payload []byte) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return mac.Sum(nil)
}

// TestVerifyAcceptsAWellSignedRequest covers each scheme in the shape a real
// sender uses it: GitHub's "sha256="-prefixed hex, Shopify's bare base64.
func TestVerifyAcceptsAWellSignedRequest(t *testing.T) {
	body := []byte(`{"event":"order.created"}`)
	now := time.Unix(1_700_000_000, 0).UTC()

	for _, tc := range []struct {
		name   string
		src    SourceConfig
		header http.Header
	}{
		{
			name: "hex with a prefix",
			src: SourceConfig{
				Scheme: SchemeHMACSHA256Hex, Secret: testSecret,
				SignatureHeader: "X-Hub-Signature-256", SignaturePrefix: "sha256=",
			},
			header: http.Header{"X-Hub-Signature-256": {"sha256=" + hex.EncodeToString(sign(testSecret, body))}},
		},
		{
			name: "bare base64",
			src: SourceConfig{
				Scheme: SchemeHMACSHA256Base64, Secret: testSecret,
				SignatureHeader: "X-Shopify-Hmac-Sha256",
			},
			header: http.Header{"X-Shopify-Hmac-Sha256": {base64.StdEncoding.EncodeToString(sign(testSecret, body))}},
		},
		{
			name: "base64 without padding",
			src: SourceConfig{
				Scheme: SchemeHMACSHA256Base64, Secret: testSecret,
				SignatureHeader: "X-Sig",
			},
			header: http.Header{"X-Sig": {base64.RawStdEncoding.EncodeToString(sign(testSecret, body))}},
		},
		{
			name: "timestamped: the signed payload is ts.body",
			src: SourceConfig{
				Scheme: SchemeHMACSHA256Hex, Secret: testSecret,
				SignatureHeader: "X-Sig", TimestampHeader: "X-Ts",
			},
			header: http.Header{
				"X-Ts":  {strconv.FormatInt(now.Unix(), 10)},
				"X-Sig": {hex.EncodeToString(sign(testSecret, append([]byte(strconv.FormatInt(now.Unix(), 10)+"."), body...)))},
			},
		},
		{
			name: "timestamped, RFC3339 spelling",
			src: SourceConfig{
				Scheme: SchemeHMACSHA256Hex, Secret: testSecret,
				SignatureHeader: "X-Sig", TimestampHeader: "X-Ts",
			},
			header: http.Header{
				"X-Ts":  {now.Format(time.RFC3339)},
				"X-Sig": {hex.EncodeToString(sign(testSecret, append([]byte(now.Format(time.RFC3339)+"."), body...)))},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verified, err := verify(tc.src, tc.header, body, now, 5*time.Minute)
			if err != nil {
				t.Fatalf("a correctly signed request was refused: %v", err)
			}
			if !verified {
				t.Error("verify reported the request as unverified despite checking it; the staged " +
					"row would claim the source runs unsigned")
			}
		})
	}
}

// The refusal half. Every case here is a request that must NOT reach the graph.
func TestVerifyRefusesEverythingThatIsNotWellSigned(t *testing.T) {
	body := []byte(`{"event":"order.created"}`)
	now := time.Unix(1_700_000_000, 0).UTC()
	good := hex.EncodeToString(sign(testSecret, body))
	base := SourceConfig{Scheme: SchemeHMACSHA256Hex, Secret: testSecret, SignatureHeader: "X-Sig"}

	for _, tc := range []struct {
		name   string
		src    SourceConfig
		header http.Header
		body   []byte
	}{
		{"no signature header", base, http.Header{}, body},
		{"empty signature header", base, http.Header{"X-Sig": {"  "}}, body},
		{"wrong secret's digest", base, http.Header{"X-Sig": {hex.EncodeToString(sign("other", body))}}, body},
		{"digest of a different body", base, http.Header{"X-Sig": {good}}, []byte(`{"event":"order.deleted"}`)},
		{"not hex at all", base, http.Header{"X-Sig": {"zzzz"}}, body},
		{"truncated digest", base, http.Header{"X-Sig": {good[:32]}}, body},
		{
			"prefix required but absent",
			SourceConfig{Scheme: SchemeHMACSHA256Hex, Secret: testSecret, SignatureHeader: "X-Sig", SignaturePrefix: "sha256="},
			http.Header{"X-Sig": {good}}, body,
		},
		{
			"timestamp header required but absent",
			SourceConfig{Scheme: SchemeHMACSHA256Hex, Secret: testSecret, SignatureHeader: "X-Sig", TimestampHeader: "X-Ts"},
			http.Header{"X-Sig": {good}}, body,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verified, err := verify(tc.src, tc.header, tc.body, now, 5*time.Minute)
			if err == nil {
				t.Fatal("an unverifiable request was ACCEPTED; it would be staged as a real event")
			}
			if !errors.Is(err, errUnverified) {
				t.Errorf("every refusal must carry errUnverified so the handler answers 401 "+
					"uniformly; got %v", err)
			}
			if verified {
				t.Error("verify reported true alongside an error")
			}
		})
	}
}

// The replay window, both directions. A future timestamp is refused too: a
// captured request that could be backdated -- or postdated -- indefinitely is
// not replay-protected at all.
func TestVerifyEnforcesTheReplayWindowInBothDirections(t *testing.T) {
	body := []byte(`{"a":1}`)
	now := time.Unix(1_700_000_000, 0).UTC()
	src := SourceConfig{
		Scheme: SchemeHMACSHA256Hex, Secret: testSecret,
		SignatureHeader: "X-Sig", TimestampHeader: "X-Ts",
	}
	signedAt := func(ts time.Time) http.Header {
		s := strconv.FormatInt(ts.Unix(), 10)
		return http.Header{
			"X-Ts":  {s},
			"X-Sig": {hex.EncodeToString(sign(testSecret, append([]byte(s+"."), body...)))},
		}
	}

	for _, tc := range []struct {
		name   string
		offset time.Duration
		want   bool // want accepted
	}{
		{"just inside the window, past", -4 * time.Minute, true},
		{"just inside the window, future (clock skew)", 4 * time.Minute, true},
		{"outside the window, past -- the replay case", -10 * time.Minute, false},
		{"outside the window, future -- a postdated capture", 10 * time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := verify(src, signedAt(now.Add(tc.offset)), body, now, 5*time.Minute)
			if tc.want && err != nil {
				t.Errorf("a request %s of now was refused inside the window: %v", tc.offset, err)
			}
			if !tc.want && err == nil {
				t.Errorf("a request %s of now was accepted; the signature is valid forever, so "+
					"the window is the only thing stopping a replay", tc.offset)
			}
		})
	}

	// A signature valid for one timestamp must not carry another: without
	// binding the timestamp into the signed payload the window is decorative,
	// because an attacker just rewrites the header.
	t.Run("timestamp cannot be swapped after signing", func(t *testing.T) {
		h := signedAt(now)
		h.Set("X-Ts", strconv.FormatInt(now.Add(-time.Minute).Unix(), 10))
		if _, err := verify(src, h, body, now, 5*time.Minute); err == nil {
			t.Error("the timestamp was swapped for another inside the window and the signature " +
				"still passed -- it is not bound into the signed payload")
		}
	})
}

// scheme=none is a real configuration, and the bool it returns is what the
// staged row records. Getting this backwards would mark unverified traffic as
// verified.
func TestVerifySchemeNoneAcceptsAndReportsUnverified(t *testing.T) {
	verified, err := verify(SourceConfig{Scheme: SchemeNone}, http.Header{}, []byte("x"), time.Now(), time.Minute)
	if err != nil {
		t.Fatalf("scheme=none must accept: %v", err)
	}
	if verified {
		t.Error("scheme=none must report the request as UNVERIFIED -- the row's " +
			"signatureVerified field is how an operator finds sources running unsigned")
	}
}

// TestCheckTimestampDoesNotLeakTheHeaderValue pins the taint CodeQL flagged on
// memql#2957 (go/clear-text-logging, component/inbound/handler.go:99 and :125).
//
// checkTimestamp's error is LOGGED by the handler, and its input is a request
// header value on an unauthenticated endpoint -- so quoting the value wrote
// caller-controlled bytes straight into the log. The header NAME and the
// measured delta diagnose a real sender just as well.
//
// Failing-first: put %q of `raw` back in the parse error and the first subtest
// fails.
func TestCheckTimestampDoesNotLeakTheHeaderValue(t *testing.T) {
	const header = "X-Timestamp"
	now := time.Unix(1_700_000_000, 0).UTC()

	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"unparseable", "not-a-timestamp-9f8e7d"},
		{"injection-shaped", "\" evil=1 secret=hunter2"},
		// Parseable but far outside the window, so it takes the OTHER error
		// branch. Deliberately a long distinctive string: a short value like
		// "1" appears incidentally inside the rendered delta ("471166h39m59s"),
		// which would make a containment check fire for a reason that is not a
		// leak.
		{"stale but parseable", "2020-04-05T06:07:08Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkTimestamp(header, tc.raw, now, time.Minute)
			if err == nil {
				t.Fatalf("checkTimestamp(%q) unexpectedly succeeded", tc.raw)
			}
			if strings.Contains(err.Error(), tc.raw) {
				t.Errorf("the error quotes the caller-supplied header VALUE, which the handler "+
					"logs verbatim:\n  err:   %v\n  value: %q\n"+
					"Name the header, not its contents (memql#2957).", err, tc.raw)
			}
			if !strings.Contains(err.Error(), header) {
				t.Errorf("the error must still name WHICH header failed, or it cannot be "+
					"diagnosed:\n  %v", err)
			}
		})
	}
}
