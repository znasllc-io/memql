package automations

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// originCapturingRegistry records the origin of the context each step is
// dispatched with.
type originCapturingRegistry struct {
	got auth.CallOrigin
}

func (r *originCapturingRegistry) Execute(ctx context.Context, step *Step, stepCtx *StepContext) (*StepResult, error) {
	r.got = auth.OriginFromContext(ctx)
	return &StepResult{Status: "success"}, nil
}

// TestStepDispatchCarriesInternalOrigin pins the fix for the regression that
// made memql#2800's first attempt ship a broken kill switch.
//
// The internal stamp was originally applied to executeInput -- the automation's
// `input:` block. No STEP goes through that path: every step type dispatches
// via stepRegistry.Execute, which received an unstamped context and therefore
// OriginClient. killSwitchSuspendsRunningPlans reads runningPlansForUser
// (@serverOnly) from a step, so the decide step was refused as a client call
// and no plan was suspended when a user tripped the computer-use kill switch.
//
// The DSL comment asserted the opposite ("the automation path stamps ... so it
// keeps working"), and nothing tested it, so the security control failed
// closed-looking but open -- exactly what the issue's park comment predicted.
//
// Asserting on the STEP DISPATCH BOUNDARY rather than on the kill switch
// specifically is deliberate: it holds for every step type and every
// @serverOnly construct any automation reaches later, instead of pinning one
// automation that happens to exercise it today.
func TestStepDispatchCarriesInternalOrigin(t *testing.T) {
	reg := &originCapturingRegistry{}
	e := &Executor{
		logger:       slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		stepRegistry: reg,
	}

	step := &Step{ID: "s1", Name: "decide", Type: StepTypeFunction}
	stepCtx := &StepContext{Execution: NewExecution("killSwitchSuspendsRunningPlans", "test")}
	if _, err := e.executeStep(context.Background(), step, stepCtx); err != nil {
		t.Fatalf("executeStep: %v", err)
	}

	if !reg.got.IsInternal() {
		t.Fatalf("step dispatched with origin %v, want internal.\n"+
			"An automation step is server-side by construction -- an AUTHORED body "+
			"dispatched from a graph event -- so it must be able to reach @serverOnly "+
			"constructs. Without this the computer-use kill switch is refused as a "+
			"client call and silently suspends nothing.", reg.got)
	}
}

// TestUnstampedExecutorContextIsStillClient guards the other direction: the
// stamp must come from executeStep, not from something ambient. If the origin
// were internal before executeStep applied it, the assertion above would pass
// for the wrong reason and could not detect the stamp being removed.
func TestUnstampedExecutorContextIsStillClient(t *testing.T) {
	if auth.OriginFromContext(context.Background()).IsInternal() {
		t.Fatal("a bare context reported internal origin")
	}
}
