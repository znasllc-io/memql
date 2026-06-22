package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/memql/taskstamp"
	"github.com/znasllc-io/memql/component/router"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/env"
)

// nonstreaming.go -- the background / batch AI execution lane (memql#896).
//
// Planner-dispatched plan/task execution turns run HERE, not through the
// interactive streaming path (streaming.go). The two lanes share prompt
// assembly (Replier.prepareTurn), the tool registry, the respondToUser
// envelope, the iteration cap (MEMQL_TOOL_LOOP_MAX_ITERATIONS), and the
// per-turn wallclock cap. They differ ONLY in how the model is driven:
//
//   - interactive (streaming.go): CallChatStreamWithTools + a per-chunk
//     idle watchdog. Correct when a human waits on tokens in real time.
//   - background (here): CallChatWithTools -- one request, one response,
//     one OVERALL request timeout. No idle watchdog: a legitimately-slow
//     file-writing turn (produceArtifact emitting a large tool input) used
//     to be false-killed by the interactive idle watchdog (memql#893);
//     request/response has no such failure mode, and synchronous calls are
//     far easier to fan out in parallel (memql#899).

// defaultBackgroundRequestTimeoutSeconds bounds a SINGLE non-streaming
// CallChatWithTools invocation (the model step + the router's whole
// fallback-chain walk for that step). Generous: a produceArtifact turn
// authoring a large markdown deliverable legitimately spends well over a
// minute composing the tool input before the call returns. On timeout the
// step fails honestly and the loop surfaces it (builds on memql#893's
// honest-failure stance) rather than silently hanging.
//
// Override via MEMQL_BACKGROUND_REQUEST_TIMEOUT_SECONDS.
const defaultBackgroundRequestTimeoutSeconds = 300

const envBackgroundRequestTimeoutSeconds = "MEMQL_BACKGROUND_REQUEST_TIMEOUT_SECONDS"

var (
	loadBgTimeoutOnce sync.Once
	cachedBgTimeout   time.Duration
)

// backgroundRequestTimeout resolves the per-call timeout once per process.
func backgroundRequestTimeout() time.Duration {
	loadBgTimeoutOnce.Do(func() {
		cachedBgTimeout = time.Duration(defaultBackgroundRequestTimeoutSeconds) * time.Second
		reader := env.NewEnvReader("")
		if ptr, err := reader.OptionalInt(envBackgroundRequestTimeoutSeconds); err == nil && ptr != nil && *ptr > 0 {
			cachedBgTimeout = time.Duration(*ptr) * time.Second
		}
	})
	return cachedBgTimeout
}

// defaultBackgroundEscalateAfterErroredRounds is how many consecutive
// all-errored tool rounds the cheap background tier may rack up before the
// executor escalates that turn to the stronger tier (memql#898). The cheap
// model getting tools wrong round after round is the explicit "stuck"
// signal -- it usually means it's mishandling the workbench/worker tool, and
// a stronger model is far more likely to recover. Kept BELOW the hard
// all-errored break (maxConsecutiveAllErrored) so escalation gets a chance
// to fire before the loop gives up. 0 disables escalation (cheap tier only).
const defaultBackgroundEscalateAfterErroredRounds = 2

const envBackgroundEscalateAfterErroredRounds = "MEMQL_BACKGROUND_ESCALATE_AFTER_ERRORED_ROUNDS"

var (
	loadBgEscalateOnce sync.Once
	cachedBgEscalate   int
)

// backgroundEscalateAfterErroredRounds resolves the escalation threshold
// once per process. A non-negative override is honored (0 = disabled);
// a negative / unparsable value falls back to the default.
func backgroundEscalateAfterErroredRounds() int {
	loadBgEscalateOnce.Do(func() {
		cachedBgEscalate = defaultBackgroundEscalateAfterErroredRounds
		reader := env.NewEnvReader("")
		if ptr, err := reader.OptionalInt(envBackgroundEscalateAfterErroredRounds); err == nil && ptr != nil && *ptr >= 0 {
			cachedBgEscalate = *ptr
		}
	})
	return cachedBgEscalate
}

