package work

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

func TestAppDispatchDBClaimAndAbandonArbitrateAcrossReplicas(t *testing.T) {
	first, second := openActsDB(t), openActsDB(t)
	for _, claimantFirst := range []bool{true, false} {
		name := "abandon-first"
		if claimantFirst {
			name = "claim-first"
		}
		t.Run(name, func(t *testing.T) {
			a, b := freshActsDBPair(t, first, second)
			now := time.Now().UTC()
			runID := a.openRun(t, map[string]any{"heartbeatAt": rfc(now.Add(-2 * time.Minute)), "routing": common.RouteChoice{Source: "route:apps", By: a.owner, NoPaidProviders: true}.Map()})
			ctx := actorCtx(a.owner)
			stale, err := a.i.store().runForOwner(memql.ContextWithFreshRead(ctx), runID)
			if err != nil {
				t.Fatal(err)
			}
			claim := automations.NewClusterExecutionGuard(func() *bun.DB { return b.db }, nil).StrictClaimer()
			req := DispatchRequest{RunId: runID, OwnerUserId: a.owner, GoalId: rowString(stale, "goalId"), Status: "running", AppPlacement: true, Recovery: true, RecoverySnapshot: appRecoverySnapshot(stale)}
			entered, resume := make(chan struct{}), make(chan struct{})
			var once sync.Once
			blocker := a.i
			if claimantFirst {
				blocker = b.i
			}
			original := blocker.decisionGate
			blocker.decisionGate = func(c context.Context, keys ...string) (func(), error) {
				release, err := original(c, keys...)
				if err == nil {
					once.Do(func() { close(entered); <-resume })
				}
				return release, err
			}
			claimDone := make(chan bool, 1)
			type outcome struct {
				claimed, abandoned bool
				err                error
			}
			sweepDone := make(chan outcome, 1)
			claimIt := func() { claimDone <- b.i.claimAppRun(ctx, req, claim, runID, runClaimTTL) }
			closeIt := func() {
				c, a, e := a.i.finishAppRecovery(ctx, stale, now.Add(-time.Minute), now, claim)
				sweepDone <- outcome{c, a, e}
			}
			if claimantFirst {
				go claimIt()
			} else {
				go closeIt()
			}
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("first replica never entered shared decision gate")
			}
			if claimantFirst {
				go closeIt()
			} else {
				go claimIt()
			}
			select {
			case <-claimDone:
				t.Fatal("claim escaped the held gate")
			case <-sweepDone:
				t.Fatal("abandon escaped the held gate")
			case <-time.After(50 * time.Millisecond):
			}
			close(resume)
			var claimed bool
			select {
			case claimed = <-claimDone:
			case <-time.After(3 * time.Second):
				t.Fatal("claim did not settle")
			}
			var result outcome
			select {
			case result = <-sweepDone:
			case <-time.After(3 * time.Second):
				t.Fatal("sweep did not settle")
			}
			if result.err != nil {
				t.Fatal(result.err)
			}
			if claimed != claimantFirst || result.claimed != claimantFirst || result.abandoned == claimantFirst {
				t.Fatalf("claim=%t sweep=%+v first=%t", claimed, result, claimantFirst)
			}
			row, err := a.i.store().runForOwner(memql.ContextWithFreshRead(ctx), runID)
			if err != nil {
				t.Fatal(err)
			}
			if rowString(row, "heartbeatAt") != rowString(stale, "heartbeatAt") {
				t.Fatal("arbitration fabricated a heartbeat")
			}
			if claimantFirst && rowString(row, "status") != "running" {
				t.Fatal("peer's fresh claim was abandoned")
			}
			if !claimantFirst && rowString(row, "status") != "abandoned" {
				t.Fatal("unclaimed run did not terminate normally")
			}
		})
	}
}

