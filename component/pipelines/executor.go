package pipelines

import (
	"context"
	"sync"
)

// Executor runs compiled steps. The substrate registers one on each agent
// node (epic memql#5478): it routes a step naming a need to the fleet and
// forwards every other step to the workbench node's Job runner.
//
// With no executor registered a step fails pipeline_runner_unavailable. That
// is deliberate rather than a gap papered over: the design record rules out
// running a pull request's code in the workbench's shared directory, so until
// a runner exists the honest answer is a typed failure the check run shows.
type Executor interface {
	// Execute runs one step and answers how it ended. An error means the
	// executor could not report an outcome at all; the driver fails the step
	// pipeline_executor_error and does not retry, because a runner that
	// re-attaches by (runId, stepKey, attempt) is the retry.
	Execute(ctx context.Context, req StepRequest) (StepResult, error)
	// Cancel stops every step of a run still in flight. It must be safe to
	// call for a run with nothing running.
	Cancel(ctx context.Context, runID string) error
}

// ReceiptAcknowledger releases an executor's recoverable result/resources after
// the caller has durably committed the step receipt. Returning from Execute is
// not that proof. Calls are idempotent and may be repeated by a replacement
// driver reading a committed receipt. Nil confirms cleanup, including resource
// absence; an accepted asynchronous delete is not confirmation. Failure must
// leave the run recoverable without repeating the work.
type ReceiptAcknowledger interface {
	AcknowledgeReceipt(ctx context.Context, req StepRequest) error
}

var (
	executorMu sync.RWMutex
	executor   Executor
)

// RegisterExecutor installs the node's executor and returns the one it
// replaced, so a test can restore it. A nil executor uninstalls.
func RegisterExecutor(e Executor) (previous Executor) {
	executorMu.Lock()
	defer executorMu.Unlock()
	previous = executor
	executor = e
	return previous
}

// CurrentExecutor is the node's executor, or nil when none is registered.
func CurrentExecutor() Executor {
	executorMu.RLock()
	defer executorMu.RUnlock()
	return executor
}
