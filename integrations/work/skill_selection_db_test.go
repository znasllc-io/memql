package work

import (
	"encoding/json"
	"reflect"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// The separate installed-seed test verifies release provenance. This seam
// supplies one reviewed test skill while exercising REAL row authority, gates,
// inserts, version collapse and receipt restoration across two engine instances.
type selectionDBEngine struct {
	*memql.MemQLEngine
	skill string
}

func (e selectionDBEngine) IsInstalledSkillBaseline(row map[string]any) bool {
	return memql.BareShortId(argString(row, "id")) == e.skill && argInt(row, "version", 0) == 1
}

func TestSkillSelectionSurvivesReplicaHopAndCatalogChange(t *testing.T) {
	a := openActsDB(t)
	skillID := "selection-" + a.owner
	skill := map[string]any{"skillId": skillID, "slug": skillID, "name": "Software test skill", "tier": "A", "version": 1, "predefined": true, "active": true, "status": "active", "tags": []string{"report"}, "instructions": map[string]any{"body": "Preserve the completed action and original approval.", "selectionTerms": []string{"report"}}}
	a.write(t, "createSkill", skill)
	run := a.openRun(t, nil)
	a.writeVersion(t, run, run, "draft", 1, 1, nil, "", map[string]any{"status": "running"})
	goal := a.query(t, a.owner, "query "+call("workRunForOwner", map[string]any{"runId": run}))[0]["goalId"].(string)
	ctx := common.ContextWithRun(actorCtx(a.owner), common.RunContext{RunId: run, GoalId: goal, StepKey: "draft", OwnerUserId: a.owner, Mode: common.RunModeLive})
	first := New(selectionDBEngine{a.eng, skillID}, testLogger())
	first.SetGateDB(testGates(t))
	second := New(selectionDBEngine{openWorkTestEngine(t), skillID}, testLogger())
	second.SetGateDB(testGates(t))
	args := map[string]any{"limit": 3, "minimumScore": 1, "maxBytes": 8000}
	var wg sync.WaitGroup
	errors := make(chan error, 2)
	for _, replica := range []*Integration{first, second} {
		wg.Go(func() { _, err := replica.handleSelectSkills(ctx, args, 0); errors <- err })
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	q := "query " + call("workObservationsForOwnerRun", map[string]any{"runId": run})
	before := a.query(t, a.owner, q)
	if len(before) != 1 {
		t.Fatalf("concurrent replicas created %d receipts", len(before))
	}
	raw, _ := json.Marshal(before[0]["data"])
	var data struct {
		Selection struct {
			Selected []struct {
				Text    string `json:"text"`
				Version int    `json:"version"`
			} `json:"selected"`
		} `json:"skillSelection"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Selection.Selected) != 1 || data.Selection.Selected[0].Text != "Preserve the completed action and original approval." || data.Selection.Selected[0].Version != 1 {
		t.Fatalf("lost exact selected content: %s", raw)
	}
	skill["version"] = 2
	skill["instructions"] = map[string]any{"body": "New catalog content after the checkpoint."}
	a.write(t, "createSkill", skill)
	if _, err := second.handleSelectSkills(ctx, args, 0); err != nil {
		t.Fatal(err)
	}
	after := a.query(t, a.owner, q)
	again, _ := json.Marshal(after[0]["data"])
	if len(after) != 1 || string(raw) != string(again) {
		t.Fatal("another replica reselected changed content")
	}
	// Replay and a fork's shared prefix must reproduce the source prompt,
	// even though the catalog now contains different instructions.
	source := run
	for _, mode := range []string{common.RunModeReplay, common.RunModeReplay, common.RunModeFork} {
		derived := a.openRun(t, map[string]any{"goalId": goal, "mode": mode, "forkedFromRunId": source, "forkAtStepKey": "next"})
		a.writeVersion(t, derived, derived, "draft", 1, 1, nil, "", map[string]any{"status": "running"})
		derivedCtx := common.ContextWithRun(actorCtx(a.owner), common.RunContext{RunId: derived, GoalId: goal, StepKey: "draft", OwnerUserId: a.owner, Mode: mode, SourceRunId: source, ForkAtStepKey: "next", StepOrder: []string{"draft", "next"}})
		got, err := second.handleSelectSkills(derivedCtx, args, 0)
		if err != nil {
			t.Fatal(err)
		}
		var response, saved map[string]any
		if err := json.Unmarshal(got[0].Payload, &response); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &saved); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(response["selection"], saved["skillSelection"]) || memql.BareShortId(argString(response, "receiptId")) != memql.BareShortId(argString(before[0], "id")) {
			t.Fatalf("%s did not restore the original receipt and exact content", mode)
		}
		own := a.query(t, a.owner, "query "+call("workObservationsForOwnerRun", map[string]any{"runId": derived}))
		if len(own) != 1 || argString(own[0]["data"].(map[string]any), "stage") != "restored-for-context" {
			t.Fatal("replay did not freeze its restored context")
		}
		source = derived
	}
	if got := a.query(t, a.owner+"-stranger", q); len(got) != 0 {
		t.Fatal("another owner read the task's skill receipt")
	}
	if _, err := second.handleSelectSkills(actorCtx(a.owner), args, 0); err == nil {
		t.Fatal("public caller forged a selection without trusted work context")
	}
	wrong := common.ContextWithRun(actorCtx(a.owner+"-stranger"), common.RunContext{RunId: run, GoalId: goal, StepKey: "draft", OwnerUserId: a.owner, Mode: common.RunModeLive})
	if _, err := second.handleSelectSkills(wrong, args, 0); err == nil {
		t.Fatal("another owner supplied the run context")
	}
	// Cancellation remains authoritative even with a saved receipt.
	a.write(t, "updateWorkRun", map[string]any{"runId": run, "cancelRequested": true})
	if _, err := second.handleSelectSkills(ctx, args, 0); err == nil {
		t.Fatal("cancelled work resumed selection")
	}
}