func TestAppDispatchDBOldClaimsAndChangedGenerationCannotCountAsProgress(t *testing.T) {
	a := openActsDB(t)
	now := time.Now().UTC()
	ctx := actorCtx(a.owner)
	claim := automations.NewClusterExecutionGuard(func() *bun.DB { return a.db }, nil).StrictClaimer()
	for _, shape := range []string{"old-claim", "new-answer", "cancelled", "new-rerun"} {
		t.Run(shape, func(t *testing.T) {
			id := a.openRun(t, map[string]any{"heartbeatAt": rfc(now.Add(-2 * time.Minute)), "routing": common.RouteChoice{Source: "route:apps", By: a.owner}.Map()})
			stale, err := a.i.store().runForOwner(memql.ContextWithFreshRead(ctx), id)
			if err != nil {
				t.Fatal(err)
			}
			req := DispatchRequest{RunId: id, OwnerUserId: a.owner, GoalId: rowString(stale, "goalId"), Status: "running", Recovery: true, AppPlacement: true, RecoverySnapshot: appRecoverySnapshot(stale)}
			switch shape {
			case "old-claim":
				if !claim.ClaimWithTTL(ctx, runClaimName, id, runClaimTTL) {
					t.Fatal("claim missing")
				}
				_, err = a.db.DB.ExecContext(ctx, `UPDATE automation_execution_claims SET claimed_at=now()-interval '2 minutes' WHERE automation_name=$1 AND dedup_key=$2`, runClaimName, id)
				if err != nil {
					t.Fatal(err)
				}
				c, ab, e := a.i.finishAppRecovery(ctx, stale, now.Add(-time.Minute), now, claim)
				if e != nil || c || !ab {
					t.Fatalf("old claim renewed progress: %t %t %v", c, ab, e)
				}
			case "new-answer":
				a.write(t, "updateWorkRun", map[string]any{"runId": id, "humanResumeId": "new-answer"})
			case "cancelled":
				a.write(t, "updateWorkRun", map[string]any{"runId": id, "cancelRequested": true})
			case "new-rerun":
				a.write(t, "updateWorkRun", map[string]any{"runId": id, "rerun": map[string]any{"requestId": "new-rerun", "reason": "rerun", "stepKey": "reason"}})
			}
			if a.i.claimAppRun(ctx, req, claim, id, runClaimTTL) {
				t.Fatal("stale recovery claimed a changed or abandoned run")
			}
		})
	}
}

func TestAppDispatchDBCanonicalAndBareHintsShareExistingClaims(t *testing.T) {
	a := openActsDB(t)
	ctx := actorCtx(a.owner)
	claim := automations.NewClusterExecutionGuard(func() *bun.DB { return a.db }, nil).StrictClaimer()
	for _, kind := range []string{"run", "rerun", "answer"} {
		t.Run(kind, func(t *testing.T) {
			id := a.openRun(t, map[string]any{"routing": common.RouteChoice{Source: "route:apps", By: a.owner}.Map()})
			req := DispatchRequest{RunId: memql.BareShortId(id), OwnerUserId: a.owner, Status: "running", AppPlacement: true}
			switch kind {
			case "rerun":
				req.RerunRequestId = "request"
				a.write(t, "updateWorkRun", map[string]any{"runId": id, "rerun": map[string]any{"requestId": "request", "reason": "rerun", "stepKey": "reason"}})
			case "answer":
				req.HumanResumeId = "answer"
				a.write(t, "updateWorkRun", map[string]any{"runId": id, "humanResumeId": "answer"})
			}
			// This is a legacy claim acquired without the new decision gate.
			bareKey, ttl := runClaimKey(req)
			if !claim.ClaimWithTTL(ctx, runClaimName, bareKey, ttl) {
				t.Fatal("legacy bare claim missing")
			}
			req.RunId = runConcept + ":" + memql.BareShortId(id)
			canonicalKey, _ := runClaimKey(req)
			if a.i.claimAppRun(ctx, req, claim, canonicalKey, ttl) {
				t.Fatal("canonical hint ignored an existing bare claim")
			}
			row, err := a.i.store().runForOwner(memql.ContextWithFreshRead(ctx), id)
			if err != nil {
				t.Fatal(err)
			}
			seen, err := claim.HasRecentClaim(ctx, runClaimName, appClaimKeys(row), time.Minute)
			if err != nil || !seen {
				t.Fatalf("sweep missed genuine bare peer claim: %t %v", seen, err)
			}
		})
	}
}

