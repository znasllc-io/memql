package calls

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	callrules "github.com/znasllc-io/memql/component/calls"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

const reportConcept = "v1:calls:reportCase"
const reportAuditConcept = "v1:calls:reportAccessAudit"

type reportReview struct {
	Actor, Operation, RequestHash, Outcome, Note string
	At                                           time.Time
	Revision                                     int
}
type reportState struct {
	ReporterGrant    string
	ReporterSeat     string
	ReporterIdentity string
	SubjectSeat      string
	SubjectIdentity  string
	Category         string
	Description      string
	SubmitHash       string
	Status           string
	Appeal           string
	AppealOperation  string
	AppealHash       string
	Reviews          []reportReview
}
type reportCase struct {
	ID, SiteID, ReporterKeyID, SubjectKeyID, RoomID string
	IssuedAt, ExpiresAt, Version                    time.Time
	Revision                                        int
	State                                           reportState
}

func reportGate(id string) string { return "calls.report." + id }
func reportCapabilities(p callrules.Policy) callrules.ReportCapabilities {
	return callrules.ReportCapabilities{Available: p.ReportsEnabled && p.ReportMetadataDays >= 1 && p.ReportMetadataDays <= 365, MetadataDays: p.ReportMetadataDays}
}
func (r *reportCase) receipt() callrules.ReportReceipt {
	out := callrules.ReportReceipt{ID: r.ID, Status: r.State.Status, Category: r.State.Category, CreatedAt: r.IssuedAt, ExpiresAt: r.ExpiresAt, Revision: r.Revision, CanAppeal: r.State.Status == "reviewed" && r.State.Appeal == ""}
	if n := len(r.State.Reviews); n > 0 {
		out.Outcome = r.State.Reviews[n-1].Outcome
	}
	return out
}
func reportFromRow(row map[string]any) (*reportCase, error) {
	r := &reportCase{ID: memql.BareShortId(rowString(row, "id")), SiteID: memql.BareShortId(rowString(row, "siteId")), ReporterKeyID: memql.BareShortId(rowString(row, "reporterKeyId")), SubjectKeyID: memql.BareShortId(rowString(row, "subjectKeyId")), RoomID: memql.BareShortId(rowString(row, "roomId")), IssuedAt: rowTime(row, "issuedAt"), ExpiresAt: rowTime(row, "expiresAt"), Version: rowTime(row, "createdAt"), Revision: rowInt(row, "revision")}
	raw := rowString(row, "stateData")
	if len(raw) > 16384 || json.Unmarshal([]byte(raw), &r.State) != nil || rowString(row, "reporterActor") != reportActorKey(r.State.ReporterGrant) || r.Revision < 1 || r.Revision > 4 || len(r.State.Reviews) > 2 || !r.ExpiresAt.After(r.IssuedAt) {
		return nil, accountRefusal("report_unavailable")
	}
	return r, nil
}
func (s *Store) report(ctx context.Context, owner, id string) (*reportCase, error) {
	call, e := langparser.RenderCall("callsReportById", map[string]any{"id": id})
	rows, e := s.readAs(auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(ctx)), owner, "private report", call, e)
	if e != nil || len(rows) == 0 {
		return nil, e
	}
	return reportFromRow(rows[0])
}
func (s *Store) saveReport(ctx context.Context, owner string, r *reportCase, now time.Time) error {
	raw, e := json.Marshal(r.State)
	if e != nil || len(raw) > 16384 || r.Revision < 1 || r.Revision > 4 || !now.Before(r.ExpiresAt) {
		return accountRefusal("report_unavailable")
	}
	v := persistedCallTime(memql.VersionTimeAfter(r.Version, now))
	args := map[string]any{"caseId": r.ID, "versionTime": versionStamp(v), "siteId": r.SiteID, "reporterKeyId": r.ReporterKeyID, "reporterActor": reportActorKey(r.State.ReporterGrant), "roomId": r.RoomID, "issuedAt": versionStamp(r.IssuedAt), "expiresAt": versionStamp(r.ExpiresAt), "revision": r.Revision, "stateData": string(raw)}
	if r.SubjectKeyID != "" {
		args["subjectKeyId"] = r.SubjectKeyID
	}
	call, e := langparser.RenderCall("saveCallsReport", args)
	fence := absent(reportConcept, r.ID)
	if !r.Version.IsZero() {
		fence = at(reportConcept, r.ID, r.Version)
	}
	e = s.writeAs(auth.ContextWithInternalOrigin(ctx), owner, fence, "private report", call, e)
	if e == nil {
		r.Version = v
	}
	return e
}
func (s *Store) reportPage(ctx context.Context, owner, site, key, actor, cursor string, now time.Time) ([]*reportCase, string, error) {
	owned, ok := asOwner(ctx, owner)
	if !ok {
		return nil, "", errNoOwner
	}
	args := map[string]any{"siteId": site, "asOf": versionStamp(now)}
	if key != "" {
		args["reporterKeyId"] = key
		args["reporterActor"] = reportActorKey(actor)
	}
	call, e := langparser.RenderCall("callsReportsForRecipient", args)
	if e != nil {
		return nil, "", accountRefusal("report_unavailable")
	}
	result, e := s.engine.Execute(memql.ContextWithCursor(memql.ContextWithRootsOnlyRead(memql.ContextWithFreshRead(auth.ContextWithInternalOrigin(owned))), cursor), "query "+call)
	if e != nil {
		return nil, "", accountRefusal("report_unavailable")
	}
	out, ok := result.(*memql.ExecuteResult)
	if !ok {
		return nil, "", accountRefusal("report_unavailable")
	}
	rows := memql.MaterializeRows(out.OutputPayload())
	cases := make([]*reportCase, 0, len(rows))
	for _, row := range rows {
		r, e := reportFromRow(row)
		if e != nil {
			return nil, "", e
		}
		cases = append(cases, r)
	}
	next := ""
	if out.GetMeta() != nil {
		next = out.GetMeta().Cursor
	}
	return cases, next, nil
}
func (s *Store) reportBudget(ctx context.Context, owner, site, key, room string, now time.Time) error {
	call, e := langparser.RenderCall("callsReportBudget", map[string]any{"siteId": site, "since": versionStamp(now.Add(-24 * time.Hour))})
	rows, e := s.readAs(auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(ctx)), owner, "report submission budget", call, e)
	if e != nil {
		return e
	}
	byKey, byRoom := 0, 0
	for _, r := range rows {
		if memql.BareShortId(rowString(r, "reporterKeyId")) == key {
			byKey++
		}
		if memql.BareShortId(rowString(r, "roomId")) == room {
			byRoom++
		}
	}
	if len(rows) >= 1000 || byKey >= 5 || byRoom >= 100 {
		return accountRefusal("report_rate_limited")
	}
	return nil
}
func (s *Store) reportAudits(ctx context.Context, owner, caseID string) ([]map[string]any, error) {
	call, e := langparser.RenderCall("callsReportAudit", map[string]any{"caseId": caseID})
	return s.readAs(auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(ctx)), owner, "private report access audit", call, e)
}

// The caller holds the case gate and has freshly checked its immutable expiry.
// An audit is committed before disclosure. Failure never returns private text.
func (s *Store) auditReport(ctx context.Context, owner string, r *reportCase, actor, action string, now time.Time) error {
	if !now.Before(r.ExpiresAt) {
		return accountRefusal("report_unavailable")
	}
	rows, e := s.reportAudits(ctx, owner, r.ID)
	if e != nil {
		return e
	}
	if len(rows) >= 256 {
		return accountRefusal("report_access_limit")
	}
	auditID := string(id.NewUntracked().FromBytes([]byte(r.ID + "\x00" + actor + "\x00" + action + "\x00" + versionStamp(now) + "\x00" + strconv.Itoa(len(rows)))))
	call, e := langparser.RenderCall("createCallsReportAudit", map[string]any{"auditId": auditID, "siteId": r.SiteID, "caseId": r.ID, "actorId": actor, "action": action, "recordedAt": versionStamp(now), "expiresAt": versionStamp(r.ExpiresAt), "revision": r.Revision})
	return s.writeAs(auth.ContextWithInternalOrigin(ctx), owner, absent(reportAuditConcept, auditID), "private report access audit", call, e)
}
