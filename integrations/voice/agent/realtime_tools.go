package agent

// realtime_tools.go is the non-blocking in-voice tool-call scheduler (#1430).
//
// The GA function-calling pattern (docs/public/operate/voice-realtime-ga.md):
// the model emits a function_call output item and its response completes; the
// application executes in the background and later injects
// conversation.item.create{function_call_output, call_id} + response.create.
// The protocol explicitly supports the async shape -- items are injectable
// mid-stream, the session keeps conversing while a call is pending -- but only
// ONE response may write to the default conversation at a time, and nothing in
// the protocol produces the acknowledge-first behaviour by itself (that is the
// prompt's job, instructions.go).
//
// Before #1430 the executor chained SendFunctionResult + CreateResponse at the
// end of each per-call goroutine: mechanically off the event drain, but
// conversationally blocking -- the user heard silence across the full memql
// CallTool engine/DB round-trip, and the follow-up response.create was fired
// blind (no single-writer discipline, no user-speech check, no ordering, no
// cap, no timeout).
//
// The scheduler here makes the arc conversational:
//
//   - dispatchToolCall enqueues the call on a per-session FIFO and runs the
//     memql CallTool round-trip on its own goroutine, bounded by a per-call
//     timeout. The model's acknowledgment audio (prompt-taught) never waits on
//     it, and the conversation keeps flowing.
//   - A single worker goroutine (runToolLoop) injects each call's
//     function_call_output as soon as its FIFO turn completes -- in CALL ORDER,
//     so the conversation history is deterministic per session. Item injection
//     is always legal mid-stream, so the result is in the model's context even
//     if the user has long moved the conversation on (or barged in: the
//     pending call_id stays valid in history).
//   - The audible follow-up is ONE coalesced response.create at a QUIET
//     boundary: no active response (the GA single-writer constraint), no
//     conductor gate round-trip pending, the user not mid-speech, and a short
//     grace after the last speech_stopped (so it never races the native
//     auto-response). It is driven through the turn machine as a "tool_result"
//     directive, so the announcement itself gets the normal barge-in
//     discipline. Queue-on-default-conversation rather than an out-of-band
//     conversation:"none" response: the result and its spoken announcement
//     must live in the default conversation history, or follow-up turns would
//     find a model amnesiac about what it just told the user.
//   - A call that exceeds the timeout injects a spoken-failure-style
//     "[tool error]" output so the model can tell the user; the late real
//     result is dropped (the call_id was already answered). A call arriving
//     past the pending cap is answered the same way without executing.
//
// #1432 timing keys, kept meaningful across the async arc: tool.execute
// (execution incl. timeout), tool.result_inject (+ queue_wait_ms),
// tool.roundtrip (call emitted -> result injected -- the per-call async arc;
// the announce leg is shared/coalesced) and tool.announce (first uncovered
// result injected -> announce response fired).

import (
	"context"
	"fmt"
	"time"
)

const (
	// defaultToolCallTimeout bounds one tool's memql CallTool round-trip.
	// Tunable via MEMQL_REALTIME_TOOL_TIMEOUT_SEC.
	defaultToolCallTimeout = 45 * time.Second
	// defaultMaxPendingToolCalls bounds concurrently-executing model-driven
	// calls. Tunable via MEMQL_REALTIME_MAX_PENDING_TOOLS.
	defaultMaxPendingToolCalls = 4
	// defaultToolLoopTick is the worker's safety-net wakeup: kicks cover the
	// common transitions (result ready, response done, speech stopped); the
	// tick guarantees progress on paths that emit none of them (e.g. the
	// announce grace expiring in silence).
	defaultToolLoopTick = 200 * time.Millisecond
	// defaultToolAnnounceGrace is how long after the model's last
	// speech_stopped the announcer holds back. It covers the
	// speech_stopped -> response.created window where the native path's
	// auto-response (or the gate's engage) is about to take the
	// default-conversation writer slot but has not yet surfaced as in-flight.
	defaultToolAnnounceGrace = 1200 * time.Millisecond

	// toolResultDirectiveMode is the per-response directive mode the announce
	// response is rendered with (RealtimeInstructionsForDirective).
	toolResultDirectiveMode = "tool_result"
)

// pendingToolCall is one model-driven function call moving through the
// scheduler. output/outcome/doneAt are written exactly once, before done is
// closed; the worker reads them only after observing the close.
type pendingToolCall struct {
	callID  string
	name    string
	emitted time.Time

	done    chan struct{}
	output  string
	outcome string // "completed" | "timeout" | "capped" | "cancelled"
	doneAt  time.Time
}