// handleBackground covers the planner-dispatched background execution path.
// It shares prepareTurn with handleStreaming, then drives the model through
// the non-streaming tool surface instead of the streaming one.
func (r *Replier) handleBackground(ctx context.Context, msg *memqlv1.AgentGenerateTurnMsg, sink DeltaSink) (*TurnResult, error) {
	turnStart := time.Now()
	prep, err := r.prepareTurn(ctx, msg, turnStart)
	if err != nil {
		return nil, err
	}

	// Resume from a saved checkpoint (memql#907). When the planner re-admits
	// a previously-passed task (its slot freed and now available again), it
	// re-dispatches with the resume hint set. The executor loads the
	// taskState that #906 persisted at the pause and injects a "here's what
	// you already did -- continue, don't redo it" block, so the agent picks
	// up the thread instead of starting the deliverable over. Best-effort:
	// no saved state (or a lookup miss) just runs a fresh turn. The slot
	// re-admission itself is Session B's (#902 controller + queue).
	if IsResume(msg.Hints) {
		if block, ok := r.loadResumeContext(ctx, prep.turnCtx.PlanId); ok {
			prep.messages = injectResumeContext(prep.messages, block)
			r.logger.Info("agent background: resuming from persisted taskState",
				"plan_id", prep.turnCtx.PlanId, "requestId", msg.RequestId)
		}
	}

	// Isolate the lane's provider selection (memql#897). prepareTurn set
	// PolicyName from the agent's INTERACTIVE preferences (its stored policy
	// or the role default). Background work runs on its own policy so the
	// chain (and, via #898, the model tier) is tuned independently of live
	// chat. A per-turn explicit provider pin still wins -- routerReq
	// precedence is ExplicitProvider > PolicyName -- so an admin hotfix hint
	// is honored, but the agent's stored interactive policy no longer leaks
	// into the background lane. The rate-budget isolation is independent of
	// this and holds even when both lanes resolve the same provider (the
	// si_guard buckets are split by context, not by provider).
	prep.routerReq.PolicyName = backgroundExecutionPolicy

	provider, resolved, err := r.router.ResolveWithTools(prep.routerReq)
	if err != nil {
		return nil, fmt.Errorf("router: resolve with-tools (background lane): %w", err)
	}
	r.logger.Info("agentReply: router picked provider",
		"lane", "background",
		"tier", "cheap",
		"provider", resolved.ProviderName,
		"vendor", resolved.Vendor,
		"model", resolved.Model,
		"policy", resolved.PolicyName,
		"chain", resolved.Chain,
		"explicitHint", prep.routerReq.ExplicitProvider,
		"operatorEnabled", prep.operatorEnabled,
		"requestId", prep.routerReq.RequestId,
	)

	// Resolve the escalation tier up front (memql#898). The background lane
	// runs cheap by default and escalates to the stronger tier mid-turn only
	// on a stuck signal. Resolution is a cheap registry lookup; resolving it
	// eagerly means the loop can swap providers without a resolve on the hot
	// path. ExplicitProvider is cleared so escalation always lands on the
	// strong tier even if the agent pinned a cheap provider. A resolution
	// failure (no escalation provider available) just disables escalation --
	// the cheap tier still runs to completion.
	escalationProvider := r.resolveBackgroundEscalation(prep.routerReq, resolved.ProviderName)

	// Mirror the acting-agent role + id (self-resolved in prepareTurn) onto
	// the ctx that drives the tool loop, so ExecuteTool's agent-only gate
	// admits this turn's tools. This is THE produceArtifact path: the planner
	// dispatches with execution_lane=background and a nil ActingAgent, so
	// without this re-stamp every workbenchHost call is rejected with "tools
	// are agent-only -- no acting agent on context" and no file is written.
	// Must be on THIS ctx, not inside prepareTurn (which discards it). (memql#938)
	ctx = stampActingAgentRoleIfMissing(ctx, msg)

	result, err := r.runNonStreamingToolLoop(ctx, provider, escalationProvider, prep.messages, prep.tools, sink, turnStart, msg.RequestId, prep.turnCtx)
	if err != nil {
		return result, err
	}
	r.finishTurn(result, prep, msg.RequestId)
	return result, nil
}

