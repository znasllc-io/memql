package memql

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/common"
)

// ReadCompletedWorkOutcomeFormat repairs the gap between a completed model
// receipt and the reply-stage write. It never asks a provider or reads an App
// transcript. The same-generation attempt barrier and exact retained input
// exclude earlier executions; multiple later receipts fail closed.
func (e *MemQLEngine) ReadCompletedWorkOutcomeFormat(ctx context.Context, input WorkDelegatedOutcome) (string, error) {
	state, err := e.ReadWorkReplyReview(ctx)
	if err != nil {
		return "", err
	}
	var pending struct {
		OutcomeFormat *struct {
			Input        WorkDelegatedOutcome `json:"input"`
			Attempts     int                  `json:"attempts"`
			Done         bool                 `json:"done"`
			NeedsSources bool                 `json:"needsSources"`
		} `json:"outcomeFormat"`
	}
	if state.RecordedAt.IsZero() || json.Unmarshal(state.Pending, &pending) != nil || pending.OutcomeFormat == nil || pending.OutcomeFormat.Attempts != 1 || pending.OutcomeFormat.Done || pending.OutcomeFormat.NeedsSources {
		return "", fmt.Errorf("completed format receipt requires its retained consumed attempt")
	}
	want, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	got, err := json.Marshal(pending.OutcomeFormat.Input)
	if err != nil || !bytes.Equal(want, got) || input.SessionID == "" {
		return "", fmt.Errorf("completed format receipt input changed")
	}
	run, _ := common.RunFromContext(ctx)
	// Old model receipts do not record the execution generation. Only the
	// step's first execution can use this migration: a later version could
	// otherwise mistake a late earlier-generation response for this attempt.
	stepCall, err := parser.RenderCall("workStepForOwnerRun", map[string]any{"runId": run.RunId, "stepKey": run.StepKey})
	if err != nil {
		return "", err
	}
	stepResult, err := e.Execute(ContextWithFreshRead(ctx), "query "+stepCall)
	if err != nil {
		return "", err
	}
	steps := MaterializeRows(stepResult.OutputPayload())
	if len(steps) != 1 || steps[0]["version"] != float64(1) {
		return "", fmt.Errorf("completed format receipt recovery requires the initial step execution")
	}
	call, err := parser.RenderCall("workOutcomeFormatReceiptsForOwnerStep", map[string]any{"runId": run.RunId, "stepKey": run.StepKey, "attemptedAt": state.RecordedAt})
	if err != nil {
		return "", err
	}
	result, err := e.Execute(ContextWithFreshRead(ctx), "query "+call)
	if err != nil {
		return "", err
	}
	rows := MaterializeRows(result.OutputPayload())
	if len(rows) != 1 {
		return "", fmt.Errorf("completed format receipt missing or ambiguous")
	}
	row := rows[0]
	response, _ := row["response"].(map[string]any)
	text := stringArg(response, "text")
	at, valid := askTimestamp(row["createdAt"])
	if !valid || at.Before(state.RecordedAt) || row["promptRef"] != "workOutcomeFormat" ||
		BareShortId(stringArg(row, "runId")) != BareShortId(run.RunId) || row["stepKey"] != run.StepKey ||
		BareShortId(stringArg(row, "ownerUserId")) != BareShortId(run.OwnerUserId) ||
		stringArg(row, "error") != "" || len(stringArg(row, "requestHash")) != 64 || !json.Valid([]byte(text)) {
		return "", fmt.Errorf("completed format receipt is not a successful owned-step result")
	}
	return text, nil
}
