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
			args = applyToolDefaults(tool, args, common.ToolDefaultsFromContext(ctx))
			var toolResult *ToolCallResult
			if err != nil {
				toolResult = &ToolCallResult{
					IsError: true,
					Content: []ToolResultContent{{Type: "text", Text: fmt.Sprintf("tool %q not found", toolName)}},
				}
			} else {
				toolResult, err = e.ExecuteTool(ctx, tool, args)
				if err != nil {
					toolResult = &ToolCallResult{
						IsError: true,
						Content: []ToolResultContent{{Type: "text", Text: err.Error()}},
					}
				}
			}

			toolResultJSON, _ := json.Marshal(toolResult)
			toolResultStr := string(toolResultJSON)
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
func (e *MemQLEngine) InvokeSIChatWithFilteredTools(ctx context.Context, templateId string, data map[string]any, toolNames []string) (string, error) {
	if e == nil {
		return "", fmt.Errorf("engine is nil")
	}
	if e.siRuntime == nil || e.prompts == nil || e.providers == nil {
		return "", fmt.Errorf("SI runtime is not configured")
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

	for iter := 0; iter < maxIterations; iter++ {
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
			args = applyToolDefaults(tool, args, common.ToolDefaultsFromContext(ctx))

			pending = append(pending, pendingCall{
				index:   i,
				callId:  callId,
				name:    tn,
				rawArgs: rawArgs,
				args:    args,
			})
			resultCacheKeys[i] = cacheKey
		}

		// Execute pending tool calls in parallel.
		if len(pending) > 0 {
			type execResult struct {
				index     int
				cacheKey  string
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

					tool, toolErr := e.tools.Get(p.name)
					var toolResult *ToolCallResult
					if toolErr != nil {
						if e.Logger != nil {
							e.Logger.Warn("tool loop: tool not found", "tool", p.name, "error", toolErr)
						}
						toolResult = &ToolCallResult{
							IsError: true,
							Content: []ToolResultContent{{Type: "text", Text: fmt.Sprintf("tool %q not found", p.name)}},
						}
					} else {
						toolResult, toolErr = e.ExecuteTool(ctx, tool, p.args)
						if toolErr != nil {
							if e.Logger != nil {
								e.Logger.Warn("tool loop: tool execution error", "tool", p.name, "error", toolErr)
							}
							toolResult = &ToolCallResult{
								IsError: true,
								Content: []ToolResultContent{{Type: "text", Text: toolErr.Error()}},
							}
						}
					}

					if activityCb != nil {
						activityCb(common.ToolActivityEvent{
							ToolName: p.name,
							Phase:    "end",
							IsError:  toolResult != nil && toolResult.IsError,
							Label:    p.rawArgs,
						})
					}

					toolResultJSON, _ := json.Marshal(toolResult)
					toolResultStr := string(toolResultJSON)
					results[resultIdx] = execResult{
						index:     p.index,
						cacheKey:  resultCacheKeys[p.index],
						resultStr: toolResultStr,
						msg: common.ChatMessage{
							Role:       "tool",
							Name:       p.name,
							ToolCallId: p.callId,
							Content:    toolResultStr,
						},
						isError: toolResult != nil && toolResult.IsError,
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
		e.Logger.Warn("tool loop: exceeded max iterations",
			"template", templateId,
			"maxIterations", maxIterations,
			"toolsExecuted", toolsExecuted,
		)
	}
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
