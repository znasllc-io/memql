package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/memql/taskstamp"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/env"
	"github.com/znasllc-io/memql/core/id"
)

// defaultMaxStreamingToolLoopIterations is the built-in cap when no
// environment override is set.
//
// Sized for multi-field walkthroughs and teaching sessions, not
// transactional takeovers: a full Create Agent walkthrough with
// per-field uiAskUser + narration chains hits 25-40 iterations
// easily (navigate → open modal → configure-manually → ~7 fields ×
// (narrate + ask + click/type/select) + highlight submit + release).
// Teaching sessions (docs/planning/teaching-mode.md) will chain
// multiple walkthroughs plus explanatory pauses and could climb
// well past 40. Chained flows like "create agent + create space"
// measure at ~54 iterations; 120 gives ~2x headroom. Override via
// the canonical MEMQL_TOOL_LOOP_MAX_ITERATIONS env var.
//
// HISTORY: this was hardcoded at 17 and caused every mid-walkthrough
// stall we chased through several rounds of prompt / RAG debugging
// before anyone noticed. Then 80. Now 120 after a real-world
// create-agent-then-create-space flow landed at 54. Keep it
// env-configurable.
const defaultMaxStreamingToolLoopIterations = 120

// envAgentToolLoopMaxIterations is the SAME env var the engine SI
// tool loop reads (component/memql/config.go:envSIToolLoopMaxIterations).
// Both loops sharing one knob is the whole point of the unification
// -- change it in one place, both loops respect it.
const envAgentToolLoopMaxIterations = "MEMQL_TOOL_LOOP_MAX_ITERATIONS"

// Transient-error retry policy for per-iteration streaming failures.
// Upstream model APIs (Anthropic / OpenAI) return 529 "overloaded",
// 429 rate-limited, and occasional 5xx transient errors that would
// otherwise drop the agent mid-walkthrough. We retry the SAME
// iteration on these, backing off so we're not hammering an
// already-overloaded backend.
const (
	streamTransientMaxRetries    = 3
	streamTransientBaseBackoffMs = 500  // 1st retry
	streamTransientMaxBackoffMs  = 4000 // ceiling for exponential backoff
)

// streamIdleTimeout is the no-progress watchdog: how long
// consumeStreamingTurn waits for ANY chunk (text, tool-call delta,
// tool-result, or upstream `Done`) before declaring the stream stalled
// and bailing.
//
// Why this exists: Anthropic's SSE streams occasionally stay open at
// the TCP level after a model-side overload without ever sending an
// error event. Without a watchdog the agent's tool-loop goroutine
// wedges forever -- request received, RAG completed, then total silence.
// The retry block in runStreamingToolLoop only fires on errors, so a
// silent stall is invisible to it.
//
// 30s is comfortable headroom over Anthropic's ~15s SSE ping cadence.
// Real model work emits chunks well within that window; only truly
// stalled streams hit the timer. When it fires we surface a typed
// transient error so the existing retry logic catches it.
//
// INTERACTIVE-ONLY (memql#901). This watchdog lives in consumeStreamingTurn,
// which only the interactive streaming lane uses. Background plan/task
// execution runs on the non-streaming executor (runNonStreamingToolLoop,
// memql#896), which has no idle watchdog -- it bounds each step with an
// overall request timeout instead. The 30s default was briefly raised to
// 120s (memql#893) to stop the watchdog false-killing slow produceArtifact
// turns while they still ran through this path; that band-aid is retired now
// that background work bypasses the watchdog, so the default is back to a
// value tuned for genuinely-stalled LIVE streams.
//
// Override via MEMQL_STREAM_IDLE_TIMEOUT_SECONDS for ops; the resolver
// caches once per process.
const defaultStreamIdleTimeoutSeconds = 30

const envStreamIdleTimeoutSeconds = "MEMQL_STREAM_IDLE_TIMEOUT_SECONDS"

// streamIdleSentinel is the substring the runStreamingToolLoop retry
// classifier looks for to recognise the watchdog's typed error and
// retry the iteration. Keep as a const so the producer + classifier
// can't drift.
const streamIdleSentinel = "stream idle timeout"

// defaultMaxTurnWallclockSeconds bounds the TOTAL elapsed time of a
// single agent turn -- across every iteration, every retry, every
// tool call. Distinct from streamIdleTimeout (which only catches
// stalled streams) and the iteration cap (which only catches loops
// of fast-but-pointless tool calls). A turn that takes 120 iterations
// at 5s each is technically not idle and not loop-capped, but it's
// still 10 minutes of staring at "Replying...". This cap surfaces
// that as a typed error so the user gets a reply-with-what-we-have
// fallback instead of an indefinite spinner.
//
// Operator chains measure 30-90s in practice; voice + simple chat
// well under 30s. 180s gives 2x headroom over the worst legitimate
// case while still catching genuine pathological cases.
//
// Override via MEMQL_TURN_WALLCLOCK_TIMEOUT_SECONDS for ops.
const defaultMaxTurnWallclockSeconds = 180

const envMaxTurnWallclockSeconds = "MEMQL_TURN_WALLCLOCK_TIMEOUT_SECONDS"

const turnWallclockSentinel = "turn wallclock timeout"

var (
	loadWallclockOnce sync.Once
	cachedWallclock   time.Duration
)

func maxTurnWallclock() time.Duration {
	loadWallclockOnce.Do(func() {
		cachedWallclock = time.Duration(defaultMaxTurnWallclockSeconds) * time.Second
		reader := env.NewEnvReader("")
		if ptr, err := reader.OptionalInt(envMaxTurnWallclockSeconds); err == nil && ptr != nil && *ptr > 0 {
			cachedWallclock = time.Duration(*ptr) * time.Second
		}
	})
	return cachedWallclock
}

var (
	loadIdleTimeoutOnce sync.Once
	cachedIdleTimeout   time.Duration
)

