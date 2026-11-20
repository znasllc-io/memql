package calls

import (
	"context"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// Each named invitation construct is server-only. Repeat the internal-origin
// stamp at this file's calls, as rooms.go and schedule.go do for their constructs:
// the per-file caller gate requires it here. readAs/writeAs still select the
// nonblank checked site owner, and writeAs still installs the version fence.

type invitationReceipt struct {
	ID, SiteID, RoomID, Status, Fingerprint string
	AttemptedAt, RetryAt, Version           time.Time
}

func invitationFromRow(row map[string]any) invitationReceipt {
	return invitationReceipt{ID: memql.BareShortId(rowString(row, "id")), SiteID: memql.BareShortId(rowString(row, "siteId")), RoomID: memql.BareShortId(rowString(row, "roomId")), Status: rowString(row, "status"), Fingerprint: rowString(row, "fingerprint"), AttemptedAt: rowTime(row, "attemptedAt"), RetryAt: rowTime(row, "retryAt"), Version: rowTime(row, "createdAt")}
}

func (s *Store) invitationByID(ctx context.Context, owner, id string) (*invitationReceipt, error) {
	call, err := langparser.RenderCall("callsInvitationById", map[string]any{"id": id})
	rows, err := s.readAs(auth.ContextWithInternalOrigin(ctx), owner, "the invitation receipt", call, err)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	row := invitationFromRow(rows[0])
	return &row, nil
}

func (s *Store) invitationsForRoom(ctx context.Context, owner, room string) ([]invitationReceipt, error) {
	call, err := langparser.RenderCall("callsInvitationsForRoom", map[string]any{"roomId": room})
	rows, err := s.readAs(auth.ContextWithInternalOrigin(ctx), owner, "the call's invitation receipts", call, err)
	if err != nil {
		return nil, err
	}
	out := make([]invitationReceipt, 0, len(rows))
	for _, row := range rows {
		out = append(out, invitationFromRow(row))
	}
	return out, nil
}

func (s *Store) writeInvitation(ctx context.Context, owner string, row invitationReceipt, create bool, now time.Time) error {
	args := map[string]any{"invitationId": row.ID, "status": row.Status, "attemptedAt": versionStamp(row.AttemptedAt), "fingerprint": row.Fingerprint, "versionTime": versionStamp(memql.VersionTimeAfter(row.Version, now))}
	name := "updateCallsInvitation"
	expected := at(invitationConcept, row.ID, row.Version)
	if create {
		name = "createCallsInvitation"
		expected = absent(invitationConcept, row.ID)
		args["siteId"] = row.SiteID
		args["roomId"] = row.RoomID
	}
	if !row.RetryAt.IsZero() {
		args["retryAt"] = versionStamp(row.RetryAt)
	}
	call, err := langparser.RenderCall(name, args)
	return s.writeAs(auth.ContextWithInternalOrigin(ctx), owner, expected, "the invitation receipt", call, err)
}

const invitationBudgetConcept = "v1:calls:invitationBudget"

type invitationBudget struct {
	SiteID                         string
	MinuteStart, DayStart, Version time.Time
	MinuteCount, DayCount          int
}

func (s *Store) invitationBudget(ctx context.Context, owner, site string) (*invitationBudget, error) {
	call, err := langparser.RenderCall("callsInvitationBudget", map[string]any{"id": site})
	rows, err := s.readAs(auth.ContextWithInternalOrigin(ctx), owner, "the invitation budget", call, err)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	row := rows[0]
	return &invitationBudget{SiteID: site, MinuteStart: rowTime(row, "minuteStartedAt"), DayStart: rowTime(row, "dayStartedAt"), MinuteCount: rowInt(row, "minuteCount"), DayCount: rowInt(row, "dayCount"), Version: rowTime(row, "createdAt")}, nil
}

func (s *Store) writeInvitationBudget(ctx context.Context, owner string, row invitationBudget, now time.Time) error {
	args := map[string]any{"budgetId": row.SiteID, "minuteStartedAt": versionStamp(row.MinuteStart), "dayStartedAt": versionStamp(row.DayStart), "minuteCount": row.MinuteCount, "dayCount": row.DayCount, "versionTime": versionStamp(memql.VersionTimeAfter(row.Version, now))}
	name := "updateCallsInvitationBudget"
	expected := at(invitationBudgetConcept, row.SiteID, row.Version)
	if row.Version.IsZero() {
		name = "createCallsInvitationBudget"
		expected = absent(invitationBudgetConcept, row.SiteID)
		args["siteId"] = row.SiteID
	}
	call, err := langparser.RenderCall(name, args)
	return s.writeAs(auth.ContextWithInternalOrigin(ctx), owner, expected, "the invitation budget", call, err)
}
