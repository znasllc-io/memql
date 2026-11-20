package calls

import (
	"context"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	callrules "github.com/znasllc-io/memql/component/calls"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

const historyConcept = "v1:calls:history"
const historyReceiptConcept = "v1:calls:historyReceipt"
const historySyncConcept = "v1:calls:historySync"

// History metadata belongs to the site's owner in the graph, but every service
// operation additionally binds the authenticated key and site. Internal origin is
// stamped at this named-construct seam; raw client graph reads remain closed.
type historyRow struct {
	CostCents                                                                           *int64
	CanRename                                                                           bool // Derived from the fresh operational owner, never stored.
	ID, SiteID, OwnerUserID, KeyID, SourceKind, SourceID, Title, Role, Status, OrderKey string
	StartedAt, EndedAt, ArchivedAt, PurgeAt, Version                                    time.Time
}

func historyFromRow(r map[string]any) historyRow {
	return historyRow{CostCents: optionalRoomCents(r, "costCents"), ID: memql.BareShortId(rowString(r, "id")), SiteID: memql.BareShortId(rowString(r, "siteId")), OwnerUserID: memql.BareShortId(rowString(r, "ownerUserId")), KeyID: memql.BareShortId(rowString(r, "keyId")), SourceKind: rowString(r, "sourceKind"), SourceID: memql.BareShortId(rowString(r, "sourceId")), Title: rowString(r, "title"), Role: rowString(r, "role"), Status: rowString(r, "status"), OrderKey: rowString(r, "orderKey"), StartedAt: rowTime(r, "startedAt"), EndedAt: rowTime(r, "endedAt"), ArchivedAt: rowTime(r, "archivedAt"), PurgeAt: rowTime(r, "purgeAt"), Version: rowTime(r, "createdAt")}
}
func (s *Store) historyByID(ctx context.Context, owner, id string) (*historyRow, error) {
	call, err := langparser.RenderCall("callsHistoryById", map[string]any{"id": id})
	rows, err := s.readAs(auth.ContextWithInternalOrigin(ctx), owner, "private call history", call, err)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	r := historyFromRow(rows[0])
	return &r, nil
}
func (s *Store) historyForKey(ctx context.Context, owner, site, key, after string, archived bool) ([]historyRow, error) {
	call, err := langparser.RenderCall("callsHistoryForKey", map[string]any{"siteId": site, "keyId": key, "afterKey": after, "archived": archived})
	rows, err := s.readAs(auth.ContextWithInternalOrigin(ctx), owner, "private call history page", call, err)
	if err != nil {
		return nil, err
	}
	out := make([]historyRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, historyFromRow(r))
	}
	return out, nil
}

// Source bits are authoritative even if an event hook failed or its process
// stopped. Each probe is key/site-scoped and returns at most one private row.
func (s *Store) historyPendingForKey(ctx context.Context, owner, site, key string) (bool, error) {
	args := map[string]any{"siteId": site, "keyId": key}
	call, err := langparser.RenderCall("callsHistoryPendingRooms", args)
	rows, err := s.readAs(auth.ContextWithInternalOrigin(ctx), owner, "pending room history", call, err)
	if err != nil || len(rows) > 0 {
		return len(rows) > 0, err
	}
	call, err = langparser.RenderCall("callsHistoryPendingBookings", args)
	rows, err = s.readAs(auth.ContextWithInternalOrigin(ctx), owner, "pending booked history", call, err)
	if err != nil || len(rows) > 0 {
		return len(rows) > 0, err
	}
	call, err = langparser.RenderCall("callsHistoryPendingSeats", args)
	rows, err = s.readAs(auth.ContextWithInternalOrigin(ctx), owner, "pending joined history", call, err)
	return len(rows) > 0, err
}
func historyFields(r historyRow) map[string]any {
	fields := map[string]any{"title": r.Title, "role": r.Role, "status": r.Status}
	if r.CostCents != nil {
		fields["costCents"] = *r.CostCents
	}
	for k, v := range map[string]time.Time{"startedAt": r.StartedAt, "endedAt": r.EndedAt, "archivedAt": r.ArchivedAt, "purgeAt": r.PurgeAt} {
		if !v.IsZero() {
			fields[k] = versionStamp(v)
		} else {
			fields[k] = ""
		}
	}
	return fields
}
func (s *Store) createHistory(ctx context.Context, owner string, r historyRow, now time.Time) error {
	args := historyFields(r)
	args["historyId"] = r.ID
	args["siteId"] = r.SiteID
	args["keyId"] = r.KeyID
	args["sourceKind"] = r.SourceKind
	args["sourceId"] = r.SourceID
	args["orderKey"] = r.OrderKey
	args["versionTime"] = versionStamp(now)
	call, err := langparser.RenderCall("createCallsHistory", args)
	return s.writeAs(auth.ContextWithInternalOrigin(ctx), owner, absent(historyConcept, r.ID), "private call history", call, err)
}
func (s *Store) updateHistory(ctx context.Context, owner string, r historyRow, fields map[string]any, now time.Time) error {
	args := map[string]any{"historyId": r.ID, "versionTime": versionStamp(memql.VersionTimeAfter(r.Version, now))}
	for k, v := range fields {
		args[k] = v
	}
	call, err := langparser.RenderCall("updateCallsHistory", args)
	return s.writeAs(auth.ContextWithInternalOrigin(ctx), owner, at(historyConcept, r.ID, r.Version), "private call history", call, err)
}

