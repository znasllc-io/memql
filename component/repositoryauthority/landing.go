package repositoryauthority

import (
	"context"
	"errors"

	"github.com/znasllc-io/memql/core/common"
)

// Land is a separate human command, never an operation available to a request's
// drafting recipe. The UI confirms the exact saved commit; it supplies no files,
// credentials, destination branch or substituted owner. Retrying reconciles only
// that same effect, including after the provider reply or journal commit is lost.
func (i *Integration) Land(ctx context.Context, id, expectedCommit string) (Receipt, error) {
	actor, err := person(ctx, "execute", RequestCapability)
	if err != nil {
		return Receipt{}, err
	}
	if _, running := common.RunFromContext(ctx); running {
		return Receipt{}, errors.New("owner landing requires a separate interactive command")
	}
	if !commit.MatchString(expectedCommit) {
		return Receipt{}, errors.New("confirm the exact reviewed proposal commit")
	}
	r, err := i.get(ctx, id, actor)
	if err != nil {
		return Receipt{}, err
	}
	if !same(actor, r.Maintainer.OwnerUserID) || r.Route.Remote != "" {
		return Receipt{}, errors.New("only this proposal's connected maintainer can land it")
	}
	// A completed receipt is history, not renewed write authority.
	if r.ProposalReceipt != nil && r.ProposalReceipt.Commit == expectedCommit && r.ProposalReceipt.LandedCommit == expectedCommit {
		return r.receipt(actor), nil
	}
	if i.deps().Land == nil {
		return Receipt{}, errors.New("owner landing is unavailable")
	}
	r, err = i.update(ctx, id, func(current *record) error {
		authorize := func(c context.Context) error {
			observed, e := i.observe(c, *current)
			if e != nil {
				return e
			}
			if observed.State != "approved" || observed.Proposal == nil || observed.ProposalReceipt == nil || observed.ProposalReceipt.State != "complete" || observed.ProposalReceipt.LandingMode != "owner_push" || observed.ProposalReceipt.Commit != expectedCommit || observed.ProposalHash != digest(*observed.Proposal) {
				return errors.New("owner landing requires the exact completed, currently approved proposal")
			}
			_, e = i.currentAuthority(c, observed)
			return e
		}
		if current.ProposalReceipt != nil && current.ProposalReceipt.Commit == expectedCommit && current.ProposalReceipt.LandedCommit == expectedCommit {
			return nil
		}
		if e := authorize(ctx); e != nil {
			return e
		}
		grant, e := i.currentAuthority(ctx, *current)
		if e != nil {
			return e
		}
		grant.EffectID, grant.EffectCreatedAt = current.ProposalHash, current.ProposalCreatedAt
		landed, e := i.deps().Land(ctx, grant, *current.Proposal, *current.ProposalReceipt, authorize)
		if e != nil {
			return e
		}
		if landed != expectedCommit {
			return errors.New("landing adapter returned a different commit")
		}
		current.ProposalReceipt.LandedCommit = landed
		return nil
	})
	return r.receipt(actor), err
}
