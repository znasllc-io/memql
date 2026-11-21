package calls

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

func TestReportReceiptSurvivesGuestPasskeyClaimAndFreshLogin(t *testing.T) {
	cw, _, _, start, _ := processingFixture(t, false)
	if cw == nil {
		return
	}
	enableReports(t, cw)
	ctx := context.Background()
	joined, guest, err := cw.join(start.Call.GuestToken, "Reporting guest")
	require.NoError(t, err)
	ask := reportAsk(start, "report-before-claim-0001")
	ask.Identity = joined.Identity
	submitted := reportDo(t, cw, guest, "report_submit", ask)
	before, err := cw.svc.store.report(ctx, cw.owner, submitted.Receipt.ID)
	require.NoError(t, err)
	require.Equal(t, guest.KeyID, before.ReporterKeyID)
	require.NoError(t, cw.svc.Leave(ctx, guest))
	authn := newSoftwareAuthenticator(t)
	begin, err := cw.svc.Account(ctx, cw.ref, guest.Token, &callrules.AccountRequest{Action: "claim_begin"})
	require.NoError(t, err)
	challenge, rp, handle := accountCreation(t, begin)
	claimed, err := cw.svc.Account(ctx, cw.ref, guest.Token, &callrules.AccountRequest{Action: "claim_finish", ChallengeID: begin.ChallengeID, Label: "Keep report access", Credential: authn.Attest(challenge, rp, "https://"+rp)})
	require.NoError(t, err)
	require.Equal(t, guest.KeyID, claimed.Account.KeyID)
	require.NotEqual(t, guest.Token, claimed.Session.Token)
	require.Equal(t, submitted, reportDo(t, cw, *claimed.Session, "report_status", callrules.ReportRequest{ID: submitted.Receipt.ID}))
	_, err = cw.svc.report(ctx, cw.ref, guest.Token, "report_status", callrules.ReportRequest{ID: submitted.Receipt.ID})
	require.ErrorIs(t, err, callrules.ErrNoSession)
	logged, err := accountAssert(t, cw, accountTestKey{auth: authn, handle: handle, rp: rp, origin: "https://" + rp}, "", "login", "")
	require.NoError(t, err)
	require.Equal(t, guest.KeyID, logged.Account.KeyID)
	require.Equal(t, submitted, reportDo(t, cw, *logged.Session, "report_status", callrules.ReportRequest{ID: submitted.Receipt.ID}))
	listed := reportDo(t, cw, *logged.Session, "report_list", callrules.ReportRequest{})
	require.Equal(t, []callrules.ReportReceipt{*submitted.Receipt}, listed.Items)
	after, err := cw.svc.store.report(ctx, cw.owner, submitted.Receipt.ID)
	require.NoError(t, err)
	require.Equal(t, before.ReporterKeyID, after.ReporterKeyID)
	require.Equal(t, before.Version, after.Version)
}
