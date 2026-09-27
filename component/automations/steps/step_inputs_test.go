package steps

import (
	"context"
	"database/sql"
	"os"
	"reflect"
	"testing"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/core/common"
)

func TestOverrideInputsReplaceTheArgumentsTheyName(t *testing.T) {
	args := map[string]any{"month": "2026-07", "format": "md"}
	ctx := common.ContextWithRun(context.Background(), common.RunContext{
		RunId: "run", StepKey: "draft",
		Override: &common.StepOverride{Inputs: map[string]any{"month": "2026-08", "draft": true}},
	})
	got := withOverrideInputs(ctx, args)
	if want := map[string]any{"month": "2026-08", "format": "md", "draft": true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v: named inputs replace, the rest keep their values", got, want)
	}
	if args["month"] != "2026-07" {
		t.Fatal("the evaluated arguments were mutated; they may be shared with the evaluator's bindings")
	}
}

func TestAStepNobodyOverrodeKeepsItsArguments(t *testing.T) {
	args := map[string]any{"month": "2026-07"}
	for name, ctx := range map[string]context.Context{
		"no run":         context.Background(),
		"no override":    common.ContextWithRun(context.Background(), common.RunContext{RunId: "run"}),
		"override, none": common.ContextWithRun(context.Background(), common.RunContext{RunId: "run", Override: &common.StepOverride{Level: "strong"}}),
	} {
		if got := withOverrideInputs(ctx, args); !reflect.DeepEqual(got, args) {
			t.Errorf("%s: got %v", name, got)
		}
	}
}

// Through the real executor, engine and integration: the inputs a person set
// for this version are what the call carries, and the step's own evaluated
// argument is what it carries without them.
func TestAFunctionStepCallsWithThePersonsInputs(t *testing.T) {
	source, err := os.ReadFile("../../../dsl/workbench/automations.memql")
	if err != nil {
		t.Fatal(err)
	}
	auto, err := automations.NewLoader(automations.LoaderOptions{}).CompileSource(string(source), "workbench/automations.memql")
	if err != nil {
		t.Fatal(err)
	}
	var teardown *automations.Step
	for _, step := range auto.Steps {
		if step.ID == "teardown" {
			teardown = step
		}
	}
	if teardown == nil {
		t.Fatal("shipped automation has no teardown step")
	}
	engine := bootEmbeddedEngine(t)
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(
		pgdriver.WithDSN("postgres://unused:unused@127.0.0.1:1/unused?sslmode=disable"))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	engine.SetDatabaseGetter(func() *bun.DB { return db })
	probe := &teardownArgumentProbe{}
	if err := engine.RegisterIntegration(probe); err != nil {
		t.Fatal(err)
	}
	run := automations.NewEvaluator()
	run.SetCustom("args", map[string]any{"id": "run-evaluated", "status": "succeeded"})
	eval := run.ChildFrame()
	eval.Bind("terminal", true)

	overridden := common.ContextWithRun(context.Background(), common.RunContext{
		RunId: "run-1", StepKey: "teardown",
		Override: &common.StepOverride{Inputs: map[string]any{"runId": "run-from-the-person"}},
	})
	if result, err := (&FunctionExecutor{}).Execute(overridden, teardown, &Context{Engine: engine, Evaluator: eval}); err != nil || result.Status != "success" {
		t.Fatalf("overridden call refused: result=%+v err=%v", result, err)
	}
	if result, err := (&FunctionExecutor{}).Execute(context.Background(), teardown, &Context{Engine: engine, Evaluator: eval}); err != nil || result.Status != "success" {
		t.Fatalf("plain call refused: result=%+v err=%v", result, err)
	}
	if want := []string{"run-from-the-person", "run-evaluated"}; !reflect.DeepEqual(probe.runIDs, want) {
		t.Fatalf("the integration received %v, want %v", probe.runIDs, want)
	}
}
