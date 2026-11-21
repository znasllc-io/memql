package calls

import (
	"context"
	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"testing"
)

func TestFamilyClaimFailureDoesNotDiscloseUnboundCookieAndNextCallUsesFreshSession(t *testing.T) {
	cw, k, start, r := familyFixture(t, "fam-claim-retry")
	if cw == nil {
		return
	}
	ctx := context.Background()
	req := familyGuardianRequest(cw, k, start, "invite")
	invited, err := cw.svc.Family(ctx, cw.ref, k.result.Session.Token, req)
	require.NoError(t, err)
	broken, injected := brokenMeterService(cw, "saveCallsFamily(")
	failed, err := broken.Family(ctx, cw.ref, "", callrules.FamilyRequest{Action: "claim", InvitationToken: invited.Invitation.Token})
	require.Error(t, err)
	require.True(t, injected.failed)
	require.Nil(t, failed.Session, "an unbound child cookie must never escape a partial write")
	retry, err := cw.svc.Family(ctx, cw.ref, "", callrules.FamilyRequest{Action: "claim", InvitationToken: invited.Invitation.Token})
	require.NoError(t, err)
	require.NotNil(t, retry.Session)
	oldChild := *retry.Session
	// A second invitation cannot move a child away from an active family call.
	req.IdempotencyKey = "family-second-invite"
	second, err := cw.svc.Family(ctx, cw.ref, k.result.Session.Token, req)
	require.NoError(t, err)
	_, err = cw.svc.Family(ctx, cw.ref, oldChild.Token, callrules.FamilyRequest{Action: "claim", InvitationToken: second.Invitation.Token})
	require.ErrorContains(t, err, "family_separate_device_required")
	require.NoError(t, cw.svc.closeFamily(ctx, r, "test-ended"))
	quote, err := cw.svc.Account(ctx, cw.ref, k.result.Session.Token, &callrules.AccountRequest{Action: "quote_call", AccountID: k.result.Account.AccountID, ParticipantLimit: 4, DurationMinutes: 60, ParticipationMode: familyMode})
	require.NoError(t, err)
	next, err := cw.svc.Start(ctx, *k.result.Session, callrules.StartBody{Name: "Guardian", Confirm: confirmed, QuoteID: quote.Quote.QuoteID, IdempotencyKey: "family-second-start"})
	require.NoError(t, err)
	nextInvite, err := cw.svc.Family(ctx, cw.ref, k.result.Session.Token, familyGuardianRequest(cw, k, next, "invite"))
	require.NoError(t, err)
	nextClaim, err := cw.svc.Family(ctx, cw.ref, oldChild.Token, callrules.FamilyRequest{Action: "claim", InvitationToken: nextInvite.Invitation.Token})
	require.NoError(t, err)
	require.NotNil(t, nextClaim.Session)
	require.NotEqual(t, oldChild.Token, nextClaim.Session.Token)
	restored, err := cw.svc.FamilySession(ctx, cw.ref, nextClaim.Session.Token)
	require.NoError(t, err)
	require.Equal(t, nextInvite.Invitation.ID, restored.InvitationID)
	prior, err := cw.svc.FamilySession(ctx, cw.ref, oldChild.Token)
	require.NoError(t, err)
	require.Equal(t, invited.Invitation.ID, prior.InvitationID)
	require.Equal(t, "ended", prior.Status)
}
