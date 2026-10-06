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
	"io/fs"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/database/dbtest"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// openTestEngine builds a PRIVATE engine on dbtest.DSN(), the way the dry-run
// trust test does. The schema is applied by this package's TestMain through
// dbtest.EnsureSchema.
//
// Private is for a test that changes the engine's own state: installs a logic
// runner (SetLogicRunner) or registers a function on it (registerLogic). A
// test that only writes and reads rows through a stock engine borrows
// sharedJournalEngine instead, because each boot here is a full
// LoadUnifiedConcepts + New + Init (memql#5668).
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

// THE ENGINE-BORROWING HALF (memql#5668), memql#4075's split applied here.
//
// Every db-gated test in this package booted its own engine, and the boot is
// most of what such a test costs: about 0.8s of each journal test's 1s on a
// developer machine, fourteen times per run, in a package the db-tests lane
// gives 180s. The tests that only write and read rows -- open a run, fail a
// step, read the journal back, resume -- leave the engine as they found it,
// so they share ONE, booted by whichever of them runs first. The tests that
// change it keep booting privately through openTestEngine.
//
// WHY A PACKAGE-LEVEL ONCE IS SAFE HERE, when component/memql learned that one
// can leak a mounted fixture into every test that walks memorynodes.All().
// That leak needs the shared engine to boot WHILE a fixture is mounted, and
// then outlive the fixture's restore. This package does mount fixtures -- the
// fleet bundle (fleet_pricelist_db_test.go, fleet_billing_filters_test.go,
// loop_tree_measure_test.go) and throwaway DSL domains
// (strict_automation_boot_test.go, block_comment_automation_test.go,
// edition_frontend_test.go) -- but each of those boots its own engine or none,
// restores the global registries in its own t.Cleanup, and no test in the
// package calls t.Parallel, so a mount and a borrow never overlap. Nothing
// that borrows mounts anything. And rather than leave that resting on this
// paragraph, the boot refuses when the tree or the concept registry differs
// from a pristine one, and every borrow re-checks both, so the leak fails the
// test that would have caused it instead of a stranger later.
var sharedJournalEngineState struct {
	once sync.Once
	// boots counts Once-body executions, so
	// TestSharedJournalEngine_SharesOneBoot asserts the sharing as a NUMBER.
	// A regression to a boot per borrower would still hand every test a
	// working engine, and would show only as the lane creeping back toward its
	// budget, blamed on whoever commits next (memql#3257).
	boots int
	eng   *memql.MemQLEngine
	dsn   string
	// domains and concepts are the global state the engine booted against; a
	// borrow that reads different ones is running beside a mounted fixture.
	domains, concepts []string
	// pingErr records "no database answered", the skip-or-fail case; bootErr
	// "it answered and the engine did not come up", always a failure. The
	// Once body never touches a *testing.T, so the first borrower's t cannot
	// answer for the package and hand everyone after it a nil engine.
	pingErr error
	bootErr error
}

// pristineDomains is the DSL tree's top-level domains as this package starts:
// the embedded ones plus whatever an imported package registered from init().
// A package-level variable is initialized after every import's init() and
// before TestMain, so no test has mounted anything yet.
var pristineDomains = treeDomains()

