package work

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

var _ memql.ProviderAdmissionGuard = (*RunCeilings)(nil)

// The provider boundary counts actual inference attempts, including auxiliary
// embeddings and transport retries, separately from logical answered requests.
// One immutable cumulative receipt per admission avoids a lifetime-sized ID set.
// The write gate spans fresh accounting, progress checks and the reservation;
// a lost response conservatively consumes an attempt, never grants a refund.
func (c *RunCeilings) AdmitProvider(ctx context.Context, rc common.RunContext, policy memql.ProviderAdmission) error {
	spent := c.Spent(ctx, rc)
	fail := func(b memql.RunCeilingBreach, spent memql.RunSpend) error {
		return &memql.RunCeilingError{RunId: rc.RunId, StepKey: rc.StepKey, Breach: b, Spent: spent}
	}
	unknown := func(err error) error {
		return fail(memql.RunCeilingBreach{Ceiling: "unevaluated", Reason: "provider admission could not be verified: " + err.Error()}, spent)
	}
	host, ok := c.store.engine.(interface {
		IntegrationByName(string) memql.IntegrationProvider
	})
	if !ok {
		return unknown(fmt.Errorf("work coordinator is not wired"))
	}
	i, ok := host.IntegrationByName("work").(*Integration)
	if !ok || i == nil {
		return unknown(fmt.Errorf("work coordinator is unavailable"))
	}
	if rc.OwnerUserId == "" || rc.GoalId == "" || memql.BareShortId(callerUserId(ctx)) != memql.BareShortId(rc.OwnerUserId) {
		return unknown(fmt.Errorf("admission requires the authenticated work owner"))
	}
	ctx = ownerActor(memql.ContextWithFreshRead(ctx), rc.OwnerUserId)
	if breach := c.Admit(ctx, rc, 0); breach != nil {
		return fail(*breach, c.Spent(ctx, rc))
	}
	// Logical admission holds its local budget mutex before taking the work
	// gate for renewal. Never take that mutex while holding the gate here:
	// a concurrent logical admission would otherwise invert the lock order.
	// The ledger below supplies the fresh, serialized provider-attempt total.
	spent = c.Spent(ctx, rc)
	release, err := i.decisionGate(ctx, "workload:"+memql.BareShortId(rc.RunId), "provider-admission:"+memql.BareShortId(rc.RunId))
	if err != nil {
		return unknown(err)
	}
	defer release()
	run, err := i.store().runForOwner(ctx, rc.RunId)
	if err != nil {
		return unknown(err)
	}
	if run == nil || rowString(run, "status") != runStatusRunning || run["cancelRequested"] == true || memql.BareShortId(rowString(run, "goalId")) != memql.BareShortId(rc.GoalId) {
		return unknown(fmt.Errorf("work is no longer running under this goal"))
	}
	rows, err := i.store().query(ctx, "query "+call("workProviderAdmissionForOwnerRun", map[string]any{"runId": rc.RunId}))
	if err != nil {
		return unknown(err)
	}
	base, admitted, cost := spent.ModelCalls, 0, spent.Cost
	receiptAt := i.clock()
	tokens := spent.Tokens
	coverage := "legacy-attempts-unknown"
	if rowInt(run, "providerAccountingVersion") == 1 {
		coverage = "complete-from-run-creation"
	}
	if len(rows) > 1 {
		return unknown(fmt.Errorf("ambiguous provider ledger head"))
	}
	if len(rows) == 1 {
		row := rows[0]
		if _, valid := rowTime(row, "createdAt"); !valid {
			return unknown(fmt.Errorf("provider ledger head has no valid timestamp"))
		}
		receiptAt = workRowVersionAfter(row["createdAt"], receiptAt)
		if memql.BareShortId(rowString(row, "ownerUserId")) != memql.BareShortId(rc.OwnerUserId) || memql.BareShortId(rowString(row, "runId")) != memql.BareShortId(rc.RunId) {
			return unknown(fmt.Errorf("provider receipt attribution changed"))
		}
		state := rowMap(rowMap(row, "data"), "providerAdmission")
		var ledger struct {
			SchemaVersion       *int     `json:"schemaVersion"`
			LegacyAnsweredCalls *int     `json:"legacyAnsweredCalls"`
			AdmittedCalls       *int     `json:"admittedCalls"`
			EstimatedCost       *float64 `json:"estimatedCost"`
			Coverage            *string  `json:"coverage"`
			EstimatedTokens     *int     `json:"estimatedTokens"`
		}
		raw, marshalErr := json.Marshal(state)
		if marshalErr != nil || json.Unmarshal(raw, &ledger) != nil || ledger.SchemaVersion == nil || *ledger.SchemaVersion != 1 || ledger.LegacyAnsweredCalls == nil || ledger.AdmittedCalls == nil || ledger.EstimatedCost == nil || ledger.EstimatedTokens == nil || ledger.Coverage == nil || (*ledger.Coverage != "complete-from-run-creation" && *ledger.Coverage != "legacy-attempts-unknown") {
			return unknown(fmt.Errorf("provider ledger head is malformed or has an unsupported version"))
		}
		base, admitted, cost = *ledger.LegacyAnsweredCalls, *ledger.AdmittedCalls, *ledger.EstimatedCost
		coverage = *ledger.Coverage
		tokens = *ledger.EstimatedTokens
	}
	if base < 0 || admitted < 0 || base >= math.MaxInt-admitted-1 || tokens < 0 || policy.EstimatedTokens < 0 || tokens > math.MaxInt-policy.EstimatedTokens || math.IsNaN(cost) || math.IsInf(cost, 0) || cost < 0 || policy.EstimatedCost < 0 || math.IsNaN(policy.EstimatedCost) || math.IsInf(policy.EstimatedCost, 0) {
		return unknown(fmt.Errorf("invalid cumulative provider usage"))
	}
	spent.ModelCalls = max(spent.ModelCalls, base+admitted)
	spent.Tokens = max(spent.Tokens, tokens)
	spent.Cost = max(spent.Cost, cost)
	if coverage == "legacy-attempts-unknown" && (policy.HardCallLimit > 0 || (policy.HardCostExplicit && policy.HardCostLimit > 0)) {
		return unknown(fmt.Errorf("historical provider attempts are unknown; the explicit operator call limit requires accounting recovery"))
	}
	if policy.HardCallLimit > 0 && spent.ModelCalls >= policy.HardCallLimit {
		return fail(memql.RunCeilingBreach{Ceiling: "providerCalls", Limit: fmt.Sprint(policy.HardCallLimit), Actual: fmt.Sprint(spent.ModelCalls), Reason: "explicit operator provider-call limit reached"}, spent)
	}
	if policy.HardCostLimit > 0 && cost+policy.EstimatedCost > policy.HardCostLimit {
		return fail(memql.RunCeilingBreach{Ceiling: "cost", Limit: fmt.Sprint(policy.HardCostLimit), Actual: fmt.Sprint(cost), Reason: "provider cost limit would be exceeded"}, spent)
	}
	limits, err := c.goalCeilings(ctx, rc)
	if err != nil {
		return unknown(err)
	}
	if coverage == "legacy-attempts-unknown" && (limits.TokenBudget > 0 || limits.CostCeiling > 0) {
		return unknown(fmt.Errorf("historical provider usage is unknown; the goal's monetary limits require accounting recovery"))
	}
	windowSpend := workstate.Spent{ModelCalls: spent.ModelCalls, Tokens: spent.Tokens, Cost: max(spent.Cost, cost+policy.EstimatedCost), WallClockMs: spent.WallClockMs}
	breach := workstate.CheckCeilings(enforceableHere(limits), windowSpend, policy.EstimatedTokens)
	if breach != nil && (breach.Ceiling == workstate.CeilingModelCalls || breach.Ceiling == workstate.CeilingWallClock) {
		continued, err := i.continueWorkProgressLocked(ctx, rc, windowSpend, policy.EstimatedTokens)
		if err != nil {
			return unknown(err)
		}
		if continued {
			limits, err = c.goalCeilings(ctx, rc)
			if err != nil {
				return unknown(err)
			}
			breach = workstate.CheckCeilings(enforceableHere(limits), windowSpend, policy.EstimatedTokens)
		}
	}
	if breach != nil {
		return fail(*breachOf(breach), spent)
	}
	// Legacy answer receipts remain the recorded baseline. Historical transport
	// attempts were process-local and are explicitly unknown, never reported as
	// zero or silently manufactured from an unrelated session count.
	state := map[string]any{"schemaVersion": 1, "legacyAnsweredCalls": base, "coverage": coverage, "admittedCalls": admitted + 1, "estimatedCost": cost + policy.EstimatedCost, "estimatedTokens": tokens + policy.EstimatedTokens}
	if err := i.store().writeInternal(memql.ContextWithTrainingEvidenceWrite(ctx), "mutation "+call("createWorkObservation", map[string]any{
		"observationId": newRowId("v1:work:observation"), "versionTime": rfc(receiptAt), "runId": rc.RunId, "stepKey": rc.StepKey, "kind": "decision", "content": "Reserved one provider inference attempt; cumulative usage is retained across work windows.", "data": map[string]any{"providerAdmission": state},
	})); err != nil {
		return unknown(err)
	}
	return nil
}