type appRecoveryDispatchProbe struct{ called chan DispatchRequest }

func (p *appRecoveryDispatchProbe) PrepareDispatch(_ context.Context, r DispatchRequest) (DispatchRequest, bool) {
	r.AppPlacement = true
	return r, true
}
func (p *appRecoveryDispatchProbe) Dispatch(_ context.Context, r DispatchRequest) { p.called <- r }

func TestAppDispatchDBNonExecutingLeaderObservesARealPeerRecovery(t *testing.T) {
	first, second := openActsDB(t), openActsDB(t)
	a, b := freshActsDBPair(t, first, second)
	now := time.Now().UTC()
	id := a.openRun(t, map[string]any{"heartbeatAt": rfc(now.Add(-2 * time.Minute)), "routing": common.RouteChoice{Source: "route:apps", By: a.owner}.Map()})
	ctx := actorCtx(a.owner)
	stale, err := a.i.store().runForOwner(memql.ContextWithFreshRead(ctx), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, peer := range []actsDB{a, b} {
		peer.i.SetRunClaimer(automations.NewClusterExecutionGuard(func() *bun.DB { return peer.db }, nil).StrictClaimer())
	}
	probe := &appRecoveryDispatchProbe{called: make(chan DispatchRequest, 3)}
	b.i.SetDispatcher(probe)
	a.i.SetAppRecoveryPublisher(b.i.HandleRunEvent)
	c, ab, err := a.i.recoverAppRun(ctx, stale, now, now.Add(-time.Minute))
	if err != nil || !c || ab {
		t.Fatalf("real peer claim not recognized: %t %t %v", c, ab, err)
	}
	select {
	case req := <-probe.called:
		if !req.Recovery || req.RecoverySnapshot != appRecoverySnapshot(stale) {
			t.Fatalf("changed recovery identity %+v", req)
		}
	case <-time.After(time.Second):
		t.Fatal("observed claim never dispatched")
	}
	current, err := a.i.store().runForOwner(memql.ContextWithFreshRead(ctx), id)
	if err != nil {
		t.Fatal(err)
	}
	if rowString(current, "heartbeatAt") != rowString(stale, "heartbeatAt") || rowString(current, "status") != "running" {
		t.Fatal("scheduling wrote invented progress")
	}
	// Duplicate recovery hints cannot dispatch again under a second claim.
	_, _, err = a.i.recoverAppRun(ctx, stale, now, now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-probe.called:
		t.Fatal("duplicate recovery executed twice")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestAppDispatchDBKeepsExactDueWaitAdmission(t *testing.T) {
	a := openActsDB(t)
	ctx := actorCtx(a.owner)
	claim := automations.NewClusterExecutionGuard(func() *bun.DB { return a.db }, nil).StrictClaimer()
	for _, shape := range []string{"due", "not-due", "wrong-wait", "ordinary-event"} {
		t.Run(shape, func(t *testing.T) {
			now := time.Now().UTC()
			since := rfc(now.Add(-time.Minute))
			at := rfc(now.Add(-time.Second))
			if shape == "not-due" {
				at = rfc(now.Add(time.Minute))
			}
			id := a.openRun(t, map[string]any{"status": "waiting", "waitingOn": map[string]any{"kind": "retry", "subject": "reason", "since": since, "resumeAt": at}, "routing": common.RouteChoice{Source: "route:apps", By: a.owner}.Map()})
			req := DispatchRequest{RunId: id, OwnerUserId: a.owner, Status: "waiting", AppPlacement: true, Recovery: true, RetryWait: since}
			if shape == "wrong-wait" {
				req.RetryWait = "earlier-wait"
			}
			if shape == "ordinary-event" {
				req.Recovery = false
			}
			key, ttl := runClaimKey(req)
			if got := a.i.claimAppRun(ctx, req, claim, key, ttl); got != (shape == "due") {
				t.Fatalf("due-wait admission %t for %s", got, shape)
			}
		})
	}
}