func streamIdleTimeout() time.Duration {
	loadIdleTimeoutOnce.Do(func() {
		cachedIdleTimeout = time.Duration(defaultStreamIdleTimeoutSeconds) * time.Second
		reader := env.NewEnvReader("")
		if ptr, err := reader.OptionalInt(envStreamIdleTimeoutSeconds); err == nil && ptr != nil && *ptr > 0 {
			cachedIdleTimeout = time.Duration(*ptr) * time.Second
		}
	})
	return cachedIdleTimeout
}

// isTransientStreamError returns true for errors the streaming loop
// should retry. Matches the textual signatures upstream providers
// surface on 529 / 429 / 503 / connection reset / timeout. We do
// substring matching (lowercase) rather than typed errors because
// the provider layer wraps everything in fmt.Errorf("received error
// while streaming: ...") and we don't want to plumb a typed-error
// surface through the common.ChatStreamWithToolsProvider interface
// just for this.
func isTransientStreamError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	transientMarkers := []string{
		"overloaded",            // Anthropic 529
		"rate_limit",            // 429
		"rate limit",            // variant
		"service unavailable",   // 503
		"internal_server_error", // 500 transient
		"timeout",
		"deadline exceeded",
		"connection reset",
		"eof",
		streamIdleSentinel, // local watchdog (see consumeStreamingTurn)
	}
	for _, m := range transientMarkers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// transientRetryBackoff returns the sleep duration for attempt N
// (1-indexed). Exponential with a hard cap so the 3rd retry isn't
// punishing the user with a 10-second pause.
func transientRetryBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	ms := streamTransientBaseBackoffMs * (1 << (attempt - 1))
	if ms > streamTransientMaxBackoffMs {
		ms = streamTransientMaxBackoffMs
	}
	return time.Duration(ms) * time.Millisecond
}

var (
	loadMaxIterationsOnce sync.Once
	cachedMaxIterations   int
)

// maxStreamingToolLoopIterations resolves the cap from env once per
// process. Falls back to defaultMaxStreamingToolLoopIterations when
// the env var is unset / non-numeric / non-positive.
func maxStreamingToolLoopIterations() int {
	loadMaxIterationsOnce.Do(func() {
		cachedMaxIterations = defaultMaxStreamingToolLoopIterations
		reader := env.NewEnvReader("")
		if ptr, err := reader.OptionalInt(envAgentToolLoopMaxIterations); err == nil && ptr != nil && *ptr > 0 {
			cachedMaxIterations = *ptr
		}
	})
	return cachedMaxIterations
}

// runStreamingToolLoop drives a bounded multi-turn streaming tool-calling
// loop. Text chunks stream into sink as they arrive; tool calls requested
// by the model are executed and their results are fed back on the next
// iteration so lookup tools (e.g. searchUsers) can influence the reply.
// The loop terminates when the model returns text without tool calls, when
// every tool in a round errored (nothing to feed back), when the iteration
// cap is hit, or when a stream error occurs.
// turnContext carries per-turn agent identity into the streaming
// tool loop so tool dispatchers that need it (worker tools today;
// any future agent-context-aware tools tomorrow) can be auto-fed
// without the LLM having to thread these ids through args. The
// fields are resolved once per turn at the top of handleStreaming
// and are immutable for the rest of the loop.
//
// PlanId is set ONLY on a post-approval execution turn (the
// planner-driven dispatch with hints["trigger"]="plan_approved");
// every other turn leaves it empty. workerHost / workerComputer
// dispatches on that turn carry the plan id through to the
// v1:worker:invocation row, which is what makes
// queryInvocationsForPlan(planId) return rows so the planner's
// outcome detector can stamp Plan succeeded/failed correctly.
type turnContext struct {
	AgentId     string
	OwnerUserId string
	SpaceId     string
	PlanId      string
	// ThreadVisibility names which chat thread is dispatching this turn:
	// "public" for Group-thread dispatches, "private" for per-user
	// Team-thread dispatches (Phase 9 of the chat-architecture plan).
	// Empty = unknown / legacy single-thread dispatch -- the runtime
	// treats this as public for canvas inheritance purposes (the
	// existing behavior).
	//
	// Used by injectAgentContext to auto-stamp visibility + forUserId
	// on canvasPublish so the LLM doesn't have to reason about thread
	// scope. The discussion-mode dispatch loop (Phase 6) sets this to
	// "private" + ForUserId; the standard Group-thread chat path leaves
	// it empty (effectively public).
	ThreadVisibility string
	// ForUserId is the user whose Team-thread dispatched this turn,
	// when ThreadVisibility == "private". Empty otherwise. Stamped onto
	// canvasPublish args alongside visibility so private cards land on
	// the dispatching user's canvas, not on every viewer's.
	ForUserId string
}

