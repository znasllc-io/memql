package calls

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/email"
)

// The provider is the only fake. Auth, token resolution, serialization, private
// receipt writes, version fences, and rate windows use the real engine and DB.
type invitationMail struct {
	mu         sync.Mutex
	messages   []email.Message
	err        error
	entered    chan struct{}
	proceed    chan struct{}
	reserved   bool
	released   bool
	outcome    email.Outcome
	reserveErr error
}

func (m *invitationMail) Reserve(ctx context.Context, _ email.Message, _ email.SendAs, _ ...string) (context.Context, func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reserveErr != nil {
		return nil, nil, m.reserveErr
	}
	m.reserved, m.released = true, false
	return ctx, func() { m.mu.Lock(); m.released = true; m.mu.Unlock() }, nil
}

func (m *invitationMail) Send(ctx context.Context, msg email.Message, as email.SendAs) error {
	m.mu.Lock()
	m.messages = append(m.messages, msg)
	err, entered, proceed := m.err, m.entered, m.proceed
	m.mu.Unlock()
	if entered != nil {
		close(entered)
	}
	if proceed != nil {
		select {
		case <-proceed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if as.Address != "" || as.FromName != "" {
		return errors.New("sender identity must come from configured transport")
	}
	return err
}

func (m *invitationMail) DeliveryOutcome(context.Context, email.Message, email.SendAs) (email.Outcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.released {
		return email.OutcomeUnknown, errors.New("provider is still reserved")
	}
	return m.outcome, nil
}

func (m *invitationMail) count() int { m.mu.Lock(); defer m.mu.Unlock(); return len(m.messages) }

func invitationWorld(t *testing.T, name string) (*callWorld, *Service, *invitationMail) {
	t.Helper()
	cw := newCallWorld(t, name, 12)
	if cw == nil {
		return nil, nil, nil
	}
	t.Cleanup(func() {
		for _, site := range cw.sites {
			_, _ = cw.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("concept IN (?, ?)", invitationConcept, invitationBudgetConcept).Where("payload->>'siteId' = ?", siteConcept+":"+site).Exec(context.Background())
		}
	})
	sender := &invitationMail{}
	return cw, cw.svc.WithInvitationSender(func() email.Sender { return sender }), sender
}

func TestInvitationConcurrentReplicasSendOneCanonicalRecipient(t *testing.T) {
	cw, a, sender := invitationWorld(t, "invite-concurrent")
	if cw == nil {
		return
	}
	require.Nil(t, cw.svc.invitationSender, "wiring must not mutate the shared service")
	b := secondReplica(t, cw.eng).Service().WithInvitationSender(func() email.Sender { return sender })
	host, start := cw.started("Host")
	sender.entered, sender.proceed = make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-sender.proceed:
		default:
			close(sender.proceed)
		}
	})
	type answer struct {
		result callrules.InviteResult
		err    error
	}
	first := make(chan answer, 1)
	go func() {
		r, err := a.Invite(context.Background(), host, callrules.InviteRequest{Email: "Person@Example.Test", Token: start.Call.GuestToken})
		first <- answer{r, err}
	}()
	select {
	case <-sender.entered:
	case reply := <-first:
		t.Fatalf("first send did not reach transport: %v", reply.err)
	case <-time.After(10 * time.Second):
		t.Fatal("first send timed out")
	}
	var wg sync.WaitGroup
	replies := make(chan answer, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := b.Invite(context.Background(), host, callrules.InviteRequest{Email: " person@example.test ", Token: start.Call.GuestToken})
			replies <- answer{r, err}
		}()
	}
	wg.Wait()
	close(replies)
	for reply := range replies {
		require.NoError(t, reply.err)
		require.Equal(t, callrules.InviteResult{Status: "pending", Duplicate: true}, reply.result)
	}
	close(sender.proceed)
	result := <-first
	require.NoError(t, result.err)
	require.Equal(t, callrules.InviteResult{Status: "sent"}, result.result)
	require.True(t, sender.released)
	require.Equal(t, 1, sender.count())
	_, cohost, err := cw.svc.Join(context.Background(), cw.ref, nil, callrules.JoinBody{HostToken: start.Call.HostToken, Name: "Co-host", Confirm: confirmed})
	require.NoError(t, err)
	result.result, result.err = b.Invite(context.Background(), cohost, callrules.InviteRequest{Email: "PERSON@example.test", Token: start.Call.GuestToken})
	require.NoError(t, result.err)
	require.Equal(t, callrules.InviteResult{Status: "sent", Duplicate: true}, result.result)
	receipts := cw.rowsOf(invitationConcept)
	require.Len(t, receipts, 1)
	for _, row := range receipts {
		text := fmt.Sprint(row)
		require.NotContains(t, text, "person@example.test")
		require.NotContains(t, text, start.Call.GuestToken)
		require.NotContains(t, text, start.Call.HostToken)
	}
	require.Equal(t, "person@example.test", sender.messages[0].To)
	require.Contains(t, sender.messages[0].TextBody, "#j="+start.Call.GuestToken)
	require.NotContains(t, sender.messages[0].HTMLBody, start.Call.HostToken)
}

