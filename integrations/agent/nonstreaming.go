package agent

import (
	"context"
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

// nonstreaming.go -- the background / batch SI execution lane (memql#896).
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
func (r *Replier) resolveBackgroundEscalation(baseReq router.ResolveRequest, cheapProviderName string) common.ToolCallingChatSIProvider {
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
	provider common.ToolCallingChatSIProvider,
	escalationProvider common.ToolCallingChatSIProvider,
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

	start := time.Now()
	reqTimeout := backgroundRequestTimeout()

	var fullText strings.Builder
	textChunks := 0
	var allToolCalls []common.ToolCall
	iterations := 0
	var terminalEnvelope *Envelope
	consecutiveAllErrored := 0
	const maxConsecutiveAllErrored = 3

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
			} else {
				content = result
				hadSuccess = true
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
	}, nil
}
