package automations

// journal_db_test.go -- the work journal against a real Postgres.
//
// The DB-free tests in journal_test.go assert the CALLS the journal renders.
// This asserts what the engine does with them, which is a different question
// and the one that has been wrong before: the mutations are @serverOnly, the
// concepts declare the composite owner tier, and the writer is a synthetic
// cluster actor whose stamped owner is blanked. Any of those three arranged
// wrongly produces a journal that is written and unreadable, or not written
// at all -- and every DB-free test in the package stays green either way,
// because a recording fake accepts whatever it is handed.
//
// Postgres-gated: skips cleanly when no DB is reachable, FAILS under
// MEMQL_REQUIRE_DB=1.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/database/dbtest"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// openTestEngine builds a real engine on dbtest.DSN(), the way the dry-run
// trust test does. The schema is applied by this package's TestMain through
// dbtest.EnsureSchema.
func openTestEngine(t *testing.T) *memql.MemQLEngine {
	t.Helper()
	dsn := dbtest.DSN()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	if err := db.PingContext(context.Background()); err != nil {
		dbtest.Unreachable(t, "work journal db test", dsn, err)
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
	if err := eng.Init(memoryNodes.DefaultRegistry()); err != nil {
		t.Fatalf("engine Init: %v", err)
	}
	return eng
}

// failOnceRegistry fails step "b" on its first execution and succeeds after,
// which is the shape a resume has to prove: the journal holds a done "a",
// a failed "b", and the resumed run re-runs "b" alone.
type failOnceRegistry struct{ failed bool }

func (r *failOnceRegistry) Execute(_ context.Context, step *Step, _ *StepContext) (*StepResult, error) {
	now := time.Now()
	if step.ID == "b" && !r.failed {
		r.failed = true
		return &StepResult{StepId: step.ID, Status: "failed", Error: "first time fails", StartedAt: now, CompletedAt: now}, errors.New("first time fails")
	}
	return &StepResult{StepId: step.ID, Status: "completed", Result: map[string]any{"ok": step.ID}, StartedAt: now, CompletedAt: now}, nil
}

func TestJournal_DB_RowsWrittenAndResumed(t *testing.T) {
	engine := openTestEngine(t)
	reg := &failOnceRegistry{}
	e := NewExecutor(ExecutorOptions{Engine: engine, StepRegistry: reg})
	defer e.Close()
	name := fmt.Sprintf("journalProbe%d", time.Now().UnixNano())
	auto := &Automation{Name: name, Steps: []*Step{
		{ID: "a", Type: StepTypeQuery, Query: &QueryStepConfig{Query: "q"}},
		{ID: "b", Type: StepTypeQuery, Query: &QueryStepConfig{Query: "q"}, OnError: ErrorStrategyStop},
	}}

	exec, _ := e.Execute(context.Background(), auto, "test")
	if exec.Status != "failed" {
		t.Fatalf("first run status = %q, want failed", exec.Status)
	}

	// The rows: one run at failed, a done step and an unfinished one.
	//
	// THIS READ IS THE POINT. It goes through workRunById / workStepsForRun,
	// which are @serverOnly and carry `actor.isClusterOwner==true`, against
	// rows whose owner the engine blanked because the writer was synthetic. If
	// the tier, the conjunct and the blanking do not agree, this comes back
	// EMPTY rather than erroring -- and a resume reading an empty journal
	// re-runs completed steps and calls it clean.
	journal, err := LoadRunJournal(context.Background(), engine, exec.ID)
	if err != nil {
		t.Fatalf("LoadRunJournal: %v", err)
	}
	if journal.AutomationName != name || journal.FailedStep != "b" {
		t.Fatalf("journal = %+v", journal)
	}
	if got := journal.Steps["a"]; got == nil || got.Status != "completed" {
		t.Fatalf("step a not journaled as done: %+v", got)
	}
	if journal.TemplateFingerprint == "" {
		t.Error("no template fingerprint on the run row, so ValidateRunJournal cannot refuse a changed automation")
	}

	// Resume re-runs only b, on the same run id.
	resumed, err := e.ResumeFrom(context.Background(), journal, auto, &ResumeOptions{})
	if err != nil {
		t.Fatalf("ResumeFrom: %v", err)
	}
	if resumed.ID != exec.ID {
		t.Fatalf("resumed run id = %s, want the original %s", resumed.ID, exec.ID)
	}
	if resumed.Status != "completed" {
		t.Fatalf("resumed status = %q", resumed.Status)
	}
	if resumed.Steps["a"] == nil {
		t.Error("step a was not rehydrated from the journal onto the resumed execution")
	}

	after, err := LoadRunJournal(context.Background(), engine, exec.ID)
	if err != nil {
		t.Fatalf("LoadRunJournal after resume: %v", err)
	}
	if after.FailedStep != "" {
		t.Fatalf("after resume the run still reports an unfinished step: %q -- the retry wrote no receipt, so the row is stuck at running", after.FailedStep)
	}
	if got := after.Steps["b"]; got == nil || got.Status != "completed" {
		t.Fatalf("step b did not reach done on the resumed run: %+v", got)
	}
}

// A step written at `running` whose node dies leaves no receipt. The journal
// must read that as the resume point, or a crash mid-step is unresumable --
// which is the case the checkpoint could not represent at all, since it was
// written only on an orderly failure.
func TestJournal_DB_AnUnfinishedStepIsResumable(t *testing.T) {
	engine := openTestEngine(t)
	runId := fmt.Sprintf("crashprobe%d", time.Now().UnixNano())
	j := newWorkJournal(engine, nil)
	auto := &Automation{Name: "crashProbe", Steps: []*Step{{ID: "a", Type: StepTypeQuery, Query: &QueryStepConfig{Query: "q"}}}}
	exec := NewExecution(auto.Name, "test")
	exec.ID = runId

	// Open the run and write the INTENT only -- no receipt, no close. That is
	// exactly the state a killed pod leaves behind.
	j.openRun(context.Background(), auto, exec, nil)
	j.stepRunning(context.Background(), exec, auto.Steps[0], 0, 1)

	journal, err := LoadRunJournal(context.Background(), engine, runId)
	if err != nil {
		t.Fatalf("LoadRunJournal: %v", err)
	}
	if journal.FailedStep != "a" {
		t.Fatalf("FailedStep = %q, want a", journal.FailedStep)
	}
	if err := ValidateRunJournal(journal, auto, fingerprintEngine); err != nil {
		t.Fatalf("a crashed run is not resumable: %v", err)
	}
}

func TestJournal_DB_SandboxWritesNothing(t *testing.T) {
	engine := openTestEngine(t)
	e := NewExecutor(ExecutorOptions{Engine: engine, StepRegistry: journalProbeRegistry{}, SandboxRun: true})
	defer e.Close()
	auto := &Automation{Name: fmt.Sprintf("sandboxProbe%d", time.Now().UnixNano()), Steps: []*Step{{ID: "a", Type: StepTypeQuery, Query: &QueryStepConfig{Query: "q"}}}}
	exec, _ := e.Execute(context.Background(), auto, "test")
	if _, err := LoadRunJournal(context.Background(), engine, exec.ID); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("a sandboxed run must leave no row; got %v", err)
	}
}
