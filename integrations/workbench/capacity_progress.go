package workbench

import (
	"context"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

// The executor records capacity evidence in the existing work journal. The
// originating browser and agent need no callbacks from this replica's memory.
func (i *Integration) acquireHostCapacity(ctx context.Context, runID, owner string) (func(), error) {
	recorder, observable := i.engine.(interface {
		RecordWorkProgress(context.Context, memql.WorkEvent) error
	})
	if !observable {
		return i.acquireCapacity(ctx, false)
	}
	progressCtx := ctx
	if _, ok := common.RunFromContext(ctx); !ok {
		progressCtx = common.ContextWithRun(ctx, common.RunContext{RunId: runID, OwnerUserId: owner})
	}
	event := memql.WorkEvent{ID: id.NewShortId(), Kind: "action", Name: "Workbench capacity", Phase: "queued", Text: "Waiting for Workbench capacity"}
	waited := false
	release, err := i.acquireCapacityObserved(ctx, false, func() error {
		waited = true
		return recorder.RecordWorkProgress(progressCtx, event)
	})
	if waited {
		event.Phase, event.Text = "completed", "Workbench capacity available"
		if err != nil {
			event.Phase, event.Text, event.Error = "failed", "Workbench queue ended before execution", err.Error()
		}
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(progressCtx), 5*time.Second)
		defer cancel()
		if recordErr := recorder.RecordWorkProgress(recordCtx, event); recordErr != nil && err == nil {
			release()
			return nil, recordErr
		}
	}
	return release, err
}
