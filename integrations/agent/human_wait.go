package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

// Only the feedback capability can yield this suspension. Arbitrary web or
// file content saying "awaiting_user" must never steer the control plane.
func feedbackWait(tool, result string) error {
	if tool != "requestUserFeedback" && tool != "requestComputerUseScope" && !strings.HasSuffix(tool, ".requestUserFeedback") && !strings.HasSuffix(tool, ".requestComputerUseScope") {
		return nil
	}
	var value any
	if json.Unmarshal([]byte(result), &value) != nil {
		return nil
	}
	var find func(any) string
	find = func(value any) string {
		switch v := value.(type) {
		case map[string]any:
			if failed, _ := v["isError"].(bool); failed {
				return ""
			}
			if v["status"] == "awaiting_user" {
				id, _ := v["approvalId"].(string)
				return id
			}
			// Query/function receipts cross the engine's data/node envelope.
			// Only these structural slots are traversed, not arbitrary strings
			// or user-authored fields that merely mention awaiting_user.
			for _, key := range []string{"data", "nodes", "payload"} {
				if id := find(v[key]); id != "" {
					return id
				}
			}
			for key, item := range v {
				if node, ok := item.(map[string]any); ok && node["id"] == key {
					if id := find(node["payload"]); id != "" {
						return id
					}
				}
			}
		case []any:
			for _, item := range v {
				if id := find(item); id != "" {
					return id
				}
			}
		}
		return ""
	}
	if id := find(value); id != "" {
		return &work.HumanWait{ApprovalID: id}
	}
	return nil
}

// A batch may contain calls after the question. Record them as unexecuted so
// every tool-call ID has a result and a resumed model can plan the remaining work.
func closePendingCalls(messages []common.ChatMessage) []common.ChatMessage {
	out := append([]common.ChatMessage(nil), messages...)
	answered := map[string]bool{}
	for _, m := range out {
		if m.Role == "tool" {
			answered[m.ToolCallId] = true
		}
	}
	for _, m := range messages {
		for _, c := range m.ToolCalls {
			if !answered[c.ID] {
				out = append(out, common.ChatMessage{Role: "tool", Name: c.Name, ToolCallId: c.ID, Content: "Not executed before the saved checkpoint. Inspect recorded results and any human answer before continuing."})
				answered[c.ID] = true
			}
		}
	}
	return out
}

func (r *Replier) saveBeforeQuestion(ctx context.Context, name string, messages []common.ChatMessage) error {
	if !isOwnedWorkExecution(ctx) || (name != "requestUserFeedback" && name != "requestComputerUseScope" && !strings.HasSuffix(name, ".requestUserFeedback") && !strings.HasSuffix(name, ".requestComputerUseScope")) {
		return nil
	}
	return r.saveWorkProgress(ctx, messages)
}

// Persist completed tool receipts before another model call can fail. A
// recovery on another replica continues from those results instead of replaying
// the goal. Unexecuted calls in a partially completed batch remain explicit.
func (r *Replier) saveWorkProgress(ctx context.Context, messages []common.ChatMessage) error {
	if !isOwnedWorkExecution(ctx) {
		return nil
	}
	writer, ok := r.engine.(interface {
		SaveWorkContinuation(context.Context, []common.ChatMessage) error
	})
	if !ok {
		return fmt.Errorf("durable work continuation is unavailable")
	}
	return writer.SaveWorkContinuation(ctx, closePendingCalls(messages))
}
func (r *Replier) restoreAfterQuestion(ctx context.Context, messages []common.ChatMessage) ([]common.ChatMessage, error) {
	if !isOwnedWorkExecution(ctx) {
		return messages, nil
	}
	if reader, ok := r.engine.(interface {
		RestoreWorkContinuation(context.Context, []common.ChatMessage) ([]common.ChatMessage, error)
	}); ok {
		return reader.RestoreWorkContinuation(ctx, messages)
	}
	return messages, nil
}

func (r *Replier) prepareWorkTool(ctx context.Context) error {
	if !isOwnedWorkExecution(ctx) {
		return nil
	}
	if writer, ok := r.engine.(interface{ PrepareWorkTool(context.Context) error }); ok {
		return writer.PrepareWorkTool(ctx)
	}
	return nil
}

func (r *Replier) workCallContext(ctx context.Context) (context.Context, context.CancelFunc, error) {
	if isOwnedWorkExecution(ctx) {
		if guard, ok := r.engine.(interface {
			ContextWithWorkCallDeadline(context.Context) (context.Context, context.CancelFunc, error)
		}); ok {
			return guard.ContextWithWorkCallDeadline(ctx)
		}
	}
	return ctx, func() {}, nil
}
