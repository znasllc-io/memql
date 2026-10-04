package pipelinerun

import (
	"context"
	"fmt"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/logger"
)

// cancel.go -- asking a run to stop.
//
// The row records the ASK and the driver records the OUTCOME, so the run can
// never claim to have stopped while it is still executing somewhere: a cancel
// sets cancelRequested and cancelledBy, and the node driving the run reads it
// -- at once when that is this node (the SignalCancel hook), at its next
// heartbeat otherwise -- cancels what is in flight, and concludes the run
// cancelled. The one exception is a run nobody drives yet: a queued run with
// no driver has nothing in flight and nobody to conclude it, so it is
// concluded cancelled on the spot, check run included.

func (i *Integration) handleCancel(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ctx = handlerContext(ctx)
	caller, err := personFrom(ctx)
	if err != nil {
		return nil, err
	}
	d := i.snapshot()
	if d.Store == nil {
		return nil, errNoStore
	}
	runID := stringArg(args, "runId")
	if runID == "" {
		return nil, fmt.Errorf("pipelines: runId is required")
	}
	// Owner-scoped: someone else's run is no run.
	owned, err := d.Store.RunForOwner(ctx, runID)
	if err != nil {
		return nil, err
	}
	if owned == nil {
		return nil, fmt.Errorf("pipelines: you have no run %q", runID)
	}
	run, err := i.requestCancel(ctx, d, owned.ID, caller)
	if err != nil {
		return nil, err
	}
	return resultNode(map[string]any{
		"runId":           run.ID,
		"status":          run.Status,
		"conclusion":      run.Conclusion,
		"cancelRequested": run.CancelRequested,
	}), nil
}

// RequestCancel asks run runID to stop, on behalf of `by` -- a person's id,
// or a sentinel naming the system that asked (the substrate's runner, when a
// Job's own deadline passed). It is the Go entry point the substrate calls; a
// person reaches it through the cancel capability, which first proves the
// person owns the run.
//
// A finished run answers ErrRunFinished; a run already asked is asked again
// harmlessly.
func (i *Integration) RequestCancel(ctx context.Context, runID, by string) error {
	_, err := i.requestCancel(ctx, i.snapshot(), runID, by)
	return err
}

func (i *Integration) requestCancel(ctx context.Context, d Deps, runID, by string) (Run, error) {
	if d.Store == nil {
		return Run{}, errNoStore
	}
	runID = bareID(runID)
	if runID == "" {
		return Run{}, ErrRunNotFound
	}
	by = strings.TrimSpace(by)
	if by == "" {
		by = "system"
	}

	var (
		result    Run
		signalled bool
		concluded bool
	)
	// The decision and its write are one short section under the run's gate:
	// a fresh read and one write, nothing of GitHub's. The check run of a run
	// concluded here is moved AFTER the gate is released (below).
	err := d.gate(ctx, RunGateKey(runID), func(gctx context.Context) error {
		r, err := d.Store.RunByID(memql.ContextWithFreshRead(gctx), runID)
		if err != nil {
			return err
		}
		if r == nil {
			return ErrRunNotFound
		}
		if r.Finished() {
			result = *r
			return ErrRunFinished
		}

		if r.Status == StatusQueued && strings.TrimSpace(r.DriverNodeID) == "" {
			// Nobody drives it: nothing is in flight, and nobody would
			// conclude it. The cancel IS the conclusion.
			now := d.now()
			patch := RunPatch{
				Status: ptr(StatusCompleted), Conclusion: ptr(ConclusionCancelled),
				CancelRequested: ptr(true), CancelledBy: ptr(by), FinishedAt: ptr(now),
			}
			if err := d.Store.UpdateRun(gctx, r.OwnerUserID, r.ID, patch); err != nil {
				return err
			}
			applyRunPatch(r, patch)
			result, concluded = *r, true
			return nil
		}

		if !r.CancelRequested {
			if err := d.Store.UpdateRun(gctx, r.OwnerUserID, r.ID, RunPatch{
				CancelRequested: ptr(true), CancelledBy: ptr(by),
			}); err != nil {
				return err
			}
			r.CancelRequested, r.CancelledBy = true, by
		}
		result = *r
		signalled = strings.TrimSpace(d.NodeID) != "" && r.DriverNodeID == d.NodeID
		return nil
	})
	if err != nil {
		return result, err
	}
	if concluded {
		// The run is concluded on its row; its check run follows, with no
		// gate held -- a mint and a write to GitHub are as slow as GitHub is.
		// A completed run is never claimed and never reopened, so nothing
		// moves between the section above and this write.
		reportConclusion(ctx, d, d.gate, result)
	}
	// Outside the gate: the driver's own conclusion takes the same key, and
	// a hook that concluded synchronously would otherwise wait on it.
	if signalled && d.SignalCancel != nil {
		d.SignalCancel(ctx, result.ID)
	}
	return result, nil
}

// reportConclusion writes the final check run of a run concluded OUTSIDE a
// drive -- a queued run cancelled before any agent claimed it -- and records
// the answer on the row. GitHub is asked with no gate held; the answer is
// recorded under the run's gate as a fresh read and one write, and only when
// it says something new. A final report that did not land is recorded
// `unavailable`, never left reading `written` over a check run that still
// shows the run unfinished: recovery republishes it (recover.go). It never
// fails the conclusion it reports.
func reportConclusion(ctx context.Context, d Deps, gate Gate, r Run) {
	p, err := d.Store.PipelineByID(ctx, r.PipelineID)
	if err != nil || p == nil {
		if err != nil {
			d.Logger.Warn("pipelines: the concluded run's pipeline could not be read, so its check run was not moved",
				"component", "pipelinerun", logger.Subject(RunConcept, r.ID), "error", err)
		}
		return
	}
	w := publishCheckRun(ctx, d, *p, r, ReportFor(*p, r, nil))
	if w.Err != nil {
		d.Logger.Warn("pipelines: the concluded run's check run was not moved",
			"component", "pipelinerun", logger.Subject(RunConcept, r.ID), "checkRunState", w.State, "error", w.Err)
	}
	if _, changed := finalCheckRunPatch(w, r); !changed {
		return // the row already says all of it: no gate for nothing
	}
	if err := recordFinalCheckRun(ctx, d, gate, r.ID, w); err != nil {
		d.Logger.Warn("pipelines: the concluded run's check-run state was not recorded",
			"component", "pipelinerun", logger.Subject(RunConcept, r.ID), "error", err)
	}
}
