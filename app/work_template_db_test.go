package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/steps"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

func workTemplateDBEngine(t *testing.T) *memql.MemQLEngine {
	t.Helper()
	dsn := dbtest.DSN()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "work template hop", dsn, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	e, err := memql.New(db)
	if err != nil {
		t.Fatal(err)
	}
	e.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	if err = e.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	e.SetLogicRunner(automations.NewLogicRunner(e, steps.NewRegistry(), e.Logger))
	return e
}

func templateMutation(t *testing.T, e *memql.MemQLEngine, ctx context.Context, name string, args map[string]any) {
	t.Helper()
	var values []string
	for k, v := range args {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		values = append(values, k+":"+string(b))
	}
	if _, err := e.Execute(ctx, "mutation "+name+"("+strings.Join(values, ",")+")"); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

// The reader owns no planner registry. Its only input is the run journal and
// persisted source, and its ordinary Execute calls must see the whole closure.
func TestWorkTemplateDBResolvesAndRunsOnAnotherEngine(t *testing.T) {
	writer, reader := workTemplateDBEngine(t), workTemplateDBEngine(t)
	owner, bundle, construct, run := id.NewShortId(), id.NewShortId(), id.NewShortId(), id.NewShortId()
	ctx := auth.ContextWithUserActor(context.Background(), owner)
	name := "runTemplate" + strings.ReplaceAll(id.NewShortId(), "-", "")
	logic := name + "Answer"
	source := fmt.Sprintf("@template\nautomation %s {\n  answer := logic %s()\n}", name, logic)
	logicSource := fmt.Sprintf("logic %s {\n  return 42\n}", logic)
	templateMutation(t, writer, ctx, "createAuthoringBundle", map[string]any{"bundleId": bundle, "title": "Execution test", "sourceRunId": run})
	templateMutation(t, writer, ctx, "createAuthoringConstruct", map[string]any{"constructId": construct, "bundleId": bundle, "kind": "automation", "name": name, "targetNamespace": "work", "source": source})
	templateMutation(t, writer, ctx, "createAuthoringConstruct", map[string]any{"constructId": id.NewShortId(), "bundleId": bundle, "kind": "logic", "name": logic, "targetNamespace": "work", "source": logicSource})
	j := &automations.RunJournal{RunId: run, GoalId: "g", OwnerUserId: owner, AutomationName: name, TemplateConstructId: construct}
	j.TemplateVersion = memql.WorkBundleVersion([]memql.SandboxConstruct{{Kind: "automation", Name: name, Source: source}, {Kind: "logic", Name: logic, Source: logicSource}})
	loader := automations.NewLoader(automations.LoaderOptions{Logger: reader.Logger})
	if _, err := loadWorkTemplate(ctx, reader, loader, j); err == nil {
		t.Fatal("unvalidated draft executed")
	}
	templateMutation(t, writer, ctx, "recordBundleValidation", map[string]any{"bundleId": bundle, "status": "validated", "validationReport": map[string]any{"ok": true}})
	reader.InvalidateCacheForConcept("v1:authoring:bundle")
	tpl, err := loadWorkTemplate(ctx, reader, loader, j)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reader.Execute(ctx, logic+"()"); err == nil {
		t.Fatal("private source leaked to shared engine")
	}
	exec := automations.NewExecutor(automations.ExecutorOptions{Engine: reader, Logger: reader.Logger, StepRegistry: steps.NewRegistry()})
	defer exec.Close()
	ctx = memql.ContextWithAuthoredExecution(ctx, owner, tpl.registry)
	result, err := exec.ExecuteWithClientEvent(ctx, tpl.automation, "test", nil)
	if err != nil || result.Status != "completed" {
		t.Fatalf("execution on separate engine: %+v %v", result, err)
	}
	version := j.TemplateVersion
	j.TemplateVersion = memql.WorkBundleVersion([]memql.SandboxConstruct{{Kind: "automation", Name: name, Source: source}, {Kind: "logic", Name: logic, Source: strings.ReplaceAll(logicSource, "42", "43")}})
	if _, err = loadWorkTemplate(ctx, reader, loader, j); err == nil || !strings.Contains(err.Error(), "source changed") {
		t.Fatalf("changed sibling body was not refused: %v", err)
	}
	j.TemplateVersion = version
	j.OwnerUserId = "stranger"
	if _, err = loadWorkTemplate(auth.ContextWithUserActor(context.Background(), "stranger"), reader, loader, j); err == nil {
		t.Fatal("stranger loaded an owned draft")
	}
	j.OwnerUserId = owner
	j.RunId = "unrelated-run"
	if _, err = loadWorkTemplate(ctx, reader, loader, j); err == nil {
		t.Fatal("draft used outside its bound run")
	}
}
