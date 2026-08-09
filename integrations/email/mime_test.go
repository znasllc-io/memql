package email

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// mime_test.go -- the RFC 5322 renderer and the throttle classifier
// (memql#3348).

func TestRenderCarriesExtraHeadersInOrder(t *testing.T) {
	raw, err := RenderRFC5322("Sender <no-reply@example.test>", Message{
		To: "person@example.test", Subject: "Hi", TextBody: "body",
		Headers: map[string]string{
			"List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
			"List-Unsubscribe":      "<https://example.test/unsubscribe?token=abc>",
		},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	out := string(raw)
	for _, want := range []string{
		"From: Sender <no-reply@example.test>\r\n",
		"To: person@example.test\r\n",
		"Subject: Hi\r\n",
		"List-Unsubscribe: <https://example.test/unsubscribe?token=abc>\r\n",
		"List-Unsubscribe-Post: List-Unsubscribe=One-Click\r\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered message is missing %q:\n%s", want, out)
		}
	}
	// Extras are sorted, so the bytes are reproducible run to run --
	// which is what makes a byte comparison possible at all.
	if strings.Index(out, "List-Unsubscribe:") > strings.Index(out, "List-Unsubscribe-Post:") {
		t.Error("extra headers are not in a deterministic order")
	}
	if !strings.Contains(out, "\r\n\r\nbody") {
		t.Error("the header/body boundary is wrong")
	}
}

func TestRenderRefusesInjectionAndReservedNames(t *testing.T) {
	base := Message{To: "person@example.test", Subject: "Hi", TextBody: "body"}

	base.Headers = map[string]string{"X-Thing": "value\r\nBcc: someone@evil.test"}
	if _, err := RenderRFC5322("s@example.test", base); err == nil {
		t.Error("a CRLF in a header value was accepted; that injects a Bcc")
	}

	base.Headers = map[string]string{"Bad:Name": "value"}
	if _, err := RenderRFC5322("s@example.test", base); err == nil {
		t.Error("a colon in a header NAME was accepted")
	}

	for _, reserved := range []string{"From", "to", "Subject", "Content-Type", "MIME-Version"} {
		base.Headers = map[string]string{reserved: "x"}
		if _, err := RenderRFC5322("s@example.test", base); err == nil {
			t.Errorf("caller-supplied %q was accepted; overriding From breaks SPF/DKIM alignment and a second To is a second recipient", reserved)
		}
	}
}

func TestValidateRejectsBadHeadersUpFront(t *testing.T) {
	msg := Message{
		To: "person@example.test", Subject: "Hi", TextBody: "body",
		Headers: map[string]string{"From": "spoofed@evil.test"},
	}
	if err := msg.Validate(); err == nil {
		t.Error("Validate accepted a reserved header; the caller only finds out at the wire boundary, where the only report is a failed send")
	}
}

func TestRenderMultipartPutsTextFirst(t *testing.T) {
	raw, err := RenderRFC5322("s@example.test", Message{
		To: "p@example.test", Subject: "Hi", TextBody: "plain", HTMLBody: "<p>rich</p>",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	out := string(raw)
	if !strings.Contains(out, "multipart/alternative") {
		t.Fatal("no multipart container")
	}
	if strings.Index(out, "plain") > strings.Index(out, "<p>rich</p>") {
		t.Error("HTML precedes text; a text alternative first is what keeps a message readable in a client that refuses HTML")
	}
}

// --- classification -----------------------------------------------------

func TestClassifyThrottle(t *testing.T) {
	se := classifyHTTPSend(http.StatusTooManyRequests, "120", "throttled")
	wait, throttled := IsThrottled(se)
	if !throttled || wait != 120*time.Second {
		t.Fatalf("429 with Retry-After: throttled=%v wait=%v, want true / 2m", throttled, wait)
	}
	if IsPermanent(se) {
		t.Error("a throttle was classified permanent; the message was never judged on its merits")
	}

	// A 429 with NO header is still a throttle -- the caller supplies its
	// own default rather than reading zero as "retry immediately".
	se = classifyHTTPSend(http.StatusTooManyRequests, "", "throttled")
	if wait, throttled = IsThrottled(se); !throttled || wait != 0 {
		t.Fatalf("429 without Retry-After: throttled=%v wait=%v, want true / 0", throttled, wait)
	}
}

func TestClassifyPermanentVsRetryable(t *testing.T) {
	cases := map[int]struct{ permanent bool }{
		400: {true}, // understood and refused
		401: {true}, // the Graph sender already retried a stale token
		403: {true},
		404: {true},
		408: {false}, // a timeout is not the message's fault
		500: {false},
		502: {false},
	}
	for status, want := range cases {
		se := classifyHTTPSend(status, "", "detail")
		if IsPermanent(se) != want.permanent {
			t.Errorf("status %d: permanent=%v, want %v", status, IsPermanent(se), want.permanent)
		}
	}
	// 503 is a throttle ONLY when it says how long. Without a
	// Retry-After there is nothing to pace against, and inventing one
	// would stall the whole send on a guess.
	if _, throttled := IsThrottled(classifyHTTPSend(503, "", "x")); throttled {
		t.Error("a bare 503 was treated as a throttle")
	}
	if _, throttled := IsThrottled(classifyHTTPSend(503, "30", "x")); !throttled {
		t.Error("a 503 WITH Retry-After was not treated as a throttle")
	}
}

func TestUnclassifiedErrorIsNeverPermanent(t *testing.T) {
	// Giving up on an error nobody classified discards mail; retrying one
	// costs an attempt. Only one of those is recoverable.
	if IsPermanent(&SendError{Detail: "connection reset"}) {
		t.Error("an unclassified SendError was treated as permanent")
	}
}

func TestParseRetryAfterAcceptsBothForms(t *testing.T) {
	if got := parseRetryAfter("45"); got != 45*time.Second {
		t.Errorf("delta-seconds: %v", got)
	}
	future := time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(future); got <= 0 || got > 95*time.Second {
		t.Errorf("HTTP-date: %v", got)
	}
	for _, bad := range []string{"", "  ", "soon", "-5", "0"} {
		if got := parseRetryAfter(bad); got != 0 {
			t.Errorf("parseRetryAfter(%q) = %v, want 0", bad, got)
		}
	}
	// A date already in the past is "no wait", not a negative one.
	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(past); got != 0 {
		t.Errorf("a past HTTP-date produced %v", got)
	}
}
