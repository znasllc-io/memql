package work

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

// Time and model-call estimates bound one work window, never the goal's lifetime.
// Only completed, previously uncredited evidence (or an explicit human answer)
// renews a window. Monetary ceilings and all execution permissions remain hard.
// The counter and evidence watermark are durable so replica changes cannot renew
// a window twice or turn heartbeats into progress.
func progressBaseCeilings(goal map[string]any, declared workstate.Ceilings, workload string) (workstate.Ceilings, error) {
	effective, err := approvedWorkloadCeilings(goal, declared, workload)
	if err != nil {
		return effective, err
	}
	if effective.MaxModelCalls == 0 {
		effective.MaxModelCalls = 96
	}
	if effective.WallClockMs == 0 {
		effective.WallClockMs = 7200000
	}
	return effective, nil
}

func progressCeilings(goal, run map[string]any, declared workstate.Ceilings) (workstate.Ceilings, error) {
	classification := rowMap(run, "classification")
	effective, err := progressBaseCeilings(goal, declared, rowString(classification, "workload"))
	if err != nil {
		return effective, err
	}
	extensions := rowInt(rowMap(classification, "progressContinuation"), "extensions")
	if extensions < 0 || extensions == math.MaxInt {
		return effective, fmt.Errorf("invalid work continuation count")
	}
	windows := int64(extensions + 1)
	if int64(effective.MaxModelCalls) > int64(math.MaxInt)/windows || effective.WallClockMs > math.MaxInt64/windows {
		return effective, fmt.Errorf("work continuation counters overflow")
	}
	effective.MaxModelCalls *= extensions + 1
	effective.WallClockMs *= windows
	return effective, nil
}

// A lifetime monetary limit is independent of renewable execution windows.
// Never clear usage, refund spend, or interpret feedback as a spending approval.
func monetaryCeilings(c workstate.Ceilings) workstate.Ceilings {
	return workstate.Ceilings{TokenBudget: c.TokenBudget, CostCeiling: c.CostCeiling}
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
	return i.continueWorkProgressLocked(ctx, rc, spent, estimatedTokens)
}

