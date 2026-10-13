package memql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

// ErrWorkCheckpointRequired prevents an exhausted turn from continuing through
// authored retry/on-error policy when its durable suspension could not be saved.
var ErrWorkCheckpointRequired = errors.New("required work checkpoint could not be saved")

// PauseWorkForBudget is called only at a tool-loop boundary, after any started
// tool has returned its receipt. Saving the exact exchange precedes publishing
// the question; the ordinary HumanWait unwind then records every child link.
func (e *MemQLEngine) PauseWorkForBudget(ctx context.Context, ceiling *RunCeilingError, messages []common.ChatMessage) (failure error) {
	defer func() {
		var wait *work.HumanWait
		if failure != nil && !errors.As(failure, &wait) {
			failure = fmt.Errorf("%w: %v", ErrWorkCheckpointRequired, failure)
		}
	}()
	run, ok := common.RunFromContext(ctx)
	ac, _ := auth.AccessFromContext(ctx)
	if !ok || ac == nil || run.GoalId == "" || run.OwnerUserId == "" || run.StepKey == "" || ceiling == nil || len(messages) == 0 ||
		BareShortId(ac.UserId) != BareShortId(run.OwnerUserId) || BareShortId(ceiling.RunId) != BareShortId(run.RunId) ||
		ceiling.StepKey != run.StepKey || !run.Override.Empty() || run.Mode == common.RunModeReplay {
		return fmt.Errorf("budget checkpoint requires the unchanged owned execution")
	}
	writer, ok := e.IntegrationByName("work").(interface {
		PauseWorkForBudget(context.Context, *RunCeilingError, string) (string, error)
	})
	if !ok {
		return fmt.Errorf("budget checkpoint coordination is unavailable")
	}
	// A budget may cancel the active model context. Receipt work alone survives
	// that deadline; this context must never be used for another model or tool.
	commit, stop := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer stop()
	if err := e.SaveWorkContinuation(commit, messages); err != nil {
		return fmt.Errorf("save budget checkpoint: %v", err)
	}
	raw, err := json.Marshal(messages)
	if err != nil {
		return err
	}
	approval, err := writer.PauseWorkForBudget(commit, ceiling, continuationID(run, raw))
	if err != nil {
		return fmt.Errorf("park budget checkpoint: %v", err)
	}
	if approval == "" {
		return fmt.Errorf("budget checkpoint returned no question")
	}
	return &work.HumanWait{ApprovalID: approval}
}
