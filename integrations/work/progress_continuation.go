package work

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

// Estimates can grow, declared ceilings cannot. The count and the evidence
// watermark belong to the run, so another replica cannot restart either.
const maxAutomaticContinuations = 3

func progressCeilings(goal, run map[string]any, declared workstate.Ceilings) (workstate.Ceilings, error) {
	classification := rowMap(run, "classification")
	effective, err := approvedWorkloadCeilings(goal, declared, rowString(classification, "workload"))
	if err != nil {
		return effective, err
	}
	state := rowMap(classification, "progressContinuation")
	extensions := rowInt(state, "extensions")
	if extensions < 0 || extensions > 16 {
		return effective, fmt.Errorf("invalid work continuation count")
	}
	// Divide before multiplying so historical overrides cannot overflow.
	if effective.MaxModelCalls > 0 && declared.MaxModelCalls > effective.MaxModelCalls {
		effective.MaxModelCalls = int(scaleProgressLimit(int64(effective.MaxModelCalls), int64(declared.MaxModelCalls), extensions+1))
	}
	if effective.WallClockMs > 0 && declared.WallClockMs > effective.WallClockMs {
		effective.WallClockMs = scaleProgressLimit(effective.WallClockMs, declared.WallClockMs, extensions+1)
	}
	return effective, nil
}

// ContinueWorkProgress is reached only after a soft estimate has been spent.
// It records a bounded extension for newly completed work, never for elapsed
// time, a heartbeat, an assertion of progress or a repeated failed call.
func (i *Integration) ContinueWorkProgress(ctx context.Context, rc common.RunContext, spent workstate.Spent, estimatedTokens int) (bool, error) {
	if rc.Mode == common.RunModeReplay || rc.OwnerUserId == "" || rc.GoalId == "" {
		return false, nil
	}
	ctx = ownerActor(memql.ContextWithFreshRead(ctx), rc.OwnerUserId)
	release, err := i.decisionGate(ctx, "workload:"+memql.BareShortId(rc.RunId))
	if err != nil {
		return false, err
	}
	defer release()
	run, err := i.store().runForOwner(ctx, rc.RunId)
	if err != nil {
		return false, err
	}
	if run == nil || run["cancelRequested"] == true || rowString(run, "status") != runStatusRunning || memql.BareShortId(rowString(run, "goalId")) != memql.BareShortId(rc.GoalId) {
		return false, nil
	}
	goal, err := i.store().goalForOwner(ctx, rc.GoalId)
	if err != nil || goal == nil {
		return false, fmt.Errorf("work continuation goal unavailable: %v", err)
	}
	if rowString(goal, "status") == "closed" {
		return false, fmt.Errorf("the goal is closed")
	}
	declared, err := ceilingsOf(goal)
	if err != nil {
		return false, err
	}
	// No automatic permission to spend beyond any explicit or overall bound.
	if workstate.CheckCeilings(enforceableHere(declared), spent, estimatedTokens) != nil {
		return false, nil
	}
	effective, err := progressCeilings(goal, run, declared)
	if err != nil {
		return false, err
	}
	breach := workstate.CheckCeilings(enforceableHere(effective), spent, estimatedTokens)
	if breach == nil {
		return true, nil
	} // A peer already extended this window.
	if breach.Ceiling != workstate.CeilingModelCalls && breach.Ceiling != workstate.CeilingWallClock {
		return false, nil
	}
	classification := rowMap(run, "classification")
	state := rowMap(classification, "progressContinuation")
	extensions := rowInt(state, "extensions")
	if extensions >= maxAutomaticContinuations {
		return false, nil
	}
	next, err := progressWindowAfter(goal, run, declared, spent, estimatedTokens, maxAutomaticContinuations)
	if err != nil || next == 0 {
		return false, err
	}
	seen := rowStringSlice(state, "receipts")
	rows, err := i.store().query(ctx, "query "+call("workProgressReceiptsForOwnerRun", map[string]any{"runId": rc.RunId}))
	if err != nil {
		return false, err
	}
	fresh := false
	for _, row := range rows {
		if memql.BareShortId(rowString(row, "ownerUserId")) != memql.BareShortId(rc.OwnerUserId) || memql.BareShortId(rowString(row, "runId")) != memql.BareShortId(rc.RunId) {
			return false, fmt.Errorf("work progress receipt attribution changed")
		}
		key := progressReceiptKey(row)
		if key != "" && !slices.Contains(seen, key) {
			fresh = true
			seen = append(seen, key)
		}
	}
	if !fresh || len(seen) > 300 {
		return false, nil
	}
	if classification == nil {
		classification = map[string]any{}
	}
	classification["progressContinuation"] = map[string]any{"extensions": next, "receipts": seen, "continuedAt": rfc(i.clock()), "reason": "Completed new work within the original limits."}
	if err := i.store().updateRun(ctx, rc.RunId, map[string]any{"classification": classification, "versionTime": rfc(workRowVersionAfter(run["createdAt"], i.clock()))}); err != nil {
		return false, err
	}
	return true, nil
}

func progressReceiptKey(row map[string]any) string {
	data := rowMap(row, "data")
	if rowString(row, "kind") != "tool_result" || data["isError"] != false || data["argsTruncated"] == true {
		return ""
	}
	name := rowString(data, "tool")
	short := name[strings.LastIndex(name, ".")+1:]
	switch short {
	case "", "discoverCapabilities", "recallWorkHistory", "requestUserFeedback", "respondToUser":
		return ""
	}
	var args any
	if json.Unmarshal([]byte(rowString(data, "args")), &args) != nil {
		return ""
	}
	return workstate.ArtifactHash(map[string]any{"tool": name, "args": args})
}

