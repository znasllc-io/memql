package calls

import (
	"context"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

const invitationLinkConcept = "v1:calls:invitationLink"

// The id is the guest alias's keyed digest. No token is stored or projected.
// A separate immutable concept avoids changing existing room payloads while older
// replicas may still update them during rollout. Its primary-id lookup needs no
// additional payload index or unbounded room scan.
type invitationLinkRow struct{ ID, SiteID, OwnerUserID, RoomID string }

func (s *Store) invitationLinkByID(ctx context.Context, owner, site, digest string) (*invitationLinkRow, error) {
	call, err := langparser.RenderCall("callsInvitationLinkById", map[string]any{"id": digest, "siteId": memql.BareShortId(site)})
	rows, err := s.readAs(auth.ContextWithInternalOrigin(ctx), owner, "the guest invitation link", call, err)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	r := rows[0]
	return &invitationLinkRow{ID: memql.BareShortId(rowString(r, "id")), SiteID: memql.BareShortId(rowString(r, "siteId")),
		OwnerUserID: memql.BareShortId(rowString(r, "ownerUserId")), RoomID: memql.BareShortId(rowString(r, "roomId"))}, nil
}

func (s *Store) createInvitationLink(ctx context.Context, owner string, row invitationLinkRow, now time.Time) error {
	call, err := langparser.RenderCall("createCallsInvitationLink", map[string]any{
		"linkId": row.ID, "siteId": row.SiteID, "roomId": row.RoomID, "versionTime": versionStamp(now),
	})
	return s.writeAs(auth.ContextWithInternalOrigin(ctx), owner, absent(invitationLinkConcept, row.ID), "the guest invitation link", call, err)
}

func (s *Store) roomByInvitationLink(ctx context.Context, owner, site, digest string) (*roomRow, error) {
	owner, site = memql.BareShortId(owner), memql.BareShortId(site)
	link, err := s.invitationLinkByID(ctx, owner, site, digest)
	if err != nil || link == nil {
		return nil, err
	}
	if link.ID != digest || link.SiteID != site || link.OwnerUserID != owner || link.RoomID == "" {
		return nil, nil
	}
	room, err := s.roomByID(ctx, owner, link.RoomID)
	if err != nil || room == nil {
		return nil, err
	}
	if room.SiteID != site || room.OwnerUserID != owner {
		return nil, nil
	}
	// Return ended rooms too: the normal join path refuses their current state. An
	// alias is only a pointer; it never revives a room or opens a scheduled booking.
	return room, nil
}
