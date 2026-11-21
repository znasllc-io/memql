package calls

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/core/common"
)

func enableReports(t *testing.T, cw *callWorld) {
	t.Helper()
	_, e := cw.builtin(actorContext(cw.developer, auth.RoleDeveloper), "callsPolicySet", map[string]any{"siteId": cw.site, "reportsEnabled": 1, "reportMetadataDays": 1})
	require.NoError(t, e)
	t.Cleanup(func() {
		_, _ = cw.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("concept IN (?, ?)", reportConcept, reportAuditConcept).Where("payload->>'siteId' = ?", siteConcept+":"+cw.site).Exec(context.Background())
	})
}
func reportAsk(start callrules.StartResult, nonce string) callrules.ReportRequest {
	return callrules.ReportRequest{RoomID: start.Call.ID, Identity: start.Join.Identity, Category: "other", Description: "Benign fixture: please review the observed interruption.", IdempotencyKey: nonce}
}
func reportDo(t *testing.T, cw *callWorld, sess callrules.Session, action string, q callrules.ReportRequest) callrules.ReportResult {
	t.Helper()
	out, e := cw.svc.Account(context.Background(), cw.ref, sess.Token, &callrules.AccountRequest{Action: action, Report: &q})
	require.NoError(t, e)
	require.NotNil(t, out.Report)
	return *out.Report
}
func TestReportsPrivateReviewAppealAndExpiry(t *testing.T) {
	cw, _, host, start, _ := processingFixture(t, false)
	if cw == nil {
		return
	}
	ctx := context.Background()
	q := reportAsk(start, "report-submit-once-0001")
	_, e := cw.svc.Account(ctx, cw.ref, host.Token, &callrules.AccountRequest{Action: "report_submit", Report: &q})
	require.ErrorContains(t, e, "reports_unavailable")
	_, e = cw.builtin(actorContext(cw.developer, auth.RoleDeveloper), "callsPolicySet", map[string]any{"siteId": cw.site, "reportsEnabled": 1})
	require.ErrorContains(t, e, "explicit report metadata expiry")
	enableReports(t, cw)
	first := reportDo(t, cw, host, "report_submit", q)
	require.Equal(t, "submitted", first.Receipt.Status)
	require.Equal(t, 1, first.Receipt.Revision)
	require.Equal(t, 24*time.Hour, first.Receipt.ExpiresAt.Sub(first.Receipt.CreatedAt))
	retry := reportDo(t, cw, host, "report_submit", q)
	require.Equal(t, first, retry)
	changed := q
	changed.Description = "Changed allegation with the old request nonce."
	_, e = cw.svc.report(ctx, cw.ref, host.Token, "report_submit", changed)
	require.ErrorContains(t, e, "report_request_changed")
	joined, guest, e := cw.join(start.Call.GuestToken, "Other participant")
	require.NoError(t, e)
	cw.media.setPresent(start.Call.ID, person(start.Join.Identity), person(joined.Identity))
	_, e = cw.svc.report(ctx, cw.ref, guest.Token, "report_status", callrules.ReportRequest{ID: first.Receipt.ID})
	require.ErrorContains(t, e, "report_unavailable")
	other := reportDo(t, cw, guest, "report_list", callrules.ReportRequest{})
	require.Empty(t, other.Items)
	list := reportDo(t, cw, host, "report_list", callrules.ReportRequest{})
	require.Len(t, list.Items, 1)
	payload, _ := json.Marshal(list)
	require.NotContains(t, string(payload), q.Description)
	require.NotContains(t, string(payload), "reporterSeat")
	args := map[string]any{"siteId": cw.site, "caseId": first.Receipt.ID}
	_, e = cw.calls.handleReportRead(actorContext(cw.developer, auth.RoleDeveloper), args, 0)
	require.ErrorContains(t, e, "report_operator_required")
	operator := actorContext(cw.owner, auth.RoleOwner)
	_, e = cw.calls.handleReportRead(common.ContextWithRun(operator, common.RunContext{RunId: "never-report-journal"}), args, 0)
	require.ErrorContains(t, e, "outside runs")
	_, e = cw.calls.handleReportRead(operator, args, 0)
	require.NoError(t, e)
	audits, e := cw.svc.store.reportAudits(ctx, cw.owner, first.Receipt.ID)
	require.NoError(t, e)
	require.Len(t, audits, 1)
	review := reportReviewRequest{CaseID: first.Receipt.ID, Revision: 1, Outcome: "no_action", Note: "Benign fixture reviewed; no action recommended.", IdempotencyKey: "review-operation-0001"}
	reviewed, e := cw.svc.reviewReport(ctx, cw.ref, "operator", review)
	require.NoError(t, e)
	require.True(t, reviewed.Receipt.CanAppeal)
	require.Equal(t, 2, reviewed.Receipt.Revision)
	again, e := cw.svc.reviewReport(ctx, cw.ref, "operator", review)
	require.NoError(t, e)
	require.Equal(t, reviewed, again)
	changedReview := review
	changedReview.Outcome = "action_recommended"
	_, e = cw.svc.reviewReport(ctx, cw.ref, "operator", changedReview)
	require.ErrorContains(t, e, "report_request_changed")
	appeal := callrules.ReportRequest{ID: first.Receipt.ID, Revision: 2, Description: "Benign fixture asks for one further metadata review.", IdempotencyKey: "report-appeal-0001"}
	appealed := reportDo(t, cw, host, "report_appeal", appeal)
	require.Equal(t, "appealed", appealed.Receipt.Status)
	require.Equal(t, 3, appealed.Receipt.Revision)
	require.Equal(t, appealed, reportDo(t, cw, host, "report_appeal", appeal))
	review.Revision = 3
	review.IdempotencyKey = "review-operation-0002"
	review.Outcome = "action_recommended"
	closed, e := cw.svc.reviewReport(ctx, cw.ref, "operator", review)
	require.NoError(t, e)
	require.Equal(t, "closed", closed.Receipt.Status)
	require.Equal(t, 4, closed.Receipt.Revision)
	require.False(t, closed.Receipt.CanAppeal)
	require.Equal(t, first.Receipt.ExpiresAt, closed.Receipt.ExpiresAt)
	var count int
	require.NoError(t, cw.db.NewSelect().TableExpr(`"MemoryNodes"`).ColumnExpr("count(*)").Where("concept = ? AND id = ?", reportConcept, reportConcept+":"+first.Receipt.ID).Scan(ctx, &count))
	require.Equal(t, 4, count)
	appeal.Revision = 4
	appeal.IdempotencyKey = "report-appeal-0002"
	_, e = cw.svc.report(ctx, cw.ref, host.Token, "report_appeal", appeal)
	require.ErrorContains(t, e, "report_appeal_unavailable")
	_, e = cw.builtin(actorContext(cw.developer, auth.RoleDeveloper), "callsPolicySet", map[string]any{"siteId": cw.site, "reportsEnabled": 0})
	require.NoError(t, e)
	require.Equal(t, closed, reportDo(t, cw, host, "report_status", callrules.ReportRequest{ID: first.Receipt.ID}))
	cw.clock.Advance(24 * time.Hour)
	_, e = cw.svc.readReport(ctx, cw.ref, "operator", first.Receipt.ID)
	require.ErrorContains(t, e, "report_unavailable")
	_, e = cw.svc.reviewReport(ctx, cw.ref, "operator", review)
	require.ErrorContains(t, e, "report_unavailable")
	finalAudits, e := cw.svc.store.reportAudits(ctx, cw.owner, first.Receipt.ID)
	require.NoError(t, e)
	require.Len(t, finalAudits, 3)
}
func TestReportsAdmissionReplayBudgetAndCrossReplicaClicks(t *testing.T) {
	cw, _, host, start, _ := processingFixture(t, false)
	if cw == nil {
		return
	}
	enableReports(t, cw)
	ctx := context.Background()
	q := reportAsk(start, "report-click-burst-0001")
	bad := q
	bad.Identity = "old-admission"
	_, e := cw.svc.report(ctx, cw.ref, host.Token, "report_submit", bad)
	require.ErrorContains(t, e, "report_admission_required")
	bad = q
	bad.TargetIdentity = "absent-peer"
	_, e = cw.svc.report(ctx, cw.ref, host.Token, "report_submit", bad)
	require.ErrorContains(t, e, "report_target_unavailable")
	peer := allowanceReplica(t, cw)
	peer.commerce = cw.svc.commerce
	var wg sync.WaitGroup
	out := make(chan callrules.ReportResult, 8)
	errs := make(chan error, 8)
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			svc := cw.svc
			if n%2 == 1 {
				svc = peer
			}
			r, e := svc.report(ctx, cw.ref, host.Token, "report_submit", q)
			out <- r
			errs <- e
		}(n)
	}
	wg.Wait()
	close(out)
	close(errs)
	for e := range errs {
		require.NoError(t, e)
	}
	ids := map[string]bool{}
	for r := range out {
		ids[r.Receipt.ID] = true
	}
	require.Len(t, ids, 1)
	for n := 1; n < 5; n++ {
		q.IdempotencyKey = fmt.Sprintf("report-distinct-%04d", n)
		reportDo(t, cw, host, "report_submit", q)
	}
	q.IdempotencyKey = "report-over-budget-0001"
	_, e = cw.svc.report(ctx, cw.ref, host.Token, "report_submit", q)
	require.ErrorContains(t, e, "report_rate_limited")
	// Keep another participant present so leaving does not correctly end an empty room.
	joined, _, e := cw.join(start.Call.GuestToken, "Remaining guest")
	require.NoError(t, e)
	cw.media.setPresent(start.Call.ID, person(start.Join.Identity), person(joined.Identity))
	// Current-session cookie plus an old admission identity cannot submit after reentry.
	require.NoError(t, cw.svc.LeaveWithOptions(ctx, host, departureAsk(t, cw.svc, host, callrules.LeaveKeepRunning, "report-host-leave-0001")))
	back, _, e := cw.svc.Join(ctx, cw.ref, &host, callrules.JoinBody{HostToken: start.Call.HostToken, Name: "Host returned", Confirm: confirmed})
	require.NoError(t, e)
	require.NotEqual(t, q.Identity, back.Identity)
	_, e = cw.svc.report(ctx, cw.ref, host.Token, "report_submit", q)
	require.ErrorContains(t, e, "report_admission_required")
}
func TestReportsFamilyChildCanReportWithoutAccountCeremony(t *testing.T) {
	cw, k, start, r := familyFixture(t, "report-family")
	if cw == nil {
		return
	}
	enableReports(t, cw)
	ctx := context.Background()
	inv, child := familyInviteClaim(t, cw, k, start)
	familyApprove(t, cw, k, start, inv)
	cw.media.setPresent(r.MediaRoom, person(start.Join.Identity))
	joined, child, e := cw.svc.Join(ctx, cw.ref, &child, callrules.JoinBody{Family: true, FamilyInvitationID: inv.ID, Name: "Child fixture"})
	require.NoError(t, e)
	q := reportAsk(start, "report-child-fixture-0001")
	q.Identity = joined.Identity
	q.TargetIdentity = start.Join.Identity
	q.Category = "child_safety"
	result := reportDo(t, cw, child, "report_submit", q)
	require.Equal(t, "submitted", result.Receipt.Status)
	// Neither the guardian's role nor sharing a family room grants access.
	_, e = cw.svc.report(ctx, cw.ref, k.result.Session.Token, "report_status", callrules.ReportRequest{ID: result.Receipt.ID})
	require.ErrorContains(t, e, "report_unavailable")
	_, e = cw.svc.Account(ctx, cw.ref, child.Token, &callrules.AccountRequest{Action: "add_begin"})
	require.ErrorContains(t, e, "family_child_account_unavailable")
}
func TestReportsAgentScopeAndOwnGrantIsolation(t *testing.T) {
	cw, k, host, start, _ := processingFixture(t, false)
	if cw == nil {
		return
	}
	enableReports(t, cw)
	cleanupDelegationTestRows(t, cw)
	ctx := context.Background()
	ordinary := reportDo(t, cw, host, "report_submit", reportAsk(start, "human-report-fixture-01"))
	grant := delegationTestRequest(cw)
	grant.Scopes = []string{"read", "join"}
	oldToken, _ := delegationTestGrant(t, cw, k, grant)
	_, e := cw.svc.Agent(ctx, cw.ref, oldToken, callrules.AgentRequest{Action: "report_list", Report: &callrules.ReportRequest{}})
	require.ErrorContains(t, e, "delegation_scope")
	grant.Scopes = append(grant.Scopes, "report")
	token, grantID := delegationTestGrant(t, cw, k, grant)
	pending := requestGuestAgent(t, cw, token, start, "report-agent-request-01")
	view, e := cw.svc.SessionCall(ctx, host)
	require.NoError(t, e)
	require.NoError(t, cw.svc.Host(ctx, host, callrules.HostActionRequest{Action: "agent_approve", RequestID: pending.RequestID, ReviewDigest: view.PendingAgents[0].ReviewDigest}))
	raw, e := cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "guest_join", GuestToken: start.Call.GuestToken, CallID: start.Call.ID, RequestID: pending.RequestID, IdempotencyKey: "report-agent-admit-01", Acceptance: acceptedAgent(t, cw, token)})
	require.NoError(t, e)
	joined := raw.(callrules.JoinResult)
	q := reportAsk(start, "agent-report-fixture-01")
	q.Identity = joined.Identity
	raw, e = cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "report_submit", Report: &q})
	require.NoError(t, e)
	submitted := raw.(callrules.ReportResult)
	_, e = cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "report_status", Report: &callrules.ReportRequest{ID: ordinary.Receipt.ID}})
	require.ErrorContains(t, e, "report_unavailable")
	_, e = cw.svc.report(ctx, cw.ref, host.Token, "report_status", callrules.ReportRequest{ID: submitted.Receipt.ID})
	require.ErrorContains(t, e, "report_unavailable")
	raw, e = cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "report_list", Report: &callrules.ReportRequest{}})
	require.NoError(t, e)
	require.Len(t, raw.(callrules.ReportResult).Items, 1)
	d, e := cw.svc.store.delegation(ctx, cw.owner, grantID)
	require.NoError(t, e)
	d.State.RevokedAt = cw.clock.Now()
	require.NoError(t, cw.svc.store.saveDelegation(ctx, cw.owner, d, cw.clock.Now()))
	_, e = cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "report_status", Report: &callrules.ReportRequest{ID: submitted.Receipt.ID}})
	require.Error(t, e)
}
func TestReportsPaginationAndConcurrentReviewStayBoundToRecipient(t *testing.T) {
	cw, _, host, start, _ := processingFixture(t, false)
	if cw == nil {
		return
	}
	enableReports(t, cw)
	ctx := context.Background()
	original := reportDo(t, cw, host, "report_submit", reportAsk(start, "report-page-fixture-01"))
	source, e := cw.svc.store.report(ctx, cw.owner, original.Receipt.ID)
	require.NoError(t, e)
	// Seed benign rows directly to cover the read pagination contract independently
	// of submission's intentionally lower rolling per-key budget.
	for n := 0; n < 28; n++ {
		r := *source
		r.ID = fmt.Sprintf("%064x", n+1)
		r.Version = time.Time{}
		r.State.ReporterGrant = ""
		if n == 27 {
			r.State.ReporterGrant = "different-grant"
		}
		require.NoError(t, cw.svc.store.saveReport(ctx, cw.owner, &r, cw.clock.Now()))
	}
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		page := reportDo(t, cw, host, "report_list", callrules.ReportRequest{Cursor: cursor})
		require.LessOrEqual(t, len(page.Items), 25)
		for _, r := range page.Items {
			require.False(t, seen[r.ID])
			seen[r.ID] = true
		}
		pages++
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
		require.Less(t, pages, 5)
		cw.clock.Advance(time.Millisecond)
	}
	require.Len(t, seen, 28)
	require.Equal(t, 2, pages)
	peer := allowanceReplica(t, cw)
	peer.commerce = cw.svc.commerce
	q := reportReviewRequest{CaseID: original.Receipt.ID, Revision: 1, Outcome: "no_action", Note: "Benign simultaneous operator review fixture.", IdempotencyKey: "report-review-race-01"}
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for n, svc := range []*Service{cw.svc, peer} {
		wg.Add(1)
		go func(n int, svc *Service) {
			defer wg.Done()
			req := q
			req.IdempotencyKey = fmt.Sprintf("report-review-race-%02d", n)
			_, err := svc.reviewReport(ctx, cw.ref, "operator", req)
			errs <- err
		}(n, svc)
	}
	wg.Wait()
	close(errs)
	success, refused := 0, 0
	for err := range errs {
		if err == nil {
			success++
		} else {
			require.ErrorContains(t, err, "report_revision_changed")
			refused++
		}
	}
	require.Equal(t, 1, success)
	require.Equal(t, 1, refused)
	r, e := cw.svc.store.report(ctx, cw.owner, original.Receipt.ID)
	require.NoError(t, e)
	require.Equal(t, 2, r.Revision)
	require.Len(t, r.State.Reviews, 1)
}
