package work

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
)

// The same explicit feedback policy used at a progress question applies to
// feedback on a finished result. Calculate against current provider accounting,
// not just the older logical-call snapshot, so the first replanning call does
// not immediately ask for that same feedback. Caller holds lifecycle, workload
// and provider gates; classification and the continuation receipt commit together.
// No cumulative spend, hard limit, permission or evidence receipt is rewritten.
func (i *Integration) finishedFeedbackWindow(ctx context.Context, goal, run map[string]any, requestID string, now time.Time) (map[string]any, error) {
	declared, err := ceilingsOf(goal)
	if err != nil {
		return nil, err
	}
	var spent workstate.Spent
	raw, err := json.Marshal(rowMap(run, "spent"))
	if err != nil || json.Unmarshal(raw, &spent) != nil {
		return nil, fmt.Errorf("work: saved usage cannot be verified")
	}
	if at, ok := rowTime(run, "startedAt"); ok {
		spent.WallClockMs = max(spent.WallClockMs, now.Sub(at).Milliseconds()-int64(rowInt(run, "humanWaitMs")))
	}
	rows, err := i.store().query(ctx, "query "+call("workProviderAdmissionForOwnerRun", map[string]any{"runId": rowString(run, "id")}))
	if err != nil {
		return nil, err
	}
	if len(rows) > 1 {
		return nil, fmt.Errorf("work: provider accounting is ambiguous")
	}
	if len(rows) == 1 {
		row := rows[0]
		if _, valid := rowTime(row, "createdAt"); !valid ||
			memql.BareShortId(rowString(row, "ownerUserId")) != memql.BareShortId(rowString(run, "ownerUserId")) ||
			memql.BareShortId(rowString(row, "runId")) != memql.BareShortId(rowString(run, "id")) {
			return nil, fmt.Errorf("work: provider accounting attribution cannot be verified")
		}
		var ledger struct {
			SchemaVersion *int     `json:"schemaVersion"`
			Legacy        *int     `json:"legacyAnsweredCalls"`
			Admitted      *int     `json:"admittedCalls"`
			Tokens        *int     `json:"estimatedTokens"`
			Cost          *float64 `json:"estimatedCost"`
			Coverage      *string  `json:"coverage"`
		}
		raw, err = json.Marshal(rowMap(rowMap(row, "data"), "providerAdmission"))
		if err != nil || json.Unmarshal(raw, &ledger) != nil || ledger.SchemaVersion == nil || *ledger.SchemaVersion != 1 || ledger.Legacy == nil || ledger.Admitted == nil || ledger.Tokens == nil || ledger.Cost == nil || ledger.Coverage == nil ||
			*ledger.Legacy < 0 || *ledger.Admitted < 0 || *ledger.Legacy >= math.MaxInt-*ledger.Admitted || *ledger.Tokens < 0 || *ledger.Cost < 0 || math.IsNaN(*ledger.Cost) || math.IsInf(*ledger.Cost, 0) ||
			(*ledger.Coverage != "complete-from-run-creation" && *ledger.Coverage != "legacy-attempts-unknown") {
			return nil, fmt.Errorf("work: provider accounting is malformed")
		}
		if *ledger.Coverage == "legacy-attempts-unknown" && (declared.TokenBudget > 0 || declared.CostCeiling > 0) {
			return nil, fmt.Errorf("work: historical provider usage is unknown; hard spending limits require accounting recovery")
		}
		spent.ModelCalls = max(spent.ModelCalls, *ledger.Legacy+*ledger.Admitted)
		spent.Tokens = max(spent.Tokens, *ledger.Tokens)
		spent.Cost = max(spent.Cost, *ledger.Cost)
	}
	if spent.ModelCalls < 0 || spent.Tokens < 0 || spent.Cost < 0 || spent.WallClockMs < 0 || math.IsNaN(spent.Cost) || math.IsInf(spent.Cost, 0) {
		return nil, fmt.Errorf("work: saved usage is invalid")
	}
	next, err := progressWindowAfter(goal, run, declared, spent, 0)
	if err != nil {
		return nil, err
	}
	if next == 0 {
		return nil, errProgressLimit
	}
	classification := maps.Clone(rowMap(run, "classification"))
	if classification == nil {
		classification = map[string]any{}
	}
	state := maps.Clone(rowMap(classification, "progressContinuation"))
	if state == nil {
		state = map[string]any{}
	}
	if rowInt(state, "policyVersion") < 2 {
		state["legacyReceipts"] = rowStringSlice(state, "receipts")
	}
	state["policyVersion"] = 2
	state["extensions"], state["feedbackId"], state["continuedAt"] = next, "result:"+requestID, rfc(now)
	state["receipts"], state["receiptAfter"] = []string{}, rfc(now)
	state["reason"] = "The owner requested further work on the unaccepted result."
	classification["progressContinuation"] = state
	return classification, nil
}
