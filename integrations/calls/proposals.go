package calls

import (
	"context"
	"encoding/json"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
	"slices"
	"strings"
	"time"
)

type delegationProposal struct {
	Proposal            callrules.AgentProposal
	OperationID, Digest string
}

func validProposal(p callrules.AgentProposalRequest, now time.Time) bool {
	if len(p.Label) == 0 || len(p.Label) > 120 || p.Analysis || !callrules.ValidDelegationApproval(p.Grant, now) || len(p.Participants) > 100 || len(p.Resources) > 32 {
		return false
	}
	if p.SkillID != "" && !slices.Contains(p.Grant.SkillVersions, callrules.SkillVersionAuthority{SkillID: p.SkillID, Version: p.SkillVersion}) {
		return false
	}
	for _, r := range p.Resources {
		if !slices.Contains(p.Grant.ResourceIDs, r) {
			return false
		}
	}
	for _, participant := range p.Participants {
		if participant.Label == "" || len(participant.Label) > 120 || !callrules.ValidWorkflowID(participant.RoleID) {
			return false
		}
		if participant.Email != "" && !callrules.DelegationRecipientAllowed(p.Grant, participant.Email) {
			return false
		}
	}
	// A displayed task time is an enforced single-booking constraint. Standing
	// grants omit both fields; the review must not imply a decorative time is a cap.
	return p.Start.Equal(p.Grant.ScheduleStart)
}
func (s *Service) agentProposal(ctx context.Context, site callrules.SiteRef, token string, d *delegationRow, req callrules.AgentRequest) (callrules.AgentProposal, error) {
	var none callrules.AgentProposal
	release, err := s.gates(ctx, delegationGate(d.ID))
	if err != nil {
		return none, err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	d, _, err = s.agentAuthority(fresh, site, token)
	if err != nil {
		return none, err
	}
	if req.Action == "proposal_status" {
		for _, p := range d.State.Proposals {
			if p.Proposal.ID == req.ProposalID {
				return proposalView(p.Proposal, s.now()), nil
			}
		}
		return none, callrules.ErrNotFound
	}
	if req.Proposal == nil || !fundingOperationID.MatchString(req.IdempotencyKey) || !validProposal(*req.Proposal, s.now()) {
		return none, callrules.ErrInvalid
	}
	if err = s.approvedPortrait(fresh, site, d.KeyID, req.Proposal.Grant); err != nil {
		return none, err
	}
	raw, _ := json.Marshal(req.Proposal)
	digest := callrules.HistoryDigest(nil, "proposal", string(raw))
	for _, p := range d.State.Proposals {
		if p.OperationID == req.IdempotencyKey {
			if p.Digest != digest {
				return none, fundingConflict("idempotency_conflict")
			}
			return proposalView(p.Proposal, s.now()), nil
		}
	}
	if len(d.State.Proposals) >= 20 {
		return none, accountRefusal("proposal_limit")
	}
	proposalID := d.ID + "." + id.NewShortId()
	p := callrules.AgentProposal{AgentProposalRequest: *req.Proposal, ID: proposalID, RequestingAgent: d.State.Request.Label, Status: "pending", ExpiresAt: s.now().Add(30 * time.Minute), ApprovalPath: "/?approval=" + proposalID, ReviewDigest: digest}
	d.State.Proposals = append(d.State.Proposals, delegationProposal{Proposal: p, OperationID: req.IdempotencyKey, Digest: digest})
	if err = s.store.saveDelegation(fresh, site.OwnerUserID, d, s.now()); err != nil {
		return none, err
	}
	return p, nil
}
func proposalView(p callrules.AgentProposal, now time.Time) callrules.AgentProposal {
	if p.Status == "pending" && !now.Before(p.ExpiresAt) {
		p.Status = "expired"
	}
	return p
}
func (s *Service) reviewAgentProposal(ctx context.Context, site callrules.SiteRef, token string, h keyHolder, req callrules.DelegationRequest) (callrules.DelegationResult, error) {
	out := callrules.DelegationResult{Grants: []callrules.DelegationInfo{}, Policy: callrules.CurrentAgentPolicy()}
	parts := strings.Split(req.ProposalID, ".")
	if len(parts) != 2 || !callrules.ValidWorkflowID(parts[0]) || !callrules.ValidWorkflowID(parts[1]) || (req.Action != "proposal_review" && req.Action != "proposal_approve" && req.Action != "proposal_reject") {
		return out, callrules.ErrInvalid
	}
	release, err := s.gates(ctx, accountGate(h.row.KeyID), keyGate(h.row.KeyID), delegationGate(parts[0]))
	if err != nil {
		return out, err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	h, err = s.authenticatedKey(fresh, callrules.Session{Site: site, Token: token})
	if err != nil {
		return out, err
	}
	d, err := s.store.delegation(fresh, site.OwnerUserID, parts[0])
	if err != nil {
		return out, err
	}
	if d == nil || d.SiteID != site.SiteID || d.KeyID != h.row.KeyID {
		return out, callrules.ErrNotFound
	}
	var entry *delegationProposal
	for i := range d.State.Proposals {
		if d.State.Proposals[i].Proposal.ID == req.ProposalID {
			entry = &d.State.Proposals[i]
			break
		}
	}
	if entry == nil {
		return out, callrules.ErrNotFound
	}
	view := proposalView(entry.Proposal, s.now())
	out.Proposal = &view
	if req.Action == "proposal_review" {
		return out, nil
	}
	if view.Status != "pending" {
		return out, accountRefusal("proposal_not_pending")
	}
	if req.ReviewDigest == "" || req.ReviewDigest != entry.Digest {
		return out, accountRefusal("proposal_review_changed")
	}
	recent, key, _, err := s.recentlyVerified(fresh, site, token)
	if err != nil {
		return out, err
	}
	if !keyActive(key.Status) || !d.State.RevokedAt.IsZero() || !s.now().Before(d.State.Request.ExpiresAt) {
		return out, accountRefusal("delegation_inactive")
	}
	if req.Action == "proposal_reject" {
		entry.Proposal.Status = "rejected"
	} else {
		if err = s.commerceAdultAllowed(fresh, site, key.ID); err != nil {
			return out, err
		}
		grant := entry.Proposal.Grant
		if err = s.approvedPortrait(fresh, site, key.ID, grant); err != nil {
			return out, err
		}
		if !validProposal(entry.Proposal.AgentProposalRequest, s.now()) {
			return out, accountRefusal("proposal_expired")
		}
		// Expanded approval never resets old consumption. The new ceilings must cover
		// all pending usage and saved-method charges, including earlier approvals.
		usagePeriod, usageTotal := delegationUsage(d, "usage", s.now())
		topPeriod, topTotal := delegationUsage(d, "topup", s.now())
		if usagePeriod > grant.PeriodLimitCents || usageTotal > grant.LifetimeLimitCents || grant.Topup.Enabled && (topPeriod > grant.Topup.PeriodLimitCents || topTotal > grant.Topup.LifetimeLimitCents) {
			return out, accountRefusal("proposal_below_spent")
		}
		if grant.Topup.Enabled {
			funds, e := s.store.commerceState(fresh, site.OwnerUserID, site.SiteID, key.ID)
			if e != nil {
				return out, e
			}
			if funds == nil || funds.CustomerID == "" || funds.PaymentMethodID == "" {
				return out, accountRefusal("saved_payment_method_required")
			}
			d.State.CustomerID, d.State.PaymentMethodID = funds.CustomerID, funds.PaymentMethodID
		}
		d.State.Request = grant
		d.State.ApprovalRevision++
		d.State.ApprovalPolicyDigest = callrules.AgentConductDigest
		d.State.ApprovedAt = s.now()
		d.State.CredentialID = recent.row.CredentialID
		entry.Proposal.Status = "approved"
	}
	if err = s.store.saveDelegation(fresh, site.OwnerUserID, d, s.now()); err != nil {
		return out, err
	}
	view = entry.Proposal
	out.Proposal = &view
	out.Grants = append(out.Grants, delegationInfo(d, s.now()))
	return out, nil
}
