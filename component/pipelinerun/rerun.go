package pipelinerun

import (
	"context"
	"fmt"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/pipelines"
)

// rerun.go -- a person's re-run: the next attempt of a run key, in the
// original's mode and event (decision 3). The original stays exactly as it
// ended; a finished row is never reopened.

func (i *Integration) handleRerun(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	run, err := i.Rerun(handlerContext(ctx), stringArg(args, "runId"))
	if err != nil {
		return nil, err
	}
	return resultNode(map[string]any{
		"runId":   run.ID,
		"attempt": run.Attempt,
		"rerunOf": run.RerunOf,
		"status":  run.Status,
	}), nil
}

// Rerun opens the next attempt of one of the caller's runs.
func (i *Integration) Rerun(ctx context.Context, runID string) (Run, error) {
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
	res, err := i.open(ctx, d, *p, rerunOf(*original, ""))
	if err != nil {
		return Run{}, err
	}
	return res.Run, nil
}
