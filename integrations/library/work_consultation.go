package library

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/common"
)

// This gateway opens only the existing reviewed-lesson reader. It accepts no
// construct, source contents, owner or run from the caller, and never returns
// an elevated context. The reader still revalidates owned sources and review
// state. Direct model access to arbitrary internal reads remains unavailable.
func (i *Integration) handleConsultWorkTraining(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	text := strings.TrimSpace(asString(args["text"]))
	if text == "" || len(text) > 4200 {
		return nil, fmt.Errorf("consultation requires bounded task-specific query text")
	}
	rc, ok := common.RunFromContext(ctx)
	if !ok || !rc.IsRun() || rc.StepKey == "" || (rc.Mode != common.RunModeLive && rc.Mode != common.RunModeFork) || (rc.Mode == common.RunModeFork && rc.BeforeForkPoint(rc.StepKey)) {
		return nil, fmt.Errorf("consultation requires a live owned work step; replay uses recorded results")
	}
	// Admission precedes retrieval even when the corpus is empty. A parked or
	// cancelled run cannot spend a new query embedding through this gateway.
	runs, err := i.lessonRows(ctx, "workRunForOwner", map[string]any{"runId": rc.RunId})
	if err != nil {
		return nil, err
	}
	if len(runs) != 1 || runs[0]["status"] != "running" || runs[0]["cancelRequested"] == true {
		return nil, fmt.Errorf("consultation requires the current running work")
	}
	steps, err := i.lessonRows(ctx, "workStepForOwnerRun", map[string]any{"runId": rc.RunId, "stepKey": rc.StepKey})
	if err != nil {
		return nil, err
	}
	if len(steps) != 1 || steps[0]["status"] != "running" {
		return nil, fmt.Errorf("consultation requires the current running step")
	}
	var receiptID, status string
	// Only this fixed native read receives internal origin; identity, model
	// route, spending limits and cancellation are inherited unchanged. Do not
	// index or train on every consultation: use existing indexed material and
	// disclose unavailable indexing through the returned receipt instead.
	passages, err := i.readOwnedTrainingLessons(auth.ContextWithInternalOrigin(ctx), map[string]any{
		"text": text, "limit": 3, "minimumSimilarity": 0.60, "maxTokens": 1200,
	}, func(id string, receipt trainingSelectionReceipt) { receiptID, status = id, receipt.Status })
	if err != nil {
		return nil, err
	}
	if receiptID == "" {
		return nil, fmt.Errorf("consultation selection was not recorded")
	}
	texts := make([]map[string]any, 0, len(passages))
	for _, passage := range passages {
		var value map[string]any
		if err := json.Unmarshal(passage.Payload, &value); err != nil {
			return nil, err
		}
		texts = append(texts, value)
	}
	return reviewResult(map[string]any{"receiptId": receiptID, "status": status, "passages": texts})
}
