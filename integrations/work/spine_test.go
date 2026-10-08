package work

import (
	"context"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"os"
	"testing"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	workcore "github.com/znasllc-io/memql/component/work"
)

func TestCreateGoalPinsSpineBeforeCompileAndRefusesUnknownSelection(t *testing.T) {
	i, engine := newTestIntegration(t)
	_, err := i.handleCreateGoal(callerContext("alice"), map[string]any{"statement": "do it", "spine": "missingCompanySpine"}, 0)
	if err == nil || engine.summary() != "(none)" {
		t.Fatalf("invalid Spine created rows: %v %s", err, engine.summary())
	}
	_, err = i.handleCreateGoal(callerContext("alice"), map[string]any{"statement": "do it", "spine": workcore.DefaultSpine}, 0)
	if err != nil {
		t.Fatal(err)
	}
	args := engine.callTo(t, "createWorkRun").Args(t)
	snapshot, err := workflowhost.SnapshotFromMap(argMap(args, "spine"))
	if err != nil || snapshot.Entry != workcore.DefaultSpine {
		t.Fatalf("run has no frozen selection: %v", err)
	}
	if _, present := argMap(args, "input")["spine"]; present {
		t.Fatal("runtime definition leaked into model input")
	}
}

func TestSpineDeveloperHooksReuseDefaultsAndSurviveReplicaHop(t *testing.T) {
	body, err := os.ReadFile("../../examples/spine/company/automations.memql")
	if err != nil {
		t.Fatal(err)
	}
	definitions := map[string]string{}
	for _, definition := range memql.ExtractAutomationSlices(string(body)) {
		definitions["automation:"+definition.Name] = definition.Source
	}
	for _, definition := range memql.ExtractFunctionSlices(string(body)) {
		definitions["logic:"+definition.Name] = definition.Source
	}
	source := func(kind, name string) (string, error) {
		if source, ok := definitions[kind+":"+name]; ok {
			return source, nil
		}
		return workflowhost.InstalledSource(kind, name)
	}
	snapshot, err := captureSpine("companyAnswerSpine", source)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := workflowhost.SnapshotFromMap(snapshot.Map())
	if err != nil {
		t.Fatal(err)
	}
	if restored.PhaseEntry("workSpineRecovery") != "companyReviewedRecovery" || restored.PhaseEntry("workSpineDraftProgram") != "workSpineDraftProgram" {
		t.Fatalf("lost phase selections: %+v", restored.Phases)
	}
	// The receiver has no company source installed. Only the persisted closure
	// and the phase's empty capability scope cross the hop.
	out, err := workflowhost.RunPhase(context.Background(), restored.Map(), workcore.SpineContract, "workSpineRecovery", map[string]any{
		"symptom": "transient", "retriesSpent": 0, "maxRetries": 3, "hasGoal": true,
	}, workflowhost.Options{})
	if err != nil || out.(map[string]any)["act"] != "ask" {
		t.Fatalf("lost custom policy: %v %v", out, err)
	}
}

func TestSpineRefusesInvalidHookConfigurationBeforeAdmission(t *testing.T) {
	for _, body := range []string{
		`return {unknown: "workSpineRecovery"}`,
		`return {workSpineRecovery: "missingRecipe"}`,
		`return {workSpineRecovery: 7}`,
		`return builtin spineContext()`,
		`return true`,
	} {
		_, err := captureSpine("defaultWorkSpine", func(kind, name string) (string, error) {
			if kind == "automation" && name == "defaultWorkSpinePhases" {
				return "@template\nautomation defaultWorkSpinePhases { " + body + " }", nil
			}
			return workflowhost.InstalledSource(kind, name)
		})
		if err == nil {
			t.Fatalf("invalid hook configuration accepted: %s", body)
		}
	}
}

func TestDefaultFrozenRecoveryPolicyCoversSymptomsAndBudgets(t *testing.T) {
	snapshot, err := CaptureSpine("")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		symptom string
		spent   int
		goal    bool
		act     string
	}{
		{"transient", 0, true, "retry"}, {"contract", 0, true, "repair"}, {"plan", 0, true, "replan"},
		{"environment", 0, true, "heal"}, {"human", 0, true, "ask"}, {"transient", 3, true, "ask"},
		{"contract", 0, false, "ask"}, {"plan", 0, false, "ask"},
	} {
		value, err := workflowhost.RunPhase(context.Background(), snapshot.Map(), workcore.SpineContract, "workSpineRecovery", map[string]any{
			"symptom": tc.symptom, "retriesSpent": tc.spent, "maxRetries": 3, "hasGoal": tc.goal,
		}, workflowhost.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if value.(map[string]any)["act"] != tc.act {
			t.Fatalf("%+v: %v", tc, value)
		}
	}
}

func TestDirectGoalsPinDefaultOrInheritTheSameOwnersRecipes(t *testing.T) {
	parent, err := captureSpine("defaultWorkSpine", func(kind, name string) (string, error) {
		source, err := workflowhost.InstalledSource(kind, name)
		if err == nil && kind == "automation" && name == "defaultWorkSpine" {
			source += "\n// frozen parent bundle revision"
		}
		return source, err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, owner string
		source      map[string]any
		wantError   bool
	}{
		{"standalone", "alice", nil, false},
		{"same owner", "alice", parent.Map(), false},
		{"another owner", "bob", parent.Map(), false},
		{"invalid inherited source", "alice", map[string]any{"version": "corrupt"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			integration, engine := newTestIntegration(t)
			ctx := common.ContextWithRun(callerContext(tc.owner), common.RunContext{RunId: "parent", OwnerUserId: "alice", Spine: tc.source})
			_, _, err := integration.OpenDirectGoal(ctx, DirectGoal{OwnerUserId: tc.owner, Statement: "test delegation", AutomationName: "invokeAgent"})
			if tc.wantError {
				if err == nil || engine.summary() != "(none)" {
					t.Fatalf("invalid source admitted: %v %s", err, engine.summary())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := workflowhost.SnapshotFromMap(argMap(engine.callTo(t, "createWorkRun").Args(t), "spine"))
			if err != nil || len(snapshot.Phases) != 5 {
				t.Fatalf("direct run has no full Spine: %v", err)
			}
			if tc.name == "another owner" && snapshot.Version == parent.Version {
				t.Fatal("different owner inherited the parent bundle")
			}
			if tc.name == "same owner" && snapshot.Version != parent.Version {
				t.Fatal("delegation substituted a different recipe")
			}
		})
	}
}
