package app

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/proving"
	"github.com/znasllc-io/memql/component/proving/figure"
	"github.com/znasllc-io/memql/component/proving/scenario"
	"github.com/znasllc-io/memql/component/work"
)

// proving_harness_gate_db_test.go -- the proving suite's learned-procedure
// harness takes its decision gates on a gate pool (memql#6022, F9 fix round 4).
//
// The harness (component/proving, procedure_harness.go) builds the work spine
// itself, outside app/, and cmd/memql-bench drives it in CI's `proving` lane.
// When the work spine's decision gates stopped falling back to the request pool,
// the harness kept building the spine without a gate pool, and every lifecycle
// that took a decision gate -- a step run again, a promotion a person approves --
// failed with "shared write coordination is unavailable". The lane's command
// only runs in CI; this runs the same harness, over the same engine shape, the
// same way, where the db-gated suites run.

// provingHarnessEngine opens an engine over the test database the way
// cmd/memql-bench's openEngine does: the bun handle beside it, no logic runner.
func provingHarnessEngine(t *testing.T) (*memql.MemQLEngine, *bun.DB) {
	t.Helper()
	dsn := dbtest.DSN()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	ping, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ping); err != nil {
		_ = db.Close()
		dbtest.Unreachable(t, "the proving harness", dsn, err)
		return nil, nil
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
	if err := e.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	return e, db
}

// provingScenario is one scenario of the committed corpus, by id.
func provingScenario(t *testing.T, id string) scenario.Scenario {
	t.Helper()
	corpus, err := scenario.Load(os.DirFS(filepath.Join("..")), "test/proving/scenarios")
	if err != nil {
		t.Fatalf("the committed corpus does not load: %v", err)
	}
	for _, s := range corpus.Scenarios {
		if s.Id == id {
			return s
		}
	}
	t.Fatalf("the committed corpus holds no scenario %q", id)
	return scenario.Scenario{}
}

func TestProvingHarnessDB(t *testing.T) {
	e, db := provingHarnessEngine(t)
	if e == nil {
		return
	}
	// The refusal precedes any lifecycle registration. Each case constructs its
	// own lifecycle and pool; only the loaded DSL and SQL handle are shared.
	t.Run("TestTheProvingHarnessRefusesToOpenWithoutAGatePool", func(t *testing.T) {
		testTheProvingHarnessRefusesToOpenWithoutAGatePool(t, e, db)
	})
	t.Run("TestTheProvingHarnessRunsALifecycleThatTakesADecisionGate", func(t *testing.T) {
		testTheProvingHarnessRunsALifecycleThatTakesADecisionGate(t, e, db)
	})
}

// TestTheProvingHarnessRunsALifecycleThatTakesADecisionGate: a person disliking
// a step and having it run again takes the goal's decision gate (rerunStep,
// "goal-lifecycle-"); a person approving a promotion takes the approval's
// (decideApproval). Both lifecycles run to a verdict through the harness, the
// way CI's proving lane runs them.
func testTheProvingHarnessRunsALifecycleThatTakesADecisionGate(t *testing.T, e *memql.MemQLEngine, db *bun.DB) {
	// The pool cmd/memql-bench opens beside its engine, as a node's database does.
	gates, err := database.NewGatePool(dbtest.DSN(), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gates.Close() })
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	runner := &proving.Runner{Engine: e, Logger: quiet, Lifecycle: proving.NewEngineLifecycle(e, db, gates, quiet)}
	for _, id := range []string{
		"amortizedCost.a-corrected-step-teaches-the-procedure",      // a step run again
		"amortizedCost.a-trusted-procedure-replays-without-a-model", // a promotion approved
	} {
		res := runner.RunScenario(context.Background(), provingScenario(t, id), figure.ArmPlatform)
		if res.Err != nil {
			t.Errorf("%s could not be run: %v", id, res.Err)
			continue
		}
		if !res.Passed {
			t.Errorf("%s ran and failed its verifier: %v", id, res.Failures)
		}
	}
	// The decisions were taken on the gate pool: it opened connections, and its
	// warm ones are still there.
	if st := gates.Stats(); st.OpenConnections+int(st.MaxIdleClosed) == 0 {
		t.Errorf("the gate pool opened no connection: the lifecycles took no decision gate on it (%+v)", st)
	}
}

// TestTheProvingHarnessRefusesToOpenWithoutAGatePool: a harness given no gate
// pool says so when a lifecycle opens -- before it writes the ladder's values --
// naming the wiring it lacks, instead of failing at the lifecycle's first
// decision with "shared write coordination is unavailable".
func testTheProvingHarnessRefusesToOpenWithoutAGatePool(t *testing.T, e *memql.MemQLEngine, db *bun.DB) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, err := proving.NewEngineLifecycle(e, db, nil, quiet).Open(context.Background(), "proving-no-gates", work.LadderPolicy{})
	if err == nil || !strings.Contains(err.Error(), "NewEngineLifecycle") || !strings.Contains(err.Error(), "database.NewGatePool") {
		t.Fatalf("a harness with no gate pool opened, or did not say which wiring it lacks: %v", err)
	}
}
