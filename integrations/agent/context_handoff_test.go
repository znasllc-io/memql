package agent

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

// TestAWorkExecutionTurnCarriesNoWallclock is the duration bar at the agent
// lane.
//
// The turn wallclock exists for a person watching a "Replying..." indicator;
// its own comment says so. A work-execution turn has no such person -- it is a
// journaled step of a goal that may legitimately run for hours, and how long
// it will take cannot be estimated before it runs. Applying an attention
// budget to it killed the work it was never measuring: a long composition
// reached through composeFile died at 180 seconds, and the run that had to
// explain it told a person a blip had happened.
func TestAWorkExecutionTurnCarriesNoWallclock(t *testing.T) {
	if got := turnWallclockFor(turnContext{IsWorkExecution: true}); got != 0 {
		t.Fatalf("a work-execution turn was given %s; goals may run for hours and nothing is watching a spinner", got)
	}
	// AND THE INTERACTIVE CAP IS UNTOUCHED, which is the control. Lifting it
	// everywhere would put chat and voice back to an indefinite spinner,
	// which is the thing the cap was built for.
	if got := turnWallclockFor(turnContext{}); got != maxTurnWallclock() {
		t.Fatalf("an interactive turn was given %s, want %s", got, maxTurnWallclock())
	}
}

// TestTurnWallclockSentinelIsTheOneWorkMatches is the RAISING END of a
// cross-module string contract. component/work's TerminalFailureCode matches
// this sentence to take a run terminal instead of parking it on a retry, and
// the two packages cannot see each other. A reworded sentinel here without a
// matching edit there is silent: the message would read "timeout", land on
// transient.timeout, and a self-imposed clock would go back to parking runs at
// `waiting` on retries that arrive at the same second.
func TestTurnWallclockSentinelIsTheOneWorkMatches(t *testing.T) {
	rendered := fmt.Errorf("agent: %s after %s", turnWallclockSentinel, (3 * time.Minute).Round(time.Second)).Error()
	code, terminal := work.TerminalFailureCode(rendered)
	if !terminal || code != work.TerminalSelfTimeout {
		t.Fatalf("component/work read %q as code=%q terminal=%v; a clock we set for ourselves must fail honestly",
			rendered, code, terminal)
	}
}

// TestALongTurnKeepsTheGuardsThatCountWorkRatherThanTime is the anti-runaway
// half of removing the clock.
//
// A clock cannot tell an infinite loop from a large job. The guards that can
// all count WORK -- iterations, repeated failures, money -- and every one of
// them still bounds a work-execution turn. This asserts the iteration cap
// specifically, because it is the one that would otherwise have been carrying
// nothing: a turn with no wallclock and no iteration cap is unbounded.
func TestALongTurnKeepsTheGuardsThatCountWorkRatherThanTime(t *testing.T) {
	if maxStreamingToolLoopIterations() <= 0 {
		t.Fatal("the iteration cap is the bound a work-execution turn relies on once the clock is gone")
	}
	if maxContextHandoffs <= 0 {
		t.Fatal("an uncapped compression loop is itself a runaway")
	}
}

// TestContextOverflowCompressesAndCarriesTheWorkOn is Jose's context-
// exhaustion edge.
//
// A long piece of work accumulates messages and walks into the model's window.
// Before this, the provider's refusal ended the turn -- and nothing had
// failed: the window is a property of the model, not of the task. Now the
// oldest middle turns are dropped, a note takes their place, and the SAME
// iteration is sent again smaller, so the work continues in the next window
// rather than starting over.
func TestContextOverflowCompressesAndCarriesTheWorkOn(t *testing.T) {
	long := strings.Repeat("earlier reasoning about the document. ", 200)
	messages := []common.ChatMessage{{Role: "system", Content: "You are composing a site."}}
	for i := 0; i < 12; i++ {
		messages = append(messages, common.ChatMessage{Role: "assistant", Content: long})
	}

	next, note, ok := handOffToNextContextWindow(messages)
	if !ok {
		t.Fatal("nothing was compressed, so the overflow would have ended a turn that had not failed")
	}
	if len(next) >= len(messages) {
		t.Fatalf("the next window is not smaller: %d -> %d", len(messages), len(next))
	}
	if note == "" {
		t.Error("the dropped turns left no note; the model would not know its history was shortened")
	}

	// THE INSTRUCTIONS SURVIVE. A turn that lost its system prompt keeps
	// working with no idea what it is doing, which is worse than the
	// overflow it was recovering from.
	if next[0].Content != messages[0].Content {
		t.Fatalf("the system prompt was dropped: %q", next[0].Content)
	}
	if next[1].Content != note {
		t.Fatalf("the note did not take the dropped turns' place: %q", next[1].Content)
	}

	// AND THE LIVE THREAD SURVIVES. The tail is the exchange the next call
	// has to continue; dropping any of it leaves a tool result without its
	// call.
	for i := 0; i < contextHandoffKeepTail; i++ {
		if next[len(next)-1-i].Content != messages[len(messages)-1-i].Content {
			t.Fatalf("the tail was disturbed at -%d; the next call cannot continue a broken exchange", i+1)
		}
	}
}

