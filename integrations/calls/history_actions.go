package calls

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

type archiveIntent struct {
	Scope, ID, Version string
	IssuedAt, DeleteAt time.Time
}

func historyActionScope(h keyHolder) string {
	return callrules.HistoryDigest(h.pepper, "archive-scope", h.site.SiteID, h.row.KeyID)
}
func historyIDValid(id string) bool {
	if len(id) != 64 {
		return false
	}
	for _, r := range id {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}
func historyGone() error {
	return callrules.Refuse(callrules.ErrConflict, "history_expired", "this archived history has reached its deletion deadline")
}

// Waiting for a metadata/room gate must not preserve authority from a session
// that was revoked, expired or moved to another key while this request waited.
func (s *Service) recheckHistoryAuthority(ctx context.Context, sess callrules.Session, h keyHolder) error {
	current, err := s.authenticatedKey(memql.ContextWithFreshRead(ctx), sess)
	if err != nil {
		return err
	}
	if current.row.KeyID != h.row.KeyID {
		return callrules.ErrNoSession
	}
	return nil
}

func (s *Service) ownedHistory(ctx context.Context, h keyHolder, id string) (*historyRow, error) {
	row, err := s.store.historyByID(memql.ContextWithFreshRead(ctx), h.site.OwnerUserID, id)
	if err != nil {
		return nil, err
	}
	if row == nil || row.SiteID != h.site.SiteID || row.OwnerUserID != h.site.OwnerUserID || row.KeyID != h.row.KeyID {
		return nil, callrules.ErrNotFound
	}
	if !row.PurgeAt.IsZero() && !s.now().Before(row.PurgeAt) {
		return nil, historyGone()
	}
	return row, nil
}
func (s *Service) HistoryAction(ctx context.Context, sess callrules.Session, req callrules.HistoryActionRequest) (callrules.HistoryActionResult, error) {
	none := callrules.HistoryActionResult{}
	if !historyIDValid(req.ID) {
		return none, callrules.ErrInvalid
	}
	switch req.Action {
	case "archive_preview", "restore":
		if req.Title != nil || req.ConfirmationToken != "" || req.Acknowledged {
			return none, callrules.ErrInvalid
		}
	case "archive":
		if req.Title != nil || req.ConfirmationToken == "" || !req.Acknowledged {
			return none, callrules.ErrInvalid
		}
	case "rename":
		if req.Title == nil || req.ConfirmationToken != "" || req.Acknowledged {
			return none, callrules.ErrInvalid
		}
	default:
		return none, callrules.ErrInvalid
	}
	h, err := s.authenticatedKey(ctx, sess)
	if err != nil {
		return none, err
	}
	row, err := s.ownedHistory(ctx, h, req.ID)
	if err != nil {
		return none, err
	}
	if req.Action == "rename" {
		return s.renameHistory(ctx, h, sess, *row, *req.Title)
	}
	// Refresh before locking: refresh takes the same entry gate. Archive and restore
	// then read the exact version again under that gate; no stale preview can act.
	if row, err = s.refreshHistory(ctx, h, *row); err != nil {
		return none, err
	}
	if row == nil {
		return none, historyGone()
	}
	release, err := s.gates(ctx, historyGate(row.ID))
	if err != nil {
		return none, err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	if err = s.recheckHistoryAuthority(fresh, sess, h); err != nil {
		return none, err
	}
	row, err = s.ownedHistory(fresh, h, req.ID)
	if err != nil {
		return none, err
	}
	now := s.now()
	if req.Action == "archive_preview" {
		if !row.ArchivedAt.IsZero() || row.Status != "ended" && row.Status != "cancelled" {
			return none, callrules.Refuse(callrules.ErrConflict, "history_not_finished", "only finished or cancelled history can be archived")
		}
		intent := archiveIntent{Scope: historyActionScope(h), ID: row.ID, Version: historyStamp(row.Version), IssuedAt: now, DeleteAt: now.Add(historyArchiveWindow)}
		body, _ := json.Marshal(intent)
		return callrules.HistoryActionResult{DeleteAt: historyStamp(intent.DeleteAt), ConfirmationToken: callrules.SealHistoryToken(h.pepper, "archive", body)}, nil
	}
	receipt, err := s.store.historyReceiptByID(fresh, h.site.OwnerUserID, row.ID)
	if err != nil {
		return none, err
	}
	if req.Action == "restore" {
		if row.ArchivedAt.IsZero() {
			reply := s.historyReply(h, *row)
			return callrules.HistoryActionResult{Call: &reply}, nil
		}
		if !now.Before(row.PurgeAt) {
			return none, historyGone()
		}
		// Restore only the metadata under the shared purge gate. Its old opaque
		// receipt remains: existing restored metadata outranks it, and it still
		// prevents resurrection if a later explicit archive is purged.
		if err = s.store.updateHistory(fresh, h.site.OwnerUserID, *row, map[string]any{"archivedAt": "", "purgeAt": ""}, now); err != nil {
			return none, err
		}
		row.ArchivedAt, row.PurgeAt = time.Time{}, time.Time{}
		reply := s.historyReply(h, *row)
		return callrules.HistoryActionResult{Call: &reply}, nil
	}
	payload, ok := callrules.OpenHistoryToken(h.pepper, "archive", req.ConfirmationToken)
	var intent archiveIntent
	if !ok || json.Unmarshal(payload, &intent) != nil || intent.Scope != historyActionScope(h) || intent.ID != row.ID || !intent.DeleteAt.Equal(intent.IssuedAt.Add(historyArchiveWindow)) {
		return none, callrules.ErrInvalid
	}
	if !row.ArchivedAt.IsZero() {
		if !row.PurgeAt.Equal(intent.DeleteAt) {
			return none, callrules.Refuse(callrules.ErrConflict, "archive_changed", "open a fresh archive confirmation")
		}
		if err = s.store.writeHistoryReceipt(fresh, h.site.OwnerUserID, h.site.SiteID, row.ID, receipt, row.PurgeAt, now); err != nil {
			return none, err
		}
		reply := s.historyReply(h, *row)
		return callrules.HistoryActionResult{DeleteAt: historyStamp(row.PurgeAt), Call: &reply}, nil
	}
	if now.Before(intent.IssuedAt) || !now.Before(intent.IssuedAt.Add(historyPreviewWindow)) || intent.Version != historyStamp(row.Version) {
		return none, callrules.Refuse(callrules.ErrConflict, "archive_changed", "open a fresh archive confirmation")
	}
	if row.Status != "ended" && row.Status != "cancelled" {
		return none, callrules.Refuse(callrules.ErrConflict, "history_not_finished", "only finished or cancelled history can be archived")
	}
	if err = s.store.writeHistoryReceipt(fresh, h.site.OwnerUserID, h.site.SiteID, row.ID, receipt, intent.DeleteAt, now); err != nil {
		return none, err
	}
	if err = s.store.updateHistory(fresh, h.site.OwnerUserID, *row, map[string]any{"archivedAt": historyStamp(now), "purgeAt": historyStamp(intent.DeleteAt)}, now); err != nil {
		return none, err
	}
	row.ArchivedAt, row.PurgeAt = now, intent.DeleteAt
	reply := s.historyReply(h, *row)
	return callrules.HistoryActionResult{DeleteAt: historyStamp(intent.DeleteAt), Call: &reply}, nil
}

func (s *Service) renameHistory(ctx context.Context, h keyHolder, sess callrules.Session, row historyRow, raw string) (callrules.HistoryActionResult, error) {
	none := callrules.HistoryActionResult{}
	title, err := checkTitle(raw)
	if err != nil {
		return none, err
	}
	if !row.ArchivedAt.IsZero() {
		return none, callrules.ErrForbidden
	}
	var room *roomRow
	if row.SourceKind == "room" {
		room, err = s.store.roomByID(ctx, h.site.OwnerUserID, row.SourceID)
	} else {
		b, e := s.store.bookingByID(ctx, h.site.OwnerUserID, row.SourceID)
		err = e
		if b == nil && err == nil {
			return none, callrules.ErrNotFound
		}
		if b != nil {
			if b.SiteID != h.site.SiteID || b.KeyID != h.row.KeyID {
				return none, callrules.ErrForbidden
			}
			if b.RoomID != "" {
				room, err = s.store.roomByID(ctx, h.site.OwnerUserID, b.RoomID)
			} else {
				release, e := s.gates(ctx, keyGate(h.row.KeyID), bookingGate(b.ID))
				if e != nil {
					return none, e
				}
				defer release()
				fresh := memql.ContextWithFreshRead(ctx)
				if e = s.recheckHistoryAuthority(fresh, sess, h); e != nil {
					return none, e
				}
				b, e = s.store.bookingByID(fresh, h.site.OwnerUserID, b.ID)
				if e != nil {
					return none, e
				}
				if b == nil || b.SiteID != h.site.SiteID || b.KeyID != h.row.KeyID {
					return none, callrules.ErrForbidden
				}
				if b.RoomID != "" {
					return none, callrules.ErrConflict
				}
				title = callTitle(title, b.Start)
				if e = s.store.updateBooking(fresh, h.site.OwnerUserID, *b, map[string]any{"title": title}, memql.VersionTimeAfter(b.Version, s.now())); e != nil {
					return none, e
				}
				row.Title, row.CanRename = title, true
				reply := s.historyReply(h, row)
				return callrules.HistoryActionResult{Call: &reply}, nil
			}
		}
	}
	if err != nil {
		return none, err
	}
	if room == nil {
		return none, callrules.ErrNotFound
	}
	if room.SiteID != h.site.SiteID || room.KeyID != h.row.KeyID {
		return none, callrules.ErrForbidden
	}
	if room.open() {
		host, e := s.hostOf(ctx, sess)
		if e != nil {
			return none, e
		}
		if host.room.ID != room.ID {
			return none, callrules.ErrForbidden
		}
		title, err = s.RenameCall(ctx, sess, title, host.seat.Identity)
	} else {
		release, e := s.gates(ctx, endGates(*room)...)
		if e != nil {
			return none, e
		}
		defer release()
		fresh := memql.ContextWithFreshRead(ctx)
		if e = s.recheckHistoryAuthority(fresh, sess, h); e != nil {
			return none, e
		}
		room, e = s.store.roomByID(fresh, h.site.OwnerUserID, room.ID)
		if e != nil {
			return none, e
		}
		if room == nil || room.SiteID != h.site.SiteID || room.KeyID != h.row.KeyID || room.open() {
			return none, callrules.ErrForbidden
		}
		title = callTitle(title, room.OpenedAt)
		err = s.store.updateRoom(fresh, h.site.OwnerUserID, *room, map[string]any{"title": title}, memql.VersionTimeAfter(room.Version, s.now()))
	}
	if err != nil {
		return none, err
	}
	row.Title, row.CanRename = title, true
	reply := s.historyReply(h, row)
	return callrules.HistoryActionResult{Call: &reply}, nil
}
