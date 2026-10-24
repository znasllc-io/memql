package repositoryauthority

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/core/common"
)

// Review executes an independent, tool-free model evaluation of exact bytes.
// Its verdict comes only from the captured native reviewer, never from args.
func (i *Integration) Review(ctx context.Context, p Proposal) (ReviewReceipt, error) {
	paths, err := validateProposal(p)
	if err != nil {
		return ReviewReceipt{}, err
	}
	grant, err := i.AuthorizeEffect(ctx, p.RequestID, OperationDeliver, paths)
	if err != nil {
		return ReviewReceipt{}, err
	}
	d := i.deps()
	if d.Review == nil {
		return ReviewReceipt{}, errors.New("independent repository review is unavailable")
	}
	r, err := i.get(ctx, p.RequestID, grant.Source.OwnerUserID)
	if err != nil {
		return ReviewReceipt{}, err
	}
	hash := digest(p)
	if r.ReviewHash != "" {
		if r.ReviewHash != hash {
			return ReviewReceipt{}, errors.New("request already captured different review bytes")
		}
		if r.ReviewReceipt != nil {
			return *r.ReviewReceipt, nil
		}
	}
	source, err := i.ReadContext(ctx, p.RequestID)
	if err != nil {
		return ReviewReceipt{}, err
	}
	if source.ExpectedHead != p.ExpectedHead || source.BaseBranch != p.BaseBranch {
		return ReviewReceipt{}, errors.New("review source moved since the proposal was drafted")
	}
	input := ReviewInput{Statement: r.Request.Statement, Repository: r.Source.Repository, Operation: r.Request.Operation, Paths: append([]string(nil), r.Request.Paths...), Source: source, Proposal: p}
	_, err = i.update(ctx, r.ID, func(current *record) error {
		if current.ReviewHash != "" && current.ReviewHash != hash {
			return errors.New("request already captured different review bytes")
		}
		if current.ReviewHash == "" {
			current.ReviewHash, current.ReviewInput = hash, &input
		}
		return nil
	})
	if err != nil {
		return ReviewReceipt{}, err
	}
	var receipt ReviewReceipt
	_, err = i.update(ctx, r.ID, func(current *record) error {
		if current.ReviewHash != hash || current.ReviewInput == nil {
			return errors.New("review identity changed")
		}
		if current.ReviewReceipt != nil {
			receipt = *current.ReviewReceipt
			return nil
		}
		if _, e := i.AuthorizeEffect(ctx, r.ID, OperationDeliver, paths); e != nil {
			return e
		}
		reviewed, e := d.Review(ctx, grant, current.Route.ReviewPrompt, *current.ReviewInput)
		if e != nil {
			return e
		}
		if strings.TrimSpace(reviewed.Reason) == "" || len(reviewed.Reason) > 4000 {
			return errors.New("independent review returned no bounded explanation")
		}
		receipt = ReviewReceipt{ProposalHash: hash, Approved: reviewed.Approved, Reason: reviewed.Reason}
		current.ReviewReceipt = &receipt
		return nil
	})
	return receipt, err
}

type deliveryRecoveryKey struct{}
type deliveryRecovery struct{ id, lease string }

