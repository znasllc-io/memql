package memql

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

// WorkDelegatedOutcome is retained protocol input, not evidence of completion.
// Messages are the native handover plus public receipts, never a transcript.
type WorkDelegatedOutcome struct {
	SessionID string               `json:"sessionId"`
	Draft     string               `json:"draft"`
	Calls     []common.ToolCall    `json:"calls,omitempty"`
	Messages  []common.ChatMessage `json:"messages"`
	AgentID   string               `json:"agentId"`
	AgentRole string               `json:"agentRole"`
}

// ReadLegacyWorkOutcome only upgrades the old, exact native failure. An ordinary
// new turn, explicit rerun (which clears that failure), or a different execution
// cannot borrow a prior App. Missing bindings fail closed rather than producing
// another operational session. No transcript is consulted to invent a draft.
func (e *MemQLEngine) ReadLegacyWorkOutcome(ctx context.Context) (*WorkDelegatedOutcome, error) {
	if _, err := e.workReplyReviewBinding(ctx); err != nil {
		return nil, err
	}
	run, _ := common.RunFromContext(ctx)
	rows, err := e.workRows(ContextWithFreshRead(ctx), "workRunForOwner", run.RunId)
	if err != nil || len(rows) != 1 {
		return nil, fmt.Errorf("outcome recovery run unavailable: %v", err)
	}
	message := stringArg(rows[0], "errorMessage")
	if !work.IsOutcomeRecoveryFailure(message) {
		return nil, nil
	}
	legacy := work.IsLegacyOutcomeFailure(message)
	call, err := parser.RenderCall("appSessionsForStep", map[string]any{"stepId": run.StepKey})
	if err != nil {
		return nil, err
	}
	cursor := ""
	for range 4 {
		result, err := e.Execute(ContextWithCursor(ContextWithFreshRead(ctx), cursor), "query "+call)
		if err != nil {
			return nil, err
		}
		for _, row := range MaterializeRows(result.OutputPayload()) {
			if BareShortId(stringArg(row, "runId")) != BareShortId(run.RunId) {
				continue
			}
			var scope common.DelegatedTools
			raw, err := json.Marshal(row["toolScope"])
			if err != nil || json.Unmarshal(raw, &scope) != nil {
				return nil, fmt.Errorf("outcome recovery scope unreadable")
			}
			if len(scope.Tools) == 0 {
				continue
			} // Inference helpers are not production.
			if !legacy {
				return nil, fmt.Errorf("outcome recovery checkpoint missing; production remains stopped")
			}
			if row["status"] != "ended" || row["exitCode"] != float64(0) ||
				BareShortId(scope.Run.RunId) != BareShortId(run.RunId) || BareShortId(scope.Run.GoalId) != BareShortId(run.GoalId) ||
				BareShortId(scope.Run.OwnerUserId) != BareShortId(run.OwnerUserId) || scope.Run.StepKey != run.StepKey ||
				scope.Run.RerunRequestId != run.RerunRequestId || !common.SameStepOverride(scope.Run.Override, run.Override) ||
				scope.AgentID == "" || scope.AgentRole == "" || len(scope.Messages) == 0 {
				return nil, fmt.Errorf("outcome recovery completed App binding unavailable")
			}
			started, a := askTimestamp(row["startedAt"])
			ended, b := askTimestamp(row["endedAt"])
			if !a || !b || ended.Before(started) {
				return nil, fmt.Errorf("outcome recovery App interval unavailable")
			}
			// A scope/question pause is a different protocol. Its saved answer must
			// resume normally; an empty App result cannot turn it into completion.
			if approvalID := stringArg(rows[0], "humanResumeId"); approvalID != "" {
				approval, _, err := e.approvalRows(ctx, approvalID)
				if err != nil {
					return nil, err
				}
				if at, ok := askTimestamp(approval["requestedAt"]); ok && !at.Before(started) && !at.After(ended) {
					return nil, fmt.Errorf("outcome recovery App ended for a native question")
				}
			}
			id := stringArg(row, "id")
			messages, err := e.WorkDelegatedOutcomeSource(ctx, id, scope.Messages)
			if err != nil {
				return nil, err
			}
			resultRaw, err := json.Marshal(row["result"])
			if err != nil {
				return nil, err
			}
			return &WorkDelegatedOutcome{SessionID: id, Draft: string(resultRaw), Calls: sessionTerminalCalls(AppSessionOutcome{Result: resultRaw}), Messages: messages, AgentID: scope.AgentID, AgentRole: scope.AgentRole}, nil
		}
		cursor = ""
		if meta := result.GetMeta(); meta != nil {
			cursor = meta.Cursor
		}
		if cursor == "" {
			// A run-level error can survive completion of its recovered step.
			// An exhaustive owned-step scan with no operational App proves this
			// step has nothing to replay; it must not inherit another step's stop.
			return nil, nil
		}
	}
	return nil, fmt.Errorf("outcome recovery completed App missing within scan bound; production remains stopped")
}

// FormatWorkOutcome is one inference-only conversion. Routing retains explicit
// model/effort and all admission gates, while old whole-prompt overrides cannot
// replace the protocol. The Apps structured door supplies an empty tool scope.
func (e *MemQLEngine) FormatWorkOutcome(ctx context.Context, input WorkDelegatedOutcome, schema json.RawMessage) (string, error) {
	if _, err := e.workReplyReviewBinding(ctx); err != nil {
		return "", err
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	direction, err := e.reviewGuidanceContext(ctx, "The native request and original return are retained in the saved protocol input.")
	if err != nil {
		return "", err
	}
	return e.InvokeAIStructured(replyReviewRoutingContext(ContextWithWorkReplyReviewSource(ctx, input.Messages)), "workOutcomeFormat", map[string]any{"saved": string(raw), "direction": direction}, "work_outcome_format", schema, true)
}
