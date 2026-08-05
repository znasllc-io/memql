package outbound

import (
	"context"
	"errors"
	"strings"
	"testing"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// worker_lasterror_escapes_3035_test.go -- memql#3035.
//
// The worker builds its status-stamping mutations by string formatting and used
// to pass lastError through %q. Go's %q and the MemQL lexer do not agree on the
// escape set, and the disagreement is a HARD ERROR rather than a fallback:
// readString implements the JSON escapes and only those, while %q emits `\x00`,
// `\a` and `\v`.
//
// # Why the failure is worse than a parse error
//
// stamp() logs a warning and returns. So the mutation simply never applies and
// the row keeps whatever status it had. A request mid-delivery stays `sending`
// FOREVER -- never retried, never reported failed -- and no error is recorded,
// because recording the error is precisely what failed. An operator sees a
// stuck row with an empty lastError and nothing to explain it.
//
// # Why it is reachable
//
// lastError is truncateError(err), and for a webhook delivery that text can
// carry bytes from a remote server: a response-body excerpt, or a TLS/DNS error
// naming a hostile hostname.
//
// lastError is NOT the only exposed argument. memql#3035's own audit item asked
// for the other Sprintf-built statements in this file to be checked, and doing
// it turns up requestId: `requestId string!` is a caller-supplied ARG of
// stageOutboundRequest (dsl/platform/mutations.memql:287), stamped verbatim as
// the row id at :298, and nothing on the id path validates its charset. sentAt
// and nextAttemptAt genuinely are generated (time.Format), but every string
// argument in this file now goes through QuoteString regardless, so no line
// here teaches %q as acceptable for a statement argument.
//
// TestHostileRequestID_StillLexes covers that variant, and it is the more
// damaging of the two -- see its own comment.
//
// These tests drive the REAL worker path and lex what it actually emitted.
// Asserting on a hand-built format string would test a copy of the code rather
// than the code.

// assertStampsLex checks every mutation the worker stamped is something the
// MemQL lexer accepts.
func assertStampsLex(t *testing.T, eng *fakeEngine) {
	t.Helper()
	stamps := eng.stamped()
	if len(stamps) == 0 {
		t.Fatal("the worker stamped nothing, so this test proves nothing about what it emits")
	}
	for _, m := range stamps {
		if _, err := langparser.NewLexer(m).Tokenize(); err != nil {
			t.Errorf("the worker emitted a mutation the MemQL lexer REFUSES, so it would never "+
				"apply -- stamp() logs a warning and returns, leaving the row stuck in its "+
				"previous status with no error recorded (memql#3035).\n"+
				"  mutation: %s\n  lexer:    %v", m, err)
		}
	}
}

// hostileError carries the bytes a remote server can put into an error string.
// \x00 / \a / \v are the three %q emits that the lexer has no escape for.
var hostileError = errors.New("upstream said: \x00 NUL \a bell \v vtab \x01\x02 and \"quotes\" and a \\ backslash")

// TestStampFailed_HostileErrorStillLexes covers the permanent-failure stamp:
// the attempt cap is 3 and IsPermanent short-circuits, so a permanent error
// reaches the `failed` stamp on the first pass.
func TestStampFailed_HostileErrorStillLexes(t *testing.T) {
	eng := &fakeEngine{pending: []map[string]any{pendingRow("v1:platform:outboundRequest:r1")}}
	tr := &fakeTransport{err: Permanent(hostileError)}
	w := newTestWorker(eng, tr, nil)

	w.drainOnce(context.Background())

	assertStampsLex(t, eng)

	var failed string
	for _, m := range eng.stamped() {
		if strings.Contains(m, `status: "failed"`) {
			failed = m
		}
	}
	if failed == "" {
		t.Fatalf("expected a `failed` stamp; got %v", eng.stamped())
	}
	// The escaped control bytes must be in the statement rather than dropped:
	// the point is to RECORD the error, not to sanitise it away.
	if !strings.Contains(failed, `\u0000`) {
		t.Errorf("the NUL byte should survive as a \\u0000 escape the lexer accepts, so the "+
			"operator still sees what the remote sent.\n  got: %s", failed)
	}
	if strings.Contains(failed, `\x00`) {
		t.Errorf("the statement still carries a Go-style \\x escape, which the lexer refuses.\n"+
			"  got: %s", failed)
	}
}

// TestStampRetrying_HostileErrorStillLexes covers the retry stamp, which is the
// worse of the two: if this mutation never applies the row is not merely
// mis-reported, it is never picked up again.
func TestStampRetrying_HostileErrorStillLexes(t *testing.T) {
	eng := &fakeEngine{pending: []map[string]any{pendingRow("v1:platform:outboundRequest:r2")}}
	tr := &fakeTransport{err: hostileError} // transient -> retry
	w := newTestWorker(eng, tr, nil)

	w.drainOnce(context.Background())

	assertStampsLex(t, eng)

	var retrying string
	for _, m := range eng.stamped() {
		if strings.Contains(m, `status: "retrying"`) {
			retrying = m
		}
	}
	if retrying == "" {
		t.Fatalf("expected a `retrying` stamp; got %v", eng.stamped())
	}
	if !strings.Contains(retrying, `\u0000`) {
		t.Errorf("the retry stamp should carry the escaped error.\n  got: %s", retrying)
	}
}

// TestStampPolicyRefusal_HostileTargetStillLexes covers the third site.
//
// HONEST NOTE: unlike the two above, this one passes against the UNFIXED code
// too, so it is a guard rather than a reproduction. The reason is worth
// recording, because it is luck rather than design: the policy error is built
// as `fmt.Errorf("target %q not in webhook allowlist", req.Target)`, so the
// target is ALREADY %q-escaped inside the error string. The outer %q then saw a
// literal backslash and escaped it to `\\x00` -- a valid escaped backslash
// followed by the letters x00 -- which lexes fine while silently mangling what
// the operator reads.
//
// So this site was never broken, and it stays unbroken only as long as that
// inner %q remains. Drop it (a reasonable-looking cleanup, since the outer
// layer now quotes properly) and this site becomes the defect the other two
// were. That is exactly what this test is here to catch.
func TestStampPolicyRefusal_HostileTargetStillLexes(t *testing.T) {
	row := pendingRow("v1:platform:outboundRequest:r3")
	row["target"] = "https://not-allowed.example/\x00\a\v"

	eng := &fakeEngine{pending: []map[string]any{row}}
	w := newTestWorker(eng, &fakeTransport{}, nil)

	w.drainOnce(context.Background())

	assertStampsLex(t, eng)
}

// TestOrdinaryErrorIsUnchanged is the counterpart: the overwhelmingly common
// case must keep working and keep reading naturally. A fix that escaped
// everything aggressively would pass the lexer tests above while making every
// ordinary lastError unreadable.
func TestOrdinaryErrorIsUnchanged(t *testing.T) {
	eng := &fakeEngine{pending: []map[string]any{pendingRow("v1:platform:outboundRequest:r4")}}
	tr := &fakeTransport{err: Permanent(errors.New("connection refused"))}
	w := newTestWorker(eng, tr, nil)

	w.drainOnce(context.Background())

	assertStampsLex(t, eng)

	var failed string
	for _, m := range eng.stamped() {
		if strings.Contains(m, `status: "failed"`) {
			failed = m
		}
	}
	if !strings.Contains(failed, `lastError: "`) || !strings.Contains(failed, "connection refused") {
		t.Errorf("an ordinary error must still render as plain readable text.\n  got: %s", failed)
	}
	if strings.Contains(failed, `\u`) {
		t.Errorf("an ordinary error should carry no escapes at all -- over-escaping would make "+
			"every lastError unreadable while still passing the lexer.\n  got: %s", failed)
	}
}

// TestHostileRequestID_StillLexes covers the argument memql#3035's audit item
// turned up, and it is the more damaging of the two failures.
//
// requestId is caller-supplied (see the file header), and it was rendered with
// %q at every stamp site. A control byte in it makes the `sending` stamp
// unlexable -- and that stamp happens BEFORE transport.Deliver, with the `sent`
// stamp after it failing the same way. So:
//
//   - the delivery HAPPENS;
//   - neither stamp applies, so the row stays `pending` with attempts=0;
//   - the claim key is fmt.Sprintf("%s:%d", req.ID, req.Attempts), which is
//     therefore unchanged;
//   - once ClaimTTL expires the same key is re-won and the same webhook is
//     delivered AGAIN.
//
// Unbounded duplicate outbound delivery, indefinitely, versus the lastError
// case's single stuck row. Both are the same defect -- %q against a lexer that
// does not implement its escapes -- reached through a different argument.
func TestHostileRequestID_StillLexes(t *testing.T) {
	const hostileID = "v1:platform:outboundRequest:r\x00bad\a\v"

	t.Run("delivery succeeds", func(t *testing.T) {
		eng := &fakeEngine{pending: []map[string]any{pendingRow(hostileID)}}
		w := newTestWorker(eng, &fakeTransport{}, nil)
		w.drainOnce(context.Background())
		assertStampsLex(t, eng)
	})

	t.Run("delivery fails", func(t *testing.T) {
		eng := &fakeEngine{pending: []map[string]any{pendingRow(hostileID)}}
		w := newTestWorker(eng, &fakeTransport{err: hostileError}, nil)
		w.drainOnce(context.Background())
		assertStampsLex(t, eng)
	})
}
