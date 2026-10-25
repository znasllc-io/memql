package pipelinerun

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
)

func (h *pipelineWorkflowHost) reportMergeGroupReview(ctx context.Context, args map[string]any) (any, error) {
	dr := h.dr
	if len(args) == 0 && dr.run.Event == pipelines.EventMergeGroup && dr.run.Mode == pipelines.ModeFull {
		name, err := dr.independentReviewer(ctx)
		if err != nil {
			return nil, err
		}
		if name != "" {
			return dr.independentReview(ctx)
		}
	}
	if len(args) != 0 || dr.d.ReportMergeGroupReview == nil || dr.workflow == nil || dr.workflow.snapshot == nil || dr.run.Event != pipelines.EventMergeGroup || dr.run.Mode != pipelines.ModeFull {
		return nil, fmt.Errorf("merge-group review reporting requires an operator-bound full merge-group run and accepts no arguments")
	}
	fence := func(c context.Context) error {
		if err := c.Err(); err != nil {
			return err
		}
		if dr.lease.isLost() || dr.lease.isCancelled() || dr.cleanupFailed.Load() {
			return fmt.Errorf("merge-group reporting run stopped")
		}
		r, err := dr.d.Store.RunByID(memql.ContextWithFreshRead(c), dr.runID)
		if err != nil {
			return err
		}
		if !dr.owns(r) || r.Finished() || r.CancelRequested || r.Status != StatusInProgress || r.PipelineID != dr.run.PipelineID || r.OwnerUserID != dr.run.OwnerUserID || r.Repository != dr.run.Repository || r.SHA != dr.run.SHA || r.BaseSHA != dr.run.BaseSHA || r.HeadBranch != dr.run.HeadBranch || r.Event != pipelines.EventMergeGroup || r.Mode != pipelines.ModeFull {
			return fmt.Errorf("merge-group reporting lease or captured run changed")
		}
		policy, err := readPipelinePolicy(r.PolicySnapshot)
		if err != nil || policy.Version != dr.workflow.snapshot.Version {
			return fmt.Errorf("merge-group captured delivery policy changed")
		}
		current, err := dr.d.Store.PipelineByID(memql.ContextWithFreshRead(c), dr.p.ID)
		if err != nil {
			return err
		}
		if current == nil || !current.Active() || current.OwnerUserID != dr.p.OwnerUserID || current.Repository != dr.p.Repository || current.PackageID != dr.p.PackageID || current.CredentialID != dr.p.CredentialID || current.InstallationID != dr.p.InstallationID || current.DefaultBranch != dr.p.DefaultBranch {
			return fmt.Errorf("merge-group pipeline source changed")
		}
		return nil
	}
	if err := fence(ctx); err != nil {
		return nil, err
	}
	receipt, err := dr.d.ReportMergeGroupReview(ctx, dr.p, dr.run, fence)
	if err != nil {
		return nil, err
	}
	if err = fence(ctx); err != nil {
		return nil, err
	}
	if receipt["state"] != "complete" || receipt["headSha"] != dr.run.SHA || receipt["checkName"] != pipelines.IndependentReviewCheckName {
		return nil, fmt.Errorf("merge-group independent review check was not confirmed")
	}
	return receipt, nil
}
