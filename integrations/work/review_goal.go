package work

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
)

// ReviewGoalReceipt identifies one request, including after an interrupted
// bootstrap or a lost response. None of these identities change on retry.
type ReviewGoalReceipt struct {
	GoalID     string `json:"goalId"`
	RunID      string `json:"runId"`
	ApprovalID string `json:"approvalId"`
}

// OpenReviewGoal opens a known template already waiting for human approval.
// The calling integration must authorize and capture its immutable proposal
// before this call, and revalidate source access/revisions before executing.
// This is an internal Go seam, not a capability accepting arbitrary templates.
// No run is published as running during bootstrap; the existing Nexus decision
// flow is the only way to release it. A partially written request is recoverable.
func (i *Integration) OpenReviewGoal(ctx context.Context, requestKey string, g DirectGoal, subject map[string]any, question string) (ReviewGoalReceipt, error) {
	var receipt ReviewGoalReceipt
	owner := strings.TrimSpace(g.OwnerUserId)
	requestKey, question = strings.TrimSpace(requestKey), strings.TrimSpace(question)
	if i == nil || owner == "" || memql.BareShortId(callerUserId(ctx)) != memql.BareShortId(owner) || requestKey == "" || len(requestKey) > 200 || strings.TrimSpace(g.Statement) == "" || strings.TrimSpace(g.AutomationName) == "" || question == "" || len(subject) == 0 {
		return receipt, fmt.Errorf("work: a reviewed request needs its authenticated owner, stable key, proposal and template")
	}
	receipt = ReviewGoalIdentity(owner, requestKey)
	identity := receipt.GoalID
	var db *sql.DB
	if i.bunDB != nil {
		if handle := i.bunDB(); handle != nil {
			db = handle.DB
		}
	}
	release, err := memql.AcquireWriteGate(ctx, db, "work-review-goal:"+identity)
	if err != nil {
		return receipt, err
	}
	defer release()
	ctx = memql.ContextWithFreshRead(ctx)
	fingerprint := workstate.ArtifactHash(map[string]any{
		"statement": g.Statement, "automation": g.AutomationName, "input": optMap(g.Input),
		"accounts": optStrings(g.AccountIds), "ceilings": optMap(g.Ceilings),
		"requestedVia": g.RequestedVia, "triggeredBy": g.TriggeredBy, "subject": subject, "question": question,
	})
	scoped := ownerActor(ctx, owner)
	st := i.store()
	goal, err := st.goalForOwner(scoped, receipt.GoalID)
	if err != nil {
		return receipt, err
	}
	if goal != nil && rowString(goal, "requestFingerprint") != fingerprint {
		return receipt, fmt.Errorf("work: this review request was already used for a different proposal")
	}
	if goal == nil {
		if err = st.createGoalRow(scoped, goalSeed{GoalId: receipt.GoalID, Statement: g.Statement, Origin: "user", Input: g.Input,
			AccountIds: g.AccountIds, Ceilings: g.Ceilings, RequestedVia: g.RequestedVia, RequestFingerprint: fingerprint}); err != nil {
			return receipt, err
		}
	}
	run, err := st.runForOwner(scoped, receipt.RunID)
	if err != nil {
		return receipt, err
	}
	if run == nil {
		if goal != nil && rowString(goal, "status") == "closed" {
			return receipt, fmt.Errorf("work: this review goal is closed")
		}
		// The consumer binds before any run or approval becomes reachable.
		if g.BeforeRun != nil {
			if err = g.BeforeRun(scoped, receipt.GoalID, receipt.RunID); err != nil {
				return receipt, err
			}
		}
		var authority map[string]any
		if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil && !ac.Synthetic && !ac.Unranked {
			authority, err = auth.CaptureExecutionAuthority(ctx)
			if err != nil {
				return receipt, err
			}
		}
		now := i.clock().UTC()
		if err = st.createRunRow(scoped, runSeed{RunId: receipt.RunID, GoalId: receipt.GoalID, AutomationName: g.AutomationName,
			Input: g.Input, Variables: g.Input, Mode: modeLive, Status: runStatusWaiting, NodeId: selfNodeId(), StartedAt: now,
			OwnerUserId: owner, TriggeredBy: g.TriggeredBy, ExecutionAuthority: authority,
			WaitingOn: map[string]any{"kind": "approval", "subject": receipt.ApprovalID, "since": rfc(now)}}); err != nil {
			return receipt, err
		}
	}
	approval, err := one(st.query(scoped, "query "+call("workApprovalForOwner", map[string]any{"approvalId": receipt.ApprovalID})))
	if err != nil {
		return receipt, err
	}
	if approval == nil {
		// A cancelled request must not acquire a fresh pending approval.
		if run != nil && rowString(run, "status") != runStatusWaiting {
			return receipt, fmt.Errorf("work: this review run is no longer waiting")
		}
		if err = st.createApprovalRow(scoped, approvalSeed{ApprovalId: receipt.ApprovalID, RunId: receipt.RunID,
			Kind: workstate.ApprovalKindPlanReview, Subject: subject, ArtifactHash: workstate.ArtifactHash(subject),
			Question: question, RequestedAt: i.clock().UTC()}); err != nil {
			return receipt, err
		}
	}
	return receipt, nil
}

