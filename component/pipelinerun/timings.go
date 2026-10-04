package pipelinerun

import (
	"context"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
)

// timings.go -- the pipeline's timing table learns from its runs (D7, D11).
//
// A step's runner reads each passing package's wall time from the step's
// `go test` output (pipelines.ParseGoTestOutput) and reports it on the step's
// result; after a SUCCESSFUL FULL run the driver merges every result's
// timings into the pipeline row, and the next run's shards balance on them.
// Only a full run teaches: an affected run measures the packages a change
// reached, a sample skewed toward whatever was edited; and only a successful
// one, because a failing run's times are a failing tree's.
//
// The merge is a read-modify-write of the whole table, under the pipeline's
// gate with a fresh read, so two runs finishing at once never write each
// other's table over: each merges into the table as the other left it.

// mergeTimings merges observed into the pipeline's table and records which
// run taught it last.
func (i *Integration) mergeTimings(ctx context.Context, d Deps, pipelineID, runID string, observed map[string]float64) error {
	if len(observed) == 0 {
		return nil
	}
	return i.driverGate(ctx, d, PipelineGateKey(pipelineID), func(gctx context.Context) error {
		p, err := d.Store.PipelineByID(memql.ContextWithFreshRead(gctx), pipelineID)
		if err != nil || p == nil {
			return err
		}
		merged := pipelines.MergeTimings(p.Timings, observed)
		return d.Store.UpdatePipeline(gctx, p.OwnerUserID, p.ID, PipelinePatch{
			Timings: ptr(merged), TimingsRunID: ptr(bareID(runID)), TimingsUpdatedAt: ptr(d.now()),
		})
	})
}