// treeDomains lists the top-level domains of the tree every loader walks,
// sorted.
func treeDomains() []string {
	entries, err := fs.ReadDir(memqldsl.Tree(), ".")
	if err != nil {
		return []string{"unreadable: " + err.Error()}
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// conceptNames lists the process-wide concept registry, sorted.
func conceptNames() []string {
	names := make([]string, 0, 512)
	for name := range memoryNodes.All() {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// drift names what now holds and was did not (+name), and the reverse
// (-name); "" when the two agree.
func drift(was, now []string) string {
	delta := map[string]int{}
	for _, n := range was {
		delta[n]--
	}
	for _, n := range now {
		delta[n]++
	}
	var out []string
	for n, d := range delta {
		switch {
		case d > 0:
			out = append(out, "+"+n)
		case d < 0:
			out = append(out, "-"+n)
		}
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

// sharedJournalEngine returns the package's shared engine, booting it on first
// use. A borrower writes and reads rows and changes nothing on the engine; a
// test that installs a logic runner, registers a function or mounts a fixture
// boots its own through openTestEngine. Each call gets the same engine and the
// pool is never closed: with one pool for the package there is nothing to
// crowd max_connections with (memql#3670), so it lives until the process
// exits.
func sharedJournalEngine(t *testing.T) *memql.MemQLEngine {
	t.Helper()
	s := &sharedJournalEngineState
	s.once.Do(func() {
		s.boots++
		s.dsn = dbtest.DSN()
		db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(s.dsn))), pgdialect.New())
		if err := db.PingContext(context.Background()); err != nil {
			s.pingErr = err
			_ = db.Close()
			return
		}
		fail := func(err error) {
			s.bootErr = err
			_ = db.Close()
		}
		if d := drift(pristineDomains, treeDomains()); d != "" {
			fail(fmt.Errorf("the DSL tree's domains differ from the package's start (%s): a fixture is mounted, and "+
				"the shared engine would load it and keep it after the fixture's test restores the tree. A test that "+
				"mounts a fixture boots its own engine through openTestEngine", d))
			return
		}
		n, err := memql.LoadUnifiedConcepts(nil)
		if err != nil {
			fail(fmt.Errorf("LoadUnifiedConcepts: %w", err))
			return
		}
		if held := len(memoryNodes.All()); held != n {
			fail(fmt.Errorf("the concept registry holds %d concepts and the tree declares %d: an earlier fixture's "+
				"concepts outlived its test, and the shared engine would boot against them", held, n))
			return
		}
		eng, err := memql.New(db)
		if err != nil {
			fail(fmt.Errorf("memql.New: %w", err))
			return
		}
		eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
		if err := eng.Init(memoryNodes.DefaultRegistry()); err != nil {
			fail(fmt.Errorf("engine Init: %w", err))
			return
		}
		s.eng, s.domains, s.concepts = eng, treeDomains(), conceptNames()
	})
	if s.pingErr != nil {
		dbtest.Unreachable(t, "work journal db test (shared engine)", s.dsn, s.pingErr)
		return nil // Unreachable skipped or failed; only a non-stdlib TB reaches this
	}
	if s.bootErr != nil {
		t.Fatalf("the shared journal engine did not boot (recorded on first use, reported per borrower): %v", s.bootErr)
	}
	if d, c := drift(s.domains, treeDomains()), drift(s.concepts, conceptNames()); d != "" || c != "" {
		t.Fatalf("this test borrows the shared journal engine beside a mounted fixture -- a t.Parallel test, or a "+
			"mount that did not restore in its t.Cleanup. Since the engine booted, the DSL domains drifted by [%s] and "+
			"the registered concepts by [%s]", d, c)
	}
	return s.eng
}

// TestSharedJournalEngine_SharesOneBoot pins the mechanism memql#5668 buys its
// time with: three borrows hand back one engine, and the process booted it
// exactly once, whichever borrower ran first.
func TestSharedJournalEngine_SharesOneBoot(t *testing.T) {
	first := sharedJournalEngine(t)
	for i := 0; i < 2; i++ {
		if eng := sharedJournalEngine(t); eng != first {
			t.Fatalf("borrow %d returned a different engine: the borrowers are not sharing one", i+2)
		}
	}
	if got := sharedJournalEngineState.boots; got != 1 {
		t.Fatalf("the shared journal engine booted %d times in this process, want exactly 1: each boot is a full "+
			"LoadUnifiedConcepts + New + Init, and one per borrower is the cost memql#5668 removed", got)
	}
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
	engine := sharedJournalEngine(t)
	reg := &failOnceRegistry{}
	e := NewExecutor(ExecutorOptions{Engine: engine, StepRegistry: reg})
	defer e.Close()
	name := fmt.Sprintf("journalProbe%d", time.Now().UnixNano())
	auto := &Automation{Name: name, Steps: []*Step{
		{ID: "a", Type: StepTypeFunction, Function: &FunctionStepConfig{Name: "q", Kind: "query"}},
		{ID: "b", Type: StepTypeFunction, Function: &FunctionStepConfig{Name: "q", Kind: "query"}, OnError: ErrorStrategyStop},
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
	resumed, err := e.ResumeFrom(context.Background(), journal, auto, &ResumeOptions{AllowSideEffects: true})
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
	engine := sharedJournalEngine(t)
	runId := fmt.Sprintf("crashprobe%d", time.Now().UnixNano())
	j := newWorkJournal(engine, nil)
	auto := &Automation{Name: "crashProbe", Steps: []*Step{{ID: "a", Type: StepTypeFunction, Function: &FunctionStepConfig{Name: "q", Kind: "query"}}}}
	exec := NewExecution(auto.Name, "test")
	exec.ID = runId

	// Open the run and write the INTENT only -- no receipt, no close. That is
	// exactly the state a killed pod leaves behind.
	j.openRun(context.Background(), auto, exec, nil, events.Cause{})
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
	engine := sharedJournalEngine(t)
	e := NewExecutor(ExecutorOptions{Engine: engine, StepRegistry: journalProbeRegistry{}, SandboxRun: true})
	defer e.Close()
	auto := &Automation{Name: fmt.Sprintf("sandboxProbe%d", time.Now().UnixNano()), Steps: []*Step{{ID: "a", Type: StepTypeFunction, Function: &FunctionStepConfig{Name: "q", Kind: "query"}}}}
	exec, _ := e.Execute(context.Background(), auto, "test")
	if _, err := LoadRunJournal(context.Background(), engine, exec.ID); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("a sandboxed run must leave no row; got %v", err)
	}
}
