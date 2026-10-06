//go:build agent

package pipelinesteps

import (
	"context"
	"time"

	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	pl "github.com/znasllc-io/memql/component/pipelines"
	worker "github.com/znasllc-io/memql/integrations/agent/worker"
)

// Capacity is admission, not an execution retry. Only a dispatcher's confirmed
// busy-before-start result may wait and re-route. A transport error, absent
// worker, policy withdrawal or uncertain execution returns immediately. Each
// re-route repeats current routing, consent and receiving-replica checks.
func (f *Fleet) dispatchWhenCapacityAvailable(ctx context.Context, req pl.StepRequest, run *StepRun, token string, capture *Capture, chunk func(*nodev1.WorkerForwardStream)) (worker.Result, *pl.StepResult, error) {
	deadline, deadlineErr := time.Parse(time.RFC3339Nano, run.RunDeadline)
	ownTimeout := run.TimeoutSeconds
	tokenAt := f.now()
	delay := f.capacityRetry
	if delay <= 0 {
		delay = 5 * time.Second
	}
	maxDelay := 6 * delay
	queued := false
	for {
		if ctx.Err() != nil {
			res := stoppedResult(ctx)
			return worker.Result{}, &res, nil
		}
		if deadlineErr == nil {
			remaining := deadline.Sub(f.now())
			if remaining < time.Second {
				res := failedResult(pl.CodeRunCeiling, "The run reached its ceiling while waiting for fleet capacity. No command was started.")
				return worker.Result{}, &res, nil
			}
			if remaining < time.Duration(ownTimeout)*time.Second {
				run.TimeoutSeconds = int(remaining / time.Second)
				run.DeadlineCode = pl.CodeRunCeiling
			}
		}
		// A long queue must not hand a new worker a stale installation token.
		// Keep every minted token masked, including an older queued refusal.
		if queued && run.InstallationID != 0 && f.now().Sub(tokenAt) >= tokenRefreshAge {
			fresh, refusal, ok := f.cloneToken(ctx, *run)
			if !ok {
				return worker.Result{}, &refusal, nil
			}
			token, tokenAt = fresh, f.now()
			capture.AddSecrets(secretValues(*run, token)...)
		}
		result, err := f.dispatchStep(ctx, req, *run, token, chunk)
		if err != nil || result.OK || !result.RefusedBeforeStart || result.RefusedByGate || result.ErrorCode != "pipeline_capacity_busy" {
			return result, nil, err
		}
		if deadlineErr != nil {
			res := failedResult(pl.CodeExecutorError, "Fleet capacity is busy and the run has no readable ceiling; no unbounded wait or replacement command was started.")
			return worker.Result{}, &res, nil
		}
		if !queued {
			capture.Note("memql: waiting for the owner's shared build capacity; no command has started")
			queued = true
		}
		wait := delay
		if remaining := deadline.Sub(f.now()); remaining < wait {
			wait = remaining
		}
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			res := stoppedResult(ctx)
			return worker.Result{}, &res, nil
		case <-timer.C:
		}
		if delay < maxDelay {
			delay = min(2*delay, maxDelay)
		}
	}
}
