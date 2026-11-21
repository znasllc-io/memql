package calls

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

func reportAction(a string) bool {
	return slices.Contains([]string{"report_submit", "report_status", "report_list", "report_appeal"}, a)
}
func reportText(s string) bool {
	return utf8.RuneCountInString(strings.TrimSpace(s)) >= 10 && plainProcessingText(s, 2000)
}
func validReportRequest(action string, q callrules.ReportRequest) bool {
	switch action {
	case "report_submit":
		return reflect.DeepEqual(q, callrules.ReportRequest{RoomID: q.RoomID, Identity: q.Identity, TargetIdentity: q.TargetIdentity, Category: q.Category, Description: q.Description, IdempotencyKey: q.IdempotencyKey}) && q.RoomID != "" && len(q.RoomID) <= 128 && q.Identity != "" && len(q.Identity) <= 128 && len(q.TargetIdentity) <= 128 && slices.Contains([]string{"harassment", "sexual_content", "child_safety", "impersonation", "spam", "other"}, q.Category) && reportText(q.Description) && fundingOperationID.MatchString(q.IdempotencyKey)
	case "report_status":
		return reflect.DeepEqual(q, callrules.ReportRequest{ID: q.ID}) && validReportID(q.ID)
	case "report_list":
		return reflect.DeepEqual(q, callrules.ReportRequest{Cursor: q.Cursor}) && len(q.Cursor) <= 4096
	case "report_appeal":
		return reflect.DeepEqual(q, callrules.ReportRequest{ID: q.ID, Revision: q.Revision, Description: q.Description, IdempotencyKey: q.IdempotencyKey}) && validReportID(q.ID) && q.Revision >= 1 && q.Revision <= 4 && reportText(q.Description) && fundingOperationID.MatchString(q.IdempotencyKey)
	}
	return false
}
func validReportID(v string) bool {
	if len(v) != 64 {
		return false
	}
	for _, c := range v {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func reportHash(v any) string {
	raw, _ := json.Marshal(v)
	return string(id.NewUntracked().FromBytes(raw))
}
func reportActorKey(grant string) string {
	if grant == "" {
		return "human"
	}
	return "agent:" + grant
}
func reportActor(ctx context.Context) string {
	a, _ := ctx.Value(agentCallContext{}).(string)
	return a
}
func reportOwned(r *reportCase, site, key, actor string, now time.Time) bool {
	return r != nil && r.SiteID == site && r.ReporterKeyID == key && r.State.ReporterGrant == actor && now.Before(r.ExpiresAt)
}

// Reports never require a paid balance, host status or an age decision. Child
// sessions intentionally reach this narrow boundary before account ceremonies.
func (s *Service) report(ctx context.Context, site callrules.SiteRef, token, action string, q callrules.ReportRequest) (callrules.ReportResult, error) {
	none := callrules.ReportResult{}
	if !validReportRequest(action, q) {
		return none, callrules.ErrInvalid
	}
	h, e := s.authenticatedKey(ctx, callrules.Session{Site: site, Token: token})
	if e != nil {
		return none, e
	}
	if action == "report_list" {
		if e = s.delegatedEffectAllowed(ctx, site, h.row.KeyID); e != nil {
			return none, e
		}
		cases, next, e := s.store.reportPage(ctx, site.OwnerUserID, site.SiteID, h.row.KeyID, reportActor(ctx), q.Cursor, s.now())
		if e != nil {
			return none, e
		}
		out := callrules.ReportResult{Items: []callrules.ReportReceipt{}, NextCursor: next}
		for _, r := range cases {
			if reportOwned(r, site.SiteID, h.row.KeyID, reportActor(ctx), s.now()) {
				out.Items = append(out.Items, r.receipt())
			}
		}
		return out, nil
	}
	caseID := q.ID
	if action == "report_submit" {
		caseID = callrules.HistoryDigest(h.pepper, "report-case", site.SiteID, h.row.KeyID, reportActor(ctx), q.RoomID, q.IdempotencyKey)
	}
	keys := []string{reportGate(caseID), accountGate(h.row.KeyID), keyGate(h.row.KeyID)}
	if action == "report_submit" {
		keys = append(keys, "calls.report-budget."+site.SiteID, roomGate(q.RoomID))
	}
	release, e := s.gates(ctx, keys...)
	if e != nil {
		return none, e
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	current, e := s.authenticatedKey(fresh, callrules.Session{Site: site, Token: token})
	if e != nil {
		return none, e
	}
	if current.row.KeyID != h.row.KeyID || current.row.ID != h.row.ID {
		return none, callrules.ErrNoSession
	}
	if e = s.delegatedEffectAllowed(fresh, site, h.row.KeyID); e != nil {
		return none, e
	}
	r, e := s.store.report(fresh, site.OwnerUserID, caseID)
	if e != nil {
		return none, e
	}
	if action == "report_submit" {
		if r != nil {
			if !reportOwned(r, site.SiteID, h.row.KeyID, reportActor(ctx), s.now()) {
				return none, accountRefusal("report_unavailable")
			}
			if r.State.SubmitHash != reportHash(q) {
				return none, accountRefusal("report_request_changed")
			}
			receipt := r.receipt()
			return callrules.ReportResult{Receipt: &receipt}, nil
		}
		p, e := s.store.PolicyFor(fresh, site.OwnerUserID, site.SiteID)
		if e != nil {
			return none, e
		}
		if !reportCapabilities(p).Available {
			return none, accountRefusal("reports_unavailable")
		}
		seat, room, e := s.seatAndRoom(fresh, site.OwnerUserID, current.row)
		if e != nil {
			return none, e
		}
		if seat == nil || room == nil || !room.open() || room.SiteID != site.SiteID || room.ID != q.RoomID || seat.Identity != q.Identity || !seat.occupying() {
			return none, accountRefusal("report_admission_required")
		}
		// Every call is bounded to <=24h; its admission cannot survive the earliest
		// one-day case expiry, so a physically purged nonce cannot recreate a case.
		if s.now().Before(room.OpenedAt) || s.now().Sub(room.OpenedAt) >= 24*time.Hour {
			return none, accountRefusal("report_admission_required")
		}
		var target *seatRow
		if q.TargetIdentity != "" {
			seats, e := s.store.seatsForRoom(fresh, site.OwnerUserID, room.ID)
			if e != nil {
				return none, e
			}
			for i := range seats {
				if seats[i].Identity == q.TargetIdentity && seats[i].occupying() {
					target = &seats[i]
					break
				}
			}
			if target == nil || target.ID == seat.ID {
				return none, accountRefusal("report_target_unavailable")
			}
		}
		if e = s.store.reportBudget(fresh, site.OwnerUserID, site.SiteID, h.row.KeyID, room.ID, s.now()); e != nil {
			return none, e
		}
		now := persistedCallTime(s.now())
		r = &reportCase{ID: caseID, SiteID: site.SiteID, ReporterKeyID: h.row.KeyID, RoomID: room.ID, IssuedAt: now, ExpiresAt: now.Add(time.Duration(p.ReportMetadataDays) * 24 * time.Hour), Revision: 1, State: reportState{ReporterGrant: reportActor(ctx), ReporterSeat: seat.ID, ReporterIdentity: seat.Identity, Category: q.Category, Description: q.Description, SubmitHash: reportHash(q), Status: "submitted"}}
		if target != nil {
			r.SubjectKeyID = target.KeyID
			r.State.SubjectSeat = target.ID
			r.State.SubjectIdentity = target.Identity
		}
		if e = s.store.saveReport(fresh, site.OwnerUserID, r, s.now()); e != nil {
			return none, e
		}
	} else {
		if !reportOwned(r, site.SiteID, h.row.KeyID, reportActor(ctx), s.now()) {
			return none, accountRefusal("report_unavailable")
		}
		if action == "report_appeal" {
			if r.State.AppealOperation == q.IdempotencyKey {
				if r.State.AppealHash != reportHash(q) {
					return none, accountRefusal("report_request_changed")
				}
			} else {
				if r.Revision != q.Revision {
					return none, accountRefusal("report_revision_changed")
				}
				if r.State.Status != "reviewed" || r.State.Appeal != "" {
					return none, accountRefusal("report_appeal_unavailable")
				}
				r.State.Appeal = q.Description
				r.State.AppealOperation = q.IdempotencyKey
				r.State.AppealHash = reportHash(q)
				r.State.Status = "appealed"
				r.Revision++
				if e = s.store.saveReport(fresh, site.OwnerUserID, r, s.now()); e != nil {
					return none, e
				}
			}
		}
	}
	receipt := r.receipt()
	return callrules.ReportResult{Receipt: &receipt}, nil
}