// resolveBackgroundEscalation resolves the strong escalation tier for the
// background lane (memql#898). Returns nil when escalation is unavailable or
// would resolve to the same provider as the cheap tier (no point swapping).
// cheapProviderName is the already-resolved cheap-tier provider name, used
// to skip a no-op escalation.
func (r *Replier) resolveBackgroundEscalation(baseReq router.ResolveRequest, cheapProviderName string) common.ToolCallingChatAIProvider {
	escReq := baseReq
	escReq.ExplicitProvider = "" // escalation always uses the strong policy chain
	escReq.PolicyName = backgroundEscalationPolicy
	provider, resolved, err := r.router.ResolveWithTools(escReq)
	if err != nil {
		r.logger.Warn("agentReply: background escalation tier unavailable; staying on cheap tier",
			"error", err, "requestId", baseReq.RequestId)
		return nil
	}
	if resolved.ProviderName == cheapProviderName {
		// Escalation resolved to the same provider as the cheap tier (e.g.
		// the cheap providers are all unavailable and both chains fell
		// through to the same entry). Swapping would be a no-op.
		return nil
	}
	r.logger.Info("agentReply: background escalation tier resolved",
		"tier", "escalation",
		"provider", resolved.ProviderName,
		"model", resolved.Model,
		"chain", resolved.Chain,
		"requestId", baseReq.RequestId,
	)
	return provider
}