func (r *Replier) runStreamingToolLoop(
	ctx context.Context,
	provider common.ChatStreamWithToolsProvider,
	messages []common.ChatMessage,
	tools []common.ToolDefinition,
	sink DeltaSink,
	turnStart time.Time,
	requestId string,
	turnCtx turnContext,
) (*TurnResult, error) {
	// Install the PlanContext that drives engine-side auto-stamping of
	// v1:planner:task rows on every tool call (Phase 2 of the
	// planner-redesign work). When turnCtx.PlanId is empty (chat-driven
	// turn with no user-initiated Plan), the stamper materializes a
	// synthetic kind='adHocAction' Plan + semantic Task wrapper on the
	// first tool call. When turnCtx.PlanId is set (post-approval Plan
	// dispatch), the stamper attaches tool calls to a freshly-created
	// semantic wrapper under that Plan. See component/memql/taskstamp/.
	ctx = taskstamp.WithPlanContext(ctx, taskstamp.PlanContext{
		PlanId:      turnCtx.PlanId,
		AgentId:     turnCtx.AgentId,
		OwnerUserId: turnCtx.OwnerUserId,
		SpaceId:     turnCtx.SpaceId,
	})

	start := time.Now()

	var fullText strings.Builder
	textChunks := 0
	var allToolCalls []common.ToolCall
	iterations := 0
	// terminalEnvelope captures the structured user-facing reply emitted
	// via the sentinel respondToUser tool. When set, it replaces the
	// turn's FinalText (envelope.Response) and contributes Citations.
	// See envelope.go for the protocol.
	var terminalEnvelope *Envelope
	// ttftLogged tracks whether we've emitted the one-shot
	// "first chunk from upstream" log line for this turn. We log it
	// exactly once -- on the first non-empty content or tool-call chunk
	// from the first iteration -- so the log stream carries a single
	// `stage=ttft` marker per turn.
	ttftLogged := false
	// Track consecutive all-errored iterations so we break out of a
	// true pathological loop (every tool call failing with the same
	// error repeatedly) without punishing the common case of ONE
	// recoverable tool failure whose error message contains the
	// retry instructions (e.g. uiSelect: "Available: a, b, c").
	consecutiveAllErrored := 0
	const maxConsecutiveAllErrored = 3
	// Per-turn circuit breaker on repeated IDENTICAL tool failures
	// (memql#1128). Unlike consecutiveAllErrored (which resets the moment ANY
	// tool call in a round succeeds), this trips when the SAME tool fails the
	// SAME way N times in a row regardless of what else happens around it --
	// the exact shape of the produceArtifact workbench-write runaway, where a
	// trivial interleaved success kept the all-errored guard from ever firing.
	failBreaker := newRepeatFailureBreaker()

	maxIter := maxStreamingToolLoopIterations()
	wallclock := maxTurnWallclock()
StreamLoop:
	for iter := 0; iter < maxIter; iter++ {
		iterations++

		// Wallclock cap: if the total turn elapsed exceeds the per-turn
		// budget, stop and surface what we have. Each iteration can be
		// fast (tool calls < 1s) but if the model is grinding through
		// many of them the user sees only a "Replying..." indicator;
		// this caps the worst-case wait. The error path below the loop
		// renders a short fallback reply.
		if elapsed := time.Since(start); elapsed >= wallclock {
			r.logger.Warn("agent streaming: turn wallclock exceeded",
				"iter", iter, "elapsed", elapsed.String(),
				"wallclock", wallclock.String(), "requestId", requestId)
			return &TurnResult{
				FinalText:  strings.TrimSpace(fullText.String()),
				TextChunks: textChunks,
				ToolCalls:  allToolCalls,
				Iterations: iterations,
			}, fmt.Errorf("agent: %s after %s", turnWallclockSentinel, elapsed.Round(time.Second))
		}

		// Insert a separator between iteration text so consecutive turns
		// don't concatenate as "Let me look.Yes, I found it." when the
		// client renders them as a single message. Only needed when the
		// previous iteration actually produced text AND didn't already
		// end with whitespace.
		if iter > 0 && needsIterationSeparator(&fullText) {
			sink.TextDelta(" ")
			fullText.WriteString(" ")
			textChunks++
		}

		// Inner retry loop: on transient upstream errors (529
		// overloaded, 429 rate-limited, 5xx, connection resets,
		// timeouts) we retry the SAME iteration with exponential
		// backoff so a momentary API hiccup doesn't kill the whole
		// walkthrough mid-stride. Non-transient errors bubble out as
		// before. Each retry emits a brief narration-style log so we
		// can see from the logs how often we had to back off.
		var (
			chunks    <-chan common.StreamToolChunk
			err       error
			turnText  string
			turnCalls []common.ToolCall
			streamErr error
		)
		attempt := 0
		for {
			chunks, err = provider.CallChatStreamWithTools(ctx, messages, tools)
			if err != nil {
				if isTransientStreamError(err) && attempt < streamTransientMaxRetries {
					attempt++
					backoff := transientRetryBackoff(attempt)
					r.logger.Warn("agent streaming: transient start error -- retrying",
						"iter", iter, "attempt", attempt,
						"backoff", backoff.String(), "error", err)
					select {
					case <-ctx.Done():
						return nil, ctx.Err()
					case <-time.After(backoff):
					}
					continue
				}
				if iter == 0 && attempt == 0 {
					return nil, fmt.Errorf("start stream: %w", err)
				}
				r.logger.Warn("agent streaming: continuation stream failed",
					"iter", iter, "attempt", attempt, "error", err)
				break StreamLoop
			}

			turnText, turnCalls, streamErr = r.consumeStreamingTurn(ctx, chunks, sink, &textChunks, &fullText, turnStart, iter, requestId, &ttftLogged)
			if streamErr != nil {
				if isTransientStreamError(streamErr) && attempt < streamTransientMaxRetries {
					attempt++
					backoff := transientRetryBackoff(attempt)
					r.logger.Warn("agent streaming: transient stream error -- retrying",
						"iter", iter, "attempt", attempt,
						"backoff", backoff.String(), "error", streamErr)
					select {
					case <-ctx.Done():
						return nil, ctx.Err()
					case <-time.After(backoff):
					}
					continue
				}
				r.logger.Warn("agent streaming: stream error",
					"iter", iter, "attempt", attempt, "error", streamErr)
				break StreamLoop
			}
			// Success: exit the inner retry loop and proceed to
			// consume turnCalls / emit tool results.
			break
		}

		assistantMsg := common.ChatMessage{Role: "assistant", Content: turnText}
		if len(turnCalls) > 0 {
			assistantMsg.ToolCalls = turnCalls
		}
		messages = append(messages, assistantMsg)

		if len(turnCalls) == 0 {
			break
		}

		// Intercept the sentinel respondToUser tool call. This is the
		// terminal structured-output envelope the model uses to emit
		// user-facing text + citations. We DO NOT execute it as a real
		// tool (no engine handler exists); instead we parse its args
		// as Envelope and stash for use as the turn's final result.
		// Side-effect tools requested in the same turn (rare; the
		// prompt instructs the model to call respondToUser alone)
		// still execute normally below.
		var nonSentinelCalls []common.ToolCall
		for _, tc := range turnCalls {
			if tc.Name == RespondToUserToolName {
				env, perr := ParseEnvelope(tc.Arguments)
				if perr != nil {
					r.logger.Warn("agent streaming: respondToUser envelope parse failed",
						"error", perr,
						"argsPreview", truncateForLog(tc.Arguments, 200),
						"requestId", requestId,
					)
					// Treat as a degenerate ack-text so the user gets
					// SOMETHING. The prompt tells the model to call
					// respondToUser; if its args are corrupt the next
					// best behaviour is to surface the raw args as the
					// reply rather than a silent failure.
					env = Envelope{Response: strings.TrimSpace(tc.Arguments)}
				}
				terminalEnvelope = &env
				// Audit-log the envelope as a tool call so cognition has
				// the full picture and downstream observers see it in the
				// trace -- but no ToolResult since we never executed it.
				sink.ToolCall(tc.ID, tc.Name, tc.Arguments)
				continue
			}
			nonSentinelCalls = append(nonSentinelCalls, tc)
		}
		turnCalls = nonSentinelCalls

		// If the model called ONLY respondToUser this iteration, the
		// turn is terminal -- record the audit trail and exit.
		if terminalEnvelope != nil && len(turnCalls) == 0 {
			break
		}
		// If the model called respondToUser plus side-effect tools,
		// dispatch the side-effects but don't loop further: respondToUser
		// is by contract the LAST act of a turn.
		if len(turnCalls) == 0 {
			break
		}
		allToolCalls = append(allToolCalls, turnCalls...)

		// Time-to-first-tool-call marker: the single most useful
		// number for diagnosing widget lag. This is the moment the
		// agent actually decided what to do and we're about to relay
		// it browser-ward via the client-tool relay. We log it once
		// per turn (iter==0) so one line per user message.
		if iter == 0 && len(turnCalls) > 0 {
			toolNames := make([]string, 0, len(turnCalls))
			for _, tc := range turnCalls {
				toolNames = append(toolNames, tc.Name)
			}
			r.logger.Info("agentReply: stage",
				"stage", "firstToolCalls",
				"elapsed_from_turn_ms", time.Since(turnStart).Milliseconds(),
				"count", len(turnCalls),
				"tools", toolNames,
				"requestId", requestId,
			)
		}

		// Emit each tool call to the sink so cognition can audit-log /
		// forward them. The sink contract allows both ToolCall and
		// ToolResult so cognition has the full picture.
		for _, tc := range turnCalls {
			sink.ToolCall(tc.ID, tc.Name, tc.Arguments)
		}

		if iter == maxIter-1 {
			// Budget exhausted. Execute the final round's tools for their
			// side effects (e.g. clawExecuteTask kicks off a task) but
			// don't feed results back -- there's no turn left to consume.
			for _, tc := range turnCalls {
				args := parseToolArgs(tc.Arguments)
				injectAgentContext(tc.Name, args, turnCtx)
				if _, execErr := r.stamper.ExecuteToolByName(ctx, tc.Name, args); execErr != nil {
					r.logger.Warn("agent streaming: tool execution failed",
						"tool", tc.Name, "error", execErr)
					sink.ToolResult(tc.ID, "", execErr.Error())
				}
			}
			break
		}

		hadSuccess := false
		wheelContested := false
		for _, tc := range turnCalls {
			args := parseToolArgs(tc.Arguments)
			// Ensure args is a non-nil map BEFORE agent-context
			// injection. parseToolArgs returns nil when the LLM
			// sends an empty argument body, which is exactly what
			// happens for runtime-context tools like workerStatus
			// (the LLM is told the args are auto-filled and
			// correctly omits them). With a nil map, injectAgentContext
			// early-returned, the stamps were never applied, and
			// downstream substituteArgsInQuery left every
			// `$args.X` placeholder as a literal string -- handler
			// then queried with `ownerUserId = "$args.ownerUserId"`
			// (the literal!) and matched no rows. The user-visible
			// symptom: workerStatus always returned "unconfigured"
			// even when the worker was clearly connected.
			if args == nil {
				args = make(map[string]any)
			}
			injectAgentContext(tc.Name, args, turnCtx)
			result, execErr := r.stamper.ExecuteToolByName(ctx, tc.Name, args)
			var content string
			if execErr != nil {
				// Structured, typed tool error (#584): classify the raw
				// error into {type, message, retryable, userFixable} so the
				// model can reason about whether to retry-with-corrected-args
				// (validation/not_found) vs. give up (permission/system).
				se := memql.ClassifyToolError(execErr)
				r.logger.Warn("agent streaming: tool execution failed",
					"tool", tc.Name, "type", string(se.Type),
					"retryable", se.Retryable, "userFixable", se.UserFixable,
					"error", execErr)
				content = se.JSON()
				sink.ToolResult(tc.ID, "", execErr.Error())
				if strings.Contains(execErr.Error(), wheelContestedMarker) {
					wheelContested = true
				}
				// Repeated-identical-failure breaker (memql#1128): abort the
				// whole turn rather than loop to maxIterations when the model
				// is stuck re-issuing the same failing call.
				if trip, count := failBreaker.observeFailure(tc.Name, tc.Arguments, execErr); trip {
					r.logger.Warn("agent streaming: aborting turn -- repeated identical tool failure",
						"tool", tc.Name, "count", count, "requestId", requestId)
					return &TurnResult{
						FinalText:  strings.TrimSpace(fullText.String()),
						TextChunks: textChunks,
						ToolCalls:  allToolCalls,
						Iterations: iterations,
					}, repeatFailureAbortError(tc.Name, count)
				}
			} else {
				content = result
				hadSuccess = true
				failBreaker.observeSuccess()
				sink.ToolResult(tc.ID, result, "")
				// Library promotion (memql#722): publishing a canvas card
				// is a standalone deliverable the agent emits, so mirror
				// it into the user's Library as a generatedOutput. Args
				// are the post-injectAgentContext map (carries the actor
				// + space stamps); title/body come from the card's `data`
				// object. Best-effort -- failures are logged inside the
				// helper and never break the turn.
				if tc.Name == "canvasPublish" {
					r.promoteCanvasOutput(ctx, turnCtx, args)
				}
				// Log worker tool results so the agent log makes the
				// "Sofia says unconfigured but the pill is green"
				// debugging path visible. Contents are short JSON
				// blobs; not noisy. Skipping non-worker tools to
				// keep the log focused on the diagnostics axis we
				// actually care about right now.
				if tc.Name == "workerStatus" || tc.Name == "workerHost" || tc.Name == "workerComputer" || tc.Name == "requestComputerUseScope" {
					preview := result
					if len(preview) > 800 {
						preview = preview[:800] + "...<truncated>"
					}
					r.logger.Info("agent streaming: worker-tool result",
						"tool", tc.Name,
						"result", preview,
					)
				}
				// Client-executed tools (uiClick, uiRequestControl, etc.)
				// return their error payload as the tool's *content*
				// rather than an execErr. We still need to detect the
				// contested-wheel marker in that case so the loop can
				// short-circuit instead of letting the model retry.
				if strings.Contains(result, wheelContestedMarker) {
					wheelContested = true
				}
			}
			messages = append(messages, common.ChatMessage{
				Role:       "tool",
				Name:       tc.Name,
				ToolCallId: tc.ID,
				Content:    content,
			})
		}

		// WHEEL_CONTESTED is a terminal signal from the browser bridge:
		// another space's agent already owns the CoPresent Control
		// widget and any further UI tool calls we make will be dropped.
		// Break the loop immediately rather than let the model retry
		// uiRequestControl in a hopeful spin -- the browser has already
		// stopped obeying us, so every subsequent round just wastes
		// tokens and keeps the user staring at "Thinking…" until the
		// cognition TTL sweeper eventually times out every tool call.
		//
		// We emit a short user-facing text reply explaining the
		// situation so the agent's stream isn't silent, then exit.
		if wheelContested {
			msg := "I can't start right now -- another agent is already driving the CoPresent Control widget. Close that session (Take back control) and ask me again."
			// Only emit the message if the model didn't already say
			// something sensible in this iteration; otherwise we'd
			// double-speak. The model often replies with explanation
			// text alongside the failing tool call.
			if !hasMeaningfulTail(&fullText, 40) {
				sink.TextDelta(msg)
				fullText.WriteString(msg)
				textChunks++
			}
			r.logger.Info("agent streaming: wheel contested -- breaking loop",
				"iter", iter)
			break
		}

		if !hadSuccess {
			// Every tool in this round errored. Most of the time this
			// means the model is one corrected arg away from succeeding
			// -- uiSelect with a bad value returns "Available: a, b, c"
			// so the next iteration can retry with a valid option.
			// Give it a few rounds to self-correct via the error message
			// before giving up; only break when we hit a truly
			// pathological loop (N consecutive all-errored rounds).
			consecutiveAllErrored++
			if consecutiveAllErrored >= maxConsecutiveAllErrored {
				r.logger.Warn("agent streaming: breaking after repeated all-errored rounds",
					"consecutive", consecutiveAllErrored)
				break
			}
			continue
		}
		// Any successful tool resets the counter so a flaky call in the
		// middle of a healthy run doesn't accumulate toward the cap.
		consecutiveAllErrored = 0

		// If the model called respondToUser this iteration (alongside
		// side-effects we just dispatched), the turn is terminal --
		// don't open another iteration since the envelope already
		// carries the user-facing reply.
		if terminalEnvelope != nil {
			break
		}
	}

	// Resolve the user-facing reply. If the model emitted the envelope
	// via respondToUser, its `response` IS the reply -- ignore any
	// stray free-form text from upstream (the prompt forbids it; if
	// the model emitted some despite the rule, the envelope is the
	// source of truth). Citations come from the envelope only -- there
	// is no other channel for them.
	finalText := strings.TrimSpace(fullText.String())
	var citations []Citation
	if terminalEnvelope != nil {
		if terminalEnvelope.Response != "" {
			finalText = terminalEnvelope.Response
		}
		citations = terminalEnvelope.Citations
	}

	r.logger.Info("agent streaming: complete",
		"textChunks", textChunks,
		"textChars", len(finalText),
		"toolCalls", len(allToolCalls),
		"iterations", iterations,
		"envelopeUsed", terminalEnvelope != nil,
		"citationCount", len(citations),
		"totalMs", time.Since(start).Milliseconds(),
	)

	return &TurnResult{
		FinalText:  finalText,
		TextChunks: textChunks,
		ToolCalls:  allToolCalls,
		Iterations: iterations,
		Citations:  citations,
	}, nil
}

