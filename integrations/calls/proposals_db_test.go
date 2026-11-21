package calls

import (
	"context"
	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
	"testing"
	"time"
)

func TestProposalDatabaseApprovalIsSingleUseAccountBoundAndKeepsSpend(t *testing.T) {
	cw, k, _ := commerceFixture(t, "proposal-approval")
	if cw == nil {
		return
	}
	cleanupDelegationTestRows(t, cw)
	commerceVerified(t, cw, k)
	ctx := context.Background()
	grant := delegationTestRequest(cw)
	credential, grantID := delegationTestGrant(t, cw, k, grant)
	_, err := cw.svc.reserveDelegated(ctx, cw.ref, credential, callrules.AgentRequest{Action: "start", IdempotencyKey: "proposal-existing-spend"}, "usage", 300)
	require.NoError(t, err)
	proposal := callrules.AgentProposalRequest{Label: "One guided interview", Grant: grant, SkillID: "working-session", SkillVersion: 1, Participants: []callrules.ProposalParticipant{{Label: "Host", RoleID: "facilitator"}}}
	proposal.Grant.Scopes = append(proposal.Grant.Scopes, "skills_read", "skills_control")
	proposal.Grant.SkillVersions = []callrules.SkillVersionAuthority{{SkillID: proposal.SkillID, Version: 1}}
	requested, err := cw.svc.Agent(ctx, cw.ref, credential, callrules.AgentRequest{Action: "proposal", IdempotencyKey: "proposal-request-one", Proposal: &proposal})
	require.NoError(t, err)
	p := requested.(callrules.AgentProposal)
	require.Equal(t, "pending", p.Status)
	require.NotContains(t, p.ApprovalPath, credential)
	row, err := cw.svc.store.delegation(memql.ContextWithFreshRead(ctx), cw.owner, grantID)
	require.NoError(t, err)
	require.NotContains(t, row.State.Request.Scopes, "skills_control")
	other := accountRegister(t, cw, newSoftwareAuthenticator(t))
	_, err = cw.svc.Delegation(ctx, cw.ref, other.result.Session.Token, &callrules.DelegationRequest{Action: "proposal_review", ProposalID: p.ID})
	require.ErrorIs(t, err, callrules.ErrNotFound)
	approved, err := cw.svc.Delegation(ctx, cw.ref, k.result.Session.Token, &callrules.DelegationRequest{Action: "proposal_approve", ProposalID: p.ID})
	require.NoError(t, err)
	require.Equal(t, "approved", approved.Proposal.Status)
	require.EqualValues(t, 300, approved.Grants[0].SpentCents)
	_, err = cw.svc.Delegation(ctx, cw.ref, k.result.Session.Token, &callrules.DelegationRequest{Action: "proposal_approve", ProposalID: p.ID})
	require.ErrorContains(t, err, "proposal_not_pending")
	row, err = cw.svc.store.delegation(memql.ContextWithFreshRead(ctx), cw.owner, grantID)
	require.NoError(t, err)
	require.Contains(t, row.State.Request.Scopes, "skills_control")
	require.Len(t, row.State.Operations, 1)
}
func TestProposalDatabaseExpiryAndAgentCannotApprove(t *testing.T) {
	cw, k, _ := commerceFixture(t, "proposal-expiry")
	if cw == nil {
		return
	}
	cleanupDelegationTestRows(t, cw)
	commerceVerified(t, cw, k)
	ctx := context.Background()
	grant := delegationTestRequest(cw)
	credential, _ := delegationTestGrant(t, cw, k, grant)
	requested, err := cw.svc.Agent(ctx, cw.ref, credential, callrules.AgentRequest{Action: "proposal", IdempotencyKey: "proposal-request-expiring", Proposal: &callrules.AgentProposalRequest{Label: "Later call", Grant: grant}})
	require.NoError(t, err)
	p := requested.(callrules.AgentProposal)
	_, err = cw.svc.Delegation(ctx, cw.ref, credential, &callrules.DelegationRequest{Action: "proposal_approve", ProposalID: p.ID})
	require.Error(t, err)
	cw.clock.Advance(31 * time.Minute)
	reviewed, err := cw.svc.Delegation(ctx, cw.ref, k.result.Session.Token, &callrules.DelegationRequest{Action: "proposal_review", ProposalID: p.ID})
	require.NoError(t, err)
	require.Equal(t, "expired", reviewed.Proposal.Status)
	_, err = cw.svc.Delegation(ctx, cw.ref, k.result.Session.Token, &callrules.DelegationRequest{Action: "proposal_approve", ProposalID: p.ID})
	require.ErrorContains(t, err, "proposal_not_pending")
}
