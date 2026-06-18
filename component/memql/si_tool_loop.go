package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/znasllc-io/memql/core/common"
)

// InvokeSIChatWithTools renders the prompt template and runs a bounded tool-calling loop
// using the resolved provider for the prompt. This is intended for interactive agents
// (e.g., cognition) where the model may need to call MCP tools/functions on demand.
//
// If the resolved provider does not support tool calling, this falls back to InvokeSI().
func (e *MemQLEngine) InvokeSIChatWithTools(ctx context.Context, templateId string, data map[string]any) (string, error) {
	if e == nil {
		return "", fmt.Errorf("engine is nil")
	}
	if e.siRuntime == nil || e.prompts == nil || e.providers == nil {
		return "", fmt.Errorf("SI runtime is not configured")
	}

	// Render the prompt template into a system message.
	invocation := &SIInvocation{TemplateId: templateId}
	if override := ProviderOverrideFromContext(ctx); override != "" {
		invocation.ProviderOverride = &override
	}
	prompt, err := e.siRuntime.resolvePrompt(templateId)
	if err != nil {
		return "", err
	}

	payload, err := normalizeSIData(data)
	if err != nil {
		return "", fmt.Errorf("data for prompt %q invalid: %w", prompt.Name, err)
	}
	if err := prompt.ValidateData(payload); err != nil {
		return "", fmt.Errorf("data for prompt %q invalid: %w", prompt.Name, err)
	}
	systemText, err := prompt.Render(payload)
	if err != nil {
		return "", fmt.Errorf("executing prompt template %q: %w", prompt.Name, err)
	}

	providerName, err := e.siRuntime.resolveProviderName(prompt, invocation)
	if err != nil {
		return "", err
	}
	entry, ok := e.providers.Entry(providerName)
	if !ok || entry == nil || !entry.Available || entry.Client == nil {
		return "", fmt.Errorf("provider %q not available", providerName)
	}

	toolCaller, ok := entry.Client.(common.ToolCallingChatSIProvider)
	if !ok || toolCaller == nil {
		// Fallback: no tool calling supported.
		result, err := e.siRuntime.Invoke(ctx, invocation, data)
		if err != nil {
			return "", err
		}
		if s, ok := result.(string); ok {
			return strings.TrimSpace(s), nil
		}
		b, _ := json.Marshal(result)
		return strings.TrimSpace(string(b)), nil
	}

	tools := e.toolsForToolCalling()
	messages := []common.ChatMessage{
		{Role: "system", Content: systemText},
	}

	maxIterations := defaultSIToolLoopMaxIterations
	maxToolCallsPerIt := defaultSIToolLoopMaxToolCallsPerIt
	if e.config.SIToolLoopMaxIterations > 0 {
		maxIterations = clampSIToolLoopMaxIterations(e.config.SIToolLoopMaxIterations)
	}
	if e.config.SIToolLoopMaxToolCallsPerIt > 0 {
		maxToolCallsPerIt = clampSIToolLoopMaxToolCallsPerIt(e.config.SIToolLoopMaxToolCallsPerIt)
	}

	// Cache tool results within the loop to avoid burning iterations on repeated
	// calls with identical arguments (a common LLM failure mode).
	toolResultCache := make(map[string]string)

	for iter := 0; iter < maxIterations; iter++ {
		step, err := toolCaller.CallChatWithTools(ctx, messages, tools)
		if err != nil {
			return "", err
		}
		if step == nil {
			return "", fmt.Errorf("tool-calling provider returned nil result")
		}

		assistantText := strings.TrimSpace(step.AssistantText)
		// IMPORTANT: preserve tool_calls in the assistant message when present.
		assistantMsg := common.ChatMessage{Role: "assistant", Content: assistantText}
		if len(step.ToolCalls) > 0 {
			assistantMsg.ToolCalls = step.ToolCalls
		}
		messages = append(messages, assistantMsg)

		if len(step.ToolCalls) == 0 {
			return assistantText, nil
		}

		calls := step.ToolCalls
		if len(calls) > maxToolCallsPerIt {
			calls = calls[:maxToolCallsPerIt]
		}

		for _, call := range calls {
			callId := strings.TrimSpace(call.ID)
			toolName := strings.TrimSpace(call.Name)
			rawArgs := strings.TrimSpace(call.Arguments)

			// Deduplicate tool calls inside this loop.
			cacheKey := toolName + "\n" + rawArgs
			if cached, ok := toolResultCache[cacheKey]; ok && strings.TrimSpace(cached) != "" {
				messages = append(messages, common.ChatMessage{
					Role:       "tool",
					Name:       toolName,
					ToolCallId: callId,
					Content:    cached,
				})
				continue
			}

			var args map[string]any
			if rawArgs != "" {
				_ = json.Unmarshal([]byte(rawArgs), &args)
			}

			// Look up the tool BEFORE merging defaults so the merge
			// can consult tool.AutoInjectedFields (memql#107). The
			// merge overwrites LLM-supplied values for auto-injected
			// fields with the server's default, or drops them when
			// the server has no default.
			tool, err := e.tools.Get(toolName)
			args = applyToolDefaults(ctx, tool, args, common.ToolDefaultsFromContext(ctx))
			var toolResultStr string
			if err != nil {
				// Structured not_found error (#584) -- the model can reason
				// about the type rather than parsing a raw string.
				toolResultStr = newStructuredToolError(ToolErrorNotFound,
					fmt.Sprintf("tool %q not found", toolName), nil).JSON()
			} else {
				toolResult, execErr := e.ExecuteTool(ctx, tool, args)
				if execErr != nil {
					se := ClassifyToolError(execErr)
					toolResultStr = se.JSON()
				} else {
					b, _ := json.Marshal(toolResult)
					toolResultStr = string(b)
				}
			}

			toolResultCache[cacheKey] = toolResultStr
			messages = append(messages, common.ChatMessage{
				Role:       "tool",
				Name:       toolName,
				ToolCallId: callId,
				Content:    toolResultStr,
			})
		}
	}

	return "", fmt.Errorf("tool-calling exceeded max iterations (%d)", maxIterations)
}

