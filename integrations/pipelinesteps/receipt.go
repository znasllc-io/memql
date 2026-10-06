package pipelinesteps

import (
	"context"
	"errors"
	"strings"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

// AcknowledgeReceipt is called only after a durable journal receipt exists.
// Until then the Job annotation is the replacement driver's recoverable result.
// The identity is reconstructed from the request, never an arbitrary result's
// jobName. Failed cleanup is returned to the durable driver for recovery.
func (e *Executor) AcknowledgeReceipt(ctx context.Context, req pl.StepRequest) error {
	if req.Step.Kind != pl.StepCommand || req.Step.RequiresFleet() || req.Step.Skip != nil {
		return nil
	}
	if strings.TrimSpace(req.RunID) == "" || strings.TrimSpace(req.StepKey) == "" || req.Attempt < 1 {
		return errors.New("pipelinesteps: cleanup requires the recorded run, step and attempt")
	}
	if e.fwd == nil {
		return errors.New("pipelinesteps: no workbench route to confirm cleanup")
	}
	run := StepRun{RunID: req.RunID, StepKey: req.StepKey, Attempt: req.Attempt}
	return e.ack(ctx, run, JobName(run.RunID, run.StepKey, run.Attempt))
}

var _ pl.ReceiptAcknowledger = (*Executor)(nil)
