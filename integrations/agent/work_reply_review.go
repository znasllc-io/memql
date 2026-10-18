package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// Activity and tool receipts remain live. Proposed answer text and its terminal
// envelope are held until the same owned run's evidence check completes.
type replyReviewSink struct {
	DeltaSink
	terminalID, terminalArguments string
}

func (*replyReviewSink) TextDelta(string) {}
func (s *replyReviewSink) ToolCall(id, name, arguments string) {
	if name == RespondToUserToolName {
		s.terminalID, s.terminalArguments = id, arguments
		return
	}
	s.DeltaSink.ToolCall(id, name, arguments)
}

func (r *Replier) handleReviewedWork(ctx context.Context, msg *memqlv1.AgentGenerateTurnMsg, sink DeltaSink) (*TurnResult, error) {
	return r.runReviewedWork(ctx, sink, func(callCtx context.Context, held *replyReviewSink) (*TurnResult, error) {
		return r.handleTurn(callCtx, msg, held)
	})
}

func (r *Replier) runReviewedWork(ctx context.Context, sink DeltaSink, produce func(context.Context, *replyReviewSink) (*TurnResult, error)) (*TurnResult, error) {
	// The native ceiling is one continuation. The installed recipe decides
	// whether to use it, and the original run retains every spend/deadline gate.
	for attempt := 0; attempt < 2; attempt++ {
		held := &replyReviewSink{DeltaSink: sink}
		result, err := produce(ctx, held)
		if err != nil {
			return nil, err
		}
		if result == nil || result.Paused || strings.TrimSpace(result.FinalText) == "" {
			return result, nil
		}
		// A successful turn has consumed any exact resume proof and may have
		// saved newer receipts. Subsequent reads/continuation use that latest
		// checkpoint, retaining owner, step, override, lineage and deadline.
		if run, ok := common.RunFromContext(ctx); ok && run.Continuation != nil {
			run.Continuation = nil
			ctx = common.ContextWithRun(ctx, run)
		}
		messages, err := r.restoreAfterQuestion(ctx, nil)
		if err != nil {
			return nil, err
		}
		state, _, err := recoveryState(messages)
		if err != nil {
			return nil, err
		}
		canContinue := attempt == 0 && len(messages) > 1 && state.EvidenceRepairs == 0
		review, err := r.checkWorkResult(ctx, result.FinalText, canContinue)
		if err != nil {
			return nil, err
		}
		continued, err := r.continueWorkEvidence(ctx, messages, review.MissingEvidence, canContinue)
		if err != nil {
			return nil, err
		}
		if continued {
			continue
		}
		return deliverReviewedWorkResult(result, held, review.Reply)
	}
	return nil, fmt.Errorf("reply evidence continuation exceeded its bound")
}

type workReplyDecision struct{ Reply, MissingEvidence string }

func (r *Replier) continueWorkEvidence(ctx context.Context, messages []common.ChatMessage, gap string, available bool) (bool, error) {
	state, marker, err := recoveryState(messages)
	if err != nil {
		return false, err
	}
	value, err := workflowhost.Run(ctx, "agentWorkReplyCompletion", map[string]any{"facts": map[string]any{
		"continuationAvailable": available, "missingEvidence": gap, "repairs": state.EvidenceRepairs,
	}}, workflowhost.Options{})
	if err != nil {
		return false, err
	}
	proceed, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("reply completion policy must return a boolean")
	}
	if !proceed {
		return false, nil
	}
	// Authored policy cannot reset the durable attempt count or invent a
	// continuation without a bounded gap and an existing source checkpoint.
	if !available || len(messages) < 2 || state.EvidenceRepairs != 0 || strings.TrimSpace(gap) == "" || len(gap) > 1200 {
		return false, fmt.Errorf("reply evidence continuation is unavailable")
	}
	instruction, err := r.engine.RenderPrompt("workEvidenceCompletion", map[string]any{"gap": gap})
	if err != nil {
		return false, err
	}
	state.EvidenceRepairs, state.EvidenceGap = 1, gap
	raw, err := json.Marshal(state)
	if err != nil {
		return false, err
	}
	out := append([]common.ChatMessage(nil), messages...)
	if marker >= 0 {
		out = append(out[:marker], out[marker+1:]...)
	}
	out = append(out, common.ChatMessage{Role: "system", Name: workRecoveryMarker, Content: string(raw)})
	// A new reasoning boundary needs an explicit correction, as with invalid
	// outcome repair. Metadata alone was carried faithfully yet ignored by the
	// live model. This is marked runtime input, never a new human authorization.
	out = append(out, common.ChatMessage{Role: "user", Name: "work_evidence_completion", Content: instruction})
	// Save before any next call. A different replica sees the same bound and
	// the same completed source receipts; it cannot restart the repair allowance.
	if err := r.saveWorkProgress(ctx, out); err != nil {
		return false, err
	}
	return true, nil
}

func (r *Replier) checkWorkResult(ctx context.Context, draft string, canContinue bool) (workReplyDecision, error) {
	call, err := parser.RenderCall("work.workReviewReply", map[string]any{"draft": map[string]any{"reply": draft, "continueLookup": canContinue}})
	if err != nil {
		return workReplyDecision{}, err
	}
	raw, err := r.engine.Execute(ctx, "builtin "+call)
	if err != nil {
		return workReplyDecision{}, err
	}
	rows := memql.MaterializeRows(raw)
	if len(rows) != 1 {
		return workReplyDecision{}, fmt.Errorf("reply evidence review returned no answer")
	}
	reply, ok := rows[0]["reply"].(string)
	if !ok || strings.TrimSpace(reply) == "" {
		return workReplyDecision{}, fmt.Errorf("reply evidence review returned an empty answer")
	}
	gap, _ := rows[0]["missingEvidence"].(string)
	if len(gap) > 1200 {
		return workReplyDecision{}, fmt.Errorf("reply evidence gap exceeds its bound")
	}
	if !canContinue {
		gap = ""
	}
	return workReplyDecision{Reply: reply, MissingEvidence: gap}, nil
}

func deliverReviewedWorkResult(result *TurnResult, held *replyReviewSink, reply string) (*TurnResult, error) {
	if held.terminalID != "" {
		var envelope map[string]any
		if err := json.Unmarshal([]byte(held.terminalArguments), &envelope); err != nil {
			return nil, err
		}
		envelope["response"] = reply
		delete(envelope, "responseFromText")
		encoded, err := json.Marshal(envelope)
		if err != nil {
			return nil, err
		}
		held.DeltaSink.ToolCall(held.terminalID, RespondToUserToolName, string(encoded))
	}
	result.FinalText, result.TextChunks = reply, 1
	held.DeltaSink.TextDelta(reply)
	return result, nil
}