func (p *pendingToolCall) isDone() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// ConfigureToolCalls sets the async tool-call bounds (#1430): the per-call
// execution timeout and the concurrent pending-call cap. Non-positive values
// keep the defaults. Call before Start (the worker reads them).
func (e *RealtimeExecutor) ConfigureToolCalls(timeout time.Duration, maxPending int) {
	if timeout > 0 {
		e.toolTimeout = timeout
	}
	if maxPending > 0 {
		e.toolMaxPending = maxPending
	}
}

// dispatchToolCall enqueues one model-driven function call (#1430). It returns
// immediately -- the event drain never waits, and neither does the model's
// prompt-taught spoken acknowledgment: execution runs on its own goroutine and
// the result is injected later by the FIFO worker at the right boundary. A nil
// bridge logs and ignores the call (model-driven tools disabled). A call past
// the pending cap is answered with a spoken-failure-style output without
// executing, so the model can tell the user instead of dangling a call_id.
func (e *RealtimeExecutor) dispatchToolCall(callID, funcName, arguments string) {
	if e.toolBridge == nil {
		if e.logger != nil {
			e.logger.Debug("voice-agent realtime: function call with no tool bridge (ignored)",
				"name", funcName, "call_id", callID)
		}
		return
	}
	p := &pendingToolCall{
		callID:  callID,
		name:    funcName,
		emitted: time.Now(),
		done:    make(chan struct{}),
	}

	e.toolMu.Lock()
	running := 0
	for _, q := range e.toolQueue {
		if !q.isDone() {
			running++
		}
	}
	capped := running >= e.toolMaxPending
	if capped {
		p.output = fmt.Sprintf("[tool error] %s was not started: %d tool calls are already running. "+
			"Tell the user you are at capacity and will get to it once the current work finishes.",
			funcName, running)
		p.outcome = "capped"
		p.doneAt = time.Now()
		close(p.done)
	}
	e.toolQueue = append(e.toolQueue, p)
	e.toolMu.Unlock()

	if capped {
		if e.logger != nil {
			e.logger.Warn("voice-agent realtime: tool call refused at pending cap (#1430)",
				"tool", funcName, "call_id", callID, "pending", running, "cap", e.toolMaxPending)
		}
		e.kickToolLoop()
		return
	}
	go e.executeToolCall(p, arguments)
}

// executeToolCall runs one call's memql CallTool round-trip (bridge Dispatch)
// bounded by the per-call timeout, then marks the entry done and wakes the
// worker. On timeout the entry carries a spoken-failure-style output (the
// model tells the user); the late real result is drained and dropped -- its
// call_id was already answered, and a second function_call_output for the same
// call_id is a protocol error.
func (e *RealtimeExecutor) executeToolCall(p *pendingToolCall, arguments string) {
	execStart := time.Now()
	execCtx, cancel := context.WithTimeout(e.ctx, e.toolTimeout)
	defer cancel()

	resCh := make(chan string, 1)
	go func() { resCh <- e.toolBridge.Dispatch(execCtx, p.name, arguments) }()

	select {
	case out := <-resCh:
		p.output = out
		p.outcome = "completed"
	case <-execCtx.Done():
		if e.ctx.Err() != nil {
			// Executor teardown: nothing to inject into a closing session.
			p.output = "[tool error] the voice session ended before the tool finished"
			p.outcome = "cancelled"
		} else {
			p.output = fmt.Sprintf("[tool error] %s did not finish within %s. The work may still "+
				"complete in the background; tell the user and offer to check again.",
				p.name, e.toolTimeout.Round(time.Second))
			p.outcome = "timeout"
			// Drain the late result so the Dispatch goroutine never leaks, and
			// log the drop for the trace.
			go func(callID, name string) {
				select {
				case late := <-resCh:
					if e.logger != nil {
						e.logger.Warn("voice-agent realtime: late tool result dropped (call already answered with timeout)",
							"tool", name, "call_id", callID, "chars", len(late))
					}
				case <-e.ctx.Done():
				}
			}(p.callID, p.name)
		}
	}

	// #1432 tool.execute: the execution leg only (Dispatch incl. the
	// tool.transport memQL round-trip it times internally), regardless of when
	// the result is later injected.
	logVoiceTiming(e.logger, "tool.execute", execStart,
		"space_id", e.cfg.SpaceID, "tool", p.name, "call_id", p.callID, "outcome", p.outcome)

	p.doneAt = time.Now()
	close(p.done)
	e.kickToolLoop()
}

// kickToolLoop wakes the worker without ever blocking the caller.
func (e *RealtimeExecutor) kickToolLoop() {
	select {
	case e.toolKick <- struct{}{}:
	default:
	}
}

