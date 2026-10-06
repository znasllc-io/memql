package planner

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// Separate engines have separate query caches. Both read the old row before
// concurrent appends, so a process-local lock or a cached read loses a directive.
func TestStandingDirectiveAppendSurvivesReplicaChange(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "standing directive replicas", dbtest.DSN(), err)
	}
	schema := fmt.Sprintf("directive_replicas_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`) })
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()),
		pgdriver.WithConnParams(map[string]any{"search_path": schema}))), pgdialect.New())
	db.SetMaxOpenConns(8) // Advisory-lock transactions and engine writes use different connections.
	t.Cleanup(func() { _ = db.Close() })
	for _, table := range []string{`"MemoryNodes"`, "node_vectors"} {
		if _, err := db.ExecContext(ctx, `CREATE TABLE `+table+` (LIKE public.`+table+` INCLUDING ALL)`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	newReplica := func() *ReactiveLoop {
		engine, err := memql.New(db)
		if err != nil {
			t.Fatal(err)
		}
		engine.Logger = testLogger()
		if err := engine.Init(memorynodes.DefaultRegistry()); err != nil {
			t.Fatal(err)
		}
		r := NewReactiveLoop(&draftDBCompiler{engine: engine}, testLogger())
		r.writeGate = func(ctx context.Context, agentID string) (func(), error) {
			return memql.AcquireWriteGate(ctx, db.DB, "agent-directive:"+strings.TrimPrefix(agentID, "v1:agents:agent:"))
		}
		return r
	}
	one, two := newReplica(), newReplica()
	ctx = ownerActorContext(ctx, "v1:identity:user:directive-owner")
	for _, query := range []string{
		`mutation createAgent(agentId:"directive-subject", ownerUserId:"v1:identity:user:directive-owner", name:"Directive subject", kind:"assistant", role:"assistant")`,
		`mutation updateAgent(agentId:"directive-subject", payload:{lineage:{createdBy:"user", originatingRunId:"v1:work:run:original", extensionGoals:["original directive"]}})`,
	} {
		if _, err := one.engine.Execute(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	for _, replica := range []*ReactiveLoop{one, two} {
		if row := replica.loadAgent(ctx, "directive-subject"); row == nil || row["lineage"] == nil {
			t.Fatalf("agent projection lost its lineage: %+v", row)
		}
	}
	start, results := make(chan struct{}), make(chan error, 2)
	for index, replica := range []*ReactiveLoop{one, two} {
		go func() {
			<-start
			identifier := "directive-subject"
			if index == 1 {
				identifier = "v1:agents:agent:" + identifier
			}
			results <- replica.appendDirective(ctx, map[string]any{"statement": fmt.Sprintf("replica %d directive", index)}, identifier)
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	row := two.loadAgent(memql.ContextWithFreshRead(ctx), "directive-subject")
	lineage, _ := row["lineage"].(map[string]any)
	goals := toStringList(lineage["extensionGoals"])
	slices.Sort(goals)
	if !slices.Equal(goals, []string{"original directive", "replica 0 directive", "replica 1 directive"}) ||
		lineage["createdBy"] != "user" || lineage["originatingRunId"] != "v1:work:run:original" {
		t.Fatalf("replica append lost directives or lineage: %+v", lineage)
	}
}
