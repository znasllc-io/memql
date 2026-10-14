package work

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work/goaleval"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

// A human-operated external evaluator publishes its artifact verdicts. The
// engine verifies ownership, immutable identity and model receipts; it does
// not claim to have independently inspected files on the operator's machine.
// A running model cannot publish a success claim about its own work.
func (i *Integration) handleRecordGoalEvaluation(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ac, err := requirePrincipal(ctx)
	if err != nil {
		return nil, err
	}
	if ac.Synthetic || ac.Unranked {
		return nil, fmt.Errorf("evaluation publication needs a signed-in person")
	}
	if rc, ok := common.RunFromContext(ctx); ok && rc.RunId != "" {
		return nil, fmt.Errorf("a model run cannot publish its own evaluation")
	}
	raw, err := json.Marshal(args["result"])
	if err != nil || len(raw) > 1024*1024 {
		return nil, fmt.Errorf("evaluation result exceeds the publication limit")
	}
	result, err := goaleval.Decode[goaleval.Result](raw)
	if err != nil {
		return nil, err
	}
	rawScenario, err := json.Marshal(args["scenario"])
	if err != nil || len(rawScenario) > 128*1024 {
		return nil, fmt.Errorf("invalid scenario contract")
	}
	scenario, err := goaleval.Decode[goaleval.Scenario](rawScenario)
	if err != nil {
		return nil, err
	}
	if err := (goaleval.Catalog{SchemaVersion: 1, Version: "1.0.0", Scenarios: []goaleval.Scenario{scenario}}).Validate(); err != nil {
		return nil, err
	}
	if err := result.Validate(scenario); err != nil {
		return nil, err
	}
	if i.decisionGate == nil {
		return nil, fmt.Errorf("evaluation publication requires shared coordination")
	}
	receipt := "goal-evaluation-" + string(id.New().MustFromMap(map[string]any{"runId": memql.BareShortId(result.RunID), "resultId": result.ID}))
	release, err := i.decisionGate(ctx, receipt)
	if err != nil {
		return nil, err
	}
	defer release()
	ctx = memql.ContextWithFreshRead(ctx)
	s := i.store()
	run, err := s.runForOwner(ctx, result.RunID)
	if err != nil {
		return nil, err
	}
	if run == nil || memql.BareShortId(argString(run, "goalId")) != memql.BareShortId(result.GoalID) || !isTerminalRunStatus(argString(run, "status")) {
		return nil, fmt.Errorf("evaluation requires the caller's finished goal run")
	}
	if result.Outcome == "autonomous" || result.Outcome == "assisted" {
		if argString(run, "status") != "succeeded" {
			return nil, fmt.Errorf("an unfinished or failed run cannot be published as a success")
		}
	}
	if result.Setup.PaidProvidersAllowed {
		return nil, fmt.Errorf("this publisher admits only evaluations without paid providers")
	}
	routing, _ := run["routing"].(map[string]any)
	if routing["noPaidProviders"] != true {
		return nil, fmt.Errorf("evaluation run lacks its no-paid-provider execution ceiling")
	}
	source := argString(routing, "source")
	if !strings.HasPrefix(source, "fleet:") || source == "fleet:*" {
		return nil, fmt.Errorf("evaluation run lacks its strict fleet source")
	}
	for _, route := range result.Setup.Routes {
		if len(route.Doors) != 1 || route.Doors[0].Source != "fleet" || (route.Level != "embeddings" && "fleet:"+route.Doors[0].Model != source) {
			return nil, fmt.Errorf("reported setup disagrees with the owned run's routing")
		}
	}
	evidence, err := goaleval.CollectRunEvidence(ctx, memql.BareShortId(result.RunID), source, func(ctx context.Context, kind, runID string) ([]map[string]any, error) {
		query := map[string]string{"run": "workRunForOwner", "modelCalls": "workModelCallsForOwnerRun", "observations": "workObservationsForOwnerRun", "steps": "workStepsForOwnerRun"}[kind]
		if query == "" {
			return nil, fmt.Errorf("unknown evaluation evidence kind")
		}
		rows, err := s.query(ctx, "query "+call(query, map[string]any{"runId": runID}))
		if err != nil {
			return nil, err
		}
		for n, row := range rows {
			rows[n] = maps.Clone(row)
			for _, key := range []string{"id", "runId", "goalId", "parentRunId", "childRunId"} {
				if v := argString(row, key); v != "" {
					rows[n][key] = memql.BareShortId(v)
				}
			}
		}
		return rows, nil
	})
	if err != nil {
		return nil, err
	}
	if err := verifyEvaluationModelReceipts(result, evidence.ModelCalls); err != nil {
		return nil, err
	}
	observations, err := s.query(ctx, "query "+call("workObservationsForOwnerRun", map[string]any{"runId": result.RunID}))
	if err != nil {
		return nil, err
	}
	var normalized map[string]any
	if err := json.Unmarshal(raw, &normalized); err != nil {
		return nil, err
	}
	for _, o := range observations {
		if memql.BareShortId(argString(o, "id")) != receipt {
			continue
		}
		data, _ := o["data"].(map[string]any)
		if !reflect.DeepEqual(data["goalEvaluation"], normalized) {
			return nil, fmt.Errorf("evaluation identity already records different evidence")
		}
		return i.resultNode(map[string]any{"receiptId": receipt, "recorded": true}), nil
	}
	err = s.writeInternal(ctx, "mutation "+call("createWorkObservation", map[string]any{"observationId": receipt, "runId": result.RunID, "kind": "decision", "content": "External evaluation: artifact verdicts supplied by the owner-operated harness; goal ownership and model receipts verified by MemQL.", "data": map[string]any{"goalEvaluation": normalized, "oracleAuthority": "external-harness", "scenario": scenario}}))
	if err != nil {
		return nil, err
	}
	return i.resultNode(map[string]any{"receiptId": receipt, "recorded": true}), nil
}

func verifyEvaluationModelReceipts(result goaleval.Result, rows []map[string]any) error {
	remaining := map[string]map[string]any{}
	for _, row := range rows {
		if argString(row, "served") != "journal" {
			remaining["modelCall:"+memql.BareShortId(argString(row, "id"))] = row
		}
	}
	for _, use := range result.ModelsUsed {
		if use.Calls != 1 || len(use.Evidence) != 1 {
			return fmt.Errorf("model evidence must name each actual call separately")
		}
		row, ok := remaining[use.Evidence[0]]
		if !ok {
			return fmt.Errorf("model evidence is missing, repeated or belongs to another run")
		}
		provider := argString(row, "provider")
		model := argString(row, "model")
		if model == "" {
			model = provider
		}
		source := "federation"
		if strings.HasPrefix(provider, "fleet:") {
			source = "fleet"
		} else if strings.HasPrefix(provider, "app:") {
			source = "app"
		}
		if use.Model != model || use.Source != source {
			return fmt.Errorf("reported model differs from the actual call receipt")
		}
		decision, _ := row["decision"].(map[string]any)
		level := argString(decision, "requestedLevel")
		if level == "" {
			level = argString(decision, "level")
		}
		if level == "" {
			level = "unreported"
		}
		role := argString(row, "promptRef")
		if role == "" {
			role = "step:" + argString(row, "stepKey")
		}
		if use.Role != role || use.Level != level {
			return fmt.Errorf("reported role or level differs from the model receipt")
		}
		delete(remaining, use.Evidence[0])
	}
	if len(remaining) != 0 {
		return fmt.Errorf("evaluation omitted actual model calls")
	}
	return nil
}