// ReviewGoalIdentity lets the requesting integration recover its own receipt.
// It confers no authority; every read still uses the caller's owner-scoped query.
func ReviewGoalIdentity(owner, requestKey string) ReviewGoalReceipt {
	identity := workstate.ArtifactHash(map[string]any{"owner": memql.BareShortId(owner), "request": requestKey})
	return ReviewGoalReceipt{GoalID: "review-goal-" + identity, RunID: "review-run-" + identity, ApprovalID: "review-approval-" + identity}
}

// SetPlanReviewValidator installs the integration that can check the current
// source behind a captured proposal. Each accepting replica needs this wiring.
func (i *Integration) SetPlanReviewValidator(kind string, validate func(context.Context, map[string]any) error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.reviewValidators == nil {
		i.reviewValidators = make(map[string]func(context.Context, map[string]any) error)
	}
	i.reviewValidators[kind] = validate
}

func (i *Integration) validatePlanReview(ctx context.Context, kind string, subject map[string]any) error {
	if kind != workstate.ApprovalKindPlanReview || rowString(subject, "reviewType") == "" {
		return nil
	}
	i.mu.RLock()
	validate := i.reviewValidators[rowString(subject, "reviewType")]
	i.mu.RUnlock()
	if validate == nil {
		return fmt.Errorf("work: this node cannot validate the source of this review")
	}
	return validate(ctx, subject)
}

// recoverReviewDecision only recovers the fixed-template review bootstrap's
// resume. It never replays training, promotion, or other approval side effects.
func (i *Integration) recoverReviewDecision(ctx context.Context, approvalID, decision string) (map[string]any, error) {
	st := i.store()
	approval, err := one(st.query(ctx, "query "+call("workApprovalForOwner", map[string]any{"approvalId": approvalID})))
	if err != nil || approval == nil {
		return nil, err
	}
	if rowString(approval, "kind") != workstate.ApprovalKindPlanReview || rowString(approval, "decision") != decision {
		return nil, nil
	}
	run, err := st.runForOwner(ctx, rowString(approval, "runId"))
	if err != nil || run == nil {
		return nil, err
	}
	goal, err := st.goalForOwner(ctx, rowString(run, "goalId"))
	if err != nil || goal == nil {
		return nil, err
	}
	if rowString(goal, "requestFingerprint") == "" || !strings.HasPrefix(memql.BareShortId(rowString(goal, "id")), "review-goal-") {
		return nil, nil
	}
	if decision != "rejected" && rowString(run, "status") == runStatusWaiting {
		if workstate.ArtifactHash(rowMap(approval, "subject")) != rowString(approval, "artifactHash") {
			return nil, fmt.Errorf("work: the reviewed proposal changed")
		}
		if err := i.validatePlanReview(ctx, workstate.ApprovalKindPlanReview, rowMap(approval, "subject")); err != nil {
			return nil, err
		}
	}
	resumed, err := i.resumeParkedRun(ownerActor(ctx, rowString(approval, "ownerUserId")), rowString(approval, "runId"), approvalID, decision, i.clock().UTC())
	if err != nil {
		return nil, err
	}
	return map[string]any{"approvalId": approvalID, "runId": memql.BareShortId(rowString(approval, "runId")), "decision": decision, "runResumed": resumed, "recovered": true}, nil
}