// consumeStreamingTurn reads a single turn from the provider's channel,
// emitting text chunks into sink as they cross flush boundaries and
// accumulating any tool-call deltas. textChunks is advanced in place so
// callers can pass it straight into the next turn for continuous counting.
// fullText accumulates across turns so the TurnResult contains the entire
// reply.
func (r *Replier) consumeStreamingTurn(
	ctx context.Context,
	chunks <-chan common.StreamToolChunk,
	sink DeltaSink,
	textChunks *int,
	fullText *strings.Builder,
	turnStart time.Time,
	iter int,
	requestId string,
	ttftLogged *bool,
) (turnText string, toolCalls []common.ToolCall, err error) {
	var turnBuilder strings.Builder
	var textBuffer strings.Builder

	type toolCallAccum struct {
		id   string
		name string
		args strings.Builder
	}
	toolAccum := make(map[int]*toolCallAccum)

	flushTicker := time.NewTicker(2 * time.Second)
	defer flushTicker.Stop()

	flush := func() {
		if textBuffer.Len() > 0 {
			sink.TextDelta(textBuffer.String())
			textBuffer.Reset()
			*textChunks++
			flushTicker.Reset(2 * time.Second)
		}
	}

	// Idle-progress watchdog: any chunk arriving (including upstream
	// pings that surface as a no-op chunk with no content / tools)
	// resets the timer. If nothing arrives for streamIdleTimeout the
	// upstream stream is considered stalled and we surface a typed
	// transient error so runStreamingToolLoop's retry kicks in.
	idleBudget := streamIdleTimeout()
	idleTimer := time.NewTimer(idleBudget)
	defer idleTimer.Stop()

	resetIdle := func() {
		// Drain a fired-but-unread tick before Reset, per time.Timer docs.
		if !idleTimer.Stop() {
			select {
			case <-idleTimer.C:
			default:
			}
		}
		idleTimer.Reset(idleBudget)
	}

loop:
	for {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				break loop
			}
			// Any signal -- error, Done, content, or tool delta -- is
			// "the stream is alive". Reset the watchdog before doing
			// anything else so even a chunk that we end up ignoring
			// counts as progress.
			resetIdle()
			if chunk.Error != nil {
				err = chunk.Error
				break loop
			}
			if chunk.Done {
				break loop
			}
			// One-shot TTFT marker: log on the first chunk carrying
			// either content or a tool-call fragment, on the first
			// iteration only. That's the earliest upstream-alive signal
			// and the right anchor for "time to first token" vs the
			// turn-arrival anchor captured in handleStreaming.
			if ttftLogged != nil && !*ttftLogged && iter == 0 && (chunk.Content != "" || len(chunk.ToolCalls) > 0) {
				*ttftLogged = true
				kind := "content"
				if chunk.Content == "" && len(chunk.ToolCalls) > 0 {
					kind = "toolCall"
				}
				r.logger.Info("agentReply: stage",
					"stage", "ttft",
					"elapsed_from_turn_ms", time.Since(turnStart).Milliseconds(),
					"chunk_kind", kind,
					"requestId", requestId,
				)
			}
			if chunk.Content != "" {
				fullText.WriteString(chunk.Content)
				turnBuilder.WriteString(chunk.Content)
				textBuffer.WriteString(chunk.Content)
				if shouldFlushTextBuffer(textBuffer.String()) {
					flush()
				}
			}
			for _, tcd := range chunk.ToolCalls {
				acc, exists := toolAccum[tcd.Index]
				if !exists {
					acc = &toolCallAccum{}
					toolAccum[tcd.Index] = acc
				}
				if tcd.ID != "" {
					acc.id = tcd.ID
				}
				if tcd.Name != "" {
					acc.name = tcd.Name
				}
				acc.args.WriteString(tcd.Arguments)
			}
		case <-flushTicker.C:
			flush()
		case <-idleTimer.C:
			err = fmt.Errorf("%s: no upstream chunks for %s", streamIdleSentinel, idleBudget)
			break loop
		case <-ctx.Done():
			break loop
		}
	}

	flush()
	turnText = strings.TrimSpace(turnBuilder.String())

	if len(toolAccum) > 0 {
		indices := make([]int, 0, len(toolAccum))
		for idx := range toolAccum {
			indices = append(indices, idx)
		}
		sort.Ints(indices)
		toolCalls = make([]common.ToolCall, 0, len(indices))
		for _, idx := range indices {
			acc := toolAccum[idx]
			toolCalls = append(toolCalls, common.ToolCall{
				ID:        acc.id,
				Name:      acc.name,
				Arguments: strings.TrimSpace(acc.args.String()),
			})
		}
	}

	return
}

