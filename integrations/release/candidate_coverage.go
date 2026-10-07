package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/component/workjournal"
)

type candidateRunPlan struct {
	Mode     string
	Receipts []pl.ReleaseWorkReceipt
}

type candidateSkippedStep struct {
	Component string `json:"component"`
	WorkRunID string `json:"workRunId"`
	StepKey   string `json:"stepKey"`
	Code      string `json:"code"`
}

type candidateCoverage struct {
	Modes   []string               `json:"modes"`
	Skipped []candidateSkippedStep `json:"skipped"`
}

// completeRun reads the whole declared run and checks its original definition
// fingerprint. Missing steps cannot disappear from release review, and a
// successful aggregate alone cannot stand in for individual execution receipts.
// Which run modes qualify for a release is decided by the installed DSL.
func (r candidateEvidenceReader) completeRun(ctx context.Context, runID string) (candidateRunPlan, error) {
	owner, err := candidateOwner(ctx)
	if err != nil {
		return candidateRunPlan{}, err
	}
	runs, err := r.rows(ctx, "workRunForOwner", map[string]any{"runId": runID}, 1)
	if err != nil {
		return candidateRunPlan{}, err
	}
	if len(runs) != 1 {
		return candidateRunPlan{}, errors.New("release run is unavailable")
	}
	rows, err := r.rows(ctx, "workStepsForOwnerRun", map[string]any{"runId": runID}, 1024)
	if err != nil {
		return candidateRunPlan{}, err
	}
	var run struct {
		AutomationName, TemplateFingerprint string
		StepOrder                           []string
		Input                               struct{ Mode string }
	}
	body, err := json.Marshal(runs[0])
	if err != nil {
		return candidateRunPlan{}, err
	}
	if err = json.Unmarshal(body, &run); err != nil {
		return candidateRunPlan{}, err
	}
	if len(run.StepOrder) == 0 || len(rows) != len(run.StepOrder) {
		return candidateRunPlan{}, errors.New("release run has incomplete declared steps")
	}
	byKey := map[string]map[string]any{}
	for _, row := range rows {
		key, ok := row["key"].(string)
		if !ok || key == "" || byKey[key] != nil {
			return candidateRunPlan{}, errors.New("release run has ambiguous step identities")
		}
		byKey[key] = row
	}
	plan := candidateRunPlan{Mode: run.Input.Mode}
	decls := make([]workjournal.StepDecl, 0, len(rows))
	seen := map[string]bool{}
	for n, key := range run.StepOrder {
		row := byKey[key]
		if row == nil || seen[key] {
			return candidateRunPlan{}, errors.New("release run declared order is incomplete or repeated")
		}
		seen[key] = true
		var step struct {
			Key, Kind, StepType string
			Seq                 int
			DependsOn           []string
			Call                map[string]any
		}
		body, err := json.Marshal(row)
		if err != nil {
			return candidateRunPlan{}, err
		}
		if err = json.Unmarshal(body, &step); err != nil {
			return candidateRunPlan{}, err
		}
		if step.Seq != n {
			return candidateRunPlan{}, errors.New("release run step order changed")
		}
		decls = append(decls, workjournal.StepDecl{Key: step.Key, Kind: step.Kind, StepType: step.StepType, DependsOn: step.DependsOn, Call: step.Call})
		proof, err := pl.InspectReleaseStepReceipt(owner, runID, key, runs[0], row)
		if err != nil {
			return candidateRunPlan{}, fmt.Errorf("run step %s: %w", key, err)
		}
		plan.Receipts = append(plan.Receipts, proof)
	}
	fingerprint, err := workjournal.DefinitionFingerprint(run.AutomationName, decls)
	if err != nil || fingerprint != run.TemplateFingerprint {
		return candidateRunPlan{}, errors.New("release run no longer matches its complete declared execution")
	}
	return plan, nil
}

// coverage requires every selected run's complete evidence set. Artifacts may
// only come from those covered runs. This is selection integrity; DSL receives
// the modes and decides which modes satisfy the release policy.
func (r candidateEvidenceReader) coverage(ctx context.Context, c pl.ReleaseCandidate) (candidateCoverage, error) {
	out := candidateCoverage{Modes: []string{}, Skipped: []candidateSkippedStep{}}
	owner, err := candidateOwner(ctx)
	if err != nil {
		return out, err
	}
	if owner != c.OwnerUserID {
		return out, errors.New("candidate belongs to another owner")
	}
	if _, _, err := pl.CanonicalReleaseCandidate(c); err != nil {
		return out, err
	}
	type source struct{ component, run string }
	selected := map[source]map[string]pl.ReleaseEvidence{}
	for _, e := range c.Evidence {
		key := source{e.Component, e.WorkRunID}
		if selected[key] == nil {
			selected[key] = map[string]pl.ReleaseEvidence{}
		}
		if _, exists := selected[key][e.StepKey]; exists {
			return out, errors.New("candidate repeats a check from the same execution")
		}
		selected[key][e.StepKey] = e
	}
	for key, checks := range selected {
		plan, err := r.completeRun(ctx, key.run)
		if err != nil {
			return out, err
		}
		if len(plan.Receipts) != len(checks) {
			return out, errors.New("candidate omitted declared execution evidence")
		}
		for _, receipt := range plan.Receipts {
			e, ok := checks[receipt.StepKey]
			if !ok || e.ReceiptID != receipt.ReceiptID || e.ReceiptDigest != receipt.ReceiptDigest {
				return out, errors.New("candidate changed its complete run evidence")
			}
			if receipt.Status == "skipped" {
				out.Skipped = append(out.Skipped, candidateSkippedStep{Component: key.component, WorkRunID: key.run, StepKey: receipt.StepKey, Code: receipt.SkipCode})
			}
		}
		if !slices.Contains(out.Modes, plan.Mode) {
			out.Modes = append(out.Modes, plan.Mode)
		}
	}
	for _, component := range c.Components {
		for _, a := range component.Artifacts {
			if _, ok := selected[source{component.Name, a.Receipt.WorkRunID}][a.Receipt.StepKey]; !ok {
				return out, errors.New("artifact producer is outside complete candidate evidence")
			}
		}
	}
	slices.Sort(out.Modes)
	slices.SortFunc(out.Skipped, func(a, b candidateSkippedStep) int {
		return strings.Compare(a.Component+"/"+a.WorkRunID+"/"+a.StepKey, b.Component+"/"+b.WorkRunID+"/"+b.StepKey)
	})
	return out, nil
}
