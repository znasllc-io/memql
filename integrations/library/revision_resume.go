package library

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
)

type revisionCheckpointKey struct{}

// PrepareCheckpointResume proves that this suspended review can continue from
// its existing decision. It authorizes no rerun of AI or of the apply effect.
func (i *Integration) PrepareCheckpointResume(ctx context.Context, capability string, request memql.CheckpointResumeRequest) (context.Context, bool, error) {
	if capability != "reviewRevision" {
		return ctx, false, nil
	}
	ctx = memql.ContextWithFreshRead(ctx)
	approval, err := i.revisionRow(ctx, "workApprovalForOwner", map[string]any{"approvalId": request.ApprovalID})
	if err != nil {
		return ctx, false, err
	}
	proposal := revisionMap(approval["subject"])
	if approval["kind"] != workstate.ApprovalKindPlanReview || approval["decision"] != "approved" || memql.BareShortId(asString(approval["runId"])) != memql.BareShortId(request.RunID) || approval["stepKey"] != request.StepKey || proposal["reviewType"] != documentReviewType || approval["artifactHash"] != workstate.ArtifactHash(proposal) {
		return ctx, false, fmt.Errorf("the exact document review checkpoint has not been approved")
	}
	ids, captured, run, current, err := i.revisionRequest(ctx, asString(proposal["requestId"]))
	if err != nil {
		return ctx, false, err
	}
	if ids.RunID != memql.BareShortId(request.RunID) || ids.ApprovalID != memql.BareShortId(request.ApprovalID) || run["status"] != "running" || run["cancelRequested"] == true || current["artifactHash"] != approval["artifactHash"] {
		return ctx, false, fmt.Errorf("this run does not own the approved checkpoint")
	}
	if err = validateRevisionResult(captured, proposal); err != nil {
		return ctx, false, err
	}
	if err = i.ValidateRevisionProposal(ctx, proposal); err != nil {
		return ctx, false, err
	}
	return context.WithValue(ctx, revisionCheckpointKey{}, ids.ApprovalID), true, nil
}

var _ memql.CheckpointResumePreparer = (*Integration)(nil)
