package work

import "testing"

// THE BUG THIS WHOLE FILE IS ABOUT, stated as a test.
//
// The Materializer wrapped its one model call in a fixed three-minute
// deadline. A long document blew through it, the compose pipeline wrote the
// composition row terminally `failed`, and the error it returned said "context
// deadline exceeded". The symptom table read those words, matched
// transient.timeout, and the run parked at `waiting` on a retry. Two rows
// disagreeing about whether the work was over, and the one a person reads said
// the patient thing.
//
// The deadline is gone. This pins the other half: even carrying the exact words
// that fooled the table, a failure the system already recorded as terminal is
// classified as terminal BEFORE the table is consulted.
func TestATerminalCompositionOutranksTheWordsThatLookTransient(t *testing.T) {
	msg := "compose: composing the draft failed: context deadline exceeded"

	// Without the code, this is the old behaviour and it is still correct for
	// a genuine provider timeout: a blip, retried inside the budget.
	sym, ev, ok := ClassifyByRules(Signal{ErrorMessage: msg})
	if !ok || sym != SymptomTransient || ev.RuleId != "transient.timeout" {
		t.Fatalf("a bare provider timeout should still read transient.timeout; got %q/%q ok=%v", sym, ev.RuleId, ok)
	}
	if _, terminal := TerminalFailureCode(msg); terminal {
		t.Fatal("a bare provider timeout must NOT be terminal -- the same call may well work again")
	}

	// With it, the failure is an outcome and never reaches the table.
	coded := TerminalCompositionFailed + ": " + msg
	code, terminal := TerminalFailureCode(coded)
	if !terminal {
		t.Fatal("a composition that recorded its own failure was read as a symptom; the run would park on a retry against a `failed` row")
	}
	if code != TerminalCompositionFailed {
		t.Fatalf("code = %q, want %q", code, TerminalCompositionFailed)
	}
	if TerminalReason(code) == "" {
		t.Fatal("a terminal code with no reason puts a bare `failed` in front of somebody")
	}
}

// A clock MemQL set for itself is not evidence about the far side, so retrying
// it is a loop that arrives at the same second. The sentinel is
// integrations/agent's, matched as a string because the modules cannot see
// each other; TestTurnWallclockSentinelIsTheOneWorkMatches pins the other end.
func TestASelfImposedClockIsTerminalRatherThanTransient(t *testing.T) {
	msg := "agent: turn wallclock timeout after 3m0s"
	code, terminal := TerminalFailureCode(msg)
	if !terminal || code != TerminalSelfTimeout {
		t.Fatalf("code = %q terminal = %v, want %q: our own deadline is not a blip", code, terminal, TerminalSelfTimeout)
	}
}

func TestTerminalFailureCodeIsNarrow(t *testing.T) {
	// EVERY ONE OF THESE MUST STAY CLASSIFIABLE AS A SYMPTOM. A loose
	// terminal matcher is worse than none: it would take an ordinary
	// retryable failure straight to a dead run, and the retry that would
	// have fixed it never happens.
	for _, msg := range []string{
		"context deadline exceeded",
		"i/o timeout",
		"Client.Timeout exceeded while awaiting headers",
		"provider returned 429 Too Many Requests",
		"dial tcp: connection refused",
		"permission denied",
		"",
	} {
		if code, ok := TerminalFailureCode(msg); ok {
			t.Errorf("%q was called terminal (%s); a retryable failure would never be retried", msg, code)
		}
	}
}

func TestTerminalReasonAnswersForEveryCode(t *testing.T) {
	for _, code := range terminalFailureCodes {
		if TerminalReason(code) == "" {
			t.Errorf("%q has no reason in words", code)
		}
	}
	if TerminalReason("something-else") == "" {
		t.Fatal("an unknown code still needs a sentence; the field is read by a person either way")
	}
}
