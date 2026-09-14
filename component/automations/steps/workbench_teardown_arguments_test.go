package steps

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/automations"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// Replace only the destructive integration handler. Compilation, argument
// resolution, FunctionExecutor rendering and the engine's object-profile
// builtin parser/dispatch are real. No filesystem teardown or DB query runs.
type teardownArgumentProbe struct{ runIDs []string }

func (p *teardownArgumentProbe) IntegrationName() string { return "workbench" }

func (p *teardownArgumentProbe) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{{
		Name: "teardownDirectory",
		Handler: func(_ context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
			p.runIDs = append(p.runIDs, args["runId"].(string))
			return nil, nil
		},
	}}
}

func TestShippedWorkbenchTeardownReachesIntegrationWithRunID(t *testing.T) {
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
	// A lazy handle satisfies engine setup. Port 1 ensures an accidental DB
	// dependency fails instead of borrowing a developer's real database.
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(
		pgdriver.WithDSN("postgres://unused:unused@127.0.0.1:1/unused?sslmode=disable"))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	engine.SetDatabaseGetter(func() *bun.DB { return db })
	probe := &teardownArgumentProbe{}
	if err := engine.RegisterIntegration(probe); err != nil {
		t.Fatal(err)
	}
	for _, runID := range []string{"run-1", "v1:work:run:run-2"} {
		eval := automations.NewEvaluator()
		eval.SetCustom("args", map[string]any{"id": runID, "status": "succeeded"})
		eval.SetCustom("argsDeclared", map[string]bool{"id": true, "status": true})
		eval.SetStepResult("terminal", &automations.StepResult{Status: "success", Result: true})
		if ok, err := eval.StepCondition(context.Background(), teardown); err != nil || !ok {
			t.Fatalf("terminal run must reach teardown: %v, %v", ok, err)
		}
		result, err := (&FunctionExecutor{}).Execute(context.Background(), teardown, &Context{Engine: engine, Evaluator: eval})
		if err != nil || result.Status != "success" {
			t.Fatalf("teardown rejected before integration dispatch: result=%+v err=%v", result, err)
		}
		if len(probe.runIDs) == 0 || probe.runIDs[len(probe.runIDs)-1] != runID {
			t.Fatalf("integration received %v, want %q", probe.runIDs, runID)
		}
	}
	if len(probe.runIDs) != 2 {
		t.Fatalf("integration called %d times, want 2", len(probe.runIDs))
	}
}
