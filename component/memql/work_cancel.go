package memql

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// WorkCancelledError distinguishes the person's Stop from a failed model.
// Model calls are safe interruption points; writes finish and journal their
// receipt. The automation executor still checks ordinary step boundaries.
type WorkCancelledError struct{ By string }

func (e *WorkCancelledError) Error() string { return "cancelled: the person asked this work to stop" }

func (e *MemQLEngine) workCancellation(ctx context.Context) error {
	run, ok := common.RunFromContext(ctx)
	if !ok || run.RunId == "" {
		return nil
	}
	call, err := parser.RenderCall("workRunForOwner", map[string]any{"runId": run.RunId})
	if err != nil {
		return err
	}
	result, err := e.Execute(ctx, "query "+call)
	if err != nil {
		return err
	}
	rows := MaterializeRows(result.OutputPayload())
	if len(rows) != 1 {
		return fmt.Errorf("work run is no longer available")
	}
	if asked, _ := rows[0]["cancelRequested"].(bool); asked {
		by, _ := rows[0]["cancelledBy"].(string)
		return &WorkCancelledError{By: by}
	}
	return nil
}

// modelCancellation watches the shared journal only while a model call is
// in flight. It works on a receiving replica with no originating session or
// local waiter. Completed calls and the step context both release the watcher.
func (e *MemQLEngine) modelCancellation(ctx context.Context, cancel context.CancelCauseFunc) func(airoute.CallObservation) {
	var mu sync.Mutex
	active := map[string]context.CancelFunc{}
	return func(call airoute.CallObservation) {
		mu.Lock()
		if stop := active[call.ID]; stop != nil {
			stop()
			delete(active, call.ID)
		}
		if call.Phase != "running" {
			mu.Unlock()
			return
		}
		watchCtx, stop := context.WithCancel(ctx)
		active[call.ID] = stop
		mu.Unlock()
		if err := e.workCancellation(watchCtx); err != nil && watchCtx.Err() == nil {
			stop()
			cancel(err)
			return
		}
		go func() {
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-watchCtx.Done():
					return
				case <-ticker.C:
					if err := e.workCancellation(watchCtx); err != nil && watchCtx.Err() == nil {
						cancel(err)
						return
					}
				}
			}
		}()
	}
}