// AuthorizeReviewReport is read-only so adapters can recheck inside the journal's
// effect lock. A recovery fence is private to the leased reporting and queue workflow;
// it cannot authorize proposal writes or another request.
func (i *Integration) AuthorizeReviewReport(ctx context.Context, id string) (Authorization, error) {
	actor, err := person(ctx, "", "")
	if err != nil {
		return Authorization{}, err
	}
	r, err := i.get(ctx, id, actor)
	if err != nil {
		return Authorization{}, err
	}
	if r.Request.Operation != OperationDeliver || r.Proposal == nil || r.ProposalReceipt == nil || r.ReviewReceipt == nil || r.ReviewInput == nil || len(r.Route.ReviewPrompt) == 0 || digest(r.ReviewInput.Proposal) != r.ProposalHash || !r.ReviewReceipt.Approved || r.ReviewReceipt.ProposalHash != r.ProposalHash || r.ReviewHash != r.ProposalHash || digest(*r.Proposal) != r.ProposalHash || r.ProposalReceipt.State != "complete" || !commit.MatchString(r.ProposalReceipt.Commit) {
		return Authorization{}, errors.New("queue admission requires delivered scope, exact proposal and independent review")
	}
	var grant Authorization
	if recovered, ok := ctx.Value(deliveryRecoveryKey{}).(deliveryRecovery); ok {
		run, hasRun := common.RunFromContext(ctx)
		if recovered.id != r.ID || recovered.lease == "" || recovered.lease != r.DeliveryLease || !i.now().Before(r.DeliveryLeaseUntil) || !hasRun || run.Mode != common.RunModeLive || !same(actor, r.Maintainer.OwnerUserID) || !same(run.OwnerUserId, r.Maintainer.OwnerUserID) || !same(run.GoalId, r.GoalID) || !same(run.RunId, r.RunID) {
			return Authorization{}, errors.New("delivery recovery is outside its leased execution")
		}
		if i.deps().DeliveryContext == nil {
			return Authorization{}, errors.New("delivery lifecycle authority is unavailable")
		}
		if _, err = i.deps().DeliveryContext(ctx, r.Maintainer.OwnerUserID, r.GoalID, r.RunID); err != nil {
			return Authorization{}, err
		}
		observed, e := i.observe(ctx, r)
		if e != nil {
			return Authorization{}, e
		}
		if observed.State != "approved" {
			return Authorization{}, fmt.Errorf("delivery request is %s", observed.State)
		}
		grant, err = i.currentAuthority(ctx, r)
	} else {
		grant, err = i.AuthorizeEffect(ctx, id, OperationDeliver, r.Request.Paths)
	}
	if err != nil {
		return Authorization{}, err
	}
	grant.EffectID, grant.EffectCreatedAt = r.ProposalHash, r.ProposalCreatedAt
	return grant, nil
}

// AuthorizeDelivery adds the durable provider review receipt to the independent
// review and current maintainer gates. A green check supplied by a caller cannot
// substitute for this journal evidence.
func (i *Integration) AuthorizeDelivery(ctx context.Context, id string) (Authorization, error) {
	grant, err := i.AuthorizeReviewReport(ctx, id)
	if err != nil {
		return Authorization{}, err
	}
	r, err := i.get(ctx, id, grant.Source.OwnerUserID)
	if err != nil {
		return Authorization{}, err
	}
	intent := reviewReportIntent(r)
	if r.ReviewReportIntent == nil || digest(*r.ReviewReportIntent) != digest(intent) || !matchingReviewReport(intent, r.ReviewReportReceipt) {
		return Authorization{}, errors.New("queue admission requires the exact app-owned independent review check receipt")
	}
	return grant, nil
}

// Queue arms one durable delivery intent. Pending checks and uncertain provider
// responses are observed again under the same PR/head, never by regenerating a
// branch or weakening repository rules. Queue admission is not a merge receipt.
func (i *Integration) Queue(ctx context.Context, id string) (DeliveryReceipt, error) {
	grant, err := i.AuthorizeDelivery(ctx, id)
	if err != nil {
		return DeliveryReceipt{}, err
	}
	d := i.deps()
	if d.Queue == nil {
		return DeliveryReceipt{}, errors.New("repository queue adapter is unavailable")
	}
	_, err = i.update(ctx, id, func(r *record) error {
		if r.DeliveryState == "" {
			r.DeliveryState = "pending"
			r.DeliveryReceipt = &DeliveryReceipt{State: "waiting", Reason: "Waiting for required repository checks and reviews.", HeadSHA: r.ProposalReceipt.Commit}
		}
		return nil
	})
	if err != nil {
		return DeliveryReceipt{}, err
	}
	var receipt DeliveryReceipt
	_, err = i.update(ctx, id, func(r *record) error {
		if _, e := i.AuthorizeDelivery(ctx, id); e != nil {
			return e
		}
		if r.DeliveryState == "merged" || r.DeliveryState == "blocked" {
			receipt = *r.DeliveryReceipt
			return nil
		}
		observeOnly := r.DeliveryAdmitted || r.DeliveryState == "queued"
		r.DeliveryAdmitted = observeOnly
		observed, e := d.Queue(ctx, grant, DeliveryIntent{ReviewReport: *r.ReviewReportReceipt, ObserveOnly: observeOnly, ProposalHash: r.ProposalHash, Proposal: *r.ProposalReceipt, BaseBranch: r.Proposal.BaseBranch})
		if e != nil {
			// Store a recoverable identity even when the provider response was lost.
			receipt = DeliveryReceipt{State: "waiting", Reason: "Repository service could not confirm queue admission; the same pull request will be checked again.", HeadSHA: r.ProposalReceipt.Commit}
			r.DeliveryState, r.DeliveryReceipt = "waiting", &receipt
			if observeOnly {
				receipt.Reason = "This change was admitted earlier; the repository service could not confirm its current queue or merge state. Read-only observation will continue."
				r.DeliveryState = "queued"
			}
			return nil
		}
		if observed.State != "waiting" && observed.State != "attention" && observed.State != "queued" && observed.State != "merged" {
			return errors.New("queue adapter returned an unknown state")
		}
		if observed.HeadSHA == "" || (observed.HeadSHA != r.ProposalReceipt.Commit && observed.State != "attention") {
			return errors.New("queue adapter returned no matching proposal head")
		}
		if observed.State == "queued" && observed.QueueEntryID == "" {
			return errors.New("queue adapter returned no durable queue identity")
		}
		receipt = observed
		r.DeliveryState, r.DeliveryReceipt = observed.State, &receipt
		if observed.State == "queued" {
			r.DeliveryAdmitted = true
		}
		if observeOnly && observed.State == "waiting" {
			r.DeliveryState = "queued"
		}
		if (observeOnly && observed.State == "attention") || observed.HeadSHA != r.ProposalReceipt.Commit {
			r.DeliveryState = "blocked"
		}
		return nil
	})
	return receipt, err
}