// runToolLoop is the single per-session tool worker (#1430): it injects ready
// results in call order and fires the coalesced announce response at quiet
// boundaries. One goroutine = single writer for both legs, so injections never
// interleave and at most one announce is ever pending. Woken by kicks (result
// ready, response done, speech stopped) with a ticker as the safety net.
func (e *RealtimeExecutor) runToolLoop() {
	ticker := time.NewTicker(e.toolTick)
	defer ticker.Stop()

	announcePending := false
	var announceArmedAt time.Time // first uncovered injection since the last announce

	for {
		select {
		case <-e.ctx.Done():
			return
		case <-e.toolKick:
		case <-ticker.C:
		}

		if e.injectReadyToolResults() {
			if !announcePending {
				announceArmedAt = time.Now()
			}
			announcePending = true
		}

		if announcePending && e.tryAnnounceToolResults(announceArmedAt) {
			announcePending = false
		}
	}
}

// injectReadyToolResults pops every completed head off the FIFO (call order --
// a still-running earlier call holds later results back, bounded by the tool
// timeout) and injects its function_call_output. Items are injectable
// mid-stream per the GA contract, so this never waits for a quiet boundary:
// the result enters the model's context as soon as ordering allows, even while
// another response streams or the user speaks. Returns true when at least one
// result was injected (arming the announce).
func (e *RealtimeExecutor) injectReadyToolResults() bool {
	injected := false
	for {
		e.toolMu.Lock()
		if len(e.toolQueue) == 0 || !e.toolQueue[0].isDone() {
			e.toolMu.Unlock()
			return injected
		}
		p := e.toolQueue[0]
		e.toolQueue = e.toolQueue[1:]
		e.toolMu.Unlock()

		injectStart := time.Now()
		if err := e.session.SendFunctionResult(p.callID, p.output); err != nil {
			if e.logger != nil {
				e.logger.Warn("voice-agent realtime: send function result failed",
					"name", p.name, "call_id", p.callID, "err", err)
			}
			// The item never reached the conversation; announcing it would
			// have the model narrate a result it cannot see. Drop and move on
			// (a failed send here means the session is going away).
			continue
		}
		// #1432 tool.result_inject: the injection leg; queue_wait_ms is how
		// long the completed result waited for its FIFO turn.
		logVoiceTiming(e.logger, "tool.result_inject", injectStart,
			"space_id", e.cfg.SpaceID, "tool", p.name, "call_id", p.callID,
			"outcome", p.outcome, "queue_wait_ms", injectStart.Sub(p.doneAt).Milliseconds())
		// #1432 tool.roundtrip: the per-call async arc, call emitted -> result
		// in the model's context. The audible announce leg is shared across
		// coalesced results and measured separately (tool.announce).
		logVoiceTiming(e.logger, "tool.roundtrip", p.emitted,
			"space_id", e.cfg.SpaceID, "tool", p.name, "call_id", p.callID, "outcome", p.outcome)
		injected = true
	}
}

// tryAnnounceToolResults fires the coalesced follow-up response for injected
// tool results when -- and only when -- the conversation is at a quiet
// boundary:
//
//   - no response is in flight (GA: one writer to the default conversation),
//   - no conductor gate round-trip is pending (its engage is about to take the
//     writer slot; colliding would cost the user their answer),
//   - the user is not mid-speech (model VAD flag for the native/semantic
//     paths, turn-machine human-turn for the labeled-ASR path) -- a result
//     NEVER interrupts the user,
//   - a short grace has passed since the last speech_stopped (covering the
//     window where the native auto-response has not yet surfaced in-flight).
//
// The announce is driven through the turn machine as a "tool_result" directive
// so it takes the normal assistant-turn path: barge-in discipline, lifecycle
// engage, output capture (the spoken result lands in chat). Returns true when
// the directive was accepted; on false the caller retries at the next
// boundary, with the results already safely in context either way.
func (e *RealtimeExecutor) tryAnnounceToolResults(armedAt time.Time) bool {
	if e.isInFlight() {
		return false
	}
	if e.gatePending.Load() > 0 {
		return false
	}
	if e.userSpeaking.Load() || e.machine.State() == StateHumanTurn {
		return false
	}
	if stop := e.lastSpeechStopWall.Load(); stop > 0 {
		if time.Since(time.Unix(0, stop)) < e.toolAnnounceGrace {
			return false
		}
	}
	if !e.machine.OnSpeak(SpeakDirective{Mode: toolResultDirectiveMode}) {
		return false
	}
	// #1432 tool.announce: first uncovered result injected -> announce
	// response fired (the coalesced audible leg of the async arc).
	logVoiceTiming(e.logger, "tool.announce", armedAt, "space_id", e.cfg.SpaceID)
	return true
}
