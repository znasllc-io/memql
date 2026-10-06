package pipelinerun

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/identity/githubconnect"
	"github.com/znasllc-io/memql/component/workjournal"
	"github.com/znasllc-io/memql/core/id"
)

func TestLostCoordinationConnectionCannotCommitAnOldReceipt(t *testing.T) {
	eng := dbEngine(t)
	store := NewDSLStore(eng)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	suffix := id.NewShortId()
	p := testPipeline(DeliveryWebhook)
	p.ID = "disconnect-p-" + suffix
	p.OwnerUserID = "disconnect-owner-" + suffix
	run := queuedRun(p, shaA)
	if err := store.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	observer := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	defer observer.Close()
	paused, resume := make(chan struct{}), make(chan struct{})
	defer close(resume)
	var intercept atomic.Bool
	oldApplication := "old-fence-" + suffix
	driver := func(application string, old bool) *runDriver {
		pool := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()), pgdriver.WithApplicationName(application)))
		t.Cleanup(func() { _ = pool.Close() })
		pool.SetMaxOpenConns(1)
		journal := workjournal.New(workjournal.ExecutorFunc(func(wctx context.Context, q string) (any, error) {
			if old && intercept.Load() && strings.Contains(q, `errorCode: "obsolete"`) {
				close(paused)
				select {
				case <-resume:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return eng.Execute(wctx, q)
		}), nil, "same-pod")
		integ := New(Deps{Store: NewDSLStore(eng), NodeID: "same-pod", Journal: journal,
			Gate: func(gctx context.Context, key string, fn func(context.Context) error) error {
				return githubconnect.WithGate(gctx, pool, key, fn)
			},
			Now: func() time.Time { return testNow },
		})
		return claimedTestDriver(t, integ, run.ID)
	}
	old := driver(oldApplication, true)
	work, err := old.d.Journal.Begin(ctx, workjournal.Work{OwnerUserID: p.OwnerUserID, Template: "disconnect-proof", GoalKey: run.ID, Statement: "Prove a disconnected lock cannot authorize a receipt", QueueSteps: true, Steps: []workjournal.StepDecl{{Key: "build"}}})
	if err != nil {
		t.Fatal(err)
	}
	step, err := work.Step(ctx, "build")
	if err != nil {
		t.Fatal(err)
	}
	intercept.Store(true)
	result := make(chan error, 1)
	go func() { result <- step.Finish(ctx, workjournal.Receipt{Status: "failed", Code: "obsolete"}) }()
	select {
	case <-paused:
	case <-ctx.Done():
		t.Fatal("old receipt never paused under its gate")
	}
	var terminated bool
	if err := observer.QueryRowContext(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name=$1`, oldApplication).Scan(&terminated); err != nil || !terminated {
		t.Fatalf("terminate the test's lock session: %v %v", terminated, err)
	}
	next := driver("replacement-fence-"+suffix, false)
	replacement := next.d.Journal.Reopen(p.OwnerUserID, work.GoalID(), work.RunID(), []workjournal.StepDecl{{Key: "build"}}, testNow)
	current, err := replacement.Step(ctx, "build")
	if err != nil {
		t.Fatal(err)
	}
	if err := current.Finish(ctx, workjournal.Receipt{Status: "done"}); err != nil {
		t.Fatal(err)
	}
	resume <- struct{}{}
	select {
	case err := <-result:
		if !errors.Is(err, errLeaseLost) {
			t.Fatalf("old receipt returned %v", err)
		}
	case <-ctx.Done():
		t.Fatal("old writer did not finish")
	}
	rows, err := store.WorkSteps(ctx, work.RunID())
	if err != nil || len(rows) != 1 || rows[0].Status != "done" {
		t.Fatalf("disconnected writer overwrote replacement: %+v %v", rows, err)
	}
	if err := replacement.Succeeded(ctx, nil); err != nil {
		t.Fatal(err)
	}
}