// InvokeSIChatWithFilteredTools is like InvokeSIChatWithTools but only includes
// the named tools. If toolNames is nil/empty, includes no tools (text-only).
//
// This is the back-compat entry point; it delegates to
// InvokeSIChatWithFilteredToolsOpts with the loop's default hardening
// options (issue #584). Existing callers are unaffected: nil options yields
// built-in defaults so single-turn latency is unchanged.
func (e *MemQLEngine) InvokeSIChatWithFilteredTools(ctx context.Context, templateId string, data map[string]any, toolNames []string) (string, error) {
	return e.InvokeSIChatWithFilteredToolsOpts(ctx, templateId, data, toolNames, nil)
}

// InvokeSIChatWithFilteredToolsOpts runs the hardened bounded tool loop
// (issue #584). Beyond the filtered-tools behavior it adds:
//   - structured, typed tool errors fed back to the model;
//   - tool-set scoping (opts.ScopedTools -- the #588 hook);
//   - per-tool timeout + retry-with-backoff (attempts surfaced as state);
//   - deterministic stopping (loop detection on repeated identical calls);
//   - context-budget trimming of oldest turns (opts.ContextBudget);
//   - step-level idempotency (opts.IdempotencyKey -- the #582/#583 hook).
//
// opts may be nil for built-in defaults.
func (e *MemQLEngine) InvokeSIChatWithFilteredToolsOpts(ctx context.Context, templateId string, data map[string]any, toolNames []string, opts *ToolLoopOptions) (string, error) {
	if e == nil {
		return "", fmt.Errorf("engine is nil")
	}
	if e.siRuntime == nil || e.prompts == nil || e.providers == nil {
		return "", fmt.Errorf("SI runtime is not configured")
	}

	// Tool-set scoping hook (#588): when opts.ScopedTools is non-empty,
	// restrict this call's tool set to that subset. Out-of-scope tools are
	// ignored. No-op when no scope is supplied.
	if opts != nil && len(opts.ScopedTools) > 0 {
		toolNames = ScopeToolNames(toolNames, opts.ScopedTools)
	}

	// Extract conversation history from data BEFORE template rendering.
	// We inject these as proper user/assistant messages (not flat text in the system prompt)
	// so the model sees a natural conversation and doesn't restart from scratch each turn.
	var chatHistory []map[string]any
	if h, ok := data["history"]; ok {
		if hs, ok2 := h.([]map[string]any); ok2 && len(hs) > 0 {
			chatHistory = hs
		}
	}

	// Replace history in data with empty slice so the template renders an empty history section.
	// The actual history is injected as chat messages below. Set a flag so the template
	// can distinguish "history in chat messages" from "no history at all".
	renderData := data
	if len(chatHistory) > 0 {
		renderData = make(map[string]any, len(data))
		for k, v := range data {
			renderData[k] = v
		}
		renderData["history"] = []map[string]any{}
		renderData["historyInMessages"] = true
	}

	// Render the prompt template into a system message.
	invocation := &SIInvocation{TemplateId: templateId}
	if override := ProviderOverrideFromContext(ctx); override != "" {
		invocation.ProviderOverride = &override
	}
	prompt, err := e.siRuntime.resolvePrompt(templateId)
	if err != nil {
		return "", err
	}

	payload, err := normalizeSIData(renderData)
	if err != nil {
		return "", fmt.Errorf("data for prompt %q invalid: %w", prompt.Name, err)
	}
	if err := prompt.ValidateData(payload); err != nil {
		return "", fmt.Errorf("data for prompt %q invalid: %w", prompt.Name, err)
	}
	systemText, err := prompt.Render(payload)
	if err != nil {
		return "", fmt.Errorf("executing prompt template %q: %w", prompt.Name, err)
	}

	providerName, err := e.siRuntime.resolveProviderName(prompt, invocation)
	if err != nil {
		return "", err
	}
	entry, ok := e.providers.Entry(providerName)
	if !ok || entry == nil || !entry.Available || entry.Client == nil {
		return "", fmt.Errorf("provider %q not available", providerName)
	}

	toolCaller, ok := entry.Client.(common.ToolCallingChatSIProvider)
	if !ok || toolCaller == nil {
		// DIAGNOSTIC: Log the provider type so we can see why tool calling is unavailable.
		if e.Logger != nil {
			e.Logger.Warn("tool loop: provider does not support tool calling, falling back to text-only",
				"template", templateId,
				"provider", providerName,
				"clientType", fmt.Sprintf("%T", entry.Client),
				"assertionOk", ok,
			)
		}
		// Fallback: no tool calling supported -- use original data (with history).
		result, err := e.siRuntime.Invoke(ctx, invocation, data)
		if err != nil {
			return "", err
		}
		if s, ok := result.(string); ok {
			return strings.TrimSpace(s), nil
		}
		b, _ := json.Marshal(result)
		return strings.TrimSpace(string(b)), nil
	}

	tools := e.toolsForToolCallingFiltered(toolNames)
	messages := []common.ChatMessage{
		{Role: "system", Content: systemText},
	}

	// Inject conversation history as actual user/assistant messages.
	for _, msg := range chatHistory {
		role, _ := msg["role"].(string)
		content, _ := msg["content"].(string)
		role = strings.TrimSpace(role)
		content = strings.TrimSpace(content)
		if role != "" && content != "" {
			messages = append(messages, common.ChatMessage{Role: role, Content: content})
		}
	}

	maxIterations := defaultSIToolLoopMaxIterations
	maxToolCallsPerIt := defaultSIToolLoopMaxToolCallsPerIt
	if e.config.SIToolLoopMaxIterations > 0 {
		maxIterations = clampSIToolLoopMaxIterations(e.config.SIToolLoopMaxIterations)
	}
	if e.config.SIToolLoopMaxToolCallsPerIt > 0 {
		maxToolCallsPerIt = clampSIToolLoopMaxToolCallsPerIt(e.config.SIToolLoopMaxToolCallsPerIt)
	}

	// Cache tool results within the loop to avoid burning iterations on repeated
	// calls with identical arguments (a common LLM failure mode).
	toolResultCache := make(map[string]string)
	toolsExecuted := 0 // Track how many tool calls actually ran.
	activityCb := common.ToolActivityCallbackFromContext(ctx)
	earlyTextCb := common.EarlyTextCallbackFromContext(ctx)
	earlyTextEmitted := false

	// Hardening state (#584).
	//   - loopDetector trips deterministic stopping on repeated identical
	//     tool calls so a model stuck on one failing call can't spin forever.
	//   - idemStore honors step idempotency so a re-dispatched call reuses
	//     the first result instead of repeating an external side effect.
	//   - retryPolicy governs per-tool timeout + retry-with-backoff.
	//   - budget, when enabled, trims the oldest turns when the window grows
	//     past the token ceiling instead of erroring.
	loopDetector := NewLoopDetector(0)
	idemStore := newLoopIdempotencyStore()
	retryPolicy := opts.resolvedRetryPolicy()
	var budget ContextBudget
	if opts != nil && opts.ContextBudget != nil {
		budget = *opts.ContextBudget
	}
	stepIdempotencyKey := ""
	if opts != nil {
		stepIdempotencyKey = opts.IdempotencyKey
	}

	for iter := 0; iter < maxIterations; iter++ {
		// Context-budget management: before each model call, if the window
		// (system + history + accumulated tool turns) exceeds the budget,
		// trim the oldest non-pinned turns and drop a summary note in their
		// place. Pinned head = the system message (index 0); we keep the
		// most-recent turns intact so the live thread is preserved.
		if budget.MaxTokens > 0 {
			sizes := make([]int, len(messages))
			for i, m := range messages {
				sizes[i] = budget.EstimateTokens(m.Content)
			}
			const keepTail = 6
			if plan := PlanContextTrim(sizes, budget.MaxTokens, 1, keepTail); plan.DropCount > 0 {
				summaryMsg := common.ChatMessage{Role: "system", Content: plan.Summary}
				trimmed := make([]common.ChatMessage, 0, len(messages)-plan.DropCount+2)
				trimmed = append(trimmed, messages[0]) // pinned system prompt
				trimmed = append(trimmed, summaryMsg)  // trim note
				trimmed = append(trimmed, messages[1+plan.DropCount:]...)
				messages = trimmed
				if e.Logger != nil {
					e.Logger.Info("tool loop: context trimmed",
						"template", templateId,
						"iteration", iter,
						"dropped", plan.DropCount,
						"maxTokens", budget.MaxTokens,
					)
				}
				opts.recordObservation(ctx, "note", plan.Summary, map[string]any{
					"dropped":   plan.DropCount,
					"maxTokens": budget.MaxTokens,
				})
			}
		}
		step, err := toolCaller.CallChatWithTools(ctx, messages, tools)
		if err != nil {
			if e.Logger != nil {
				e.Logger.Warn("tool loop: API error",
					"template", templateId,
					"iteration", iter,
					"error", err.Error(),
				)
			}
			return "", err
		}
		if step == nil {
			return "", fmt.Errorf("tool-calling provider returned nil result")
		}

		assistantText := strings.TrimSpace(step.AssistantText)
		// IMPORTANT: preserve tool_calls in the assistant message when present.
		assistantMsg := common.ChatMessage{Role: "assistant", Content: assistantText}
		if len(step.ToolCalls) > 0 {
			assistantMsg.ToolCalls = step.ToolCalls
		}
		messages = append(messages, assistantMsg)

		if len(step.ToolCalls) == 0 {
			if e.Logger != nil {
				e.Logger.Info("tool loop: complete",
					"template", templateId,
					"iterations", iter+1,
					"toolsExecuted", toolsExecuted,
					"hasText", assistantText != "",
				)
			}
			return assistantText, nil
		}

		// Early text emission: on the first iteration, if the model produced text
		// alongside tool calls, emit it immediately so the user sees an acknowledgment
		// before tool execution blocks. If no text was produced, synthesize a fallback.
		if iter == 0 && earlyTextCb != nil && !earlyTextEmitted {
			ackText := assistantText
			if ackText == "" {
				ackText = "Let me work on that..."
			}
			earlyTextCb(ackText)
			earlyTextEmitted = true
			if e.Logger != nil {
				e.Logger.Info("tool loop: early text emitted",
					"template", templateId,
					"synthetic", assistantText == "",
					"textLen", len(ackText),
				)
			}
		}

		calls := step.ToolCalls
		if len(calls) > maxToolCallsPerIt {
			calls = calls[:maxToolCallsPerIt]
		}

		// Deterministic stopping: loop detection. If the model repeats an
		// identical tool call (same name + canonical args) beyond the
		// detector's threshold, stop with a recorded reason rather than
		// spinning forever. A single repeated signature in a multi-call
		// iteration is enough to trip -- the model is stuck.
		for _, c := range calls {
			sig := ToolCallSignature(c.Name, c.Arguments)
			if loopDetector.Observe(sig) {
				reason := fmt.Sprintf("repeated identical tool call %q -- stopping (loop detected)", strings.TrimSpace(c.Name))
				if e.Logger != nil {
					e.Logger.Warn("tool loop: "+string(StopLoopDetected),
						"template", templateId,
						"iteration", iter,
						"tool", strings.TrimSpace(c.Name),
						"toolsExecuted", toolsExecuted,
					)
				}
				opts.recordObservation(ctx, "stop", reason, map[string]any{
					"reason": string(StopLoopDetected),
					"tool":   strings.TrimSpace(c.Name),
				})
				return assistantText, nil
			}
		}

		// Log the tool calls for this iteration.
		if e.Logger != nil {
			logToolNames := make([]string, 0, len(calls))
			for _, c := range calls {
				logToolNames = append(logToolNames, strings.TrimSpace(c.Name))
			}
			e.Logger.Info("tool loop: iteration",
				"template", templateId,
				"iter", iter,
				"toolCalls", len(calls),
				"tools", strings.Join(logToolNames, ","),
			)
		}

		// Separate cached calls from calls that need execution.
		type pendingCall struct {
			index   int
			callId  string
			name    string
			rawArgs string
			args    map[string]any
			idemKey string
		}
		var pending []pendingCall
		// Pre-allocated result slots (indexed by position in calls).
		resultMessages := make([]common.ChatMessage, len(calls))
		resultCacheKeys := make([]string, len(calls))

		for i, call := range calls {
			callId := strings.TrimSpace(call.ID)
			tn := strings.TrimSpace(call.Name)
			rawArgs := strings.TrimSpace(call.Arguments)
			cacheKey := tn + "\n" + rawArgs

			if cached, ok := toolResultCache[cacheKey]; ok && strings.TrimSpace(cached) != "" {
				resultMessages[i] = common.ChatMessage{
					Role:       "tool",
					Name:       tn,
					ToolCallId: callId,
					Content:    cached,
				}
				continue
			}

			// Idempotency: if a re-dispatched step (same step key + call
			// signature) already produced a result, reuse it so we don't
			// repeat an external side effect. The loop-local store covers
			// within-turn re-dispatch; cross-turn persistence lands with
			// #582/#583 behind the IdempotencyStore interface.
			idemKey := IdempotencyKeyFor(stepIdempotencyKey, tn, rawArgs)
			if prior, ok := idemStore.Get(idemKey); ok && strings.TrimSpace(prior) != "" {
				resultMessages[i] = common.ChatMessage{
					Role:       "tool",
					Name:       tn,
					ToolCallId: callId,
					Content:    prior,
				}
				toolResultCache[cacheKey] = prior
				continue
			}

			var args map[string]any
			if rawArgs != "" {
				_ = json.Unmarshal([]byte(rawArgs), &args)
			}
			// Apply tool defaults + @autoInjected validator
			// (memql#107). The goroutine below re-resolves the tool
			// for dispatch; this lookup is just so the merge can
			// consult tool.AutoInjectedFields. Lookup failure is OK
			// -- the dispatch path's unknown-tool branch rejects the
			// call.
			tool, _ := e.tools.Get(tn)
			args = applyToolDefaults(ctx, tool, args, common.ToolDefaultsFromContext(ctx))

			pending = append(pending, pendingCall{
				index:   i,
				callId:  callId,
				name:    tn,
				rawArgs: rawArgs,
				args:    args,
				idemKey: idemKey,
			})
			resultCacheKeys[i] = cacheKey
		}

		// Execute pending tool calls in parallel.
		if len(pending) > 0 {
			type execResult struct {
				index     int
				cacheKey  string
				idemKey   string
				resultStr string
				msg       common.ChatMessage
				isError   bool
			}
			results := make([]execResult, len(pending))
			var wg sync.WaitGroup
			wg.Add(len(pending))

			for ri, pc := range pending {
				go func(resultIdx int, p pendingCall) {
					defer wg.Done()

					if activityCb != nil {
						activityCb(common.ToolActivityEvent{ToolName: p.name, Phase: "start", Label: p.rawArgs})
					}

					var toolResultStr string
					isError := false

					tool, toolErr := e.tools.Get(p.name)
					if toolErr != nil {
						// Unknown tool -- structured not_found error (model
						// can recover by choosing a different tool).
						if e.Logger != nil {
							e.Logger.Warn("tool loop: tool not found", "tool", p.name, "error", toolErr)
						}
						se := newStructuredToolError(ToolErrorNotFound,
							fmt.Sprintf("tool %q not found", p.name), nil)
						toolResultStr = se.JSON()
						isError = true
					} else {
						// Per-tool timeout + retry-with-backoff (#584). The
						// retry policy wraps each attempt in a deadline,
						// backs off transient/system failures, and surfaces
						// the attempt count on the structured error.
						result, se, attempts := retryPolicy.ExecuteWithRetry(ctx,
							func(callCtx context.Context) (string, error) {
								tr, err := e.ExecuteTool(callCtx, tool, p.args)
								if err != nil {
									return "", err
								}
								if tr != nil && tr.IsError {
									// Tool reported a domain error in-band.
									// Surface it as an error so it is
									// classified + fed back structured, but
									// do NOT mechanically retry an in-band
									// error -- return the marshalled result.
									b, _ := json.Marshal(tr)
									return string(b), nil
								}
								b, _ := json.Marshal(tr)
								return string(b), nil
							})
						if se != nil {
							if e.Logger != nil {
								e.Logger.Warn("tool loop: tool execution error",
									"tool", p.name, "type", string(se.Type),
									"attempts", attempts, "error", se.Message)
							}
							toolResultStr = se.JSON()
							isError = true
						} else {
							toolResultStr = result
							// An in-band tool error still counts as an error
							// for loop accounting + idempotency (don't cache
							// a failed side effect as "done").
							isError = strings.Contains(result, `"isError":true`)
						}
					}

					if activityCb != nil {
						activityCb(common.ToolActivityEvent{
							ToolName: p.name,
							Phase:    "end",
							IsError:  isError,
							Label:    p.rawArgs,
						})
					}

					results[resultIdx] = execResult{
						index:     p.index,
						cacheKey:  resultCacheKeys[p.index],
						idemKey:   p.idemKey,
						resultStr: toolResultStr,
						msg: common.ChatMessage{
							Role:       "tool",
							Name:       p.name,
							ToolCallId: p.callId,
							Content:    toolResultStr,
						},
						isError: isError,
					}
				}(ri, pc)
			}

			wg.Wait()

			// Collect results in original order.
			for _, r := range results {
				resultMessages[r.index] = r.msg
				if r.cacheKey != "" {
					toolResultCache[r.cacheKey] = r.resultStr
				}
				// Record only SUCCESSFUL results in the idempotency store so
				// a re-dispatch of a previously-failed call is retried, not
				// short-circuited with the failure.
				if !r.isError && r.idemKey != "" && strings.TrimSpace(r.resultStr) != "" {
					idemStore.Put(r.idemKey, r.resultStr)
				}
				if !r.isError {
					toolsExecuted++
				}
			}
		}

		// Append all results in order.
		for _, msg := range resultMessages {
			if msg.Role != "" {
				messages = append(messages, msg)
			}
		}
	}

	if e.Logger != nil {
		e.Logger.Warn("tool loop: "+string(StopMaxIterations),
			"template", templateId,
			"maxIterations", maxIterations,
			"toolsExecuted", toolsExecuted,
		)
	}
	opts.recordObservation(ctx, "stop",
		fmt.Sprintf("tool-calling exceeded max iterations (%d)", maxIterations),
		map[string]any{"reason": string(StopMaxIterations), "maxIterations": maxIterations})
	return "", fmt.Errorf("tool-calling exceeded max iterations (%d)", maxIterations)
}