// RecoverDeliveries executes only captured report/queue workflows for previously armed
// requests. The scheduler supplies no target, owner, credential or approval.
func (i *Integration) RecoverDeliveries(ctx context.Context) (int, error) {
	if !auth.OriginFromContext(ctx).IsInternal() {
		return 0, errors.New("delivery recovery requires the internal scheduler")
	}
	d := i.deps()
	if d.DeliveryContext == nil {
		return 0, errors.New("delivery lifecycle authority is unavailable")
	}
	records, err := i.claimDeliveries(ctx, 25)
	if err != nil {
		return 0, err
	}
	batchCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	completed := 0
	for _, r := range records {
		if batchCtx.Err() != nil {
			break
		}
		ownerCtx, e := d.DeliveryContext(batchCtx, r.Maintainer.OwnerUserID, r.GoalID, r.RunID)
		if e == nil {
			ownerCtx = context.WithValue(ownerCtx, deliveryRecoveryKey{}, deliveryRecovery{id: r.ID, lease: r.DeliveryLease})
			if _, e = i.AuthorizeReviewReport(ownerCtx, r.ID); e == nil {
				var snapshot *workflowhost.Snapshot
				snapshot, e = workflowhost.SnapshotFromMap(r.Route.DeliveryRecipe)
				if e == nil {
					called := map[string]bool{}
					operation := func(name string, run func(context.Context, string) (any, error)) workflowhost.Operation {
						return func(c context.Context, a map[string]any) (any, error) {
							if called[name] {
								return nil, errors.New("delivery recipe exceeded one " + name + " effect per lease")
							}
							called[name] = true
							if str(a["requestId"]) != r.ID {
								return nil, errors.New("delivery recipe selected another request")
							}
							return run(c, r.ID)
						}
					}
					_, e = workflowhost.RunSnapshot(ownerCtx, snapshot, deliveryContract, map[string]any{"requestId": r.ID}, workflowhost.Options{Operations: map[string]workflowhost.Operation{
						"repositoryChangeReportReview": operation("report", func(c context.Context, id string) (any, error) {
							v, e := i.ReportReview(c, id)
							if e != nil {
								return nil, e
							}
							return decoded[map[string]any](v)
						}),
						"repositoryChangeQueue": operation("queue", func(c context.Context, id string) (any, error) {
							v, e := i.Queue(c, id)
							if e != nil {
								return nil, e
							}
							return decoded[map[string]any](v)
						}),
					}})
				}
			}
		}
		_, releaseErr := i.update(ctx, r.ID, func(current *record) error {
			if current.DeliveryLease != r.DeliveryLease {
				return nil
			}
			current.DeliveryLease = ""
			// Bound cadence natively against repeated triggers without deciding policy.
			// The installed schedule selects when the next eligible observation occurs.
			current.DeliveryLeaseUntil = i.now().Add(time.Minute)
			if e != nil {
				current.DeliveryReceipt = &DeliveryReceipt{State: "attention", Reason: "Delivery remains blocked: " + boundedReason(e.Error()), HeadSHA: current.ProposalReceipt.Commit}
				current.DeliveryState = "attention"
			}
			return nil
		})
		if releaseErr != nil {
			return completed, releaseErr
		}
		if e == nil {
			completed++
		}
	}
	return completed, nil
}
func boundedReason(s string) string {
	if len(s) > 1000 {
		return s[:1000]
	}
	return s
}
