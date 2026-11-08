package pipelinerun

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/identity/githubconnect"
	"github.com/znasllc-io/memql/component/workjournal"
	"github.com/znasllc-io/memql/core/id"
)

// work_run_liveness_db_test.go -- a pipeline's work run is not beaten
// (guardJournal, workjournal's WithRunnerOwnedLiveness), counted in stored
// versions rather than rendered calls.
//
// Before 2026-11-08 every lease renewal appended a version of the v1:work:run
// row (heartbeatAt and nodeId), and the journal's own goroutine appended one
// every 20 s besides: on production on 2026-11-06, 6,912 versions of 25 runs in
// a day. Here ten renewals on the opening replica add no version of the work
// run, a takeover by another replica adds exactly one (the node now driving
// it), and ten renewals after it add none -- while the pipelines run row, whose
// driverHeartbeatAt IS the lease, still takes one version per renewal.
func TestLeaseRenewalsWriteNoWorkRunVersionsOverRealRows(t *testing.T) {
	eng := dbEngine(t)
	store := NewDSLStore(eng)
	ctx := context.Background()
	pool := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	t.Cleanup(func() { _ = pool.Close() })

	p := testPipeline(DeliveryWebhook)
	suffix := id.NewShortId()
	p.ID, p.OwnerUserID = "liveness-p-"+suffix, "liveness-owner-"+suffix
	run := queuedRun(p, shaA)
	if err := store.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	driverOn := func(node string, now time.Time) *runDriver {
		integ := New(Deps{
			Store: NewDSLStore(eng), NodeID: node,
			Now: func() time.Time { return now },
			Gate: watchedGate(t, func(ctx context.Context, key string, fn func(context.Context) error) error {
				return githubconnect.WithGate(ctx, pool, key, fn)
			}),
			Journal: workjournal.New(workjournal.ExecutorFunc(func(ctx context.Context, q string) (any, error) { return eng.Execute(ctx, q) }), nil, node),
		})
		return claimedTestDriver(t, integ, run.ID)
	}
	versions := func(concept, rowID string) int {
		t.Helper()
		var n int
		if err := pool.QueryRowContext(ctx, `SELECT count(*) FROM "MemoryNodes" WHERE id = $1`, concept+":"+bareID(rowID)).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	renewTen := func(dr *runDriver) {
		t.Helper()
		for range 10 {
			if !dr.renew(ctx) {
				t.Fatal("a renewal found nothing to renew")
			}
		}
	}

	opener := driverOn("liveness-agent-a-"+suffix, testNow)
	work, err := opener.d.Journal.Begin(ctx, workjournal.Work{
		OwnerUserID: p.OwnerUserID, Template: "liveness-proof", GoalKey: run.ID, RunKey: "1",
		Statement: "Prove the lease is the liveness", QueueSteps: true, TriggeredBy: "pipeline:affected",
		Steps: []workjournal.StepDecl{{Key: "build", StepType: "exec"}},
	})
	if err != nil || work == nil {
		t.Fatalf("Begin: %v", err)
	}
	opener.workRun.Store(work)
	if got := workRunRow(t, store, work.RunID()); got["heartbeatAt"] != nil {
		t.Errorf("the work run opened with a heartbeat nothing judges: %v", got["heartbeatAt"])
	}
	opened, leaseBefore := versions("v1:work:run", work.RunID()), versions(RunConcept, run.ID)
	renewTen(opener)
	if got := versions("v1:work:run", work.RunID()); got != opened {
		t.Fatalf("ten renewals added %d version(s) of the work run, want none", got-opened)
	}
	if got := versions(RunConcept, run.ID); got != leaseBefore+10 {
		t.Fatalf("the lease itself was renewed %d time(s), want 10 -- the renewal must still write driverHeartbeatAt", got-leaseBefore)
	}

	// agent-a goes silent; agent-b claims the stale lease and takes the
	// work run over, as recovery does (openWork).
	taker := driverOn("liveness-agent-b-"+suffix, testNow.Add(3*time.Minute))
	taker.reopened(ctx, taker.d.Journal.Reopen(p.OwnerUserID, work.GoalID(), work.RunID(), []workjournal.StepDecl{{Key: "build", StepType: "exec"}}, testNow))
	if got := versions("v1:work:run", work.RunID()); got != opened+1 {
		t.Fatalf("a takeover added %d version(s) of the work run, want exactly 1 (its node)", got-opened)
	}
	if got := workRunRow(t, store, work.RunID()); got["nodeId"] != "liveness-agent-b-"+suffix || got["heartbeatAt"] != nil {
		t.Fatalf("after the takeover the work run says node %v, heartbeat %v", got["nodeId"], got["heartbeatAt"])
	}
	renewTen(taker)
	if got := versions("v1:work:run", work.RunID()); got != opened+1 {
		t.Fatalf("renewals after the takeover added %d version(s) of the work run, want none", got-opened-1)
	}
	t.Logf("work run versions: %d at open, %d after 20 renewals and a takeover (formerly one per renewal plus one per 20 s)", opened, versions("v1:work:run", work.RunID()))
}
