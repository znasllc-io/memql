package pipelinerun

import (
	"context"
	"fmt"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// run_detail.go -- pipelinesRunDetail, a developer's view of a run they do not
// own (design record docs/superpowers/specs/2026-11-24-pipelines-developer-view-design.md).
//
// The run row reads from developer rank up (its tier's rankFloor); its steps
// (v1:work:step) and files (v1:library:artifact) stay their owner's. Rather
// than widen two more tiers, this reads them under the run OWNER's borrowed
// authority once the CALLER has read the run, and answers only what the run
// page reads (stepFromRow / runFileFromRow in the OS). It acts on nothing:
// re-run, cancel and every other act still resolve through RunForOwner.

func (i *Integration) handleRunDetail(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	// Fresh: a run is read while it moves, and a cached read would freeze a
	// developer's page on a stale step (as artifact_expiry.go reads).
	ctx = memql.ContextWithFreshRead(handlerContext(ctx))
	if _, err := personFrom(ctx); err != nil {
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
	// The caller's own run, else the developer read, which the engine refuses
	// below developer rank. Either way the CALLER read it, unstamped.
	r, err := d.Store.RunForOwner(ctx, runID)
	if err == nil && r == nil {
		r, err = d.Store.RunView(ctx, runID)
	}
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, fmt.Errorf("pipelines: you cannot read run %q", runID)
	}
	rows, artifacts, err := d.Store.RunDetailRows(ctx, r.OwnerUserID, r.WorkRunID)
	if err != nil {
		return nil, err
	}
	steps := make([]map[string]any, 0, len(rows))
	for _, s := range rows {
		steps = append(steps, runDetailStep(s))
	}
	files := make([]map[string]any, 0, len(artifacts))
	for _, a := range artifacts {
		if rowString(a, "source") != "pipeline" {
			continue
		}
		files = append(files, map[string]any{
			"id": a["id"], "sourceConceptRef": a["sourceConceptRef"], "title": a["title"], "format": a["format"], "archived": a["archived"],
		})
	}
	return resultNode(map[string]any{"steps": steps, "files": files}), nil
}

// runDetailStep keeps the fields the run page reads off a step and nothing
// else: a step's call and result carry the plan and the executor's output.
func runDetailStep(s WorkStep) map[string]any {
	out := map[string]any{"call": map[string]any{"stage": s.Stage, "name": s.Name}}
	for _, k := range []string{"id", "runId", "key", "seq", "status", "errorCode", "errorMessage", "durationMs", "startedAt", "finishedAt", "logFileId", "artifactFileIds", "binding"} {
		if v, ok := s.Raw[k]; ok {
			out[k] = v
		}
	}
	if s.Result != nil {
		meta, _ := s.Result["metadata"].(map[string]any)
		out["result"] = map[string]any{"status": s.Result["status"], "reason": s.Result["reason"], "metadata": map[string]any{"notes": meta["notes"]}}
	}
	return out
}
