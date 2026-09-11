package agent

// context_handoff.go -- what a turn does when it runs out of context window.
//
// # The condition
//
// A turn that keeps working accumulates messages: the model's text, the tool
// calls, the tool results. A long piece of work therefore walks towards the
// model's context window, and on a goal that may run for hours it arrives.
// The provider answers with `context_length_exceeded` or one of its siblings,
// and before this file that answer ended the turn.
//
// Ending the turn there is wrong twice. Nothing failed -- the work was going
// fine and the window is a property of the model, not of the task -- and the
// error carries the word "length", so the work spine's symptom table had no
// rule for it and asked a model to guess what it meant.
//
// # The answer: compress into the next window, and carry on
//
// The remedy is the one a person would use. Keep the instructions, keep the
// recent turns that hold the live thread, drop the oldest middle, and leave a
// note in their place saying they were dropped. Then send the SAME iteration
// again, smaller. The work continues in the next window rather than starting
// over, which is the whole point: a goal's progress lives in its journaled
// steps, and the handoff is inside one step.
//
// component/memql's PlanContextTrim already decides exactly this and is
// already tested there (it was built for the engine's hardened loop and, until
// this file, had no production caller). Nothing new is invented here: this is
// the agent lanes reaching for it at the moment it applies.
//
// # Why it converges, and why it is capped
//
// We do not know the model's window. We know the request did not fit. So each
// attempt targets a fraction of what we just sent -- strictly smaller every
// time, so a few attempts cross any real limit -- and the attempts are capped.
//
// THE CAP IS THE ANTI-RUNAWAY DISCIPLINE APPLIED TO THE RECOVERY ITSELF. A
// compression loop with no bound is a runaway of the kind this tree spends
// several guards on already; and once compressing stops freeing anything, the
// honest move is to stop and let the failure through, where the symptom
// table's human.contextExhausted rule turns it into a question for a person
// instead of a retry nobody benefits from.

import (
	"strings"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// maxContextHandoffs bounds how many times ONE turn may compress and retry.
//
// Three is sized on the geometry rather than on a guess: each handoff targets
// two thirds of the previous estimate, so three of them clear a request nearly
// three times the window. A conversation still overflowing after that is not
// overflowing because of its history -- it is one oversized message, and no
// number of further passes reaches it.
const maxContextHandoffs = 3

// contextHandoffKeepTail is how many of the most recent messages a handoff
// will not touch.
//
// The tail is the live thread: the last model turn, the tool calls it made and
// the results that came back. Dropping any of it breaks the exchange the next
// call has to continue -- a tool result without its call, or a call whose
// result vanished -- so the tail is preserved even when that means the handoff
// frees nothing and the failure goes through instead. Six covers a full
// round-trip with several tools in it.
const contextHandoffKeepTail = 6

// contextHandoffPinnedHead is how many leading messages are pinned.
//
// One: the system prompt. It is the turn's instructions, and a turn that lost
// them would keep working with no idea what it was doing -- which is a worse
// outcome than the overflow.
const contextHandoffPinnedHead = 1

// contextHandoffRetainNumerator and contextHandoffRetainDenominator set the
// target size of the next window as a fraction of what just overflowed.
const (
	contextHandoffRetainNumerator   = 2
	contextHandoffRetainDenominator = 3
)

// handOffContext is what both lanes call on a failed model call.
//
// It answers (compressed messages, true) when err was an overflow this turn is
// still allowed to recover from and compressing freed something -- the caller
// retries the SAME iteration with the smaller history. It answers ok=false for
// everything else, including an overflow past the cap or one that freed
// nothing, and the caller then handles err exactly as it did before: the
// overflow goes through, and the work spine's human.contextExhausted rule
// turns it into a question rather than a retry.
//
// used is a pointer because the cap is per TURN, not per iteration: a turn
// that compresses at every iteration is not recovering, it is looping.
func (r *Replier) handOffContext(err error, messages []common.ChatMessage, used *int, iter int, requestId string) ([]common.ChatMessage, bool) {
	if !memql.IsContextOverflow(err) {
		return nil, false
	}
	if *used >= maxContextHandoffs {
		r.logger.Warn("agent: context window exhausted -- compressing freed nothing further, so the failure stands",
			"iter", iter, "handoffs", *used, "requestId", requestId, "error", err)
		return nil, false
	}
	next, note, ok := handOffToNextContextWindow(messages)
	if !ok {
		r.logger.Warn("agent: context window overflowed and there is nothing droppable, so the failure stands",
			"iter", iter, "messages", len(messages), "requestId", requestId, "error", err)
		return nil, false
	}
	*used++
	r.logger.Info("agent: context window filled -- compressed the history and carried the work into the next window",
		"iter", iter, "handoff", *used, "before", len(messages), "after", len(next),
		"note", note, "requestId", requestId)
	return next, true
}

// handOffToNextContextWindow compresses messages so the same work can continue
// in a smaller window.
//
// It returns the compressed slice, the note that replaced the dropped turns,
// and whether anything was dropped. ok=false means compressing this
// conversation frees nothing -- the caller should let the provider's error
// through rather than send the identical request again.
func handOffToNextContextWindow(messages []common.ChatMessage) ([]common.ChatMessage, string, bool) {
	if len(messages) == 0 {
		return messages, "", false
	}
	budget := memql.NewContextBudget(0)
	sizes := make([]int, len(messages))
	total := 0
	for i, m := range messages {
		sizes[i] = budget.EstimateTokens(m.Content)
		total += sizes[i]
	}
	// The target is a fraction of what the far side just refused. We are not
	// guessing the window: we are guaranteeing the next request is smaller
	// than the one that did not fit, which is the only claim available
	// without knowing the limit.
	target := total * contextHandoffRetainNumerator / contextHandoffRetainDenominator
	if target < 1 {
		return messages, "", false
	}
	plan := memql.PlanContextTrim(sizes, target, contextHandoffPinnedHead, contextHandoffKeepTail)
	if plan.DropCount == 0 {
		return messages, "", false
	}
	out := make([]common.ChatMessage, 0, len(messages)-plan.DropCount+1)
	out = append(out, messages[:contextHandoffPinnedHead]...)
	out = append(out, common.ChatMessage{Role: "system", Content: plan.Summary})
	out = append(out, messages[contextHandoffPinnedHead+plan.DropCount:]...)
	return out, strings.TrimSpace(plan.Summary), true
}