// TestNothingDroppableMeansTheFailureStands is the honest end of the recovery.
// One message larger than the whole window cannot be compressed around, and
// pretending otherwise is a loop that sends identical bytes forever.
func TestNothingDroppableMeansTheFailureStands(t *testing.T) {
	for _, tc := range []struct {
		name     string
		messages []common.ChatMessage
	}{
		{"nothing at all", nil},
		{"only the pinned prompt", []common.ChatMessage{{Role: "system", Content: "instructions"}}},
		{"one oversized message inside the tail", []common.ChatMessage{
			{Role: "system", Content: "instructions"},
			{Role: "user", Content: strings.Repeat("x", 4_000_000)},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, ok := handOffToNextContextWindow(tc.messages); ok {
				t.Fatal("compressing claimed to free something it cannot; the same request would be sent again")
			}
		})
	}
}

// TestTheHandoffIsCappedPerTurn pins the anti-runaway discipline applied to
// the recovery itself: a turn that compresses at every iteration is not
// recovering, it is looping. Past the cap the failure goes through, where the
// work spine's human.contextExhausted rule turns it into a question for a
// person rather than a retry nobody benefits from.
func TestTheHandoffIsCappedPerTurn(t *testing.T) {
	r := testReplier()
	overflow := errors.New("This model's maximum context length is 200000 tokens (context_length_exceeded)")
	long := strings.Repeat("earlier reasoning. ", 400)
	messages := []common.ChatMessage{{Role: "system", Content: "instructions"}}
	for i := 0; i < 40; i++ {
		messages = append(messages, common.ChatMessage{Role: "assistant", Content: long})
	}

	used := 0
	for attempt := 1; attempt <= maxContextHandoffs; attempt++ {
		next, ok := r.handOffContext(overflow, messages, &used, 0, "req")
		if !ok {
			t.Fatalf("handoff %d refused while the history was still compressible", attempt)
		}
		messages = next
	}
	if used != maxContextHandoffs {
		t.Fatalf("handoffs used = %d, want %d", used, maxContextHandoffs)
	}
	if _, ok := r.handOffContext(overflow, messages, &used, 0, "req"); ok {
		t.Fatal("the handoff ran past its cap; an unbounded compression loop is the runaway the cap exists to stop")
	}

	// AND ONLY AN OVERFLOW REACHES IT. Compressing a conversation that was
	// fine throws away its earlier turns for nothing, so every other failure
	// must fall through to the retry and terminal paths unchanged.
	for _, err := range []error{
		errors.New("context deadline exceeded"),
		errors.New("429 Too Many Requests"),
		errors.New("connection reset by peer"),
		errors.New("permission denied"),
	} {
		fresh := 0
		if _, ok := r.handOffContext(err, messages, &fresh, 0, "req"); ok {
			t.Fatalf("%q was treated as a context overflow", err)
		}
	}
}

// TestAnOverflowThatSurvivedCompressionIsAQuestionNotARetry closes the loop
// between the two halves: what the agent lane gives up on, the work spine
// turns into a stated gate rather than a silent wait.
func TestAnOverflowThatSurvivedCompressionIsAQuestionNotARetry(t *testing.T) {
	sym, ev, ok := work.ClassifyByRules(work.Signal{
		ErrorMessage: "background call: This model's maximum context length is 200000 tokens",
	})
	if !ok || sym != work.SymptomHuman || ev.RuleId != work.RuleIdContextExhausted {
		t.Fatalf("got %q/%q ok=%v, want human/%s", sym, ev.RuleId, ok, work.RuleIdContextExhausted)
	}
}