func TestInvitationRequiresCurrentHostAndMatchingGuestToken(t *testing.T) {
	cw, svc, sender := invitationWorld(t, "invite-auth")
	if cw == nil {
		return
	}
	host, start := cw.started("Host")
	_, other := cw.started("Other host")
	_, guest, err := cw.join(start.Call.GuestToken, "Guest")
	require.NoError(t, err)
	for _, bad := range []struct {
		session callrules.Session
		token   string
		want    error
	}{
		{guest, start.Call.GuestToken, callrules.ErrForbidden},
		{host, start.Call.HostToken, callrules.ErrInvalid},
		{host, other.Call.GuestToken, callrules.ErrInvalid},
	} {
		_, err := svc.Invite(context.Background(), bad.session, callrules.InviteRequest{Email: "person@example.test", Token: bad.token})
		require.ErrorIs(t, err, bad.want)
	}
	require.NoError(t, cw.svc.Leave(context.Background(), host))
	_, err = svc.Invite(context.Background(), host, callrules.InviteRequest{Email: "person@example.test", Token: start.Call.GuestToken})
	require.Error(t, err)
	require.Zero(t, sender.count())
	require.Empty(t, cw.rowsOf(invitationConcept))
}

func TestInvitationKnownFailureCanRetryButAmbiguousCannot(t *testing.T) {
	for _, known := range []bool{true, false} {
		t.Run(fmt.Sprintf("notAccepted=%v", known), func(t *testing.T) {
			cw, svc, sender := invitationWorld(t, "invite-retry")
			if cw == nil {
				return
			}
			host, start := cw.started("Host")
			req := callrules.InviteRequest{Email: "person@example.test", Token: start.Call.GuestToken}
			sender.err = errors.New("connection lost after submitting")
			if known {
				sender.err = &email.SendError{NotAccepted: true, Detail: "provider refused before accepting"}
			}
			r, err := svc.Invite(context.Background(), host, req)
			if known {
				require.ErrorIs(t, err, callrules.ErrInvitationUnavailable)
				_, err = svc.Invite(context.Background(), host, req)
				var limit *callrules.InvitationLimit
				require.ErrorAs(t, err, &limit)
				require.Equal(t, 30*time.Second, limit.RetryAfter)
			} else {
				require.NoError(t, err)
				require.Equal(t, "unknown", r.Status)
			}
			cw.clock.Advance(time.Minute)
			sender.err = nil
			r, err = svc.Invite(context.Background(), host, req)
			require.NoError(t, err)
			if known {
				require.Equal(t, "sent", r.Status)
				require.Equal(t, 2, sender.count())
			} else {
				require.Equal(t, callrules.InviteResult{Status: "unknown", Duplicate: true}, r)
				require.Equal(t, 1, sender.count())
			}
		})
	}
}