// needsIterationSeparator reports whether the aggregate reply text so
// far ends with a character that would concatenate awkwardly against
// the next iteration's first char. Empty text and trailing whitespace
// already read fine without a separator.
func needsIterationSeparator(b *strings.Builder) bool {
	if b == nil || b.Len() == 0 {
		return false
	}
	s := b.String()
	switch s[len(s)-1] {
	case ' ', '\t', '\n', '\r':
		return false
	}
	return true
}

// wheelContestedMarker is the substring the CoPresent Control bridge
// stamps on error responses when a tool call arrives for a space that
// doesn't own the Control Session. The streaming tool loop looks for
// this marker and breaks out immediately instead of letting the model
// retry uiRequestControl against a widget that will keep refusing.
// Kept in sync with the string the frontend's ClientToolRelayBridge +
// requestControl primitive emit.
const wheelContestedMarker = "WHEEL_CONTESTED"

// hasMeaningfulTail reports whether the text the agent has streamed
// so far contains at least `minTrailingChars` non-whitespace chars in
// its last line. Used to decide whether to synthesise an additional
// user-facing message when a terminal tool error fires -- if the model
// already produced meaningful text this iteration we don't want to
// append a redundant explanation on top.
func hasMeaningfulTail(b *strings.Builder, minTrailingChars int) bool {
	if b == nil || b.Len() == 0 {
		return false
	}
	s := b.String()
	trimmed := strings.TrimSpace(s)
	if len(trimmed) < minTrailingChars {
		return false
	}
	// Only consider the most recent "utterance" (post the last
	// double-newline, which is the paragraph boundary we emit between
	// iterations). A rich reply earlier in the turn doesn't count --
	// we care whether THIS iteration said something useful.
	if idx := strings.LastIndex(trimmed, "\n\n"); idx >= 0 {
		return len(strings.TrimSpace(trimmed[idx:])) >= minTrailingChars
	}
	return true
}

