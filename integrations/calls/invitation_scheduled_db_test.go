package calls

import (
	"context"
	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/integrations/email"
	"testing"
	"time"
)

func TestScheduledInvitationDatabaseUsesOwnedBookingAndDeduplicates(t *testing.T) {
	cw, k, _ := commerceFixture(t, "scheduled-email")
	if cw == nil {
		return
	}
	commerceVerified(t, cw, k)
	ctx := context.Background()
	q := meteredQuote(t, cw, k, 4, 60)
	booking, err := cw.svc.Schedule(ctx, *k.result.Session, callrules.ScheduleRequest{Start: cw.clock.Now().Add(10 * time.Minute), Minutes: 60, Title: "Interview", QuoteID: q.QuoteID, IdempotencyKey: "scheduled-email-booking"})
	require.NoError(t, err)
	sender := &invitationMail{}
	svc := cw.svc.WithInvitationSender(func() email.Sender { return sender })
	req := callrules.InviteRequest{Email: "guest@example.test", Token: booking.GuestToken, BookingID: booking.ID}
	out, err := svc.Invite(ctx, *k.result.Session, req)
	require.NoError(t, err)
	require.Equal(t, "sent", out.Status)
	out, err = svc.Invite(ctx, *k.result.Session, req)
	require.NoError(t, err)
	require.True(t, out.Duplicate)
	require.Equal(t, 1, sender.count())
	require.Contains(t, sender.messages[0].TextBody, booking.Start.UTC().Format(time.RFC1123))
	require.NoError(t, cw.svc.Cancel(ctx, *k.result.Session, booking.ID))
	req.Email = "another@example.test"
	_, err = svc.Invite(ctx, *k.result.Session, req)
	require.Error(t, err)
	require.Equal(t, 1, sender.count())
}
