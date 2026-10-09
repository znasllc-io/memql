package work

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/znasllc-io/memql/component/memql"
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
	if current, ok := budgetNumber(ceilings[field]); ok && limit < current {
		return fmt.Errorf("work: newLimit would lower a ceiling that has already been raised")
	}
	ceilings[field] = limit
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
