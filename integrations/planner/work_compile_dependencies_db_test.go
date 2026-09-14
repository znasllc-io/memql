package planner

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

func TestSealWorkDraftDBUsesOwnedCurrentCatalog(t *testing.T) {
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "work dependency snapshot", dbtest.DSN(), err)
	}
	for _, q := range []string{`CREATE TEMP TABLE "MemoryNodes" (LIKE public."MemoryNodes" INCLUDING ALL)`, `CREATE TEMP TABLE node_vectors (LIKE public.node_vectors INCLUDING ALL)`} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	e, err := memql.New(db)
	if err != nil {
		t.Fatal(err)
	}
	e.Logger = testLogger()
	if err := e.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	write := func(owner, name string, args map[string]any) {
		t.Helper()
		if _, err := e.Execute(auth.ContextWithUserActor(ctx, owner), "mutation "+name+"("+encodeArgs(args)+")"); err != nil {
			t.Fatal(err)
		}
	}
	for _, owner := range []string{"alice", "bob"} {
		answer := 42
		if owner == "bob" {
			answer = 99
		}
		write(owner, "createAuthoringBundle", map[string]any{"bundleId": owner + "-bundle", "title": "catalog"})
		write(owner, "createAuthoringConstruct", map[string]any{"constructId": owner + "-logic", "bundleId": owner + "-bundle", "kind": "logic", "name": "savedAnswer", "targetNamespace": "authored", "source": fmt.Sprintf("logic savedAnswer {\n  return %d\n}", answer)})
		write(owner, "setConstructStatus", map[string]any{"constructId": owner + "-logic", "status": "active"})
		write(owner, "catalogueConstruct", map[string]any{"constructId": owner + "-logic", "catalogKey": "answer", "catalogMatchText": "answer", "fromBundleId": owner + "-bundle"})
	}
	l := &PlannerAgentLoop{engine: &draftDBCompiler{engine: e}, logger: testLogger()}
	bundle := authoringBundle{AutomationName: "sealedRun", Constructs: []memql.SandboxConstruct{{Kind: "automation", Name: "sealedRun", Source: "@template\nautomation sealedRun {\n  answer := logic savedAnswer()\n}"}}, ReuseEdges: []reuseEdge{{Kind: "logic", Name: "savedAnswer", Namespace: "authored"}}}
	sealed, err := l.sealWorkDraft(ctx, "alice", bundle)
	if err != nil {
		result, readErr := e.Execute(auth.ContextWithUserActor(ctx, "alice"), `concept=="v1:authoring:construct" && name=="savedAnswer"`)
		t.Logf("catalog rows: %+v, read error: %v", memql.MaterializeRows(result), readErr)
		t.Fatal(err)
	}
	if len(sealed.Constructs) != 2 {
		t.Fatalf("owner's saved source missing: %+v", sealed.Constructs)
	}
	for _, c := range sealed.Constructs {
		if c.Kind == "logic" && !strings.Contains(c.Source, "return 42") {
			t.Fatalf("copied another owner's source: %+v", c)
		}
	}
	write("alice", "setConstructStatus", map[string]any{"constructId": "alice-logic", "status": "retired"})
	if _, err := l.sealWorkDraft(ctx, "alice", bundle); err == nil {
		t.Fatal("historical active version of a retired dependency was reused")
	}
}
