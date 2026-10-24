package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

type outcomeRecoveryRuntime interface {
	ReadLegacyWorkOutcome(context.Context) (*memql.WorkDelegatedOutcome, error)
	WorkDelegatedOutcomeSource(context.Context, string, []common.ChatMessage) ([]common.ChatMessage, error)
	FormatWorkOutcome(context.Context, memql.WorkDelegatedOutcome, json.RawMessage) (string, error)
}
type pendingOutcomeFormat struct {
	Input        memql.WorkDelegatedOutcome `json:"input"`
	Attempts     int                        `json:"attempts"`
	Done         bool                       `json:"done"`
	Question     bool                       `json:"question,omitempty"`
	NeedsSources bool                       `json:"needsSources,omitempty"`
}

func (r *Replier) saveOutcomeDraft(ctx context.Context, store workReplyReviewStore, binding memql.WorkReplyReviewBinding, input memql.WorkDelegatedOutcome, collect bool) (*pendingWorkReply, error) {
	_, ok := r.engine.(outcomeRecoveryRuntime)
	if !ok {
		return nil, fmt.Errorf("%w: outcome recovery runtime unavailable", memql.ErrWorkCheckpointRequired)
	}
	input.Messages = closePendingCalls(input.Messages)
	pending := &pendingWorkReply{Version: 1, Result: &TurnResult{FinalText: input.Draft}, Messages: input.Messages, OutcomeFormat: &pendingOutcomeFormat{Input: input, NeedsSources: collect}}
	// The private draft precedes its source barrier. A source write failure can
	// leave recovery stopped, but never authorize repeating the completed App.
	if err := savePendingWorkReply(ctx, store, binding, pending); err != nil {
		return nil, err
	}
	if err := r.saveWorkProgress(ctx, input.Messages); err != nil {
		return nil, err
	}
	return pending, nil
}

const outcomeFormatSchema = `{"type":"object","additionalProperties":false,"required":["status","response","question"],"properties":{"status":{"type":"string","enum":["complete","needs_input"]},"response":{"type":"string","maxLength":24000},"question":{"anyOf":[{"type":"null"},{"type":"object","additionalProperties":false,"required":["text","kind"],"properties":{"text":{"type":"string","minLength":1,"maxLength":1200},"kind":{"type":"string","enum":["text"]}}}]}}}`

func (r *Replier) repairOutcomeDraft(ctx context.Context, store workReplyReviewStore, binding memql.WorkReplyReviewBinding, pending *pendingWorkReply, held *replyReviewSink) error {
	f := pending.OutcomeFormat
	if f != nil && f.Question {
		return fmt.Errorf("outcome recovery is waiting for its recorded question")
	}
	if f == nil || f.Done {
		return nil
	}
	if f.Attempts != 0 {
		return fmt.Errorf("work_outcome_format_exhausted; saved result retained, production cannot be replayed")
	}
	runtime, ok := r.engine.(outcomeRecoveryRuntime)
	if !ok {
		return fmt.Errorf("%w: outcome recovery runtime unavailable", memql.ErrWorkCheckpointRequired)
	}
	if f.NeedsSources {
		sources, err := runtime.WorkDelegatedOutcomeSource(ctx, f.Input.SessionID, f.Input.Messages)
		if err != nil {
			return err
		}
		pending.Messages, f.Input.Messages = sources, sources
		f.NeedsSources = false
		if err := savePendingWorkReply(ctx, store, binding, pending); err != nil {
			return err
		}
		if err := r.saveWorkProgress(ctx, sources); err != nil {
			return err
		}
	}
	choice, err := workflowhost.Run(ctx, "agentWorkOutcomeRecovery", map[string]any{"attempts": f.Attempts}, workflowhost.Options{})
	if err != nil {
		return err
	}
	if allow, ok := choice.(bool); !ok || !allow {
		return fmt.Errorf("outcome format recovery disabled; completed production retained")
	}
	// Persist the attempt BEFORE inference. A crash or another replica cannot
	// reset the bound. Failures retain the stage; no operational retry follows.
	f.Attempts = 1
	if err := savePendingWorkReply(ctx, store, binding, pending); err != nil {
		return err
	}
	raw, err := runtime.FormatWorkOutcome(ctx, f.Input, json.RawMessage(outcomeFormatSchema))
	if err != nil {
		return fmt.Errorf("outcome format recovery: %w", err)
	}
	calls, err := normalizeWorkOutcome([]common.ToolCall{{ID: "recovered-app-response", Name: RespondToUserToolName, Arguments: raw}}, "")
	if err != nil || len(calls) != 1 {
		return fmt.Errorf("invalid format-only outcome: %v", err)
	}
	current, err := store.ReadWorkReplyReview(ctx)
	if err != nil || current.Binding != binding {
		return fmt.Errorf("outcome recovery execution or guidance changed: %v", err)
	}
	call := calls[0]
	if call.Name == "requestUserFeedback" {
		// Only this already-supported control-plane effect is possible. The
		// formatter never receives a tool, shell, workspace or credential scope.
		run, _ := common.RunFromContext(ctx)
		turn := turnContext{AgentId: f.Input.AgentID, OwnerUserId: run.OwnerUserId, RunId: run.RunId, IsWorkExecution: true}
		if turn.AgentId == "" || f.Input.AgentRole == "" {
			return fmt.Errorf("outcome question lost its acting agent")
		}
		callCtx := memql.WithActingAgentRole(ctx, f.Input.AgentRole)
		f.Question, f.Done = true, true
		if err := savePendingWorkReply(ctx, store, binding, pending); err != nil {
			return err
		}
		messages := append(append([]common.ChatMessage(nil), pending.Messages...), common.ChatMessage{Role: "assistant", ToolCalls: calls})
		if err := r.saveBeforeQuestion(callCtx, call.Name, messages); err != nil {
			return err
		}
		args := parseToolArgs(call.Arguments)
		injectAgentContext(call.Name, args, turn)
		if err := r.prepareWorkTool(callCtx); err != nil {
			return err
		}
		held.ToolCall(call.ID, call.Name, call.Arguments)
		result, err := r.stamper.ExecuteToolByName(agentToolCallContext(callCtx, call.Name, turn), call.Name, args)
		if err != nil {
			return err
		}
		held.ToolResult(call.ID, result, "")
		if wait := feedbackWait(call.Name, result); wait != nil {
			return wait
		}
		return fmt.Errorf("outcome question did not create a native wait")
	}
	if call.Name != RespondToUserToolName {
		return fmt.Errorf("format recovery cannot dispatch operational tools")
	}
	envelope, err := ParseEnvelope(call.Arguments)
	if err != nil {
		return err
	}
	pending.Result = &TurnResult{FinalText: envelope.Response, Citations: envelope.Citations, TextChunks: 1}
	pending.TerminalID, pending.TerminalArguments = call.ID, call.Arguments
	pending.Messages = closePendingCalls(append(append([]common.ChatMessage(nil), pending.Messages...), common.ChatMessage{Role: "assistant", ToolCalls: calls}))
	f.Done = true
	if err := savePendingWorkReply(ctx, store, binding, pending); err != nil {
		return err
	}
	if err := r.saveWorkProgress(ctx, pending.Messages); err != nil {
		return err
	}
	held.terminalID, held.terminalArguments = call.ID, call.Arguments
	return nil
}
