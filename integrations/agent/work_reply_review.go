package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
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
	held := &replyReviewSink{DeltaSink: sink}
	result, err := r.handleTurn(ctx, msg, held)
	if err != nil {
		return nil, err
	}
	return r.reviewWorkResult(ctx, result, held)
}

func (r *Replier) reviewWorkResult(ctx context.Context, result *TurnResult, held *replyReviewSink) (*TurnResult, error) {
	if result == nil || result.Paused || strings.TrimSpace(result.FinalText) == "" {
		return result, nil
	}
	call, err := parser.RenderCall("work.workReviewReply", map[string]any{"draft": map[string]any{"reply": result.FinalText}})
	if err != nil {
		return nil, err
	}
	raw, err := r.engine.Execute(ctx, "builtin "+call)
	if err != nil {
		return nil, err
	}
	rows := memql.MaterializeRows(raw)
	if len(rows) != 1 {
		return nil, fmt.Errorf("reply evidence review returned no answer")
	}
	reply, ok := rows[0]["reply"].(string)
	if !ok || strings.TrimSpace(reply) == "" {
		return nil, fmt.Errorf("reply evidence review returned an empty answer")
	}
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
