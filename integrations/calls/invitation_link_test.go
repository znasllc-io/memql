package calls

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

func TestInvitationLinkDerivationSeparatesEveryScope(t *testing.T) {
	pepper := bytes.Repeat([]byte{7}, 32)
	token := invitationLinkToken(pepper, "owner", "site", "room")
	// Wire-stability vector: changing framing, domain or encoding would silently
	// mint another alias for every existing room and orphan recovery records.
	require.Equal(t, "sobceuaioyttstvp5muiymahlqnsdf3n", token)
	require.True(t, callrules.TokenWellFormed(token))
	require.Equal(t, token, invitationLinkToken(pepper, "owner", "site", "room"))
	for _, other := range []string{
		invitationLinkToken(pepper, "another", "site", "room"),
		invitationLinkToken(pepper, "owner", "another", "room"),
		invitationLinkToken(pepper, "owner", "site", "another"),
		invitationLinkToken(bytes.Repeat([]byte{8}, 32), "owner", "site", "room"),
	} {
		require.NotEqual(t, token, other)
	}
	require.NotEqual(t, invitationLinkToken(pepper, "a", "bc", "d"), invitationLinkToken(pepper, "ab", "c", "d"))
	require.NotEqual(t, invitationLinkToken(pepper, "a\x00b", "c", "d"), invitationLinkToken(pepper, "a", "b\x00c", "d"))
}

func TestInvitationLinkLookupIsBoundedGuestOnlyAndChecksBindings(t *testing.T) {
	digest := strings.Repeat("a", 64)
	for _, test := range []struct {
		name                  string
		host, original        bool
		aliasField, roomField string
		wantRoom              bool
		wantReads             int
	}{
		{name: "original first", original: true, wantRoom: true, wantReads: 1},
		{name: "host never falls back", host: true, wantReads: 1},
		{name: "guest alias", wantRoom: true, wantReads: 3},
		{name: "alias wrong site", aliasField: "siteId", wantReads: 2},
		{name: "alias wrong owner", aliasField: "ownerUserId", wantReads: 2},
		{name: "alias wrong digest", aliasField: "id", wantReads: 2},
		{name: "target wrong site", roomField: "siteId", wantReads: 3},
		{name: "target wrong owner", roomField: "ownerUserId", wantReads: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			reads := 0
			store := NewStore(invitationProbeEngine(func(_ context.Context, query string) (any, error) {
				reads++
				room := map[string]any{"id": roomConcept + ":room", "siteId": siteConcept + ":site", "ownerUserId": "v1:identity:user:owner", "status": "open"}
				if strings.Contains(query, "callsRoomByGuestDigest(") || strings.Contains(query, "callsRoomByHostDigest(") {
					if test.original {
						return []map[string]any{room}, nil
					}
					return nil, nil
				}
				if strings.Contains(query, "callsInvitationLinkById(") {
					alias := map[string]any{"id": invitationLinkConcept + ":" + digest, "siteId": siteConcept + ":site", "ownerUserId": "v1:identity:user:owner", "roomId": roomConcept + ":room"}
					if test.aliasField != "" {
						alias[test.aliasField] = "wrong"
					}
					return []map[string]any{alias}, nil
				}
				require.Contains(t, query, "callsRoomById(")
				if test.roomField != "" {
					room[test.roomField] = "wrong"
				}
				return []map[string]any{room}, nil
			}), nil)
			room, err := store.roomByLink(context.Background(), "owner", "site", digest, test.host)
			require.NoError(t, err)
			require.Equal(t, test.wantRoom, room != nil)
			require.Equal(t, test.wantReads, reads)
		})
	}
}

func TestInvitationLinkMalformedIdentityCostsNoLookup(t *testing.T) {
	var service Service // no engine, clock or secret resolver: lookup would panic
	for _, value := range []string{"", "a", strings.Repeat("a", 32), strings.Repeat("A", 16), "<script>bad</script>", strings.Repeat("a", 16) + "\n"} {
		result, err := service.InvitationLink(context.Background(), callrules.Session{}, callrules.InvitationLinkRequest{Identity: value})
		require.ErrorIs(t, err, callrules.ErrInvalid)
		require.Empty(t, result.GuestToken)
	}
}

func TestInvitationLinkStoreRetainsOwnerOriginAndAbsentFence(t *testing.T) {
	incoming := auth.ContextWithClientOrigin(auth.ContextWithUserActor(context.Background(), "caller"))
	var observed context.Context
	var query string
	store := NewStore(invitationProbeEngine(func(ctx context.Context, q string) (any, error) {
		observed, query = ctx, q
		return nil, nil
	}), nil)
	row := invitationLinkRow{ID: strings.Repeat("a", 64), SiteID: "site", RoomID: "room"}
	for _, write := range []bool{false, true} {
		run := func(owner string) error {
			if write {
				return store.createInvitationLink(incoming, owner, row, time.Now())
			}
			_, err := store.invitationLinkByID(incoming, owner, row.SiteID, row.ID)
			return err
		}
		require.NoError(t, run("site-owner"))
		require.True(t, auth.OriginFromContext(observed).IsInternal())
		actor, ok := auth.AccessFromContext(observed)
		require.True(t, ok)
		require.Equal(t, "site-owner", memql.BareShortId(actor.UserId))
		require.Contains(t, query, `siteId: "site"`)
		if write {
			require.True(t, memql.HasRowVersionFence(observed, invitationLinkConcept, row.ID))
		}
		observed = nil
		require.ErrorIs(t, run(" "), errNoOwner)
		require.Nil(t, observed)
	}
	require.False(t, auth.OriginFromContext(incoming).IsInternal())
}