// shouldFlushTextBuffer decides whether the streaming buffer should flush
// a text chunk to the sink. Mirrors cognition's heuristic: flush on
// sentence boundaries (., !, ?, newline) or when the buffer exceeds a
// soft cap.
func shouldFlushTextBuffer(s string) bool {
	if len(s) >= 200 {
		return true
	}
	if len(s) == 0 {
		return false
	}
	last := s[len(s)-1]
	switch last {
	case '.', '!', '?', '\n':
		return true
	}
	return false
}

func parseToolArgs(raw string) map[string]any {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var args map[string]any
	_ = json.Unmarshal([]byte(raw), &args)
	return args
}

// agentContextStamp is the per-tool spec for which runtime
// turnContext fields the streaming loop auto-injects into the
// LLM-supplied tool args BEFORE dispatch. Each tool's @input
// schema is the source of truth for which fields ARE accepted;
// the schema is generated with additionalProperties=false
// (tool_parser.go: toTool) so we can only stamp names the schema
// declares -- otherwise validateToolArgs rejects the call with
// "additional property X not allowed."
//
// Per-tool routing matters because the field names diverge across
// tools: requestComputerUseScope expects `spaceId`; canvasPublish
// expects `space` (matches the v1:copresent:canvasState concept's
// `space` column name); workerHost / workerComputer / workerStatus
// don't need space at all. canvasPublish doesn't expose `agentId`
// or `ownerUserId` at the top level -- the runtime agentId rides
// inside the `actor` object instead.
type agentContextStamp struct {
	// StampAgentId / StampOwnerUserId stamp the matching turnContext
	// fields onto args["agentId"] / args["ownerUserId"]. Only set
	// these for tools whose @input declares them.
	StampAgentId     bool
	StampOwnerUserId bool
	// SpaceField is the @input field name the tool uses for the
	// space id. Empty = don't stamp a space at all. The two known
	// names diverge by tool author choice: `spaceId` (camelCase id
	// convention) for most agent tools, `space` for canvasPublish
	// (mirrors the canvasState concept column name).
	SpaceField string
	// StampActor stamps args["actor"] = {kind: "agent", agentId: <id>}.
	// canvasPublish takes actor as a nested discriminated-union
	// object rather than a flat string id, so this gets its own
	// flag separate from StampAgentId.
	StampActor bool
	// StampPlanId stamps args["planId"] when the turnContext carries
	// one (set on post-approval execution turns -- see turnContext
	// docs). Used by worker tools so the v1:worker:invocation row
	// they persist downstream is filed under the right Plan id;
	// without it the row lands with planId="" and the planner's
	// queryInvocationsForPlan filter misses it, surfacing as
	// Plan-stamped-failed even when the worker tool succeeded.
	StampPlanId bool
	// StampThreadVisibility stamps args["visibility"] from the
	// turnContext's ThreadVisibility, and args["forUserId"] from the
	// turnContext's ForUserId when visibility == "private". Phase 9
	// of the chat-architecture plan: canvas-state visibility
	// inherits from the dispatching thread context. Empty
	// ThreadVisibility leaves visibility unset so the mutation's
	// "public" default applies (today's group-only dispatch path is
	// unchanged until Phase 6's discussion-mode dispatcher populates
	// the field).
	StampThreadVisibility bool
}

