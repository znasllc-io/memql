package release

import (
	"context"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
	pl "github.com/znasllc-io/memql/component/pipelines"
)

// reusedFrom proves one step a release reused from another run, through the same
// owner-scoped fresh reads as the rest of candidate evidence. The proof itself is
// pl.ProveReusedStep, which says what it requires and why: it lives there so the
// driver's tests can run it over rows the driver really wrote, since this package is
// another module and cannot run the driver.
func (r candidateEvidenceReader) reusedFrom(ctx context.Context, owner string, reuser carriedRun, reuserRow map[string]any, stepKey string) (pl.ReleaseReusedFrom, error) {
	declared, err := pl.SameTreeRunFromRow(reuser.row, memql.BareShortId)
	if err != nil {
		return pl.ReleaseReusedFrom{}, err
	}
	return pl.ProveReusedStep(pl.ReusedStepReads{
		PipelineRun: func(id string) (pl.SameTreeRun, error) {
			run, err := r.pipelineRun(ctx, id)
			if err != nil {
				return pl.SameTreeRun{}, err
			}
			return pl.SameTreeRunFromRow(run.row, memql.BareShortId)
		},
		WorkRun: func(id string) (map[string]any, error) {
			return r.oneRow(ctx, "workRunForOwner", map[string]any{"runId": id})
		},
		WorkStep: func(id, key string) (map[string]any, error) {
			return r.oneRow(ctx, "workStepForOwnerRun", map[string]any{"runId": id, "stepKey": key})
		},
	}, owner, declared, reuserRow, stepKey)
}

// reusedSkip is the run a step row reused, or "" for a row that names none.
func reusedSkip(step map[string]any) string {
	return memql.BareShortId(pl.ReusedEvidenceRun(step))
}

// stepDefinitionDigest is a step row's definition in a receipt's digest spelling.
func stepDefinitionDigest(step map[string]any) string {
	call, _ := step["call"].(map[string]any)
	fingerprint, _ := call["definitionFingerprint"].(string)
	return "sha256:" + strings.TrimPrefix(fingerprint, "work-definition-v2:")
}