// toolsForToolCallingFiltered returns tool definitions only for the named tools.
// If names is nil/empty, returns nil (text-only mode).
func (e *MemQLEngine) toolsForToolCallingFiltered(names []string) []common.ToolDefinition {
	if e == nil || e.tools == nil || len(names) == 0 {
		return nil
	}
	out := make([]common.ToolDefinition, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		t, err := e.tools.Get(name)
		if err != nil || t == nil {
			if e.Logger != nil {
				e.Logger.Warn("toolsForToolCallingFiltered: tool not in registry",
					"tool", name, "error", err)
			}
			continue
		}
		var schema any
		if len(t.InputSchema) > 0 {
			_ = json.Unmarshal(t.InputSchema, &schema)
		}
		out = append(out, common.ToolDefinition{
			Name:        strings.TrimSpace(t.Name),
			Description: strings.TrimSpace(t.Description),
			InputSchema: schema,
		})
	}
	return out
}

func (e *MemQLEngine) toolsForToolCalling() []common.ToolDefinition {
	if e == nil || e.tools == nil {
		return nil
	}
	list := e.tools.List()
	if len(list) == 0 {
		return nil
	}

	out := make([]common.ToolDefinition, 0, len(list))
	for _, t := range list {
		if t == nil {
			continue
		}
		var schema any
		if len(t.InputSchema) > 0 {
			_ = json.Unmarshal(t.InputSchema, &schema)
		}
		out = append(out, common.ToolDefinition{
			Name:        strings.TrimSpace(t.Name),
			Description: strings.TrimSpace(t.Description),
			InputSchema: schema,
		})
	}
	return out
}
