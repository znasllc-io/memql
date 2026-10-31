package memql

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/common"
)

type workReplanSourcesKey struct{}

func ContextWithWorkReplanSources(ctx context.Context, sources string) context.Context {
	return context.WithValue(ctx, workReplanSourcesKey{}, sources)
}
func WorkReplanSources(ctx context.Context) string {
	value, _ := ctx.Value(workReplanSourcesKey{}).(string)
	return value
}

// ReadWorkStepSources projects exact retained tool exchanges, never an App's
// private transcript or claimed result. Native replanning uses the failed step;
// resumed production uses the same run's receipts independently of the generated
// replacement prompt. Bounded omissions remain explicit reconciliation gaps.
func (e *MemQLEngine) ReadWorkStepSources(ctx context.Context, runID, stepKey string) (map[string]any, error) {
	actor, _ := auth.AccessFromContext(ctx)
	if actor == nil || actor.UserId == "" || runID == "" {
		return nil, fmt.Errorf("operation sources require an owned run")
	}
	ctx = ContextWithFreshRead(ctx)
	runs, err := e.workRows(ctx, "workRunForOwner", runID)
	if err != nil || len(runs) != 1 || BareShortId(stringArg(runs[0], "ownerUserId")) != BareShortId(actor.UserId) {
		return nil, fmt.Errorf("operation source run unavailable: %v", err)
	}
	rows, omitted, err := e.workReplySourceReceipts(ctx, runID)
	if err != nil {
		return nil, err
	}
	selected := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if BareShortId(stringArg(row, "runId")) != BareShortId(runID) || BareShortId(stringArg(row, "ownerUserId")) != BareShortId(actor.UserId) {
			return nil, fmt.Errorf("operation source attribution mismatch")
		}
		key := stringArg(row, "stepKey")
		if key == "" {
			omitted = true
			continue
		}
		if stepKey == "" || key == stepKey {
			selected = append(selected, row)
		}
	}
	remaining := replyReviewMaxBytes
	sources, previewOmitted := e.workCheckpointSources(ctx, selected, runID, &remaining)
	return map[string]any{"runId": BareShortId(runID), "stepKey": stepKey, "recordedSources": sources, "sourcesOmitted": omitted || previewOmitted, "reconciliationRequired": omitted || previewOmitted || len(sources) == 0}, nil
}

// A replan gets a new execution binding. Do not restore an old conversation as
// that new task, but do carry its tool receipts across the boundary. Read fresh
// after an owner-scoped run lookup; no replan means no source scan or extra call.
func (e *MemQLEngine) ReadWorkReplanSources(ctx context.Context) (string, error) {
	run, ok := common.RunFromContext(ctx)
	actor, _ := auth.AccessFromContext(ctx)
	if !ok || !run.IsRun() || actor == nil || BareShortId(actor.UserId) != BareShortId(run.OwnerUserId) {
		return "", fmt.Errorf("replan source context requires owned work")
	}
	rows, err := e.workRows(ContextWithFreshRead(ctx), "workRunForOwner", run.RunId)
	if err != nil || len(rows) != 1 {
		return "", fmt.Errorf("replan source run unavailable: %v", err)
	}
	outcome, _ := rows[0]["outcome"].(map[string]any)
	if stringArg(outcome, "replannedFrom") == "" {
		return "", nil
	}
	sources, err := e.ReadWorkStepSources(ctx, run.RunId, "")
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(sources)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
