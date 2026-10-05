package pipelinerun

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/identity/githubconnect"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/workjournal"
	"github.com/znasllc-io/memql/core/id"
)

// Separate drivers and advisory-lock connections share only persisted rows.
// A new process keeps its pod's node name; the token must still distinguish it.
func TestClaimFencingPersistsAcrossDriversOverRealRows(t *testing.T) {
	eng := dbEngine(t)
	store := NewDSLStore(eng)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := testPipeline(DeliveryWebhook)
	suffix := id.NewShortId()
	p.ID, p.OwnerUserID = "fence-p-"+suffix, "fence-owner-"+suffix
	run := queuedRun(p, shaA)
	if err := store.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	newDriver := func() *runDriver {
		pool := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
		t.Cleanup(func() { _ = pool.Close() })
		integ := New(Deps{
			Store: NewDSLStore(eng), NodeID: "fence-agent-" + suffix,
			Now: func() time.Time { return testNow },
			Gate: watchedGate(t, func(ctx context.Context, key string, fn func(context.Context) error) error {
				return githubconnect.WithGate(ctx, pool, key, fn)
			}),
			Journal: workjournal.New(workjournal.ExecutorFunc(func(ctx context.Context, q string) (any, error) { return eng.Execute(ctx, q) }), nil, "fence-agent-"+suffix),
		})
		return claimedTestDriver(t, integ, run.ID)
	}
	old := newDriver()
	work, err := old.d.Journal.Begin(ctx, workjournal.Work{
		OwnerUserID: p.OwnerUserID, Template: "fence-proof", GoalKey: run.ID,
		Statement: "Prove receipt ownership", QueueSteps: true,
		Steps: []workjournal.StepDecl{{Key: "build"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := work.Step(ctx, "build")
	if err != nil {
		t.Fatal(err)
	}
	next := newDriver()
	if next.claimID == "" || next.claimID == old.claimID {
		t.Fatal("claim token was not replaced")
	}
	durable := mustRun(t, store, run.ID)
	if durable.DriverLeaseID != next.claimID {
		t.Fatal("token was not stored")
	}
	replacement := next.d.Journal.Reopen(p.OwnerUserID, work.GoalID(), work.RunID(), []workjournal.StepDecl{{Key: "build"}}, testNow)
	step, err := replacement.Step(ctx, "build")
	if err != nil {
		t.Fatal(err)
	}
	if err := step.Finish(ctx, workjournal.Receipt{Status: "done", Result: map[string]any{"owner": "replacement"}}); err != nil {
		t.Fatal(err)
	}
	if err := pending.Finish(ctx, workjournal.Receipt{Status: "failed", Code: "obsolete"}); !errors.Is(err, errLeaseLost) {
		t.Fatalf("stale receipt: %v", err)
	}
	work.Heartbeat(ctx)
	if err := work.Failed(ctx, "obsolete", "old process returned"); !errors.Is(err, errLeaseLost) {
		t.Fatalf("stale terminal: %v", err)
	}
	if err := replacement.Succeeded(ctx, nil); err != nil {
		t.Fatal(err)
	}
	rows, err := store.WorkSteps(memqlengine.ContextWithFreshRead(ctx), work.RunID())
	if err != nil || len(rows) != 1 || rows[0].Status != "done" {
		t.Fatalf("replacement receipt overwritten: %+v %v", rows, err)
	}
	if got := workRunRow(t, store, work.RunID()); got["status"] != "succeeded" {
		t.Fatalf("replacement run overwritten: %v", got["status"])
	}
}