func TestInvitationProviderBusyDoesNotStrandReceiptAndReconcilesAcceptance(t *testing.T) {
	cw, svc, sender := invitationWorld(t, "invite-reserve")
	if cw == nil {
		return
	}
	host, start := cw.started("Host")
	req := callrules.InviteRequest{Email: "person@example.test", Token: start.Call.GuestToken}
	sender.reserveErr = errors.New("provider reservation busy")
	_, err := svc.Invite(context.Background(), host, req)
	require.ErrorIs(t, err, callrules.ErrInvitationUnavailable)
	require.Empty(t, cw.rowsOf(invitationConcept))
	require.Empty(t, cw.rowsOf(invitationBudgetConcept))
	sender.reserveErr = &email.SendError{NotAccepted: true, Throttled: true, RetryAfter: 45 * time.Second, Detail: "provider asked to wait"}
	_, err = svc.Invite(context.Background(), host, req)
	var wait *callrules.InvitationLimit
	require.ErrorAs(t, err, &wait)
	require.Equal(t, 45*time.Second, wait.RetryAfter)
	require.Empty(t, cw.rowsOf(invitationConcept))
	require.Empty(t, cw.rowsOf(invitationBudgetConcept))
	sender.reserveErr = nil
	sender.err = context.DeadlineExceeded
	r, err := svc.Invite(context.Background(), host, req)
	require.NoError(t, err)
	require.Equal(t, "unknown", r.Status)
	sender.outcome = email.OutcomeAccepted
	r, err = svc.Invite(context.Background(), host, req)
	require.NoError(t, err)
	require.Equal(t, callrules.InviteResult{Status: "sent", Duplicate: true}, r)
	require.Equal(t, 1, sender.count())
}

func TestInvitationBudgetsAreDurableAndRoomLimitIsPermanent(t *testing.T) {
	cw, svc, sender := invitationWorld(t, "invite-limits")
	if cw == nil {
		return
	}
	host, start := cw.started("Host")
	for i := range 20 {
		if i == 10 {
			_, err := svc.Invite(context.Background(), host, callrules.InviteRequest{Email: "extra@example.test", Token: start.Call.GuestToken})
			var limit *callrules.InvitationLimit
			require.ErrorAs(t, err, &limit)
			require.Equal(t, time.Minute, limit.RetryAfter)
			cw.clock.Advance(time.Minute)
		}
		_, err := svc.Invite(context.Background(), host, callrules.InviteRequest{Email: fmt.Sprintf("person%d@example.test", i), Token: start.Call.GuestToken})
		require.NoError(t, err)
	}
	_, err := svc.Invite(context.Background(), host, callrules.InviteRequest{Email: "extra@example.test", Token: start.Call.GuestToken})
	require.ErrorIs(t, err, callrules.ErrConflict)
	require.Equal(t, "invitation_full", callrules.ReasonOf(err))
	require.Equal(t, 20, sender.count())
	cw.clock.Advance(time.Minute)
	// A retry is still a read of the original receipt, even when the budget is full.
	r, err := svc.Invite(context.Background(), host, callrules.InviteRequest{Email: "person0@example.test", Token: start.Call.GuestToken})
	require.NoError(t, err)
	require.True(t, r.Duplicate)
	// Seed an exhausted daily counter, then use another room on the same deployable.
	ctx := memql.ContextWithFreshRead(context.Background())
	budget, err := svc.store.invitationBudget(ctx, cw.owner, cw.site)
	require.NoError(t, err)
	budget.DayCount = 100
	require.NoError(t, svc.store.writeInvitationBudget(ctx, cw.owner, *budget, cw.clock.Now()))
	otherHost, other := cw.started("Other")
	_, err = svc.Invite(ctx, otherHost, callrules.InviteRequest{Email: "other@example.test", Token: other.Call.GuestToken})
	var limit *callrules.InvitationLimit
	require.ErrorAs(t, err, &limit)
	require.Equal(t, 24*time.Hour-2*time.Minute, limit.RetryAfter)
	require.Equal(t, 20, sender.count())
}

func TestInvitationRefusesUnconfiguredLoggingTransport(t *testing.T) {
	cw, _, _ := invitationWorld(t, "invite-log")
	if cw == nil {
		return
	}
	host, start := cw.started("Host")
	svc := cw.svc.WithInvitationSender(func() email.Sender { return email.NewLogSender(nil) })
	_, err := svc.Invite(context.Background(), host, callrules.InviteRequest{Email: "person@example.test", Token: start.Call.GuestToken})
	require.ErrorIs(t, err, callrules.ErrInvitationUnavailable)
	require.Empty(t, cw.rowsOf(invitationConcept))
	require.Empty(t, cw.rowsOf(invitationBudgetConcept))
	require.False(t, strings.Contains(sharedBoot.logs.String(), start.Call.GuestToken))
}