// agentContextStamps drives the per-tool injection. Adding a new
// tool that needs runtime context: pick which fields its @input
// declares, set the matching flags + space-field name here.
var agentContextStamps = map[string]agentContextStamp{
	"workerHost":              {StampAgentId: true, StampOwnerUserId: true, StampPlanId: true},
	"workerComputer":          {StampAgentId: true, StampOwnerUserId: true, StampPlanId: true},
	"workerStatus":            {StampAgentId: true, StampOwnerUserId: true},
	// workbenchHost runs HEADLESS work in the per-Plan sandbox. planId is
	// REQUIRED -- it keys the workspace AND is the producedByPlanId stamped
	// on the promoted v1:library:generatedOutput row; without it the
	// dispatch fails "workbench: missing required arg planId" and the
	// produceArtifact deliverable is never written (memql#948). agentId
	// attributes the output and overwrites any value the LLM hallucinated
	// (the schema marks both @autoInjected -- LLM-supplied values are not
	// trusted). taskId stays optional (invocation filing only).
	"workbenchHost": {StampAgentId: true, StampPlanId: true},
	"requestComputerUseScope": {StampAgentId: true, StampOwnerUserId: true, SpaceField: "spaceId"},
	// requestUserFeedback parks the ACTIVE Plan, so it needs the
	// turn-context planId stamped (the LLM never knows its own Plan id);
	// agentId / ownerUserId scope the mutation's owner attribution and
	// spaceId targets the canvas card.
	"requestUserFeedback": {StampAgentId: true, StampOwnerUserId: true, StampPlanId: true, SpaceField: "spaceId"},
	// produceArtifact CREATES a new plan (it doesn't park the active one),
	// so it needs the calling agent, the owning user (-> the new plan's
	// requestedBy), and the space the plan lives in. No planId stamp -- the
	// handler mints a fresh one.
	"produceArtifact": {StampAgentId: true, StampOwnerUserId: true, SpaceField: "spaceId"},
	// canvasPublish: no flat agentId / ownerUserId in the schema;
	// agentId rides inside `actor`. Space is `space`, not `spaceId`.
	// StampThreadVisibility carries the Phase 9 visibility-inheritance
	// rule: per-user Team-thread dispatches stamp visibility=private +
	// forUserId; Group-thread dispatches leave them unset so the
	// mutation's public default applies.
	"canvasPublish": {SpaceField: "space", StampActor: true, StampThreadVisibility: true},
}

