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
// result; after a SUCCESSFUL FULL run the driver merges the results of the steps
// that declare shards into the pipeline row, and the next run's shards balance on
// them. Only a full run teaches: an affected run measures the packages a change
// reached, a sample skewed toward whatever was edited; and only a successful
// one, because a failing run's times are a failing tree's.
//
// ONLY A STEP THAT DECLARES SHARDS TEACHES (pipelines.Step.Balanced). The table's
// one reader is pipelines.Partition, placing that step's packages, so the figure
// it needs is a package's time in that step. Other steps run the same packages
// and report other figures, in both directions, measured on this repository's
// pipeline in October 2026:
//
//   - component/memql takes about 800 s in tests.db-tests, 40 s in tests.fuzz and
//     5 s in checks.go-checks. When the last figure read took the row, it flipped
//     between 800 and 40 from run to run, and at 40 the package shared a db-tests
//     shard with 24 others: 1209 s beside a shard of 79 s.
//   - app takes about 80 s in tests.db-tests and about 420 s in tests.test-agent.
//     Keeping each package's largest time across the run's steps fixes
//     component/memql and leaves app weighing five times what its shard spends on
//     it. Replaying one run's real times through Partition, the shards beside
//     component/memql's ran 193 to 615 s under the largest times and 450 to 501 s
//     under the shards' own.
//
// Comparing a run's steps also cannot protect a row from a run that did not run
// them. A failed-only re-run reports only what it ran, and a replica that takes a
// run over only what it sees finish, since observed lives in the drive's memory.
// Neither can write a wrong figure under this rule: what they ran of a sharded
// step teaches, and every other row keeps its figure, which is stale at worst
// until the next full run.
//
// A package that only other steps run has no row, and no Partition asks for one.
// A pipeline whose only sharded step runs in events that are never a full run
// never teaches at all, and splits by package count. Neither happens here: all 294
// packages with tests reported from a sharded step on 2026-11-01.
//
// Regression tests: TestOnlyAStepThatDeclaresShardsTeachesTheTimingTable,
// TestAFailedOnlyRerunOfAnotherStepLeavesTheTimingTableAlone,
// TestAFailedOnlyRerunOfAShardTeachesWhatItRan,
// TestAReplicaThatTakesARunOverTeachesWhatItSawFinish,
// TestAStepThatDeclaresShardsTeachesWhenItCouldNotSplit and
// TestTwoShardedStepsOverOnePackageTeachTheLargerTime.
//
// The merge is a read-modify-write of the whole table, under the
// repository's gate -- the one every writer of the pipeline row takes -- with
// a fresh read, so two runs finishing at once never write each other's table
// over: each merges into the table as the other left it.

// mergeTimings merges observed into p's table and records which run taught it
// last. A table is the repository's: when the row names another repository by
// now -- its source was reconnected elsewhere -- the measurements of this one
// teach it nothing, and are dropped.
func (i *Integration) mergeTimings(ctx context.Context, d Deps, p Pipeline, runID string, observed map[string]float64) error {
	if len(observed) == 0 {
		return nil
	}
	return i.driverGate(ctx, d, RepositoryGateKey(p.Repository), func(gctx context.Context) error {
		current, err := d.Store.PipelineByID(memql.ContextWithFreshRead(gctx), p.ID)
		if err != nil || current == nil || current.Repository != p.Repository {
			return err
		}
		merged := pipelines.MergeTimings(current.Timings, observed)
		return d.Store.UpdatePipeline(gctx, current.OwnerUserID, current.ID, PipelinePatch{
			Timings: ptr(merged), TimingsRunID: ptr(bareID(runID)), TimingsUpdatedAt: ptr(d.now()),
		})
	})
}
