package calls

import (
	"context"
	"encoding/json"

	"github.com/znasllc-io/memql/component/auth"
	callrules "github.com/znasllc-io/memql/component/calls"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

type reportReviewRequest struct {
	CaseID         string `json:"caseId"`
	Revision       int    `json:"revision"`
	Outcome        string `json:"outcome"`
	Note           string `json:"note"`
	IdempotencyKey string `json:"idempotencyKey"`
}

// Private metadata never enters persisted runs, and deployable ownership alone
// grants no reviewer authority. Authentication is checked before any payload.
func (i *Integration) reportOperator(ctx context.Context, args map[string]any, allowed ...string) (callrules.SiteRef, string, error) {
	if e := refuseInARun(ctx, "Report review is available only to private operators outside runs."); e != nil {
		return callrules.SiteRef{}, "", e
	}
	a, ok := auth.AccessFromContext(ctx)
	if !ok || a == nil || !a.IsClusterOwner() {
		return callrules.SiteRef{}, "", accountRefusal("report_operator_required")
	}
	for key := range args {
		found := false
		for _, v := range allowed {
			if key == v {
				found = true
			}
		}
		if !found {
			return callrules.SiteRef{}, "", callrules.ErrInvalid
		}
	}
	siteID, ok := args["siteId"].(string)
	if !ok || siteID == "" || len(siteID) > 128 {
		return callrules.SiteRef{}, "", callrules.ErrInvalid
	}
	site, e := i.siteFor(memql.ContextWithFreshRead(ctx), siteID)
	if e != nil {
		return callrules.SiteRef{}, "", accountRefusal("report_unavailable")
	}
	return callrules.SiteRef{SiteID: site.ID, OwnerUserID: site.OwnerUserID}, a.UserId + ":" + a.IdentityId, nil
}
func (i *Integration) reportNode(value any) ([]memorynodes.MemoryNode, error) {
	raw, e := json.Marshal(value)
	if e != nil {
		return nil, e
	}
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return nil, accountRefusal("report_unavailable")
	}
	return i.node("report", payload)
}
func (i *Integration) handleReportList(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	site, _, e := i.reportOperator(ctx, args, "siteId", "cursor")
	if e != nil {
		return nil, e
	}
	cursor := ""
	if v, exists := args["cursor"]; exists && v != nil {
		var ok bool
		cursor, ok = v.(string)
		if !ok || len(cursor) > 4096 {
			return nil, callrules.ErrInvalid
		}
	}
	rows, next, e := i.store.reportPage(ctx, site.OwnerUserID, site.SiteID, "", "", cursor, i.now())
	if e != nil {
		return nil, e
	}
	out := callrules.ReportResult{Items: []callrules.ReportReceipt{}, NextCursor: next}
	for _, r := range rows {
		out.Items = append(out.Items, r.receipt())
	}
	return i.reportNode(out)
}
func (i *Integration) handleReportRead(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	site, actor, e := i.reportOperator(ctx, args, "siteId", "caseId")
	if e != nil {
		return nil, e
	}
	caseID, ok := args["caseId"].(string)
	if !ok || !validReportID(caseID) {
		return nil, callrules.ErrInvalid
	}
	out, e := i.service.readReport(ctx, site, actor, caseID)
	if e != nil {
		return nil, e
	}
	return i.reportNode(out)
}
func (s *Service) readReport(ctx context.Context, site callrules.SiteRef, actor, caseID string) (map[string]any, error) {
	release, e := s.gates(ctx, reportGate(caseID))
	if e != nil {
		return nil, e
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	r, e := s.store.report(fresh, site.OwnerUserID, caseID)
	if e != nil {
		return nil, e
	}
	if r == nil || r.SiteID != site.SiteID || !s.now().Before(r.ExpiresAt) {
		return nil, accountRefusal("report_unavailable")
	}
	if e = s.store.auditReport(fresh, site.OwnerUserID, r, actor, "read", s.now()); e != nil {
		return nil, e
	}
	audit, e := s.store.reportAudits(fresh, site.OwnerUserID, r.ID)
	if e != nil {
		return nil, e
	}
	return map[string]any{"receipt": r.receipt(), "roomId": r.RoomID, "reporterSeat": r.State.ReporterSeat, "subjectSeat": r.State.SubjectSeat, "reporterKind": map[bool]string{true: "agent", false: "person"}[r.State.ReporterGrant != ""], "description": r.State.Description, "appeal": r.State.Appeal, "reviews": r.State.Reviews, "audit": audit}, nil
}
func (i *Integration) handleReportReview(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	site, actor, e := i.reportOperator(ctx, args, "siteId", "caseId", "revision", "outcome", "note", "idempotencyKey")
	if e != nil {
		return nil, e
	}
	q := reportReviewRequest{}
	var ok bool
	q.CaseID, ok = args["caseId"].(string)
	if !ok {
		return nil, callrules.ErrInvalid
	}
	q.Revision, ok = wholeNumber(args["revision"])
	if !ok {
		return nil, callrules.ErrInvalid
	}
	q.Outcome, ok = args["outcome"].(string)
	if !ok {
		return nil, callrules.ErrInvalid
	}
	q.Note, ok = args["note"].(string)
	if !ok {
		return nil, callrules.ErrInvalid
	}
	q.IdempotencyKey, ok = args["idempotencyKey"].(string)
	if !ok {
		return nil, callrules.ErrInvalid
	}
	out, e := i.service.reviewReport(ctx, site, actor, q)
	if e != nil {
		return nil, e
	}
	return i.reportNode(out)
}
func (s *Service) reviewReport(ctx context.Context, site callrules.SiteRef, actor string, q reportReviewRequest) (callrules.ReportResult, error) {
	none := callrules.ReportResult{}
	if !validReportID(q.CaseID) || q.Revision < 1 || q.Revision > 4 || !reportText(q.Note) || !fundingOperationID.MatchString(q.IdempotencyKey) || (q.Outcome != "no_action" && q.Outcome != "action_recommended") {
		return none, callrules.ErrInvalid
	}
	release, e := s.gates(ctx, reportGate(q.CaseID))
	if e != nil {
		return none, e
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	r, e := s.store.report(fresh, site.OwnerUserID, q.CaseID)
	if e != nil {
		return none, e
	}
	if r == nil || r.SiteID != site.SiteID || !s.now().Before(r.ExpiresAt) {
		return none, accountRefusal("report_unavailable")
	}
	for _, v := range r.State.Reviews {
		if v.Operation == q.IdempotencyKey {
			if v.Actor != actor || v.RequestHash != reportHash(q) {
				return none, accountRefusal("report_request_changed")
			}
			out := r.receipt()
			return callrules.ReportResult{Receipt: &out}, nil
		}
	}
	if r.Revision != q.Revision {
		return none, accountRefusal("report_revision_changed")
	}
	if r.State.Status != "submitted" && r.State.Status != "appealed" {
		return none, accountRefusal("report_review_unavailable")
	}
	if len(r.State.Reviews) >= 2 {
		return none, accountRefusal("report_review_unavailable")
	}
	// Audit precedes mutation. An uncertain write may spend audit capacity, never
	// review authority or a second case revision. There is no sanction side effect.
	if e = s.store.auditReport(fresh, site.OwnerUserID, r, actor, "review", s.now()); e != nil {
		return none, e
	}
	r.State.Reviews = append(r.State.Reviews, reportReview{Actor: actor, Operation: q.IdempotencyKey, RequestHash: reportHash(q), Outcome: q.Outcome, Note: q.Note, At: persistedCallTime(s.now()), Revision: q.Revision})
	if r.State.Status == "appealed" {
		r.State.Status = "closed"
	} else {
		r.State.Status = "reviewed"
	}
	r.Revision++
	if e = s.store.saveReport(fresh, site.OwnerUserID, r, s.now()); e != nil {
		return none, e
	}
	out := r.receipt()
	return callrules.ReportResult{Receipt: &out}, nil
}
