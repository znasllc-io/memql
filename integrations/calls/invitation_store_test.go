package calls

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

type invitationProbeEngine func(context.Context, string) (any, error)

func (e invitationProbeEngine) Execute(ctx context.Context, query string) (any, error) {
	return e(ctx, query)
}

// Every operation, including both create/update branches, must reach the engine
// only with the checked site owner and internal origin. A missing owner never
// borrows the request's actor; writes retain their concurrency fence.
func TestInvitationStoreScopesEveryOperationToItsOwner(t *testing.T) {
	incoming := auth.ContextWithClientOrigin(auth.ContextWithUserActor(context.Background(), "caller"))
	now := time.Now().UTC().Truncate(time.Microsecond)
	var observedContext context.Context
	var observedQuery string
	store := NewStore(invitationProbeEngine(func(ctx context.Context, query string) (any, error) {
		observedContext, observedQuery = ctx, query
		return nil, nil
	}), nil)
	receipt := invitationReceipt{ID: "receipt", SiteID: "site", RoomID: "room", Status: "attempting", Fingerprint: strings.Repeat("a", 64), AttemptedAt: now, Version: now.Add(-time.Minute)}
	budget := invitationBudget{SiteID: "site", MinuteStart: now, DayStart: now, MinuteCount: 1, DayCount: 1, Version: now.Add(-time.Minute)}
	for _, operation := range []struct {
		name, fenceConcept, fenceID string
		run                         func(string) error
	}{
		{"callsInvitationById", "", "", func(owner string) error { _, err := store.invitationByID(incoming, owner, receipt.ID); return err }},
		{"callsInvitationsForRoom", "", "", func(owner string) error {
			_, err := store.invitationsForRoom(incoming, owner, receipt.RoomID)
			return err
		}},
		{"callsInvitationBudget", "", "", func(owner string) error { _, err := store.invitationBudget(incoming, owner, budget.SiteID); return err }},
		{"createCallsInvitation", invitationConcept, receipt.ID, func(owner string) error { return store.writeInvitation(incoming, owner, receipt, true, now) }},
		{"updateCallsInvitation", invitationConcept, receipt.ID, func(owner string) error { return store.writeInvitation(incoming, owner, receipt, false, now) }},
		{"createCallsInvitationBudget", invitationBudgetConcept, budget.SiteID, func(owner string) error {
			created := budget
			created.Version = time.Time{}
			return store.writeInvitationBudget(incoming, owner, created, now)
		}},
		{"updateCallsInvitationBudget", invitationBudgetConcept, budget.SiteID, func(owner string) error { return store.writeInvitationBudget(incoming, owner, budget, now) }},
	} {
		t.Run(operation.name, func(t *testing.T) {
			observedContext, observedQuery = nil, ""
			require.NoError(t, operation.run("site-owner"))
			require.NotNil(t, observedContext)
			require.True(t, auth.OriginFromContext(observedContext).IsInternal())
			actor, ok := auth.AccessFromContext(observedContext)
			require.True(t, ok)
			require.Equal(t, "site-owner", memql.BareShortId(actor.UserId))
			require.Contains(t, observedQuery, operation.name+"(")
			if operation.fenceConcept != "" {
				require.True(t, memql.HasRowVersionFence(observedContext, operation.fenceConcept, operation.fenceID))
			}
			require.False(t, auth.OriginFromContext(incoming).IsInternal(), "the authority must not escape into the request context")
			observedContext = nil
			require.ErrorIs(t, operation.run(" "), errNoOwner)
			require.Nil(t, observedContext, "missing ownership reached the engine")
		})
	}
}