// Caller holds the workload write gate (possibly with the provider gate).
func (i *Integration) continueWorkProgressLocked(ctx context.Context, rc common.RunContext, spent workstate.Spent, estimatedTokens int) (bool, error) {
	run, err := i.store().runForOwner(ctx, rc.RunId)
	if err != nil {
		return false, err
	}
	// Compilation and remedies have the same progress contract as execution,
	// but only their still-current native claim may renew. A matching status
	// alone is not authority, and terminal answer checks cannot extend work.
	status := rowString(run, "status")
	if (status != runStatusRunning && status != runStatusCompiling && status != runStatusWaiting) || i.admitProviderPhase(ctx, rc, run) != nil {
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
	// Progress never authorizes spending beyond the monetary limits.
	if workstate.CheckCeilings(monetaryCeilings(declared), spent, estimatedTokens) != nil {
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
	next, err := progressWindowAfter(goal, run, declared, spent, estimatedTokens)
	if err != nil || next == 0 {
		return false, err
	}
	seen, next, err := i.claimProgressReceiptPage(ctx, rc, state, next)
	if err != nil {
		return false, err
	}
	if len(seen) == 0 {
		return false, nil
	}
	if classification == nil {
		classification = map[string]any{}
	}
	if state == nil {
		state = map[string]any{}
	}
	if rowInt(state, "policyVersion") < 2 {
		state["legacyReceipts"] = rowStringSlice(state, "receipts")
	}
	state["policyVersion"] = 2
	state["extensions"], state["receipts"], state["continuedAt"] = next, seen, rfc(i.clock())
	// Only explicit feedback advances the evidence epoch. Advancing to now
	// here would discard unseen older pages and receipts committed between
	// the read and this write. Durable credits prevent reuse across renewals.
	state["reason"] = "Completed new work; renewed the execution window."
	classification["progressContinuation"] = state
	if err := i.store().updateRun(ctx, rc.RunId, map[string]any{"classification": classification, "versionTime": rfc(workRowVersionAfter(run["createdAt"], i.clock()))}); err != nil {
		return false, err
	}
	return true, nil
}

func progressResultEvidenceKeys(row map[string]any) []string {
	data := rowMap(row, "data")
	kind := rowString(row, "kind")
	if (kind != "tool_result" && (kind != "note" || data["delegatedEvidence"] != true)) || data["isError"] != false || data["argsTruncated"] == true {
		return nil
	}
	name := rowString(data, "tool")
	short := name[strings.LastIndex(name, ".")+1:]
	switch short {
	case "", "discoverCapabilities", "workCapabilities", "recallWorkHistory", "requestUserFeedback", "respondToUser":
		return nil
	}
	var args any
	// The native recorder omits an empty argument object. Returned evidence
	// can still be substantive; argument text is not the credit identity.
	if raw := rowString(data, "args"); raw != "" && json.Unmarshal([]byte(raw), &args) != nil {
		return nil
	}
	evidence := rowString(data, "nativeResultEvidence")
	var digests []string
	if raw, err := hex.DecodeString(evidence); err == nil && len(raw) == 32 {
		digests = []string{evidence}
	} else if json.Unmarshal([]byte(evidence), &digests) != nil {
		return nil
	}
	if len(digests) == 0 || len(digests) > 100 {
		return nil
	}
	var keys []string
	for _, digest := range digests {
		raw, err := hex.DecodeString(digest)
		if err != nil || len(raw) != 32 {
			return nil
		}
		keys = append(keys, "source-content-v1:"+digest)
	}
	return keys
}

func progressReceiptKey(row map[string]any) string {
	keys := progressResultEvidenceKeys(row)
	if len(keys) > 0 {
		return keys[0]
	}
	return ""
}

// A real feedback answer may request one further work window. It does not
// reset usage or monetary limits. The answer starts a fresh evidence epoch so
// bounded duplicate tracking cannot become a hidden lifetime limit.
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
	if rowMap(approval, "subject")["continuationAllowed"] == false {
		// Recheck under the workload gate. A stale lifetime-policy stop may
		// use an already-granted window, never buy another one or clear its
		// evidence epoch. The ordinary decision/resume receipt is idempotent.
		if i.progressStopWithinCurrentWindow(ctx, approval, run) {
			return nil
		}
		return errProgressLimit
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
	if workstate.CheckCeilings(monetaryCeilings(declared), spent, 0) != nil {
		return errProgressLimit
	}
	if decisionID != "" && rowString(state, "feedbackId") == decisionID {
		return nil
	}
	next, err := progressWindowAfter(goal, run, declared, spent, 0)
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
	// Earlier evidence cannot buy another window after this human-directed restart.
	// Its exact receipts remain in the journal; only the bounded deduplication set
	// is reset. A replay of this decision is checked above and does not reset it.
	state["receipts"] = []string{}
	state["receiptAfter"] = rfc(i.clock())
	classification["progressContinuation"] = state
	if err := verifyApprovalDecisionWrite(ctx); err != nil {
		return err
	}
	return i.store().updateRun(ctx, runID, map[string]any{"classification": classification, "versionTime": rfc(workRowVersionAfter(run["createdAt"], i.clock()))})
}

var errProgressLimit = errors.New("the work reached its monetary limit or its continuation cannot be verified; the goal and results remain saved")

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
	next, err := progressWindowAfter(goal, run, declared, spent, 0)
	if err != nil {
		return workstate.ApprovalRequest{}, err
	}
	req := workstate.ProgressFeedback(runID, stepKey, breach, i.clock(), DefaultApprovalTTL)
	if (breach.Ceiling != workstate.CeilingModelCalls && breach.Ceiling != workstate.CeilingWallClock) ||
		workstate.CheckCeilings(monetaryCeilings(declared), spent, 0) != nil || next == 0 {
		workstate.StopProgressFeedback(&req)
	}
	return req, nil
}

// A long operation may cross several estimates. Compute the next window in
// constant time; the old fixed iteration count was itself a lifetime cap. This
// computes allowance only: the caller still requires fresh evidence or feedback.
func progressWindowAfter(goal, run map[string]any, declared workstate.Ceilings, spent workstate.Spent, tokens int) (int, error) {
	if workstate.CheckCeilings(monetaryCeilings(declared), spent, tokens) != nil {
		return 0, nil
	}
	classification := rowMap(run, "classification")
	current := rowInt(rowMap(classification, "progressContinuation"), "extensions")
	if current < 0 || current == math.MaxInt {
		return 0, fmt.Errorf("invalid work continuation count")
	}
	base, err := progressBaseCeilings(goal, declared, rowString(classification, "workload"))
	if err != nil {
		return 0, err
	}
	next := max(int64(current)+1, int64(spent.ModelCalls/base.MaxModelCalls), spent.WallClockMs/base.WallClockMs)
	if next >= int64(math.MaxInt) || int64(base.MaxModelCalls) > int64(math.MaxInt)/(next+1) || base.WallClockMs > math.MaxInt64/(next+1) {
		return 0, fmt.Errorf("work continuation counters overflow")
	}
	return int(next), nil
}
