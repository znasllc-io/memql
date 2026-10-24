package work

import (
	"context"
	"errors"
	"testing"
)

func TestDirectGoalBindsItsConsumerBeforeRunCanDispatch(t *testing.T) {
	i, eng := newTestIntegration(t)
	attached := false
	_, _, err := i.OpenDirectGoal(callerContext("u-alice"), DirectGoal{
		OwnerUserId: "u-alice", Statement: "materialize report", AutomationName: "materializeFile", RequestedVia: "materializer",
		AccountIds: []string{"acme"}, Ceilings: map[string]any{"maxModelCalls": 2},
		BeforeRun: func(ctx context.Context, goal, run string) error {
			for _, c := range eng.calls {
				if c.Name() == "createWorkRun" {
					t.Fatal("run emitted before composition was attached")
				}
			}
			if goal == "" || run == "" {
				t.Fatal("missing identities")
			}
			attached = true
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !attached {
		t.Fatal("composition was never attached")
	}
	args := eng.callTo(t, "createWorkGoal").Args(t)
	if args["ceilings"].(map[string]any)["maxModelCalls"] != float64(2) || len(args["accountIds"].([]any)) != 1 {
		t.Fatalf("direct goal dropped input policy: %v", args)
	}
}
func TestDirectGoalDoesNotEmitRunWhenConsumerCannotBind(t *testing.T) {
	i, eng := newTestIntegration(t)
	_, _, err := i.OpenDirectGoal(callerContext("u-alice"), DirectGoal{OwnerUserId: "u-alice", Statement: "report", AutomationName: "materializeFile", BeforeRun: func(context.Context, string, string) error { return errors.New("composition disappeared") }})
	if err == nil {
		t.Fatal("failed binding accepted")
	}
	for _, c := range eng.calls {
		if c.Name() == "createWorkRun" {
			t.Fatal("dispatched run without composition")
		}
	}
	if got := eng.callsTo("updateWorkGoal"); len(got) != 0 {
		t.Fatalf("admission failure must not close a human goal: %v", got)
	}
}
