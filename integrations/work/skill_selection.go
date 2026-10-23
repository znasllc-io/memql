package work

import (
	"context"
	"fmt"
	"maps"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	workvalues "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

func (i *Integration) handleSkillCatalog(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if _, err := requirePrincipal(ctx); err != nil {
		return nil, err
	}
	baselines, ok := i.engine.(interface{ IsInstalledSkillBaseline(map[string]any) bool })
	if !ok {
		return nil, fmt.Errorf("installed skill baseline verifier is unavailable")
	}
	rows, err := i.store().query(memql.ContextWithFreshRead(ctx), "query skillsForTraining()")
	if err != nil {
		return nil, err
	}
	for n, row := range rows {
		rows[n] = maps.Clone(row)
		rows[n]["reviewedBaseline"] = baselines.IsInstalledSkillBaseline(row)
	}
	return i.resultNode(map[string]any{"skills": rows}), nil
}

// A saved selection belongs to one owned step version. The shared gate and
// fresh reads make a second replica consume the same content snapshot, even
// after the catalog changes. Selection never grants a tool or approval.
func (i *Integration) handleSelectSkills(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	return i.selectSkillContext(ctx, args, false)
}

// Consultation is a new, journaled lookup for a discovered need, not a rewrite
// of the step's initial prompt snapshot. Keeping the two identities separate
// preserves replay while allowing live work to change its knowledge selection.
func (i *Integration) selectSkillContext(ctx context.Context, args map[string]any, consultation bool) ([]memorynodes.MemoryNode, error) {
	actor, err := requirePrincipal(ctx)
	if err != nil {
		return nil, err
	}
	rc, ok := common.RunFromContext(ctx)
	if !ok || !rc.IsRun() || rc.GoalId == "" || rc.StepKey == "" ||
		memql.BareShortId(actor.UserId) != memql.BareShortId(rc.OwnerUserId) ||
		(rc.Mode != common.RunModeLive && rc.Mode != common.RunModeFork && rc.Mode != common.RunModeReplay) {
		return nil, fmt.Errorf("skill context requires an owned work step")
	}
	if i.decisionGate == nil {
		return nil, fmt.Errorf("skill selection requires the shared decision gate")
	}
	release, err := i.decisionGate(ctx, "skill-selection:"+memql.BareShortId(rc.RunId)+":"+rc.StepKey)
	if err != nil {
		return nil, err
	}
	defer release()
	ctx = memql.ContextWithFreshRead(ctx)
	s := i.store()
	run, err := s.runForOwner(ctx, rc.RunId)
	if err != nil {
		return nil, err
	}
	if run == nil || memql.BareShortId(argString(run, "goalId")) != memql.BareShortId(rc.GoalId) || run["cancelRequested"] == true || run["status"] != "running" {
		return nil, fmt.Errorf("skill selection requires the current running goal")
	}
	step, err := one(s.query(ctx, "query "+call("workStepForOwnerRun", map[string]any{"runId": rc.RunId, "stepKey": rc.StepKey})))
	if err != nil {
		return nil, err
	}
	if step == nil || step["status"] != "running" {
		return nil, fmt.Errorf("skill selection requires the current running step")
	}
	version := argInt(step, "version", 0)
	if version < 1 {
		return nil, fmt.Errorf("skill selection requires a versioned step")
	}
	receiptID := skillContextReceiptID(rc.RunId, rc.StepKey, version)
	fromJournal := rc.Mode == common.RunModeReplay || (rc.Mode == common.RunModeFork && rc.BeforeForkPoint(rc.StepKey))
	if consultation && fromJournal {
		return nil, fmt.Errorf("replay must consume its recorded knowledge consultation")
	}
	if consultation {
		receiptID = "skill-consultation-" + id.NewShortId()
	}
	saved, err := readSkillContext(ctx, s, rc.RunId, receiptID)
	if err != nil {
		return nil, err
	}
	if saved != nil {
		return i.resultNode(skillContextResponse(saved, receiptID)), nil
	}
	if fromJournal {
		// Replayed model requests must contain the ORIGINAL prompt context,
		// including its original receipt citation. Selecting again changes the
		// model request hash and can turn a valid replay into a divergence.
		if rc.SourceRunId == "" || memql.BareShortId(argString(run, "forkedFromRunId")) != memql.BareShortId(rc.SourceRunId) {
			return nil, fmt.Errorf("skill context source does not match the owned replay")
		}
		source, err := s.runForOwner(ctx, rc.SourceRunId)
		if err != nil {
			return nil, err
		}
		if source == nil || memql.BareShortId(argString(source, "goalId")) != memql.BareShortId(rc.GoalId) {
			return nil, fmt.Errorf("skill context source belongs to another goal")
		}
		sourceStep, err := one(s.query(ctx, "query "+call("workStepForOwnerRun", map[string]any{"runId": rc.SourceRunId, "stepKey": rc.StepKey})))
		if err != nil {
			return nil, err
		}
		sourceVersion := argInt(sourceStep, "version", 0)
		if sourceVersion < 1 {
			return nil, fmt.Errorf("skill context source step is unavailable")
		}
		sourceReceiptID := skillContextReceiptID(rc.SourceRunId, rc.StepKey, sourceVersion)
		data, err := readSkillContext(ctx, s, rc.SourceRunId, sourceReceiptID)
		if err != nil {
			return nil, err
		}
		var selection any
		origin := receiptID
		if data != nil {
			restored := skillContextResponse(data, sourceReceiptID)
			selection, origin = restored["selection"], argString(restored, "receiptId")
		} else {
			// Preserve a pre-feature source's absence. This is a receipt of an
			// empty context, not an invented historical skill selection.
			selection = workvalues.SkillSelection{SchemaVersion: 1, Method: "source-without-recorded-context", Selected: []workvalues.SkillContext{}, Rejected: []workvalues.SkillContext{}}
		}
		// Freeze the restored content on this run too. A replay of a replay
		// can then restore it without recursively consulting mutable sources.
		return i.saveSkillContext(ctx, rc, version, receiptID, origin, "restored-for-context", selection)
	}
	goal, err := s.goalForOwner(ctx, rc.GoalId)
	if err != nil {
		return nil, err
	}
	if goal == nil {
		return nil, fmt.Errorf("skill selection goal is unavailable")
	}
	catalog, err := s.query(ctx, "query activeSkillsFull()")
	if err != nil {
		return nil, err
	}
	baselines, ok := i.engine.(interface{ IsInstalledSkillBaseline(map[string]any) bool })
	if !ok {
		return nil, fmt.Errorf("installed skill baseline verifier is unavailable")
	}
	for n, row := range catalog {
		catalog[n] = maps.Clone(row)
		catalog[n]["reviewedBaseline"] = baselines.IsInstalledSkillBaseline(row)
	}
	query := argString(goal, "statement") + "\nCurrent step: " + argString(args, "text")
	stage := "selected-for-context"
	if consultation {
		// Broad goal wording must not repeatedly crowd a newly discovered,
		// narrow requirement out of this separate context allocation.
		query, stage = argString(args, "text"), "consulted-for-need"
	}
	selection, err := workvalues.SelectSkillContext(query, catalog, argInt(args, "limit", 0), argInt(args, "minimumScore", 0), argInt(args, "maxBytes", 0))
	if err != nil {
		return nil, err
	}
	return i.saveSkillContext(ctx, rc, version, receiptID, receiptID, stage, selection)
}

func skillContextReceiptID(runID, stepKey string, version int) string {
	return "skill-selection-" + string(id.New().MustFromMap(map[string]any{"runId": memql.BareShortId(runID), "stepKey": stepKey, "version": version}))
}

func readSkillContext(ctx context.Context, s *store, runID, receiptID string) (map[string]any, error) {
	rows, err := s.query(ctx, "query "+call("workSkillSelectionForOwnerRun", map[string]any{"runId": runID, "observationId": receiptID}))
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	data, _ := rows[0]["data"].(map[string]any)
	saved, _ := data["skillSelection"].(map[string]any)
	if saved == nil || argInt(saved, "schemaVersion", 0) != 1 {
		return nil, fmt.Errorf("invalid saved skill selection")
	}
	return data, nil
}

func skillContextResponse(data map[string]any, receiptID string) map[string]any {
	if origin := argString(data, "contextReceiptId"); origin != "" {
		receiptID = origin
	}
	return map[string]any{"receiptId": receiptID, "selection": data["skillSelection"]}
}

func (i *Integration) saveSkillContext(ctx context.Context, rc common.RunContext, version int, receiptID, contextReceiptID, stage string, selection any) ([]memorynodes.MemoryNode, error) {
	data := map[string]any{"skillSelection": selection, "stepVersion": version, "contextReceiptId": contextReceiptID, "stage": stage}
	err := i.store().writeInternal(ctx, "mutation "+call("createWorkObservation", map[string]any{
		"observationId": receiptID, "runId": rc.RunId, "stepKey": rc.StepKey, "kind": "decision",
		"content": "Versioned skill context recorded for this work step; selection does not prove adherence or improvement.", "data": data,
	}))
	if err != nil {
		return nil, err
	}
	return i.resultNode(skillContextResponse(data, receiptID)), nil
}
