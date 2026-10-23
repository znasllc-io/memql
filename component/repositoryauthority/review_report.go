package repositoryauthority

import (
	"context"
	"errors"
	"github.com/znasllc-io/memql/component/pipelines"
)

// ReviewCheckName is a distinct repository-policy gate, never a pipeline test
// context. Only a persisted accepted independent review can authorize it.
const ReviewCheckName = pipelines.IndependentReviewCheckName

func reviewReportIntent(r record) ReviewReportIntent {
	reviewDigest := digest(struct {
		Review *ReviewReceipt
		Input  *ReviewInput
		Prompt map[string]any
	}{r.ReviewReceipt, r.ReviewInput, r.Route.ReviewPrompt})
	external := digest(struct {
		RequestID, SourceID              string
		RepositoryID                     int64
		ProposalHash, Head, ReviewDigest string
	}{r.ID, r.Maintainer.ID, r.RepositoryID, r.ProposalHash, r.ProposalReceipt.Commit, reviewDigest})
	return ReviewReportIntent{ProposalHash: r.ProposalHash, ReviewDigest: reviewDigest, ExternalID: "memql-review:" + external, Proposal: *r.ProposalReceipt, BaseBranch: r.Proposal.BaseBranch}
}

func matchingReviewReport(intent ReviewReportIntent, receipt *ReviewReportReceipt) bool {
	return receipt != nil && receipt.State == "complete" && receipt.CheckName == ReviewCheckName && receipt.CheckRunID > 0 && receipt.AppID > 0 && receipt.HeadSHA == intent.Proposal.Commit && receipt.ProposalHash == intent.ProposalHash && receipt.ReviewDigest == intent.ReviewDigest && receipt.ExternalID == intent.ExternalID
}

// ReportReview publishes only the native accepted review for the exact published
// commit. The public operation accepts a request identity, never check fields or
// an approval. Persist intent before contacting the provider so another replica
// can recover a lost response by the same immutable external identity.
func (i *Integration) ReportReview(ctx context.Context, id string) (ReviewReportReceipt, error) {
	grant, err := i.AuthorizeReviewReport(ctx, id)
	if err != nil {
		return ReviewReportReceipt{}, err
	}
	d := i.deps()
	if d.ReportReview == nil {
		return ReviewReportReceipt{}, errors.New("repository review reporting is unavailable")
	}
	_, err = i.update(ctx, id, func(r *record) error {
		intent := reviewReportIntent(*r)
		if r.ReviewReportIntent != nil && digest(*r.ReviewReportIntent) != digest(intent) {
			return errors.New("recorded review report intent changed")
		}
		if r.ReviewReportIntent == nil {
			r.ReviewReportIntent = &intent
		}
		if r.DeliveryState == "" {
			r.DeliveryState = "reporting"
			r.DeliveryReceipt = &DeliveryReceipt{State: "waiting", Reason: "Waiting for the independent review check to be confirmed.", HeadSHA: intent.Proposal.Commit}
		}
		return nil
	})
	if err != nil {
		return ReviewReportReceipt{}, err
	}
	var receipt ReviewReportReceipt
	_, err = i.update(ctx, id, func(r *record) error {
		if _, e := i.AuthorizeReviewReport(ctx, id); e != nil {
			return e
		}
		intent := reviewReportIntent(*r)
		if r.ReviewReportIntent == nil || digest(*r.ReviewReportIntent) != digest(intent) {
			return errors.New("recorded review report intent changed")
		}
		if r.ReviewReportReceipt != nil && r.ReviewReportReceipt.State != "waiting" {
			if !matchingReviewReport(intent, r.ReviewReportReceipt) {
				return errors.New("recorded review check does not match the accepted review")
			}
			receipt = *r.ReviewReportReceipt
			return nil
		}
		observed, e := d.ReportReview(ctx, grant, intent)
		if e != nil {
			receipt = ReviewReportReceipt{State: "waiting", CheckName: ReviewCheckName, HeadSHA: intent.Proposal.Commit, ProposalHash: intent.ProposalHash, ReviewDigest: intent.ReviewDigest, ExternalID: intent.ExternalID}
			r.ReviewReportReceipt = &receipt
			r.DeliveryState = "reporting"
			r.DeliveryReceipt = &DeliveryReceipt{State: "waiting", Reason: "Independent review check could not be confirmed: " + boundedReason(e.Error()), HeadSHA: intent.Proposal.Commit}
			return nil
		}
		if !matchingReviewReport(intent, &observed) {
			return errors.New("repository review report did not verify exact app-owned check evidence")
		}
		receipt = observed
		r.ReviewReportReceipt = &receipt
		return nil
	})
	return receipt, err
}