// runNonStreamingToolLoop drives a bounded multi-step request/response
// tool-calling loop. Each iteration makes ONE CallChatWithTools call
// (bounded by an overall request timeout), executes any requested tools,
// feeds their results back, and repeats until the model returns text
// without tool calls, emits the terminal respondToUser envelope, every
// tool in a round errors, or a cap (iterations / wallclock) trips.
//
// Mirrors runStreamingToolLoop's tool-handling semantics deliberately --
// envelope interception, agent-context stamping, structured tool errors,
// the all-errored break, and Library promotion all behave identically so
// the only behavioral difference between the lanes is the transport. The
// streaming loop's per-chunk idle watchdog has no analogue here: a
// request/response call can't "stall mid-stream", so the overall request
// timeout is the only no-progress guard needed.
func (r *Replier) runNonStreamingToolLoop(
	ctx context.Context,
	provider common.ToolCallingChatAIProvider,
	escalationProvider common.ToolCallingChatAIProvider,
	messages []common.ChatMessage,
	tools []common.ToolDefinition,
	sink DeltaSink,
	turnStart time.Time,
	requestId string,
	turnCtx turnContext,
) (*TurnResult, error) {
	// Same engine-side auto-stamping of v1:planner:task rows the streaming
	// loop installs. On a post-approval Plan dispatch turnCtx.PlanId is set,
	// so tool calls attach to a semantic wrapper under that Plan.
	ctx = taskstamp.WithPlanContext(ctx, taskstamp.PlanContext{
		PlanId:      turnCtx.PlanId,
		AgentId:     turnCtx.AgentId,
		OwnerUserId: turnCtx.OwnerUserId,
		SpaceId:     turnCtx.SpaceId,
	})

	// Tag the lane so every model HTTP call this loop makes counts against
	// the background si_guard rate bucket, not the interactive one
	// (memql#897). The vendor SDKs thread this context to the request, where
	// guardedTransport reads it. A batch burst can trip the background
	// ceiling without starving live chat/voice of the interactive budget.
	ctx = memql.ContextWithBackgroundLane(ctx)

	// Charge this turn's LLM calls against the per-conversation + per-plan
	// cumulative budgets (memql#1144) so a single space or plan-lineage that
	// loops is latched on its own, without touching other conversations.
	ctx = memql.ContextWithBudgetScope(ctx,
		memql.BudgetScopeId("space", turnCtx.SpaceId),
		memql.BudgetScopeId("plan", turnCtx.PlanId))

	// Cooperative preemption (memql#906): clear any pause flag for this
	// turn's requestId on exit so a stale "pass" can never leak into a later
	// turn that reuses the id.
	defer ClearPause(requestId)

	start := time.Now()
	reqTimeout := backgroundRequestTimeout()

	var fullText strings.Builder
	textChunks := 0
	var allToolCalls []common.ToolCall
	iterations := 0
	var terminalEnvelope *Envelope
	consecutiveAllErrored := 0
	const maxConsecutiveAllErrored = 3
	// Per-turn circuit breaker on repeated IDENTICAL tool failures
	// (memql#1128). This is the lane the produceArtifact direct turn runs on
	// (background), so it is the primary site of the workbench-write runaway:
	// the all-errored guard resets on any interleaved success, but this breaker
	// trips on the SAME tool failing the SAME way N times regardless.
	failBreaker := newRepeatFailureBreaker()
	// produceArtifact re-delegation refusals (memql#1138). The #1134 guard
	// returns a corrective tool-result and continued, which let the model
	// re-call produceArtifact every iteration up to maxIterations (the guard
	// never fed the #1128 breaker -- it short-circuits before the exec path).
	// An executor turn calling produceArtifact is ALWAYS wrong; count refusals
	// per turn (tool-name keyed, args-independent) and ABORT after a small N so
	// the plan fails cleanly instead of burning the whole iteration budget.
	produceArtifactRefusals := 0
	const maxProduceArtifactRefusals = 2
	// paused is set when the loop stops at a checkpoint because the turn was
	// preempted ("passed", memql#906). Surfaced on TurnResult.Paused.
	paused := false

	// Model tiering (memql#898): the turn starts on the cheap provider and
	// escalates to escalationProvider at most once, when the cheap tier gets
	// stuck (repeated all-errored tool rounds). escalated guards the
	// one-shot; escalateAt is the threshold (0 disables).
	escalated := false
	escalateAt := backgroundEscalateAfterErroredRounds()

	maxIter := maxStreamingToolLoopIterations()
	wallclock := maxTurnWallclock()

BackgroundLoop:
	for iter := 0; iter < maxIter; iter++ {
		// Cooperative preemption checkpoint (memql#906). The boundary
		// between tool rounds is the only safe place to pause -- a model
		// call can't be interrupted mid-token. If this turn was "passed",
		// persist the in-progress working state (so #907 can resume the
		// thread) and stop cleanly without burning more tokens. The
		// planner-side "pass" action already marked the Plan paused (which
		// frees its slot via #905), so the loop just needs to stop + persist.
		if PauseRequested(requestId) {
			r.persistTaskStateOnPause(ctx, turnCtx, messages, allToolCalls, fullText.String())
			r.logger.Info("agent background: preempted at checkpoint -- pausing",
				"iter", iter, "toolCalls", len(allToolCalls), "requestId", requestId)
			paused = true
			break
		}

		iterations++

		// Wallclock cap across the whole turn (every step + retry). Surfaces
		// a typed error so the caller renders a fail-with-what-we-have
		// fallback instead of an indefinite wait.
		if elapsed := time.Since(start); elapsed >= wallclock {
			r.logger.Warn("agent background: turn wallclock exceeded",
				"iter", iter, "elapsed", elapsed.String(),
				"wallclock", wallclock.String(), "requestId", requestId)
			return &TurnResult{
				FinalText:  strings.TrimSpace(fullText.String()),
				TextChunks: textChunks,
				ToolCalls:  allToolCalls,
				Iterations: iterations,
			}, fmt.Errorf("agent: %s after %s", turnWallclockSentinel, elapsed.Round(time.Second))
		}

		// One model step, bounded by the overall request timeout. The
		// router's fallback wrapper walks the whole provider chain inside
		// this single call, so a pre-flight failure on the primary advances
		// to the next provider transparently. Transient errors that escape
		// the chain (overload / rate-limit / reset) are retried in-place
		// with backoff, same policy as the streaming loop.
		var (
			stepResult *common.ToolCallingChatResult
			stepErr    error
		)
		attempt := 0
		for {
			callCtx, cancel := context.WithTimeout(ctx, reqTimeout)
			stepResult, stepErr = provider.CallChatWithTools(callCtx, messages, tools)
			cancel()
			if stepErr == nil {
				break
			}
			// Parent cancellation is terminal -- propagate, don't retry.
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if isTransientStreamError(stepErr) && attempt < streamTransientMaxRetries {
				attempt++
				backoff := transientRetryBackoff(attempt)
				r.logger.Warn("agent background: transient call error -- retrying",
					"iter", iter, "attempt", attempt,
					"backoff", backoff.String(), "error", stepErr)
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(backoff):
				}
				continue
			}
			if iter == 0 && attempt == 0 {
				// First step never produced anything -- a hard start error.
				return nil, fmt.Errorf("background call: %w", stepErr)
			}
			r.logger.Warn("agent background: call error -- ending turn",
				"iter", iter, "attempt", attempt, "error", stepErr)
			break BackgroundLoop
		}

		turnText := ""
		var turnCalls []common.ToolCall
		if stepResult != nil {
			turnText = strings.TrimSpace(stepResult.AssistantText)
			turnCalls = stepResult.ToolCalls
		}

		// Emit any assistant prose as a single delta. Under the
		// respondToUser contract the model puts its reply in the envelope
		// and emits little/no free text, but relaying what it does emit
		// keeps the audit trail honest and feeds an opt-in "watch agent
		// work" view (memql#900).
		if turnText != "" {
			if iter > 0 && needsIterationSeparator(&fullText) {
				sink.TextDelta(" ")
				fullText.WriteString(" ")
			}
			sink.TextDelta(turnText)
			fullText.WriteString(turnText)
			textChunks++
		}

		if iter == 0 {
			toolNames := make([]string, 0, len(turnCalls))
			for _, tc := range turnCalls {
				toolNames = append(toolNames, tc.Name)
			}
			r.logger.Info("agentReply: stage",
				"stage", "firstStep",
				"lane", "background",
				"elapsed_from_turn_ms", time.Since(turnStart).Milliseconds(),
				"toolCount", len(turnCalls),
				"tools", toolNames,
				"textChars", len(turnText),
				"requestId", requestId,
			)
		}

		assistantMsg := common.ChatMessage{Role: "assistant", Content: turnText}
		if len(turnCalls) > 0 {
			assistantMsg.ToolCalls = turnCalls
		}
		messages = append(messages, assistantMsg)

		// No tool calls -> the model is done talking; terminal.
		if len(turnCalls) == 0 {
			break
		}

		// Intercept the sentinel respondToUser envelope (no engine executor
		// exists for it). Side-effect tools requested alongside it still run.
		var nonSentinelCalls []common.ToolCall
		for _, tc := range turnCalls {
			if tc.Name == RespondToUserToolName {
				env, perr := ParseEnvelope(tc.Arguments)
				if perr != nil {
					r.logger.Warn("agent background: respondToUser envelope parse failed",
						"error", perr,
						"argsPreview", truncateForLog(tc.Arguments, 200),
						"requestId", requestId,
					)
					env = Envelope{Response: strings.TrimSpace(tc.Arguments)}
				}
				terminalEnvelope = &env
				sink.ToolCall(tc.ID, tc.Name, tc.Arguments)
				continue
			}
			nonSentinelCalls = append(nonSentinelCalls, tc)
		}
		turnCalls = nonSentinelCalls

		// respondToUser alone (the common terminal shape) -> done.
		if len(turnCalls) == 0 {
			break
		}
		allToolCalls = append(allToolCalls, turnCalls...)

		for _, tc := range turnCalls {
			sink.ToolCall(tc.ID, tc.Name, tc.Arguments)
		}

		if iter == maxIter-1 {
			// Budget exhausted: run the final round's tools for their side
			// effects but don't feed results back (no turn left to consume).
			for _, tc := range turnCalls {
				args := parseToolArgs(tc.Arguments)
				if args == nil {
					args = make(map[string]any)
				}
				injectAgentContext(tc.Name, args, turnCtx)
				if _, execErr := r.stamper.ExecuteToolByName(ctx, tc.Name, args); execErr != nil {
					r.logger.Warn("agent background: tool execution failed",
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
			if args == nil {
				args = make(map[string]any)
			}
			// memql#1133: refuse a produceArtifact re-delegation from within a
			// produceArtifact executor turn BEFORE dispatch -- no new plan is
			// minted; the model is told to write the file directly. This is THE
			// lane the produceArtifact direct turn runs on (background), so the
			// cap matters most here.
			if refuse := guardProduceArtifactRedelegation(tc.Name, turnCtx); refuse != "" {
				produceArtifactRefusals++
				r.logger.Warn("agent background: refusing produceArtifact re-delegation",
					"tool", tc.Name, "refusals", produceArtifactRefusals, "requestId", requestId)
				if produceArtifactRefusals >= maxProduceArtifactRefusals {
					// The model keeps trying to re-delegate instead of writing
					// the deliverable -- ABORT the turn (the plan fails cleanly)
					// rather than spin to maxIterations. The earlier guard only
					// fed back a corrective result and continued, which is the
					// runaway #1138 fixes.
					r.logger.Warn("agent background: aborting turn -- repeated produceArtifact re-delegation",
						"count", produceArtifactRefusals, "requestId", requestId)
					return &TurnResult{
						FinalText:  strings.TrimSpace(fullText.String()),
						TextChunks: textChunks,
						ToolCalls:  allToolCalls,
						Iterations: iterations,
					}, fmt.Errorf("produceArtifact re-delegation refused %d times -- aborting turn to avoid a runaway", produceArtifactRefusals)
				}
				se := memql.ClassifyToolError(fmt.Errorf("%s", refuse))
				messages = append(messages, common.ChatMessage{
					Role:       "tool",
					Name:       tc.Name,
					ToolCallId: tc.ID,
					Content:    se.JSON(),
				})
				sink.ToolResult(tc.ID, "", refuse)
				continue
			}
			injectAgentContext(tc.Name, args, turnCtx)
			result, execErr := r.stamper.ExecuteToolByName(ctx, tc.Name, args)
			var content string
			if execErr != nil {
				se := memql.ClassifyToolError(execErr)
				r.logger.Warn("agent background: tool execution failed",
					"tool", tc.Name, "type", string(se.Type),
					"retryable", se.Retryable, "userFixable", se.UserFixable,
					"error", execErr)
				content = se.JSON()
				sink.ToolResult(tc.ID, "", execErr.Error())
				if strings.Contains(execErr.Error(), wheelContestedMarker) {
					wheelContested = true
				}
				// Repeated-identical-failure breaker (memql#1128): abort the
				// turn rather than spin to maxIterations on a stuck call.
				if trip, count := failBreaker.observeFailure(tc.Name, tc.Arguments, execErr); trip {
					r.logger.Warn("agent background: aborting turn -- repeated identical tool failure",
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
				// Library promotion (memql#722): mirror published canvas
				// cards into the user's Library as a generatedOutput.
				if tc.Name == "canvasPublish" {
					r.promoteCanvasOutput(ctx, turnCtx, args)
				}
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

		if wheelContested {
			// Background turns rarely drive the CoPresent Control widget,
			// but a worker/UI tool can still hit a contested wheel. Break
			// rather than spin retrying a widget that has stopped obeying.
			r.logger.Info("agent background: wheel contested -- breaking loop", "iter", iter)
			break
		}

		if !hadSuccess {
			// Every tool in this round errored. Give the model a few rounds
			// to self-correct from the structured error before giving up.
			consecutiveAllErrored++

			// Model tiering escalation (memql#898). Repeated all-errored
			// rounds are the explicit "the cheap tier is stuck" signal --
			// it's mishandling the tool surface and the structured errors
			// aren't getting it unstuck. Swap to the stronger tier ONCE and
			// let it continue with the in-progress history (one stronger
			// continuation, not a full re-run). Reset the counter so the
			// strong tier gets its own fresh budget of self-correction
			// rounds before the hard break.
			if escalationProvider != nil && !escalated && escalateAt > 0 && consecutiveAllErrored >= escalateAt {
				r.logger.Warn("agent background: escalating to stronger tier after stuck cheap-tier rounds",
					"erroredRounds", consecutiveAllErrored,
					"escalateAt", escalateAt,
					"iter", iter,
					"requestId", requestId)
				provider = escalationProvider
				escalated = true
				consecutiveAllErrored = 0
				continue
			}

			if consecutiveAllErrored >= maxConsecutiveAllErrored {
				r.logger.Warn("agent background: breaking after repeated all-errored rounds",
					"consecutive", consecutiveAllErrored, "escalated", escalated)
				break
			}
			continue
		}
		consecutiveAllErrored = 0

		// respondToUser dispatched alongside side-effects -> terminal.
		if terminalEnvelope != nil {
			break
		}
	}

	// Resolve the user-facing reply. The respondToUser envelope, when
	// present, is the source of truth for both text and citations.
	finalText := strings.TrimSpace(fullText.String())
	var citations []Citation
	if terminalEnvelope != nil {
		if terminalEnvelope.Response != "" {
			finalText = terminalEnvelope.Response
		}
		citations = terminalEnvelope.Citations
	}

	r.logger.Info("agent background: complete",
		"textChunks", textChunks,
		"textChars", len(finalText),
		"toolCalls", len(allToolCalls),
		"iterations", iterations,
		"envelopeUsed", terminalEnvelope != nil,
		"citationCount", len(citations),
		"totalMs", time.Since(start).Milliseconds(),
		"requestId", requestId,
	)

	return &TurnResult{
		FinalText:  finalText,
		TextChunks: textChunks,
		ToolCalls:  allToolCalls,
		Iterations: iterations,
		Citations:  citations,
		Paused:     paused,
	}, nil
}

// persistTaskStateOnPause writes a v1:planner:taskState row capturing the
// turn's in-progress working state when it's preempted at a checkpoint
// (memql#906), so a later resume (memql#907) can pick up the thread. Best-
// effort: every failure is logged and swallowed -- a persistence miss must
// never turn a clean pause into a crash.
//
// taskId contract (flagged for Session B on #906): the background turn is
// plan-scoped today (one executeApprovedPlan turn per Plan; per-task
// fan-out is memql#899, not yet live-wired), so the state is keyed to the
// Plan's execution unit via turnCtx.PlanId. If resume needs a semantic
// v1:planner:task.id instead, thread it through turnContext and swap here.
func (r *Replier) persistTaskStateOnPause(ctx context.Context, turnCtx turnContext, messages []common.ChatMessage, toolCalls []common.ToolCall, reasoning string) {
	if r.engine == nil {
		return
	}
	taskId := strings.TrimSpace(turnCtx.PlanId)
	if taskId == "" {
		// No plan-scoped unit to key the state to -- nothing to resume.
		return
	}

	// Replayable tool-call history: {toolName, args, result} per call,
	// matching each result back to its call by tool-call id.
	resultByID := make(map[string]string)
	for _, m := range messages {
		if m.Role == "tool" && m.ToolCallId != "" {
			resultByID[m.ToolCallId] = m.Content
		}
	}
	history := make([]map[string]any, 0, len(toolCalls))
	var lastResult string
	for _, tc := range toolCalls {
		entry := map[string]any{"toolName": tc.Name, "args": tc.Arguments}
		if res, ok := resultByID[tc.ID]; ok {
			entry["result"] = res
			lastResult = res
		}
		history = append(history, entry)
	}

	stateId := "taskstate-" + string(genOutputIdEngine.MustFromMap(map[string]any{
		"taskId": taskId,
		"kind":   "pause",
	}))[:16]

	args := map[string]any{
		"stateId":         stateId,
		"taskId":          taskId,
		"reasoningChain":  strings.TrimSpace(reasoning),
		"toolCallHistory": history,
		"workingMemory": map[string]any{
			"pausedAtCheckpoint": true,
			"toolCallCount":      len(history),
			"lastToolResult":     lastResult,
		},
	}
	payload, err := json.Marshal(args)
	if err != nil {
		r.logger.Warn("agent background: marshal taskState on pause failed",
			"error", err, "task_id", taskId)
		return
	}
	call := fmt.Sprintf("mutationPersistTaskState(%s)", string(payload))
	if _, err := r.engine.Execute(withUserActor(ctx, turnCtx.OwnerUserId), call); err != nil {
		r.logger.Warn("agent background: persist taskState on pause failed",
			"error", err, "task_id", taskId)
	}
}
