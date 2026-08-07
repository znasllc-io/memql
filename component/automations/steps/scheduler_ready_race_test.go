package steps

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/database/dbtest"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
)

// TestSchedulerReadyMeansAutomationsRegistered is the #2648 root-cause
// regression, asserted through the SAME public call that flaked in CI.
//
// Scheduler.Start used to close readyCh BEFORE `go s.run(ctx)`, so
// Ready() meant "Start was called", not "automations are registered".
// Every caller that waited on Ready() and immediately triggered an
// automation raced the loader goroutine -- and lost whenever the
// goroutine had not been scheduled yet. That is the db-tests lane
// flake: three packages competing for CPU widened the window until
// `automation "accountDeletionSweep" not found` surfaced on diffs that
// touched no automation code, while passing locally every time.
//
// The assertion is the invariant, not a timing hope: the instant
// Ready() fires, a known automation must resolve. No sleeps, no
// polling, no retries -- under the pre-fix code this fails whenever the
// scheduler goroutine has not finished loading, which is precisely the
// CI condition; under the fix it cannot fail.
func TestSchedulerReadyMeansAutomationsRegistered(t *testing.T) {
	dsn := dbtest.DSN()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	if err := db.PingContext(context.Background()); err != nil {
		dbtest.Unreachable(t, "scheduler-ready race test", dsn, err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	eng, err := memql.New(db)
	if err != nil {
		t.Fatalf("memql.New: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(concept.DefaultRegistry()); err != nil {
		t.Fatalf("engine Init: %v", err)
	}

	stepReg := NewRegistry()
	eng.SetLogicRunner(automations.NewLogicRunner(eng, stepReg, eng.Logger))

	sched, err := automations.NewScheduler(automations.SchedulerOptions{
		Logger:       eng.Logger,
		Loader:       automations.NewLoader(automations.LoaderOptions{Logger: eng.Logger}),
		Engine:       eng,
		EventBus:     events.NewBus(),
		StepRegistry: stepReg,
	})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sched.Start(ctx)

	select {
	case <-sched.Ready():
	case <-time.After(30 * time.Second):
		t.Fatal("scheduler did not become ready within 30s")
	}

	// The instant readiness is signalled, the automation the CI flake
	// could not find must resolve.
	if _, err := sched.TriggerAutomationWithEvent(ctx, "accountDeletionSweep", nil); err != nil {
		if strings.Contains(err.Error(), "not found") {
			t.Fatalf("Ready() fired before automation registration completed (#2648): %v", err)
		}
		// Any OTHER error means the automation WAS registered and merely
		// failed to run in this bare harness, which this test does not
		// assert on -- registration is the invariant under test.
		t.Logf("automation resolved; execution error (not under test): %v", err)
	}
}
