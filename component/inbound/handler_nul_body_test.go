package inbound

import (
	"bufio"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// memql#3098: a NUL byte in a webhook body made the staging mutation
// unstorable, so the request could never be staged.
//
// U+0000 is valid UTF-8, so the handler's utf8.Valid gate passed it. The body
// then rendered into a MemQL string literal as a unicode escape -- which the
// lexer decodes correctly, so the statement PARSED -- and failed one layer down
// against `payload JSONB NOT NULL`, because PostgreSQL's jsonb type cannot
// represent U+0000.
//
// The consequence is an infinite retry loop rather than a stuck row: the insert
// fails, the handler answers 503, staging is idempotent by requestId, and every
// sender in this feature's story list retries on 5xx. The same body comes back
// forever against a request that cannot succeed.
//
// Why these tests and not the one that already existed: the package's
// TestRenderedLiteralsParseBackThroughTheRealLexer asserts on the DECODED value,
// and decoding is the part that works. It is structurally blind to this class.
// These drive the REAL handler and assert on the outcome.

// nulSignedRequest builds a correctly-signed request whose body contains a NUL.
//
// The signature is load-bearing for TestHandlerStillStagesABodyWithoutANul,
// which needs a 202. It is NOT what makes the refusal tests meaningful: the
// gate runs before verify(), so all four of those pass with a corrupt
// signature too. Do not read them as asserting anything about the signed path.
func nulSignedRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/inbound/acme", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Sig", hex.EncodeToString(sign(testSecret, []byte(body))))
	return r
}

func TestHandlerRefusesANulBodyWithoutStaging(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"NUL in the middle", "{\"a\":\"x\x00y\"}"},
		{"NUL at the end", "{\"a\":\"x\"}\x00"},
		{"NUL alone", "\x00"},
		{"NUL inside otherwise-valid JSON text", "{\"binary payload: \x00 here\":1}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := &fakeEngine{}
			rec := httptest.NewRecorder()
			testHandler(t, eng, hexSource()).ServeHTTP(rec, nulSignedRequest(t, tc.body))

			// 400, not 503. The distinction is the whole point: 5xx is what makes
			// a sender retry, and no number of retries can make this body
			// storable. A 503 here IS the infinite loop.
			if rec.Code != http.StatusBadRequest {
				t.Errorf("a body containing a NUL was answered %d, want 400.\n"+
					"If this is 503, the request will be retried forever against a row that cannot "+
					"be inserted (memql#3098). If it is 202, the insert will fail at the jsonb "+
					"column instead.\n  body: %s", rec.Code, rec.Body)
			}
			if len(eng.calls) != 0 {
				t.Errorf("a body containing a NUL reached the graph -- %d staging call(s) were "+
					"made. The mutation parses; it is the INSERT that cannot succeed, so nothing "+
					"downstream would catch this.\n  %v", len(eng.calls), eng.calls)
			}
		})
	}
}

// The converse. Without it, a gate that refused every body would pass the test
// above while breaking the entire receiver.
func TestHandlerStillStagesABodyWithoutANul(t *testing.T) {
	eng := &fakeEngine{}
	rec := httptest.NewRecorder()
	// Deliberately carries bytes NEAR the refused one -- other control
	// characters and a multi-byte rune -- so the gate is pinned to U+0000
	// specifically rather than to "control characters" or "non-ASCII".
	body := "{\"a\":\"tab\there\\nnewline\",\"u\":\"café ✓\"}"
	testHandler(t, eng, hexSource()).ServeHTTP(rec, nulSignedRequest(t, body))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("a NUL-free body was answered %d, want 202: %s", rec.Code, rec.Body)
	}
	if len(eng.calls) != 1 {
		t.Fatalf("expected exactly one staging call, got %d", len(eng.calls))
	}
}

// TestNulInAHeaderNeverReachesTheHandler is the audit half of memql#3098's
// definition of done, and it is a test rather than a second gate.
//
// The other caller-supplied strings this handler stages are contentType and
// dedupeKey. Audited:
//
//   - dedupeKey    already safe: dedupeKeyFor rejects any rune < 0x20, which
//     includes NUL, and answers 400.
//   - requestID    sha256 hex.
//   - source name  must be a key of cfg.Sources, so operator-controlled.
//   - receivedAt   RFC3339 from the clock.
//   - contentType  taken straight from the header with no gate in THIS package
//     -- and unreachable anyway, because Go's HTTP server refuses a
//     header value containing NUL before the handler runs.
//
// That last one is why no contentType gate ships with this change: it would be
// unreachable code. But "unreachable" is a claim about net/http's behaviour, not
// about ours, so it is pinned here rather than asserted in a comment. If a Go
// upgrade ever admits the byte, this fails and the gate becomes necessary.
//
// It drives a raw socket rather than httptest.NewRequest deliberately:
// Header.Set bypasses wire parsing entirely and DOES admit a NUL, so a test
// built that way would "prove" the opposite of the truth.
func TestNulInAHeaderNeverReachesTheHandler(t *testing.T) {
	reached := false
	var seenContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		seenContentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(srv.URL, "http://"), 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	req := "POST /inbound/acme HTTP/1.1\r\n" +
		"Host: example\r\n" +
		"Content-Type: application/json\x00evil\r\n" +
		"Content-Length: 1\r\n\r\nx"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	status, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}

	if reached {
		t.Errorf("net/http delivered a request whose Content-Type contains a NUL (%q). That byte "+
			"is staged into a JSONB column, so it is now the same unstageable-request defect "+
			"memql#3098 fixed for the body -- this package needs its own contentType gate.",
			seenContentType)
	}
	if !strings.Contains(status, "400") {
		t.Errorf("expected net/http to refuse the NUL header with 400, got %q. The audit in "+
			"memql#3098 concluded contentType needs no gate BECAUSE of that refusal; if the "+
			"refusal has changed shape, re-run the audit.", strings.TrimSpace(status))
	}
}