// A real feedback answer may request one further work window. It does not
// reset the automatic counter, usage, evidence watermark or declared limits.
// Recording the decision id makes a lost reply safe to retry on another replica.
func (i *Integration) continueFromProgressFeedback(ctx context.Context, approval map[string]any) error {
	runID := rowString(approval, "runId")
	release, err := i.decisionGate(ctx, "workload:"+memql.BareShortId(runID))
	if err != nil {
		return err
	}
	defer release()
	ctx = memql.ContextWithFreshRead(ctx)
	run, err := i.store().runForOwner(ctx, runID)
	if err != nil || run == nil {
		return fmt.Errorf("saved work is unavailable: %v", err)
	}
	classification := rowMap(run, "classification")
	state := rowMap(classification, "progressContinuation")
	decisionID := memql.BareShortId(rowString(approval, "id"))
	if rowString(run, "status") != runStatusWaiting || run["cancelRequested"] == true || memql.BareShortId(rowString(rowMap(run, "waitingOn"), "subject")) != decisionID {
		return fmt.Errorf("this feedback no longer belongs to the waiting work")
	}
	if err := i.validateGuidanceRun(ctx, approval); err != nil {
		return err
	}
	if err := i.validateProgressCheckpoint(ctx, approval); err != nil {
		return err
	}
	goal, err := i.store().goalForOwner(ctx, rowString(run, "goalId"))
	if err != nil || goal == nil {
		return fmt.Errorf("saved goal is unavailable: %v", err)
	}
	if rowString(goal, "status") == "closed" {
		return fmt.Errorf("the goal is closed")
	}
	declared, err := ceilingsOf(goal)
	if err != nil {
		return err
	}
	var spent workstate.Spent
	raw, err := json.Marshal(rowMap(run, "spent"))
	if err != nil {
		return err
	}
	if err = json.Unmarshal(raw, &spent); err != nil {
		return err
	}
	if workstate.CheckCeilings(enforceableHere(declared), spent, 0) != nil {
		return errProgressLimit
	}
	if decisionID != "" && rowString(state, "feedbackId") == decisionID {
		return nil
	}
	next, err := progressWindowAfter(goal, run, declared, spent, 0, 16)
	if err != nil {
		return err
	}
	if next == 0 {
		return errProgressLimit
	}
	if classification == nil {
		classification = map[string]any{}
	}
	if state == nil {
		state = map[string]any{}
	}
	state["extensions"], state["feedbackId"], state["continuedAt"] = next, decisionID, rfc(i.clock())
	classification["progressContinuation"] = state
	if err := verifyApprovalDecisionWrite(ctx); err != nil {
		return err
	}
	return i.store().updateRun(ctx, runID, map[string]any{"classification": classification, "versionTime": rfc(workRowVersionAfter(run["createdAt"], i.clock()))})
}

var errProgressLimit = errors.New("the work has reached its overall limit; its results are saved for a new request with a narrower scope")

func scaleProgressLimit(base, ceiling int64, windows int) int64 {
	if base <= 0 || ceiling <= 0 || windows < 1 {
		return base
	}
	if base > ceiling/int64(windows) {
		return ceiling
	}
	return min(ceiling, base*int64(windows))
}

// Check the original constraints when presenting feedback, and again when the
// person answers. Time spent waiting for their answer is already credited by
// the shared human-resume path; usage and completed work are never erased.
func (i *Integration) progressFeedback(ctx context.Context, runID, stepKey string, breach workstate.CeilingBreach, spent workstate.Spent) (workstate.ApprovalRequest, error) {
	run, err := i.store().runForOwner(memql.ContextWithFreshRead(ctx), runID)
	if err != nil || run == nil {
		return workstate.ApprovalRequest{}, fmt.Errorf("work progress unavailable: %v", err)
	}
	goal, err := i.store().goalForOwner(memql.ContextWithFreshRead(ctx), rowString(run, "goalId"))
	if err != nil || goal == nil {
		return workstate.ApprovalRequest{}, fmt.Errorf("work goal unavailable: %v", err)
	}
	declared, err := ceilingsOf(goal)
	if err != nil {
		return workstate.ApprovalRequest{}, err
	}
	next, err := progressWindowAfter(goal, run, declared, spent, 0, 16)
	if err != nil {
		return workstate.ApprovalRequest{}, err
	}
	req := workstate.ProgressFeedback(runID, stepKey, breach, i.clock(), DefaultApprovalTTL)
	if (breach.Ceiling != workstate.CeilingModelCalls && breach.Ceiling != workstate.CeilingWallClock) ||
		workstate.CheckCeilings(enforceableHere(declared), spent, 0) != nil || next == 0 {
		workstate.StopProgressFeedback(&req)
	}
	return req, nil
}

// One slow completed operation can cross several estimate windows. Consume the
// minimum number of bounded windows needed, without spending evidence on an
// extension that would immediately pause again. Unset hard limits do not grant
// an implicit larger allowance.
func progressWindowAfter(goal, run map[string]any, declared workstate.Ceilings, spent workstate.Spent, tokens, maximum int) (int, error) {
	candidate := maps.Clone(run)
	classification := maps.Clone(rowMap(run, "classification"))
	if classification == nil {
		classification = map[string]any{}
	}
	current := rowInt(rowMap(classification, "progressContinuation"), "extensions")
	if current < 0 || current > 16 {
		return 0, fmt.Errorf("invalid work continuation count")
	}
	for count := current + 1; count <= maximum; count++ {
		classification["progressContinuation"] = map[string]any{"extensions": count}
		candidate["classification"] = classification
		limits, err := progressCeilings(goal, candidate, declared)
		if err != nil {
			return 0, err
		}
		if workstate.CheckCeilings(enforceableHere(limits), spent, tokens) == nil {
			return count, nil
		}
	}
	return 0, nil
}
