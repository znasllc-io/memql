package pipelinerun

import (
	"context"
	"fmt"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
)

// rerun.go -- a person's re-run: the next attempt of a run key, in the
// original's mode and event (decision 3). The original stays exactly as it
// ended; a finished row is never reopened.
//
// "RE-RUN FAILED" (epic memql#5479, D13) is the same new attempt, marked to
// run only what the original did not pass: the driver carries every step the
// original passed -- with the same package slice -- over as skipped
// pipeline_passed_earlier (driver.go, carryPassed). It is refused here, by
// name, when the original has nothing to run again, so the act a surface
// offers and the act the engine takes agree on when it is legal.

func (i *Integration) handleRerun(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	run, err := i.Rerun(handlerContext(ctx), stringArg(args, "runId"), boolArg(args, "failedOnly"))
	if err != nil {
		return nil, err
	}
	return resultNode(map[string]any{
		"runId":      run.ID,
		"attempt":    run.Attempt,
		"rerunOf":    run.RerunOf,
		"status":     run.Status,
		"failedOnly": run.RerunFailedOnly,
	}), nil
}

// Rerun opens the next attempt of one of the caller's runs: every step, or
// with failedOnly only what the original did not pass.
func (i *Integration) Rerun(ctx context.Context, runID string, failedOnly bool) (Run, error) {
	if _, err := personFrom(ctx); err != nil {
		return Run{}, err
	}
	d := i.snapshot()
	if d.Store == nil {
		return Run{}, errNoStore
	}
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return Run{}, fmt.Errorf("pipelines: runId is required")
	}
	// Owner-scoped: someone else's run is no run.
	original, err := d.Store.RunForOwner(ctx, runID)
	if err != nil {
		return Run{}, err
	}
	if original == nil {
		return Run{}, fmt.Errorf("pipelines: you have no run %q", runID)
	}
	if original.RefusalCode == pipelines.CodeForkRefused {
		return Run{}, pipelines.Refuse(pipelines.CodeForkRefused, "",
			"A pull request from a fork is never run, and running it again would not change that.")
	}
	p, err := d.Store.PipelineByID(ctx, original.PipelineID)
	if err != nil {
		return Run{}, err
	}
	if p == nil {
		return Run{}, fmt.Errorf("pipelines: run %q names a pipeline that no longer exists", runID)
	}
	if !p.Active() {
		return Run{}, pipelines.Refuse(pipelines.CodeDisconnected, "",
			"This run's pipeline is disconnected, so it opens no runs. Reconnect it to run this commit again.")
	}
	if failedOnly {
		if refusal, err := nothingToRerun(ctx, d, *original); err != nil || refusal != nil {
			return Run{}, firstErr(err, refusal)
		}
	}
	res, err := i.open(ctx, d, *p, rerunOf(*original, "", failedOnly))
	if err != nil {
		return Run{}, err
	}
	return res.Run, nil
}

// nothingToRerun refuses a failed-only re-run of a run with no failed or
// cancelled step: one that passed, one refused before any step began (a fork,
// a manifest that did not compile -- no work run), or one whose steps all
// ended well. Read fresh: the run's work is written by another node's driver.
func nothingToRerun(ctx context.Context, d Deps, original Run) (*pipelines.Refusal, error) {
	refuse := func(why string) *pipelines.Refusal {
		return pipelines.Refuse(pipelines.CodeNothingToRerun, "", "%s Re-run it whole instead.", why)
	}
	switch original.Conclusion {
	case ConclusionFailure, ConclusionCancelled:
	default:
		return refuse("This run has no failed step to run again."), nil
	}
	if strings.TrimSpace(original.WorkRunID) == "" {
		return refuse("This run ended before any step ran, so there is no failed step to run again."), nil
	}
	steps, err := d.Store.WorkSteps(memql.ContextWithFreshRead(ctx), original.WorkRunID)
	if err != nil {
		return nil, err
	}
	for _, s := range steps {
		if s.Status == WorkStepFailed || s.Status == WorkStepCancelled {
			return nil, nil
		}
	}
	return refuse("No step of this run failed or was cancelled."), nil
}
