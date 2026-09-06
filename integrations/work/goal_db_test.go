package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// goal_db_test.go -- the one thing every other test in this package cannot
// prove (epic memql#4966).
//
// The unit tests here all speak to a recording executor, which accepts
// whatever string it is handed. render_test.go closes half the gap by
// handing each composed call to the REAL parser -- but a call that parses
// is not a row that lands. Four things stand between the two and none of
// them is reachable without Postgres:
//
//   - the concept's own type check (an optional object given null refuses
//     the WHOLE insert, which is the second defect this epic fixed);
//   - @serverSet stamping, i.e. that ownerUserId really comes from the
//     actor and there is no argument to forge;
//   - row-authz admission, whose failure mode is ZERO ROWS AND NO ERROR --
//     indistinguishable from "this person has no goals";
//   - whether the row exists at all afterwards.
//
// So this test writes a goal through the REAL handler against a REAL
// engine and reads it back through the owner-scoped query, then asserts a
// DIFFERENT actor cannot see it. That last assertion is the one that would
// catch a tier that silently admits everybody.

func openWorkTestEngine(t *testing.T) *memqlengine.MemQLEngine {
	t.Helper()
	dsn := dbtest.DSN()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "work integration goal acceptance", dsn, err)
		return nil
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	eng, err := memqlengine.New(db)
	if err != nil {
		t.Fatalf("memql.New: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatalf("engine Init: %v", err)
	}
	return eng
}

// actorCtx is a signed-in person, the shape every client-reachable path here
// runs under.
func actorCtx(userId string) context.Context {
	return auth.ContextWithUserActor(context.Background(), userId)
}

func TestCreateGoal_DB_WritesARowTheOwnerCanReadAndAStrangerCannot(t *testing.T) {
	eng := openWorkTestEngine(t)
	i := New(eng, slog.New(slog.NewTextHandler(io.Discard, nil)))

	owner := "dbtest-work-owner-" + time.Now().UTC().Format("20060102150405.000000000")
	stranger := owner + "-stranger"
	statement := "Reconcile the ledger for " + owner

	// The real handler, under the real actor.
	nodes, err := i.handleCreateGoal(actorCtx(owner), map[string]any{
		"statement":    statement,
		"requestedVia": "api",
		// An optional object left ABSENT. Rendered as `null` this refuses the
		// whole insert -- the defect that produced step rows with no run row.
		"accountIds": []any{"acme"},
	}, 0)
	if err != nil {
		t.Fatalf("handleCreateGoal: %v", err)
	}
	if len(nodes) == 0 {
		t.Fatal("createGoal returned no reply")
	}

	// Read it back through the OWNER-SCOPED query, which is what a browser
	// runs. This is the assertion the recording executor cannot make: the row
	// exists, it type-checked, and admission let its owner see it.
	rows := queryGoals(t, eng, owner)
	var found map[string]any
	for _, r := range rows {
		if s, _ := r["statement"].(string); s == statement {
			found = r
		}
	}
	if found == nil {
		t.Fatalf("the goal did not come back for its owner (%d rows). A call that PARSES is not a row that LANDS: check the concept's type check and @serverSet stamping.", len(rows))
	}
	if got, _ := found["ownerUserId"].(string); got != owner {
		t.Errorf("ownerUserId = %q, want %q stamped from the actor", got, owner)
	}
	if got, _ := found["origin"].(string); got != "user" {
		t.Errorf("origin = %q, want user", got)
	}
	if got, _ := found["status"].(string); got != "open" {
		t.Errorf("status = %q, want open", got)
	}

	// The negative half, and the one worth the most: a DIFFERENT person must
	// not see it. An owned tier that silently admitted everybody would pass
	// every assertion above.
	for _, r := range queryGoals(t, eng, stranger) {
		if s, _ := r["statement"].(string); s == statement {
			t.Fatal("a different actor read this person's goal -- the owned tier is not being enforced on this read")
		}
	}
}

// queryGoals runs the owner-facing read as that person.
func queryGoals(t *testing.T, eng *memqlengine.MemQLEngine, userId string) []map[string]any {
	t.Helper()
	res, err := eng.Execute(actorCtx(userId), "query workGoalsForOwner()")
	if err != nil {
		t.Fatalf("workGoalsForOwner as %s: %v", userId, err)
	}
	return memqlengine.MaterializeRows(res)
}

// The run opened beside the goal must exist too, and must carry the same
// owner: a run nobody can read is a run nobody can watch, and the OS Work
// app reads runs by owner.
func TestCreateGoal_DB_OpensARunTheOwnerCanRead(t *testing.T) {
	eng := openWorkTestEngine(t)
	i := New(eng, slog.New(slog.NewTextHandler(io.Discard, nil)))

	owner := "dbtest-work-run-" + time.Now().UTC().Format("20060102150405.000000000")
	statement := "Draft the board note for " + owner

	nodes, err := i.handleCreateGoal(actorCtx(owner), map[string]any{
		"statement": statement, "requestedVia": "api",
	}, 0)
	if err != nil {
		t.Fatalf("handleCreateGoal: %v", err)
	}
	runId := replyField(t, nodes, "runId")
	if runId == "" {
		t.Fatal("createGoal reported no runId")
	}

	res, err := eng.Execute(actorCtx(owner), "query workRunsForOwner()")
	if err != nil {
		t.Fatalf("workRunsForOwner: %v", err)
	}
	for _, r := range memqlengine.MaterializeRows(res) {
		id, _ := r["id"].(string)
		if strings.HasSuffix(id, runId) {
			if got, _ := r["ownerUserId"].(string); got != owner {
				t.Errorf("run ownerUserId = %q, want %q -- the run is written under the goal owner's borrowed authority", got, owner)
			}
			if got, _ := r["status"].(string); got != "compiling" {
				t.Errorf("run status = %q, want compiling", got)
			}
			return
		}
	}
	t.Fatalf("run %s did not come back for its owner", runId)
}

// replyField digs one field out of the builtin's id-keyed reply map.
func replyField(t *testing.T, nodes []memorynodes.MemoryNode, key string) string {
	t.Helper()
	for _, n := range nodes {
		b, err := json.Marshal(n)
		if err != nil {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			continue
		}
		if v := digString(m, key); v != "" {
			return v
		}
	}
	return ""
}

func digString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	for _, v := range m {
		if nested, ok := v.(map[string]any); ok {
			if s := digString(nested, key); s != "" {
				return s
			}
		}
	}
	return ""
}
