package memql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

// RecoverWorkModelCall is native-only: the tool loop calls it after a failed
// provider response or a capacity refusal during proactive compaction, before
// dispatching any proposed tool. It cannot make an
// uncertain tool effect retryable. The installed recipe chooses retry or ask.
func (e *MemQLEngine) RecoverWorkModelCall(ctx context.Context, failure string, attempt, maxRetries int, messages []common.ChatMessage) (retry bool, err error) {
	if source, ok := ctx.Value(replyReviewSourceKey{}).([]common.ChatMessage); ok {
		messages = append([]common.ChatMessage(nil), source...)
	}
	defer func() {
		var wait *work.HumanWait
		if err != nil && !errors.As(err, &wait) {
			err = fmt.Errorf("%w: model recovery: %v", ErrWorkCheckpointRequired, err)
		}
	}()
	run, ok := common.RunFromContext(ctx)
	ac, _ := auth.AccessFromContext(ctx)
	capacity := work.IsContextCapacityFailure(failure)
	if !ok || ac == nil || run.GoalId == "" || run.OwnerUserId == "" || run.StepKey == "" ||
		BareShortId(ac.UserId) != BareShortId(run.OwnerUserId) || run.Mode == common.RunModeReplay ||
		(!work.IsModelCallCeiling(failure) && !capacity) || len(messages) == 0 || attempt < 0 || maxRetries < 0 || maxRetries > 3 || (capacity && maxRetries != 0) {
		return false, fmt.Errorf("model recovery requires the unchanged owned execution and a provider failure")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	// A provider call can outlive a peer cancellation or explicit rerun. A new
	// inference attempt must not borrow its stale execution context.
	fresh := ContextWithFreshRead(ctx)
	runs, err := e.workRows(fresh, "workRunForOwner", run.RunId)
	if err != nil {
		return false, err
	}
	if len(runs) != 1 || runs[0]["cancelRequested"] == true || stringField(runs[0], "status") != "running" ||
		BareShortId(stringField(runs[0], "goalId")) != BareShortId(run.GoalId) {
		return false, fmt.Errorf("model recovery run is no longer active")
	}
	rerun, _ := runs[0]["rerun"].(map[string]any)
	if stringField(rerun, "requestId") != run.RerunRequestId {
		return false, fmt.Errorf("model recovery execution request changed")
	}
	goals, err := e.memoryCall(fresh, "query", "work.workGoalForOwner", map[string]any{"goalId": run.GoalId})
	if err != nil {
		return false, err
	}
	if len(goals) != 1 || stringField(goals[0], "status") == "closed" {
		return false, fmt.Errorf("model recovery goal is no longer active")
	}
	failureKind := "model_call_ceiling"
	if capacity {
		// Compaction has already exhausted its bounded recovery. Identical
		// context is never automatically resent, even under a custom recipe.
		failureKind = "context_capacity"
	}
	value, err := e.runScopedWorkflow(ctx, "workModelCallRecovery", map[string]any{"retriesSpent": attempt, "maxRetries": maxRetries, "failureKind": failureKind}, nil)
	if err != nil {
		return false, err
	}
	decision, ok := value.(map[string]any)
	if !ok {
		return false, fmt.Errorf("invalid model recovery decision")
	}
	switch decision["act"] {
	case "retry":
		if attempt < maxRetries {
			return true, nil
		}
		return false, fmt.Errorf("model recovery exceeded retry allowance")
	case "ask":
	default:
		return false, fmt.Errorf("unsupported model recovery decision")
	}
	question, _ := decision["question"].(string)
	if strings.TrimSpace(question) == "" || len(question) > 8000 {
		return false, fmt.Errorf("model recovery needs a bounded question")
	}
	writer, ok := e.IntegrationByName("work").(interface {
		PauseWorkForModelCall(context.Context, string, string, string) (string, error)
	})
	if !ok {
		return false, fmt.Errorf("model checkpoint coordination is unavailable")
	}
	// Only receipt work detaches; cancellation cannot dispatch another model or tool.
	commit, stop := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer stop()
	if err := e.SaveWorkContinuation(commit, messages); err != nil {
		return false, err
	}
	raw, err := json.Marshal(messages)
	if err != nil {
		return false, err
	}
	approval, err := writer.PauseWorkForModelCall(commit, failure, continuationID(run, raw), question)
	if err != nil {
		return false, err
	}
	if approval == "" {
		return false, fmt.Errorf("model checkpoint returned no question")
	}
	return false, &work.HumanWait{ApprovalID: approval}
}