type historyReceipt struct {
	ID, SiteID, OwnerUserID string
	PurgeAt, Version        time.Time
}

func (s *Store) historyReceiptByID(ctx context.Context, owner, id string) (*historyReceipt, error) {
	call, err := langparser.RenderCall("callsHistoryReceiptById", map[string]any{"id": id})
	rows, err := s.readAs(auth.ContextWithInternalOrigin(ctx), owner, "private history suppression receipt", call, err)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	r := rows[0]
	return &historyReceipt{ID: memql.BareShortId(rowString(r, "id")), SiteID: memql.BareShortId(rowString(r, "siteId")), OwnerUserID: memql.BareShortId(rowString(r, "ownerUserId")), PurgeAt: rowTime(r, "purgeAt"), Version: rowTime(r, "createdAt")}, nil
}
func (s *Store) writeHistoryReceipt(ctx context.Context, owner, site, id string, prior *historyReceipt, deadline, now time.Time) error {
	args := map[string]any{"historyId": id, "versionTime": versionStamp(now), "purgeAt": ""}
	if !deadline.IsZero() {
		args["purgeAt"] = versionStamp(deadline)
	}
	if prior == nil {
		args["siteId"] = site
		call, err := langparser.RenderCall("createCallsHistoryReceipt", args)
		return s.writeAs(auth.ContextWithInternalOrigin(ctx), owner, absent(historyReceiptConcept, id), "private history suppression receipt", call, err)
	}
	args["versionTime"] = versionStamp(memql.VersionTimeAfter(prior.Version, now))
	call, err := langparser.RenderCall("updateCallsHistoryReceipt", args)
	return s.writeAs(auth.ContextWithInternalOrigin(ctx), owner, at(historyReceiptConcept, id, prior.Version), "private history suppression receipt", call, err)
}

type historySync struct {
	ID, SiteID, KeyID, Phase, AfterID string
	Complete                          bool
	Version                           time.Time
}

func (s *Store) historySyncByID(ctx context.Context, owner, id string) (*historySync, error) {
	call, err := langparser.RenderCall("callsHistorySyncById", map[string]any{"id": id})
	rows, err := s.readAs(auth.ContextWithInternalOrigin(ctx), owner, "private history repair progress", call, err)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	r := rows[0]
	return &historySync{ID: memql.BareShortId(rowString(r, "id")), SiteID: memql.BareShortId(rowString(r, "siteId")), KeyID: memql.BareShortId(rowString(r, "keyId")), Phase: rowString(r, "phase"), AfterID: rowString(r, "afterCursor"), Complete: rowBool(r, "complete"), Version: rowTime(r, "createdAt")}, nil
}
func (s *Store) writeHistorySync(ctx context.Context, owner string, r historySync, now time.Time) error {
	args := map[string]any{"historyId": r.ID, "phase": r.Phase, "afterCursor": r.AfterID, "complete": r.Complete, "versionTime": versionStamp(now)}
	if r.Version.IsZero() {
		args["siteId"] = r.SiteID
		args["keyId"] = r.KeyID
		call, err := langparser.RenderCall("createCallsHistorySync", args)
		return s.writeAs(auth.ContextWithInternalOrigin(ctx), owner, absent(historySyncConcept, r.ID), "private history repair progress", call, err)
	}
	args["versionTime"] = versionStamp(memql.VersionTimeAfter(r.Version, now))
	call, err := langparser.RenderCall("updateCallsHistorySync", args)
	return s.writeAs(auth.ContextWithInternalOrigin(ctx), owner, at(historySyncConcept, r.ID, r.Version), "private history repair progress", call, err)
}
func (s *Store) historySources(ctx context.Context, owner string, state historySync) ([]map[string]any, string, error) {
	args := map[string]any{"siteId": state.SiteID, "keyId": state.KeyID}
	var call string
	var err error
	switch state.Phase {
	case "rooms":
		call, err = langparser.RenderCall("callsHistorySourceRooms", args)
	case "bookings":
		call, err = langparser.RenderCall("callsHistorySourceBookings", args)
	case "seats":
		call, err = langparser.RenderCall("callsHistorySourceSeats", args)
	default:
		return nil, "", callrules.ErrInvalid
	}
	if err != nil {
		return nil, "", fmt.Errorf("calls: render history sources: %w", scrubbed(err))
	}
	owned, ok := rowsAs(auth.ContextWithInternalOrigin(ctx), owner)
	if !ok {
		return nil, "", errNoOwner
	}
	result, err := s.engine.Execute(memql.ContextWithCursor(memql.ContextWithRootsOnlyRead(owned), state.AfterID), "query "+call)
	if err != nil {
		return nil, "", fmt.Errorf("calls: history sources: %w", scrubbed(err))
	}
	executed, ok := result.(*memql.ExecuteResult)
	if !ok {
		return nil, "", fmt.Errorf("calls: history sources returned no continuation metadata")
	}
	next := ""
	if executed.GetMeta() != nil {
		next = executed.GetMeta().Cursor
	}
	return memql.MaterializeRows(executed.OutputPayload()), next, nil
}