// injectAgentContext stamps the runtime turnContext fields onto
// the LLM-supplied args map per agentContextStamps. No-op when
// the tool isn't in the map, when args is nil, or when the
// turnContext wasn't resolved (empty ids -- the handler will
// then fail with a clear "ownerUserId required" error and we
// surface it as the tool result; that's a real configuration
// problem worth seeing, not something to mask).
func injectAgentContext(toolName string, args map[string]any, ctx turnContext) {
	if args == nil {
		return
	}
	stamp, ok := agentContextStamps[toolName]
	if !ok {
		return
	}
	if stamp.StampAgentId && ctx.AgentId != "" {
		// Always overwrite -- the LLM may have hallucinated a value;
		// the runtime id is the source of truth.
		args["agentId"] = ctx.AgentId
	}
	if stamp.StampOwnerUserId && ctx.OwnerUserId != "" {
		args["ownerUserId"] = ctx.OwnerUserId
	}
	if stamp.SpaceField != "" && ctx.SpaceId != "" {
		args[stamp.SpaceField] = ctx.SpaceId
	}
	if stamp.StampActor && ctx.AgentId != "" {
		args["actor"] = map[string]any{
			"kind":    "agent",
			"agentId": ctx.AgentId,
		}
	}
	if stamp.StampPlanId && ctx.PlanId != "" {
		// Always overwrite -- the LLM may have hallucinated a plan
		// id (or left it empty); the runtime turn-context value is
		// the source of truth.
		args["planId"] = ctx.PlanId
	}
	if stamp.StampThreadVisibility && ctx.ThreadVisibility != "" {
		// Phase 9 visibility inheritance: stamp the dispatching
		// thread's visibility regardless of what the LLM set. For
		// private dispatches, also stamp forUserId. The LLM cannot
		// override these -- the dispatcher knows the thread context
		// authoritatively; the model does not.
		args["visibility"] = ctx.ThreadVisibility
		if ctx.ThreadVisibility == "private" && ctx.ForUserId != "" {
			args["forUserId"] = ctx.ForUserId
		}
	}
}

// promoteCanvasOutput records a v1:library:generatedOutput row for a
// canvas card the agent just published (memql#722). canvasPublish args
// carry the card content in a nested `data` object: `data.title` and
// `data.source` (markdown body) -- see the agentReply seed prompt and
// the canvasPublish arg schema (kind/data/importance/space/actor).
// Ambient / empty cards (no meaningful body) are skipped so the Library
// isn't polluted with status chrome. Idempotent: outputId is derived
// deterministically from (ownerUserId, spaceId, title) so a re-publish
// of the same card re-versions the same row. Best-effort: a promotion
// failure is logged and swallowed and never breaks the turn.
func (r *Replier) promoteCanvasOutput(ctx context.Context, turnCtx turnContext, args map[string]any) {
	if r.engine == nil {
		return
	}
	ownerUserId := strings.TrimSpace(turnCtx.OwnerUserId)
	if ownerUserId == "" {
		return
	}
	data, _ := args["data"].(map[string]any)
	title := strings.TrimSpace(stringField(data, "title"))
	body := strings.TrimSpace(stringField(data, "source"))
	// Skip ambient / empty cards: a card with no body isn't a
	// deliverable worth surfacing in the Library. Fall back to the
	// top-level title only when there's a body to anchor it.
	if body == "" {
		return
	}
	if title == "" {
		title = strings.TrimSpace(stringField(args, "title"))
	}
	if title == "" {
		title = "Canvas card"
	}

	outputId := deriveGeneratedOutputId("agent_generated", ownerUserId, turnCtx.SpaceId+":"+title)

	mutationCtx := withUserActor(ctx, ownerUserId)
	var b strings.Builder
	fmt.Fprintf(&b, `mutationCreateGeneratedOutput({outputId:%q, ownerUserId:%q, title:%q, body:%q, source:%q`,
		outputId, ownerUserId, title, body, "agent_generated")
	if turnCtx.AgentId != "" {
		fmt.Fprintf(&b, `, producedByAgentId:%q`, turnCtx.AgentId)
	}
	if turnCtx.PlanId != "" {
		fmt.Fprintf(&b, `, producedByPlanId:%q`, turnCtx.PlanId)
	}
	if turnCtx.SpaceId != "" {
		fmt.Fprintf(&b, `, spaceId:%q`, turnCtx.SpaceId)
	}
	b.WriteString("})")

	if _, err := r.engine.Execute(mutationCtx, b.String()); err != nil {
		r.logger.Warn("agent streaming: generatedOutput promotion failed",
			"title", title, "owner_user_id", ownerUserId, "error", err)
	}
}

// stringField reads a string field out of an args map, tolerating a
// nil map. Returns "" when absent or not a string.
func stringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

// genOutputIdEngine is the content-id engine used to derive deterministic
// generatedOutput ids. Safe for concurrent use.
var genOutputIdEngine = id.New()

// deriveGeneratedOutputId mints a deterministic v1:library:generatedOutput
// id from (source, ownerUserId, stableKey). Deterministic by design:
// re-inserting with the same outputId re-versions the same logical row
// (the indexGeneratedOutputOnCreate automation is idempotent), so a
// re-publish updates in place instead of minting a duplicate. NEVER
// derive from time. Built on core/id (content-addressed SHA256) per the
// integrations no-raw-sha256 conformance rule.
func deriveGeneratedOutputId(source, ownerUserId, stableKey string) string {
	h := genOutputIdEngine.MustFromMap(map[string]any{
		"source":      source,
		"ownerUserId": ownerUserId,
		"stableKey":   stableKey,
	})
	return "genout-" + string(h)[:16]
}

// withUserActor stamps a synthetic TokenInfo on ctx so engine mutations
// attribute their createdBy column to the named user. The agent-tool
// dispatch path doesn't carry the user's JWT through to per-turn context
// (cognition's forwarder builds a fresh context), and bare engine.Execute
// calls a mutation handler that requires an actor. Mirrors the worker
// integration's helper (integrations/agent/worker/integration.go) and
// automations/executor.go's contextWithSystemActor pattern, but scoped
// to a real user.
func withUserActor(ctx context.Context, ownerUserId string) context.Context {
	if strings.TrimSpace(ownerUserId) == "" {
		return ctx
	}
	claims := map[string]any{
		"sub":   ownerUserId,
		"email": ownerUserId,
		"role":  "user",
	}
	token := auth.BuildTokenInfo(claims)
	ctx = auth.ContextWithClaims(ctx, claims)
	return auth.ContextWithToken(ctx, token)
}
