package steps

import (
	"context"
	"database/sql"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"strings"
	"testing"

	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// Use the real function executor and logic runner: a read-only logic called
// by a hook must not emit even a legacy automation:step journal write.
func TestBeforeWritePureLogicNeverTouchesTheStore(t *testing.T) {
	engine := bootEmbeddedEngine(t)
	registry := NewRegistry()
	engine.SetLogicRunner(automations.NewLogicRunner(engine, registry, engine.Logger))
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN("postgres://unused:unused@127.0.0.1:1/unused?sslmode=disable"))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	db.AddQueryHook(beforeWriteNoQueries{t: t})
	engine.SetDatabaseGetter(func() *bun.DB { return db })
	a, err := automations.NewLoader(automations.LoaderOptions{Functions: engine.Functions()}).CompileSource(`@trigger(before="create", concept="v1:forge:request")
automation probeBeforeWrite {
 chosen := logic requestRouteStatus(submitterRole: row.submitterRole)
 row.status = chosen
 if chosen == "queued" {
  row.approvedByUserId = row.submitterUserId
 }
}`, "test")
	if err != nil {
		t.Fatal(err)
	}
	hook := automations.BuildBeforeWriteHooks(engine, []*automations.Automation{a}, registry, engine.Logger)[a.BeforeWrite.Concept][0]
	row := map[string]any{"submitterRole": "owner", "submitterUserId": "owner-id", "status": "submitted"}
	if err := hook.Apply(common.ContextWithRun(context.Background(), common.RunContext{RunId: "parent-run", StepKey: "parent-write"}), row); err != nil {
		t.Fatal(err)
	}
	if row["status"] != "queued" || row["approvedByUserId"] != "owner-id" {
		t.Fatal(row)
	}
	// A query's static kind is read-only, but an embedded builtin must still
	// satisfy the handler-level read-only classification before dispatch.
	if err := engine.Functions().Upsert(&memql.Function{Name: "beforeWriteNestedEffect", Enabled: true, FunctionKind: "query", Origin: "unified:probe/queries.memql", Expr: &memql.BuiltinFunctionExpression{Name: "nestedEffect", Executor: "unknown.effectful.executor"}}); err != nil {
		t.Fatal(err)
	}
	guarded, err := automations.NewLoader(automations.LoaderOptions{Functions: engine.Functions()}).CompileSource(`@trigger(before="write", concept="v1:forge:request")
automation guardedBeforeWrite { query beforeWriteNestedEffect() }`, "test")
	if err != nil {
		t.Fatal(err)
	}
	guardedHook := automations.BuildBeforeWriteHooks(engine, []*automations.Automation{guarded}, registry, engine.Logger)[guarded.BeforeWrite.Concept][0]
	if err := guardedHook.Apply(context.Background(), row); err == nil || !strings.Contains(err.Error(), "dry-run refused builtin executor") {
		t.Fatalf("nested builtin escaped read-only classification: %v", err)
	}
	if query := RecordStepExecution(memql.ContextWithBeforeWrite(context.Background()), engine, StepRecordData{StepId: "read"}); query != "" {
		t.Fatal("hook produced journal query", query)
	}
}

type beforeWriteNoQueries struct{ t *testing.T }

func (h beforeWriteNoQueries) BeforeQuery(ctx context.Context, event *bun.QueryEvent) context.Context {
	h.t.Fatalf("pure hook issued SQL: %s", event.Query)
	return ctx
}
func (h beforeWriteNoQueries) AfterQuery(context.Context, *bun.QueryEvent) {}
