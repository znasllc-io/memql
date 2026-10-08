package work

import (
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
