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
//
// A SUPERSEDE OF A WAITING PUSH asks for ONLY that exception
// (cancelIfWaiting, push_lane.go): a push run an agent has already claimed is
// executing, and the push lane lets it finish rather than flag it to stop. So
// it concludes the run on the spot while nobody drives it and otherwise leaves
// it entirely alone -- no flag, no write -- deciding under the same gate the
// claim takes, so of a claim and a supersede of one run exactly one wins.

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
			if err := concludeUndriven(gctx, d, r, by); err != nil {
				return err
			}
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

// concludeUndriven concludes r -- queued, claimed by no agent, read fresh
// under the run's gate that gctx holds -- cancelled on by's word: the one
// write both a cancel and a supersede make of a run nobody drives, so the two
// conclusions never differ but in who asked. The write is fenced to the row
// version read, as a claim's is: should the gate's session be lost between the
// read and the write, a claim that landed meanwhile refuses it rather than
// having a claimed run concluded under its driver.
func concludeUndriven(gctx context.Context, d Deps, r *Run, by string) error {
	patch := RunPatch{
		Status: ptr(StatusCompleted), Conclusion: ptr(ConclusionCancelled),
		CancelRequested: ptr(true), CancelledBy: ptr(by), FinishedAt: ptr(d.now()),
	}
	wctx := memql.ContextWithRowVersionFence(gctx, RunConcept, r.ID, r.CreatedAt)
	if err := d.Store.UpdateRun(wctx, r.OwnerUserID, r.ID, patch); err != nil {
		return err
	}
	applyRunPatch(r, patch)
	return nil
}

// cancelIfWaiting concludes runID cancelled on by's word only while nobody
// drives it -- queued, never started, claimed by no agent, not asked to stop
// (waitingRun) -- and reports whether it did. A run an agent claimed, started
// or not, is left entirely alone, with no flag written, and the log says so:
// it is already executing, and finishes. A finished run answers
// ErrRunFinished, and keeps its answer.
//
// It is how the push lane supersedes (supersede.go, push_lane.go). The
// decision is the claim's mirror: one fresh read and at most one write under
// the run's gate -- the gate the claim takes -- so of a claim and this cancel,
// racing on two replicas, exactly one wins. It takes the gate as a drive does
// (driverGate), one of this node's sections at a time: recovery and the
// hand-off after a drive call it on an agent replica, whose direct pool its
// drives already share. The check run of a run concluded here is moved after
// the gate is released, as RequestCancel moves it.
func (i *Integration) cancelIfWaiting(ctx context.Context, d Deps, runID, by string) (Run, bool, error) {
	if d.Store == nil {
		return Run{}, false, errNoStore
	}
	runID = bareID(runID)
	if runID == "" {
		return Run{}, false, ErrRunNotFound
	}
	by = strings.TrimSpace(by)
	if by == "" {
		by = "system"
	}
	var (
		result    Run
		concluded bool
	)
	gate := func(ctx context.Context, key string, fn func(context.Context) error) error {
		return i.driverGate(ctx, d, key, fn)
	}
	err := gate(ctx, RunGateKey(runID), func(gctx context.Context) error {
		r, err := d.Store.RunByID(memql.ContextWithFreshRead(gctx), runID)
		if err != nil {
			return err
		}
		if r == nil {
			return ErrRunNotFound
		}
		result = *r
		if r.Finished() {
			return ErrRunFinished
		}
		if !waitingRun(*r) {
			return nil
		}
		if err := concludeUndriven(gctx, d, r, by); err != nil {
			return err
		}
		result, concluded = *r, true
		return nil
	})
	if err != nil {
		return result, false, err
	}
	if !concluded {
		// Claimed or started: executing, and it finishes. A run already asked
		// to stop is being stopped by whoever asked, and says nothing here.
		if !result.CancelRequested {
			d.Logger.Info("pipelines: an agent claimed the run before it could be cancelled, so it is already executing; it finishes",
				"component", "pipelinerun", logger.Subject(RunConcept, result.ID), "status", result.Status,
				"holder", result.DriverNodeID, "cancelledBy", by)
		}
		return result, false, nil
	}
	reportConclusion(ctx, d, gate, result)
	return result, true, nil
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
