package calls

import (
	"context"
	"errors"
	"time"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

// roomFromHistory resolves private metadata to current operational authority.
// No invitation is reconstructed or exposed. It is called again under admission
// gates, so a logout, key change, archived entry or ended room cannot authorize a
// join from the earlier lookup. An unopened booking is not opened by recovery.
func (s *Service) roomFromHistory(ctx context.Context, site callrules.SiteRef, held *sessionRow, id string) (*roomRow, error) {
	if !held.usable(s.now()) || held.SiteID != site.SiteID || held.KeyID == "" {
		return nil, callrules.ErrNoSession
	}
	key, err := s.store.keyByID(ctx, site.OwnerUserID, held.KeyID)
	if err != nil {
		return nil, err
	}
	if key == nil || key.SiteID != site.SiteID {
		return nil, callrules.ErrNoSession
	}
	if !keyActive(key.Status) {
		return nil, callrules.Refuse(callrules.ErrForbidden, callrules.ReasonKeyNotActive, "the key cannot rejoin a call")
	}
	row, err := s.ownedHistory(ctx, keyHolder{site: site, row: held}, id)
	if err != nil {
		return nil, err
	}
	if !row.ArchivedAt.IsZero() {
		return nil, callrules.ErrNotFound
	}
	roomID := row.SourceID
	switch row.SourceKind {
	case "room":
	case "booking":
		booking, err := s.store.bookingByID(ctx, site.OwnerUserID, row.SourceID)
		if err != nil {
			return nil, err
		}
		if booking == nil || booking.SiteID != site.SiteID || booking.RoomID == "" {
			return nil, callrules.ErrNotFound
		}
		roomID = booking.RoomID
	default:
		return nil, callrules.ErrNotFound
	}
	room, err := s.store.roomByID(ctx, site.OwnerUserID, roomID)
	if err != nil {
		return nil, err
	}
	if room == nil || room.SiteID != site.SiteID || room.OwnerUserID != site.OwnerUserID || (row.SourceKind == "booking" && room.ScheduledCallID != row.SourceID) {
		return nil, callrules.ErrNotFound
	}
	if !room.open() {
		return nil, callrules.Refuse(callrules.ErrEnded, callrules.ReasonCallEnded, "the call has ended")
	}
	return room, nil
}

// recoverHistorySeat runs under the account, key, room and admission gates.
// Reuse the same key-bound identity across fresh sessions and replicas, rather
// than creating a seat on every dashboard click. Operational seat authority wins
// over history metadata; a removed seat stays removed and another live call is
// never abandoned. The fresh session's credential becomes the recovered seat's
// binding, so revoking an older credential does not revoke the new sign-in.
func (s *Service) recoverHistorySeat(ctx context.Context, site callrules.SiteRef, room roomRow, held *sessionRow, seat *seatRow, now time.Time) (*seatRow, error) {
	if seat != nil && seat.RoomID != room.ID && seat.placed() {
		other, err := s.store.roomByID(ctx, site.OwnerUserID, seat.RoomID)
		if err != nil {
			return nil, err
		}
		if other != nil && other.open() {
			return nil, callrules.Refuse(callrules.ErrConflict, callrules.ReasonInAnotherCall, "the session is in another call")
		}
	}
	bound := seat != nil && seat.RoomID == room.ID
	if !bound {
		seat = nil
	}
	seats, err := s.store.seatsForRoom(ctx, site.OwnerUserID, room.ID)
	if err != nil {
		return nil, err
	}
	for _, candidate := range seats {
		if candidate.KeyID != held.KeyID {
			continue
		}
		// History is not a fresh invitation. A different session or an older
		// still-bound seat must not turn a recorded removal into readmission.
		if candidate.Status == seatRemoved {
			return nil, callrules.Refuse(callrules.ErrForbidden, callrules.ReasonRemoved, "the host removed this key's seat")
		}
		if !bound && (seat == nil || candidate.Version.After(seat.Version)) {
			copy := candidate
			seat = &copy
		}
	}
	if seat == nil {
		if room.KeyID != held.KeyID {
			return nil, callrules.ErrNotFound
		}
		return nil, nil
	}
	if seat.KeyID != held.KeyID {
		return nil, callrules.ErrNotFound
	}
	if seat.Status == seatRemoved {
		return nil, callrules.Refuse(callrules.ErrForbidden, callrules.ReasonRemoved, "the host removed this seat")
	}
	if seat.CredentialID != held.CredentialID {
		version := memql.VersionTimeAfter(seat.Version, now)
		if err := s.store.updateSeat(ctx, site.OwnerUserID, *seat, map[string]any{"credentialId": held.CredentialID}, version); err != nil {
			return nil, err
		}
		seat.CredentialID, seat.Version = held.CredentialID, version
	}
	if held.SeatID != seat.ID || held.RoomID != room.ID {
		version := memql.VersionTimeAfter(held.Version, now)
		if err := s.store.seatSession(ctx, site.OwnerUserID, held, seat.ID, room.ID, version); err != nil {
			if errors.Is(err, memql.ErrRowVersionChanged) {
				return nil, s.movedSession(err)
			}
			return nil, err
		}
		held.SeatID, held.RoomID, held.Version = seat.ID, room.ID, version
	}
	return seat, nil
}
