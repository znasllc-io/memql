package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// An owned task must distinguish completion from a question. The latter uses
// the same durable capability as an explicit requestUserFeedback call; this
// is not a second approval store or a text heuristic for detecting questions.
func workResponseToolDefinition() common.ToolDefinition {
	def := RespondToUserToolDefinition()
	def.Description = "Report the outcome of the current request. Choose status=complete only when the requested answer or action is established. " +
		"If a necessary fact or decision is still missing after checking relevant available sources, choose status=needs_input and supply the actual question. " +
		"An unknown personal preference needs input from that person; it is not a completed lookup. " +
		"Do not put an invitation to tell you later in a completed response. " +
		"The runtime presents a needs_input question in Ask and pauses this task until answered. " +
		"Use kind=text for an open answer, or choice/multi with useful options. Reuse earlier answers and avoid questions about minor preferences. " +
		"Call this tool alone. Computer-access approval uses requestComputerUseScope instead."
	schema := def.InputSchema.(map[string]any)
	schema["required"] = []string{"status"}
	props := schema["properties"].(map[string]any)
	props["status"] = map[string]any{"type": "string", "enum": []string{"complete", "needs_input"}}
	props["response"].(map[string]any)["description"] = "Required for complete: the supported answer or confirmed result. Omit for needs_input."
	props["question"] = map[string]any{
		"type": "object", "required": []string{"text", "kind"},
		"description": "Required for needs_input. Explain the missing information briefly and ask a self-contained question.",
		"properties": map[string]any{
			"text": map[string]any{"type": "string"},
			"kind": map[string]any{"type": "string", "enum": []string{"text", "choice", "multi"}},
			"options": map[string]any{"type": "array", "items": map[string]any{
				"type": "object", "required": []string{"label", "value"},
				"properties": map[string]any{"label": map[string]any{"type": "string"}, "value": map[string]any{"type": "string"}},
			}},
		},
	}
	return def
}

func requiresWorkOutcome(ctx context.Context, tools []common.ToolDefinition) bool {
	if !isOwnedWorkExecution(ctx) {
		return false
	}
	for _, tool := range tools {
		if tool.Name == RespondToUserToolName {
			schema, _ := tool.InputSchema.(map[string]any)
			props, _ := schema["properties"].(map[string]any)
			return props["status"] != nil
		}
	}
	return false // Specialists do not have a human-facing response tool.
}

// A delegated app owns the entire step and its human-feedback protocol. Its
// completed session result must not cause MemQL to run a second agent loop.
func isDelegatedWorkSession(provider any) bool {
	reporter, ok := provider.(interface {
		LastSession() (memql.AppSessionOutcome, bool)
	})
	if !ok {
		return false
	}
	_, completed := reporter.LastSession()
	return completed
}

func normalizeWorkOutcome(calls []common.ToolCall) ([]common.ToolCall, error) {
	if len(calls) == 0 {
		return nil, fmt.Errorf("use respondToUser with status=complete and response, or status=needs_input and question; free text does not complete this task")
	}
	for _, call := range calls {
		if call.Name != RespondToUserToolName {
			continue
		}
		if len(calls) != 1 {
			return nil, fmt.Errorf("respondToUser must be called alone; no calls in this response were executed")
		}
		var outcome struct {
			Status   string `json:"status"`
			Response string `json:"response"`
			Question *struct {
				Text    string `json:"text"`
				Kind    string `json:"kind"`
				Options []struct {
					Label string `json:"label"`
					Value string `json:"value"`
				} `json:"options,omitempty"`
			} `json:"question"`
		}
		if err := json.Unmarshal([]byte(call.Arguments), &outcome); err != nil {
			return nil, fmt.Errorf("respondToUser requires valid JSON: %w", err)
		}
		switch outcome.Status {
		case "complete":
			if strings.TrimSpace(outcome.Response) == "" || outcome.Question != nil {
				return nil, fmt.Errorf("complete requires a nonempty response and no question; choose needs_input when an answer is missing")
			}
			if _, err := ParseEnvelope(call.Arguments); err != nil {
				return nil, fmt.Errorf("invalid completed response: %w", err)
			}
		case "needs_input":
			q := outcome.Question
			if q == nil || strings.TrimSpace(q.Text) == "" || strings.TrimSpace(outcome.Response) != "" {
				return nil, fmt.Errorf("needs_input requires question.text and question.kind; omit response")
			}
			switch q.Kind {
			case "text":
				if len(q.Options) != 0 {
					return nil, fmt.Errorf("text questions do not have options")
				}
			case "choice", "multi":
				if len(q.Options) == 0 {
					return nil, fmt.Errorf("choice and multi questions require options")
				}
				seen := map[string]bool{}
				for _, option := range q.Options {
					if strings.TrimSpace(option.Label) == "" || strings.TrimSpace(option.Value) == "" || seen[option.Value] {
						return nil, fmt.Errorf("question options require nonempty labels and distinct nonempty values")
					}
					seen[option.Value] = true
				}
			default:
				return nil, fmt.Errorf("question.kind must be text, choice or multi")
			}
			feedback := map[string]any{"question": strings.TrimSpace(q.Text), "kind": q.Kind}
			if len(q.Options) > 0 {
				feedback["options"] = q.Options
			}
			args, err := json.Marshal(feedback)
			if err != nil {
				return nil, err
			}
			// Preserve the call ID. The assistant checkpoint, audit trail and
			// tool result all name the actual capability that suspends work.
			call.Name, call.Arguments = "requestUserFeedback", string(args)
			return []common.ToolCall{call}, nil
		default:
			return nil, fmt.Errorf("respondToUser requires status=complete or status=needs_input; choose needs_input when a necessary fact or decision is unknown")
		}
	}
	return calls, nil
}

func rejectedWorkOutcome(messages []common.ChatMessage, text string, calls []common.ToolCall, err error, sink DeltaSink) []common.ChatMessage {
	content, _ := json.Marshal(map[string]any{"type": "validation", "message": err.Error(), "executed": false})
	messages = append(messages, common.ChatMessage{Role: "assistant", Content: text, ToolCalls: calls})
	if len(calls) == 0 {
		return append(messages, common.ChatMessage{Role: "user", Content: string(content)})
	}
	for _, call := range calls {
		messages = append(messages, common.ChatMessage{Role: "tool", Name: call.Name, ToolCallId: call.ID, Content: string(content)})
		sink.ToolCall(call.ID, call.Name, call.Arguments)
		sink.ToolResult(call.ID, "", err.Error())
	}
	return messages
}
