package steps

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/automations"
)

// memql#2235 -- arg-VALUE coverage for the authored forEach sweep pattern.
//
// The forEach feature (memql#2246) ships with execution coverage that proves
// the inner call fires once per item (count), but it dispatches a
// sub-automation and asserts only the fire count -- not the per-row ARGUMENT
// VALUES, and not the conditional-gating shape. The #2235 sweep migration
// turns each impure per-row sweep logic into a PURE logic (returns the rows)
// + an automation that does the per-row WRITE via a step-wrapped forEach:
//
//	step decide { logic <pure> { event: event } }
//	step apply  { forEach item in decide.Nodes() { mut({ id: item.id, ... }) } }
//
// For the destructive sweeps (accountDeletionSweep -> deleteUserHard) and the
// safety sweep (killSwitchSuspendsRunningPlans -> updatePlanStatus) it is not
// enough to know the loop fires N times -- it must fire with the RIGHT per-row
// args (so the right user is deleted / the right plan is suspended), and a
// conditionally-gated write must fire ONLY on matching rows. These tests pin
// that, driven through the REAL loader (rewriter -> parser -> compiler -> IR)
// and the REAL ForEachExecutor.

// argRecorder is a function-step executor that records the fully-resolved
// per-call args via the SAME resolveArgsRefs path the real FunctionExecutor
// uses, so the captured values are exactly what would reach the engine -- no
// live DB needed.
type argRecorder struct {
	name string
	args []map[string]any
}

func (r *argRecorder) Execute(_ context.Context, step *automations.Step, stepCtx *Context) (*automations.StepResult, error) {
	if step.Function != nil {
		r.name = step.Function.Name
		resolved, err := resolveArgsRefs(step.Function.Args, stepCtx.Evaluator)
		if err != nil {
			return nil, err
		}
		if obj, ok := resolved["0"].(map[string]any); ok {
			r.args = append(r.args, obj)
		} else {
			r.args = append(r.args, resolved)
		}
	}
	return &automations.StepResult{
		StepId:      step.ID,
		Status:      "success",
		StartedAt:   time.Now(),
		CompletedAt: time.Now(),
		Result:      "ok",
	}, nil
}

func sweepRow(id, label string) any {
	return map[string]any{"id": id, "payload": map[string]any{"label": label}}
}

func bundleOf(rows ...any) any {
	return map[string]any{"Bundle": map[string]any{"nodes": rows}}
}

// runSweepForEach compiles a sweep automation, finds its forEach step, seeds
// the `decide` step result with the given rows, executes the loop through the
// real ForEachExecutor, and returns the recorder of the per-row writes.
func runSweepForEach(t *testing.T, src string, rows []any) *argRecorder {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	loader := automations.NewLoader(automations.LoaderOptions{Logger: logger})
	auto, err := loader.CompileSource(src, "test:sweep-argvalue")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	var loop *automations.Step
	for _, s := range auto.Steps {
		if s != nil && s.Type == automations.StepTypeForEach {
			loop = s
		}
	}
	if loop == nil {
		t.Fatalf("compiled automation has NO forEach step -- the rewriter dropped the loop. Steps: %+v", auto.Steps)
	}

	eval := automations.NewEvaluator()
	eval.SetStepResult("decide", &automations.StepResult{
		StepId: "decide",
		Status: "success",
		Result: bundleOf(rows...),
	})

	rec := &argRecorder{}
	reg := NewRegistry()
	reg.Register(automations.StepTypeFunction, rec)
	exec := &ForEachExecutor{Registry: reg}

	res, err := exec.Execute(context.Background(), loop, &Context{Evaluator: eval, Logger: logger})
	if err != nil {
		t.Fatalf("forEach execute: %v", err)
	}
	if res.Status != "success" {
		t.Fatalf("forEach status = %q, want success (err=%q)", res.Status, res.Error)
	}
	return rec
}

// TestForEachSweep_BareWrite_PerRowArgs: an unconditional per-row write fires
// once per row, with item.id and a nested item.payload.* both resolved to the
// row's own values (the magicLinkExpirySweep / revokeExpiredDelegations shape).
func TestForEachSweep_BareWrite_PerRowArgs(t *testing.T) {
	const src = `@description("bare per-row write")
automation sweepBare {
  step decide { logic decideRows { event: event } }
  step apply {
    forEach item in decide.Nodes() {
      mutate markRow { rowId: item.id, label: item.payload.label }
    }
  }
}`
	rows := []any{
		sweepRow("v1:identity:magiclink:a", "alpha"),
		sweepRow("v1:identity:magiclink:b", "bravo"),
		sweepRow("v1:identity:magiclink:c", "charlie"),
	}
	rec := runSweepForEach(t, src, rows)

	if rec.name != "markRow" {
		t.Errorf("write name = %q, want markRow", rec.name)
	}
	if len(rec.args) != 3 {
		t.Fatalf("per-row write fired %d times, want 3", len(rec.args))
	}
	wantID := []string{"v1:identity:magiclink:a", "v1:identity:magiclink:b", "v1:identity:magiclink:c"}
	wantLabel := []string{"alpha", "bravo", "charlie"}
	for i, got := range rec.args {
		if got["rowId"] != wantID[i] {
			t.Errorf("call %d rowId = %#v, want %q", i, got["rowId"], wantID[i])
		}
		if got["label"] != wantLabel[i] {
			t.Errorf("call %d label (nested item.payload.label) = %#v, want %q", i, got["label"], wantLabel[i])
		}
	}
}

// TestForEachSweep_ConditionalWrite_GatesPerRow: a conditionally-gated per-row
// write fires ONLY on matching rows, with correct per-row args (the
// accountDeletionSweep / killSwitchSuspendsRunningPlans / expiry-sweep shape).
// This is the load-bearing guarantee for the destructive deleteUserHard sweep:
// the wrong rows must NOT be written.
func TestForEachSweep_ConditionalWrite_GatesPerRow(t *testing.T) {
	const src = `@description("conditional per-row write")
automation sweepConditional {
  step decide { logic decideRows { event: event } }
  step apply {
    forEach item in decide.Nodes() {
      if item.payload.label == "expired" {
        mutate retireRow { rowId: item.id }
      }
    }
  }
}`
	rows := []any{
		sweepRow("u-1", "expired"),
		sweepRow("u-2", "active"),
		sweepRow("u-3", "expired"),
		sweepRow("u-4", "active"),
	}
	rec := runSweepForEach(t, src, rows)

	if len(rec.args) != 2 {
		t.Fatalf("conditional write fired %d times, want 2 (only 'expired' rows)", len(rec.args))
	}
	for i, got := range rec.args {
		want := []string{"u-1", "u-3"}[i]
		if got["rowId"] != want {
			t.Errorf("call %d rowId = %#v, want %q (only expired rows must be written)", i, got["rowId"], want)
		}
	}
}
