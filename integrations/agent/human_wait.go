package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

// PauseWorkBeforeTurn handles a native budget refusal before prompt/tool
// preparation. Only receipt work detaches from the expired run deadline. A
// same-lineage saved exchange wins over the new request's shorter history.
func (r *Replier) PauseWorkBeforeTurn(ctx context.Context, failure error, msg *memqlv1.AgentGenerateTurnMsg) error {
	return r.pauseBeforeTools(ctx, failure, msg, nil)
}

// Preparation and provider selection are still before the first tool boundary.
// Preserve a pause already completed by preparation; only native budget causes
// may turn another preparation error into a durable suspension.
func (r *Replier) pauseBeforeTools(ctx context.Context, failure error, msg *memqlv1.AgentGenerateTurnMsg, messages []common.ChatMessage) error {
	var wait *work.HumanWait
	if errors.As(failure, &wait) || errors.Is(failure, memql.ErrWorkCheckpointRequired) || !isOwnedWorkExecution(ctx) {
		return failure
	}
	var ceiling *memql.RunCeilingError
	if !errors.As(failure, &ceiling) && r != nil {
		if budget := r.workCallFailure(ctx); budget != nil {
			failure = budget
		}
	}
	if !errors.As(failure, &ceiling) {
		return failure
	}
	if r == nil || msg == nil || len(msg.History) == 0 {
		return fmt.Errorf("%w: entry budget pause has no work turn", memql.ErrWorkCheckpointRequired)
	}
	// Prompt preparation has not run. RestoreWorkContinuation refreshes this
	// system slot from the installed prompt when the approved turn resumes.
	if len(messages) == 0 {
		messages = []common.ChatMessage{{Role: "system"}}
		for _, m := range msg.History {
			if m != nil {
				messages = append(messages, common.ChatMessage{Role: m.Role, Content: m.Content, Name: m.Name, ToolCallId: m.ToolCallId})
			}
		}
	}
	if len(messages) == 1 {
		return fmt.Errorf("%w: entry budget pause has no work history", memql.ErrWorkCheckpointRequired)
	}
	readCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stop()
	saved, err := r.restoreAfterQuestion(readCtx, messages)
	if err != nil {
		return fmt.Errorf("%w: restore before entry budget pause: %v", memql.ErrWorkCheckpointRequired, err)
	}
	return r.pauseAtBudgetBoundary(ctx, failure, saved)
}

// Only the feedback capability can yield this suspension. Arbitrary web or
// file content saying "awaiting_user" must never steer the control plane.
func feedbackWait(tool, result string) error {
	if receipt := feedbackReceipt(tool, result, "awaiting_user"); receipt != nil {
		return &work.HumanWait{ApprovalID: receipt["approvalId"].(string)}
	}
	return nil
}

func feedbackReceipt(tool, result, status string) map[string]any {
	if tool != "requestUserFeedback" && tool != "requestComputerUseScope" && !strings.HasSuffix(tool, ".requestUserFeedback") && !strings.HasSuffix(tool, ".requestComputerUseScope") {
		return nil
	}
	var value any
	if json.Unmarshal([]byte(result), &value) != nil {
		return nil
	}
	var find func(any) map[string]any
	find = func(value any) map[string]any {
		switch v := value.(type) {
		case map[string]any:
			if failed, _ := v["isError"].(bool); failed {
				return nil
			}
			if v["status"] == status {
				id, _ := v["approvalId"].(string)
				if id != "" {
					return v
				}
			}
			// Query/function receipts cross the engine's data/node envelope.
			// Only these structural slots are traversed, not arbitrary strings
			// or user-authored fields that merely mention awaiting_user.
			for _, key := range []string{"data", "nodes", "payload"} {
				if id := find(v[key]); id != nil {
					return id
				}
			}
			for key, item := range v {
				if node, ok := item.(map[string]any); ok && node["id"] == key {
					if id := find(node["payload"]); id != nil {
						return id
					}
				}
			}
		case []any:
			for _, item := range v {
				if id := find(item); id != nil {
					return id
				}
			}
		}
		return nil
	}
	return find(value)
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
				out = append(out, common.ChatMessage{Role: "tool", Name: c.Name, ToolCallId: c.ID, Content: common.UnexecutedWorkToolResult})
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
	run, _ := common.RunFromContext(ctx)
	if !isOwnedWorkExecution(ctx) {
		if run.Continuation != nil {
			return nil, fmt.Errorf("the required continuation lost its owned execution context")
		}
		return messages, nil
	}
	if reader, ok := r.engine.(interface {
		RestoreWorkContinuation(context.Context, []common.ChatMessage) ([]common.ChatMessage, error)
	}); ok {
		return reader.RestoreWorkContinuation(ctx, messages)
	}
	if run.Continuation != nil {
		return nil, fmt.Errorf("the required continuation reader is unavailable")
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

// Inspect the attempt before its cleanup cancels it. A run budget refusal is
// terminal for this turn; transient provider errors still use ordinary retries.
func (r *Replier) workCallFailure(ctx context.Context) error {
	if guard, ok := r.engine.(interface{ WorkCallFailure(context.Context) error }); ok {
		return guard.WorkCallFailure(ctx)
	}
	return nil
}

// Only call at a reasoning boundary or before dispatching a tool. An arbitrary
// in-flight tool failure is not evidence that its effects can be replayed.
func (r *Replier) pauseAtBudgetBoundary(ctx context.Context, failure error, messages []common.ChatMessage) error {
	var ceiling *memql.RunCeilingError
	if !errors.As(failure, &ceiling) || !isOwnedWorkExecution(ctx) {
		return failure
	}
	writer, ok := r.engine.(interface {
		PauseWorkForBudget(context.Context, *memql.RunCeilingError, []common.ChatMessage) error
	})
	if !ok {
		return fmt.Errorf("%w: durable budget continuation is unavailable", memql.ErrWorkCheckpointRequired)
	}
	err := writer.PauseWorkForBudget(ctx, ceiling, closePendingCalls(messages))
	var wait *work.HumanWait
	if errors.As(err, &wait) {
		return err
	}
	return fmt.Errorf("%w: budget suspension failed: %v", memql.ErrWorkCheckpointRequired, err)
}
