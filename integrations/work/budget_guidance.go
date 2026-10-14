package work

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	workstate "github.com/znasllc-io/memql/component/work"
)

// Saving guidance is not a decision. Keep the exhausted run and its approval
// parked, including their usage, execution identity and completed receipts.
// The caller holds the approval receipt gate and has freshly validated the
// owner, exact pending wait, admitted step and artifact hash.
func (i *Integration) saveBudgetGuidance(ctx context.Context, approval map[string]any, text string, now time.Time) ([]memorynodes.MemoryNode, error) {
	if err := verifyApprovalDecisionWrite(ctx); err != nil {
		return nil, err
	}
	answer := rowMap(approval, "answer")
	if len(answer) != 1 || rowString(answer, "feedback") != text {
		if err := i.store().writeInternal(ctx, "mutation "+call("saveWorkApprovalGuidance", map[string]any{
			"approvalId": rowString(approval, "id"), "answer": map[string]any{"feedback": text},
			"versionTime": rfc(workRowVersionAfter(approval["createdAt"], now)),
		})); err != nil {
			return nil, err
		}
	}
	return i.resultNode(map[string]any{
		"approvalId": rowString(approval, "id"), "runId": rowString(approval, "runId"),
		"decision": "", "runResumed": false, "guidanceSaved": true, "savedGuidance": text,
	}), nil
}

// Only an explicit later budget approval can release saved guidance to the
// native checkpoint. Read it from the locked durable row, never a browser's
// cached discussion, and copy it beside the newly approved total limit.
func budgetAnswerWithSavedGuidance(approval, answer map[string]any) (map[string]any, error) {
	saved := rowMap(approval, "answer")
	if len(saved) == 0 {
		return answer, nil
	}
	text, valid := saved["feedback"].(string)
	if len(saved) != 1 || !valid || text == "" || text != strings.TrimSpace(text) || utf8.RuneCountInString(text) > maxApprovalGuidanceCharacters ||
		!workstate.ApprovalAcceptsGuidance(workstate.ApprovalKindBudget, rowMap(approval, "subject"), approval["options"]) {
		return nil, fmt.Errorf("work: this budget request has invalid saved guidance")
	}
	merged := make(map[string]any, len(answer)+1)
	for key, value := range answer {
		merged[key] = value
	}
	merged["feedback"] = text
	return merged, nil
}
