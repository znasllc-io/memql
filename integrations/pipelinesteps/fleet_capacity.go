//go:build agent

package pipelinesteps

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"

	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	pl "github.com/znasllc-io/memql/component/pipelines"
	worker "github.com/znasllc-io/memql/integrations/agent/worker"
)

// Capacity is admission, not an execution retry. Only a dispatcher's confirmed
// busy-before-start result, retained local-build fence or eligible worker
// awaiting connection may wait and re-route. A transport error, policy
// withdrawal or uncertain execution returns immediately. Each re-route repeats
// current routing, consent and receiving-replica checks.
//
// HOW LONG IT WAITS depends on whose decision the wait is.
//
//   - A fleet step's first attempt waits until its run's ceiling: nothing else
//     can run the step, and the ceiling bounds everything.
//   - A pool step's FIRST attempt waits fleetWaitSeconds while the policy allows
//     cloud, then gives the cluster the chance (the Codex preference deadline,
//     kept whole inside that one attempt).
//   - An attempt the DRIVER placed on the fleet waits run.MaxWaitSeconds when it
//     is positive, and then answers nothing ran, not tried here again
//     (waitRanOut). The driver chose the wait -- the largest retry delay for a
//     machine it pinned, fleetWaitSeconds for a pool step's failover -- and it
//     chooses the cloud, so the pool preference deadline and the pool step's
//     stricter reading of a refusal (below) do not apply to a placed attempt:
//     applying them as well would cut the wait short with the wrong answer, and
//     would turn a refusal the driver can route around (pipelines_not_allowed) into
//     pipeline_executor_error, which it cannot. Zero is no bound beyond the
//     ceiling, as for a fleet step's first attempt.
//
// None of these waits ever cancels or shortens a command that has started: a
// wait is for admission, and a started command is bounded by its own timeout.
func (f *Fleet) dispatchWhenCapacityAvailable(ctx context.Context, req pl.StepRequest, run *StepRun, token string, capture *Capture, chunk func(*nodev1.WorkerForwardStream)) (worker.Result, *pl.StepResult, error) {
	deadline, deadlineErr := time.Parse(time.RFC3339Nano, run.RunDeadline)
	ownTimeout := run.TimeoutSeconds
	tokenAt := f.now()
	// A pool step's PREFERENCE (fleetWaitSeconds, then the cloud) is a first
	// attempt's. An attempt the driver placed on the fleet is bounded by its own
	// wait instead (waitEnds), and takes a refusal as nothing ran.
	pool := req.Step.Placement == pl.PlacementPool && !placedOnFleet(req)
	preferenceEnds := tokenAt
	if pool && req.ComputePlacement != nil {
		preferenceEnds = tokenAt.Add(time.Duration(req.ComputePlacement.FleetWaitSeconds) * time.Second)
	}
	var waitEnds time.Time
	if run.MaxWaitSeconds > 0 {
		waitEnds = tokenAt.Add(time.Duration(run.MaxWaitSeconds) * time.Second)
	}
	delay := f.capacityRetry
	if delay <= 0 {
		delay = 5 * time.Second
	}
	maxDelay := 6 * delay
	queued := false
	lastReason := ""
	capacityNotices := 0
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
		if queued && pool && req.ComputePlacement != nil && req.ComputePlacement.CloudFallback && !f.now().Before(preferenceEnds) {
			res := refusedResult(pl.CodeNoMachineForNeed, "Fleet preference wait elapsed after confirmed pre-start capacity refusal. No Fleet command was started.")
			return worker.Result{}, &res, nil
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
		if err != nil || result.OK || !result.RefusedBeforeStart || result.RefusedByGate {
			return result, nil, err
		}
		if pool {
			if ctx.Err() != nil {
				res := stoppedResult(ctx)
				return worker.Result{}, &res, nil
			}
			if deadlineErr == nil && !f.now().Before(deadline) {
				res := failedResult(pl.CodeRunCeiling, "The run reached its ceiling while waiting for fleet capacity. No command was started.")
				return worker.Result{}, &res, nil
			}
			if !fleetPoolCapacityRefusal(result) {
				res := refusedResult(pl.CodeExecutorError, "Fleet refused this request for a reason other than confirmed pre-start capacity. No cloud replacement was started.")
				return worker.Result{}, &res, nil
			}
			if req.ComputePlacement != nil && req.ComputePlacement.CloudFallback && !f.now().Before(preferenceEnds) {
				res := refusedResult(pl.CodeNoMachineForNeed, "Fleet preference wait elapsed after confirmed pre-start capacity refusal. No Fleet command was started.")
				return worker.Result{}, &res, nil
			}
		} else if !fleetMayWait(result) {
			return result, nil, nil
		}
		// The wait the driver allowed has run out. Checked here, after a refusal
		// that WOULD have waited, so a refusal that never could (a machine that is
		// not a candidate at all) is answered at once and the budget is only ever
		// spent on a wait that could have ended differently.
		if !waitEnds.IsZero() && !f.now().Before(waitEnds) {
			res := waitRanOut(run, result)
			return worker.Result{}, &res, nil
		}
		if deadlineErr != nil {
			res := failedResult(pl.CodeExecutorError, "Fleet capacity is unavailable and the run has no readable ceiling; no unbounded wait or replacement command was started.")
			return worker.Result{}, &res, nil
		}
		if !queued {
			capture.Note("memql: waiting for the owner's shared build capacity; no command has started")
			queued = true
		}
		// Preserve the machine's actionable capacity explanation (for example
		// insufficient disk) without repeating it on every polling attempt.
		// Available bytes may change every poll: cap live notices, retaining
		// further changes in the bounded archive rather than flooding the store.
		if reason := cutBytes(strings.TrimSpace(result.ErrorMessage), 2048); reason != "" && reason != lastReason {
			if capacityNotices < 4 {
				capture.Notice("memql: " + reason)
				capacityNotices++
			} else {
				capture.Note("memql: " + reason)
			}
			lastReason = reason
		}
		wait := delay
		if remaining := deadline.Sub(f.now()); remaining < wait {
			wait = remaining
		}
		if pool && req.ComputePlacement != nil && req.ComputePlacement.CloudFallback {
			if remaining := preferenceEnds.Sub(f.now()); remaining < wait {
				wait = remaining
			}
		}
		if !waitEnds.IsZero() {
			if remaining := waitEnds.Sub(f.now()); remaining < wait {
				wait = remaining
			}
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

// The dispatcher proves both eligibility and that nothing started. A missing
// candidate alone is not enough: it may mean revoked consent or a mismatched
// contract. A lost reply after dispatch must never replay the command.
func fleetMayWait(result worker.Result) bool {
	switch result.ErrorCode {
	case "pipeline_capacity_busy", "pipeline_capacity_fenced", "worker_disconnected", "worker_unreachable":
		return true
	case "no_worker_available":
		return result.WaitForConnection
	default:
		return false
	}
}

// The pool deadline applies only between positively refused dispatch attempts.
// It never cancels a worker that has started, or converts uncertainty/authority
// failure into permission to run a second copy elsewhere.
func fleetPoolCapacityRefusal(r worker.Result) bool {
	switch r.ErrorCode {
	case "pipeline_capacity_busy", "pipeline_capacity_fenced", "pipeline_capacity_unavailable", "pipeline_runtime_unavailable", "worker_disconnected", "worker_unreachable", "no_worker_available":
		return true
	default:
		return false
	}
}

// placedOnFleet says the driver placed this attempt on the fleet by name
// (pl.AttemptPlacement), as opposed to the step's own placement, which is how a
// step's first attempt goes.
func placedOnFleet(req pl.StepRequest) bool {
	return req.Placement != nil && req.Placement.Surface == pl.PlacementFleet
}

// waitRanOut is the answer of a placed attempt whose wait for the owner's
// machines ran out while they stayed busy or disconnected: a refusal, because
// no command was started on any machine.
//
// NOTHING RAN, and NOT TRIED HERE AGAIN. Nothing started, so another attempt
// cannot run the command twice. And the machine it names is not tried again
// because the wait the driver granted for it is spent: a same-machine retry
// that pins it again would wait as long and end the same way, so the driver
// moves on at once to another machine, and from there to the cluster where the
// policy allows (spec section 5, rules 2 and 3). The class is set here, where
// it is decided, because it cannot be read from the result: the result looks
// like the refusal a pool step's preference deadline makes, which is nothing
// ran and IS retried on the same machine.
//
// The sentence names the code the dispatcher last answered with and the wait,
// never the machine's own words, which can quote the step's credentials and are
// not masked here. The machine the dispatcher last named, if it named one, is
// kept: it is where the step was last looked for.
func waitRanOut(run *StepRun, last worker.Result) pl.StepResult {
	res := refusedResult(pl.CodeNoMachineForNeed, fmt.Sprintf(
		"The wait of %s that the driver allowed for the owner's machines ran out while none could take the step (last answer: %s). "+
			"No command was started.", time.Duration(run.MaxWaitSeconds)*time.Second, last.ErrorCode))
	res.Where = pl.Where{Surface: surfaceFleet, WorkerID: last.WorkerId, MachineName: last.MachineName, NodeID: last.NodeId, MachineLabels: maps.Clone(last.Labels)}
	res.AttemptClass, res.NotRetriedHere = pl.AttemptNothingRan, true
	return res
}
