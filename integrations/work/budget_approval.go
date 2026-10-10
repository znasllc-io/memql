package work

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
)

// A person may supply one explicit new limit with a budget approval. Keep the
// lifecycle mutation internal, preserve other ceilings and never reset spend.
// The goal lock serializes decisions on different runs of the same goal.
func (i *Integration) raiseApprovedBudget(ctx context.Context, approval, answer map[string]any, now time.Time) error {
	requested, present := answer["newLimit"]
	if !present {
		return nil // The caller may already have raised the ceiling separately.
	}
	subject := rowMap(approval, "subject")
	ceiling := rowString(subject, "ceiling")
	field := map[string]string{"modelCalls": "maxModelCalls", "tokens": "tokenBudget", "cost": "costCeiling", "wallClock": "wallClockMs", "retries": "maxRetries", "events": "maxEvents"}[ceiling]
	limit, valid := budgetNumber(requested)
	if field == "" || !valid || limit <= 0 || limit > 9007199254740991 || (field != "costCeiling" && math.Trunc(limit) != limit) {
		return fmt.Errorf("work: provide a finite newLimit above the recorded usage for this budget approval; count and time limits must be whole numbers")
	}
	ctx = memql.ContextWithFreshRead(ctx)
	run, err := i.store().runForOwner(ctx, rowString(approval, "runId"))
	if err != nil {
		return err
	}
	if run == nil || run["cancelRequested"] == true || rowString(run, "status") != runStatusWaiting || memql.BareShortId(rowString(rowMap(run, "waitingOn"), "subject")) != memql.BareShortId(rowString(approval, "id")) || rowString(run, "goalId") == "" {
		return fmt.Errorf("work: this run is no longer waiting on this budget approval")
	}
	spentField := ceiling
	if ceiling == "wallClock" {
		spentField = "wallClockMs"
	}
	actual, known := budgetNumber(rowMap(run, "spent")[spentField])
	if !known || limit <= actual {
		return fmt.Errorf("work: newLimit must exceed this run's recorded usage")
	}
	goalID := rowString(run, "goalId")
	release, err := i.decisionGate(ctx, "goal-budget-"+memql.BareShortId(goalID))
	if err != nil {
		return err
	}
	defer release()
	goal, err := i.store().goalForOwner(ctx, goalID)
	if err != nil {
		return err
	}
	if goal == nil || rowString(goal, "status") == "closed" || memql.BareShortId(rowString(goal, "ownerUserId")) != memql.BareShortId(rowString(approval, "ownerUserId")) {
		return fmt.Errorf("work: this budget's owned goal is unavailable")
	}
	ceilings := map[string]any{}
	for key, value := range rowMap(goal, "ceilings") {
		ceilings[key] = value
	}
	declared, err := ceilingsOf(goal)
	if err != nil {
		return err
	}
	effective, err := approvedWorkloadCeilings(goal, declared, rowString(rowMap(run, "classification"), "workload"))
	if err != nil {
		return err
	}
	current, known := budgetNumber(ceilings[field])
	minimum := current
	switch field {
	case "wallClockMs":
		minimum = float64(effective.WallClockMs)
	case "maxModelCalls":
		minimum = float64(effective.MaxModelCalls)
	case "maxRetries":
		minimum = float64(effective.MaxRetries)
	}
	if limit < minimum {
		return fmt.Errorf("work: newLimit would lower a ceiling that has already been raised")
	}
	// Raising a 45-minute estimate to 60 minutes need not lower an existing
	// two-hour hard ceiling. A newer explicit approval still wins above.
	if !known || limit > current {
		ceilings[field] = limit
	}
	// An explicit approval supersedes the workload estimate for this ceiling
	// only. Persist it with the declared budget so every replica agrees and
	// resumption cannot immediately park on the unchanged estimate again.
	if field == "wallClockMs" || field == "maxModelCalls" || field == "maxRetries" {
		overrides := map[string]any{}
		for key, value := range rowMap(ceilings, "workloadOverrides") {
			overrides[key] = value
		}
		overrides[field] = limit
		ceilings["workloadOverrides"] = overrides
	}
	// Write before consuming the approval. A write failure leaves it pending;
	// a decision-write failure can retry the same limit without spending more.
	return i.store().writeInternal(ctx, "mutation "+call("updateWorkGoal", map[string]any{
		"goalId": goalID, "ceilings": ceilings, "versionTime": workRowVersionAfter(goal["createdAt"], now),
	}))
}

func budgetNumber(value any) (float64, bool) {
	raw, err := json.Marshal(value)
	if err != nil || string(raw) == "null" {
		return 0, false
	}
	var number float64
	err = json.Unmarshal(raw, &number)
	return number, err == nil && !math.IsNaN(number) && !math.IsInf(number, 0)
}

// Workload estimates may be raised by a recorded human budget decision. Keep
// every unapproved estimate and always intersect with the current hard limit;
// a later tighter goal limit must win over an older approval.
func approvedWorkloadCeilings(goal map[string]any, declared work.Ceilings, workload string) (work.Ceilings, error) {
	effective := work.EffectiveWorkloadCeilings(declared, workload)
	approved, err := ceilingsOf(map[string]any{"ceilings": rowMap(rowMap(goal, "ceilings"), "workloadOverrides")})
	if err != nil {
		return work.Ceilings{}, fmt.Errorf("invalid approved workload ceilings: %w", err)
	}
	if declared.WallClockMs > 0 && approved.WallClockMs > effective.WallClockMs {
		effective.WallClockMs = min(declared.WallClockMs, approved.WallClockMs)
	}
	if declared.MaxModelCalls > 0 && approved.MaxModelCalls > effective.MaxModelCalls {
		effective.MaxModelCalls = min(declared.MaxModelCalls, approved.MaxModelCalls)
	}
	if declared.MaxRetries > 0 && approved.MaxRetries > effective.MaxRetries {
		effective.MaxRetries = min(declared.MaxRetries, approved.MaxRetries)
	}
	return effective, nil
}
