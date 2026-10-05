package pipelinesteps

import (
	"context"
	"strings"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

// AcknowledgeReceipt is called only after a durable journal receipt exists.
// Until then the Job annotation is the replacement driver's recoverable result.
// The identity is reconstructed from the request, never an arbitrary result's
// jobName. The existing bounded retry and TTL cover failed cleanup.
func (e *Executor) AcknowledgeReceipt(ctx context.Context, req pl.StepRequest) {
	if req.Step.Kind != pl.StepCommand || req.Step.RequiresFleet() || e.fwd == nil ||
		strings.TrimSpace(req.RunID) == "" || strings.TrimSpace(req.StepKey) == "" || req.Attempt < 1 {
		return
	}
	run := StepRun{RunID: req.RunID, StepKey: req.StepKey, Attempt: req.Attempt}
	e.ack(ctx, run, JobName(run.RunID, run.StepKey, run.Attempt))
}

var _ pl.ReceiptAcknowledger = (*Executor)(nil)
