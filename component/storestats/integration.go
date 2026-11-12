package storestats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// rollupConcept is the concept of the one node the rollup answers with, its
// report. Not a DSL concept: nothing stores it and no client reads it, which
// is how the email and website statistics rollups answer too.
const rollupConcept = "integration:storeStats:rollup"

// rollupPrincipal is the only caller the rollup admits: the maintenance
// principal its scheduled automation runs under
// (auth.MaintenanceActor("storeStatisticsRollupSweep")).
const rollupPrincipal = "system:maintenance:storeStatisticsRollupSweep"

// Engine is what the rollup needs of the engine: the store statistics reads,
// each a @serverOnly query, executed under walkContext.
type Engine interface {
	Execute(ctx context.Context, query string) (*memql.ExecuteResult, error)
}

// wholesaleState is one store's wholesale standing as a run reads it: whether
// the wholesale pack is enabled for it, the folded emails of its granted
// buyers, and a digest of the two that says when a store's wholesale split has
// to be judged again. Read in memory and never written, logged or returned.
type wholesaleState struct {
	Enabled bool
	Granted map[string]bool // folded emails
	Digest  string
}

// isWholesale reports whether an order whose email is this is wholesale in
// the store: never when the store is not enabled, nor for a blank address;
// otherwise whether the folded address is in the granted set. A method on
// the value, not a func, so the fold may call it inside a write transaction
// (the write gate in tx_test.go refuses a function value there).
func (s wholesaleState) isWholesale(email string) bool {
	if !s.Enabled {
		return false
	}
	folded := foldEmail(email)
	return folded != "" && s.Granted[folded]
}

// A RUN HAS A BUDGET, A STORE'S TURN HAS ONE, AND THE HARD DEADLINE IS MINUTES.
//
// runBudget paces a run: once it is spent, the run takes up no new store and
// ends normally (rollup.go, "One long store does not starve the others"). It
// is the schedule's period, a minute, less ten seconds for the store under
// way to finish and the run to log.
//
// turnBudget paces a store's turn: once it is spent, the turn asks no new
// page of any source and starts no new purge batch, and ends normally
// (rollup.go, "A turn has a budget"). A turn's work is then at most its
// budget plus the one unit under way when the budget ran out.
//
// txTimeout bounds every transaction of the package (tx.go): a page's write
// makes statements in proportion to its rows, so a count of statements cannot
// bound it and a time can.
//
// runDeadline is a safety bound for a run that hangs -- a read that never
// answers, a seam deaf to its context -- not the pacing. It cancels the run's
// context, and a run that has not answered runUnwind later is left behind,
// its hold released, so the next minute runs. It sits at or above
// runDeadlineBound, which a test holds at or below fifteen minutes: work
// added to a store's turn -- the wholesale pass and the periodic check come
// next -- asks turnSpent between its units, so that what is under way when
// the budget runs out stays one unit, rather than adding a term to a sum.
// Variables so a test can make a run outlast them.
var (
	runBudget   = 50 * time.Second
	turnBudget  = 30 * time.Second
	runDeadline = runDeadlineBound() + time.Minute
	runUnwind   = 5 * time.Second
)

// clockForTests is nil outside tests. When a test sets it, it is the clock the
// run's and the turn's budgets read (budgetNow), so a test drives a budget
// past its end without sleeping.
// TestNoProductionCodeSetsOnlyStoresForTests holds that only a test file sets
// it.
var clockForTests func() time.Time

// The bounds a run's hard deadline is worked out from.
const (
	// readBound is the longest one engine read can take: pgdriver's client
	// read deadline, ten seconds, which ends a statement before or when the
	// request pool's eight-second statement timeout does.
	readBound = 10 * time.Second
	// wholesaleReadsBound is the most reads wholesaleFor may make in a run
	// (Task 15's wholesaleOf pages two reads); more must be read across runs.
	wholesaleReadsBound = 10
	// unbudgetedTransactions are the transactions a store's turn makes that
	// no budget is asked between, counted from the code: its row
	// (ensureStore, 1); keepStore's purging read, its keeping and a purge
	// batch under way (3); its generation (1); the walk's purging read (1);
	// the floor's read and write (2); each source's watermark read (3); and
	// the walked mark (1).
	unbudgetedTransactions = 12
	// unbudgetedReads are the engine reads a turn makes that no budget is
	// asked between: the shop, the verdict and the floor's probes.
	unbudgetedReads = 2 + floorProbes
)

// runDeadlineBound is the most a run can take, its budgets spent, line by
// line:
//
//	the stores read:      maxStorePages (20) reads x readBound (10 s)       200 s
//	the wholesale read:   wholesaleReadsBound (10) x readBound (10 s)        100 s
//	the run's budget:     runBudget, after which no store is taken up         50 s
//	the turn's budget:    turnBudget, of the last store taken up              30 s
//	the unit under way:   the largest of
//	  one page            one read (10 s) + its transaction (txTimeout 20 s)   30 s
//	  one purge batch     its transaction (txTimeout 20 s), BEGIN to COMMIT    20 s
//	  the store's keeping unbudgetedTransactions (12) x txTimeout (20 s)
//	                      + unbudgetedReads (16) x readBound (10 s)          400 s
//	  a check's start     checkStartUnit: three transactions (20 s)           60 s
//	                                                               total     780 s
//
// The keeping is the largest unit: a turn asks no budget before its first
// page or batch, so when the budget is spent at the turn's start the whole of
// it runs, and a page or a batch is one transaction of it. The purge is
// counted as it runs: a turn can purge twice, a batch at a time, the budget
// asked before each, so one batch is under way at most; each transaction is
// timed BEGIN to COMMIT by txTimeout. The check a rebuild's end begins in the
// same turn (rollup.go, rollStore) asks turnSpent before its start and before
// each of its pages and batches, so it adds a unit to the list, its start,
// and no term to the sum. Thirteen minutes, and runDeadline adds a minute
// over it.
func runDeadlineBound() time.Duration {
	keeping := unbudgetedTransactions*txTimeout + unbudgetedReads*readBound
	unit := max(readBound+txTimeout, txTimeout, keeping, checkStartUnit())
	return maxStorePages*readBound + wholesaleReadsBound*readBound + runBudget + turnBudget + unit
}

// aRun is one run of the rollup on this node, held while it runs.
type aRun struct{ started time.Time }

// Integration exposes the rollup.
type Integration struct {
	engine Engine
	bunDB  func() *bun.DB
	logger *slog.Logger

	// now is the run's clock; nil is time.Now. A field so a test runs the
	// rollup at an instant of its own choosing.
	now func() time.Time

	// The seams the later parts of the walk are set through, each in
	// NewIntegration. Each is nil-safe: a nil storeState keeps every store, a
	// nil walkStore walks nothing, and a nil wholesaleFor means no store has
	// the wholesale pack enabled. A test sets its own.
	storeState   func(ctx context.Context, store map[string]any, now time.Time) (outcome string, err error)
	walkStore    func(ctx context.Context, store map[string]any, generation int64, now time.Time, wholesale wholesaleState, report *RollupReport) error
	wholesaleFor func(ctx context.Context, storeIDs []string) (map[string]wholesaleState, error)
	// verdictOf reads a store's verdict; NewIntegration sets it to askVerdict,
	// which asks the mirror's builtin (store.go). keepStore reads a nil one as
	// a verdict that cannot be read.
	verdictOf func(ctx context.Context, storeID string) (storeVerdict, error)
	// noted is what is wrong with each store that is logged when it begins
	// and when it ends (store.go).
	noted conditions

	// running is the run under way on this node, or nil: one rollup per
	// process, the email statistics rollup's guard. A run slower than the
	// schedule must not have the next one start beside it and walk the same
	// stores again. Two nodes need no such guard, because every write takes
	// the one lock and checks the store's generation (tx.go).
	runningMu sync.Mutex
	running   *aRun
	// lastTaken is the store the last run on this node took up last: the
	// next run begins with the store after it (rollup.go, "One long store
	// does not starve the others"). In memory, under runningMu.
	lastTaken string
	// checkLastTaken is lastTaken for the check's runs (check.go, runCheck).
	checkLastTaken string
}

// NewIntegration wires the handles. The db getter is resolved at call time,
// not at construction: a node's database handle is not necessarily up when
// providers are materialized.
func NewIntegration(engine Engine, bunDB func() *bun.DB, logger *slog.Logger) *Integration {
	if logger == nil {
		logger = slog.Default()
	}
	i := &Integration{
		engine: engine,
		bunDB:  bunDB,
		logger: logger,
	}
	// One seam a line, each named for the task that sets it.
	i.storeState = i.keepStore // Task 14: what is kept about each store (store.go)
	i.verdictOf = i.askVerdict // Task 14: a store's verdict, asked of the mirror's builtin (store.go)

	i.walkStore = i.walkOneStore // Task 13: the fold and the derivation (fold.go)
	return i
}

// IntegrationName implements memql.IntegrationProvider.
func (i *Integration) IntegrationName() string { return integrationName }

// Capabilities implements memql.IntegrationProvider.
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "rollup",
			Description: "Keep every store's statistics: list every configured store through the engine, give each its row in the store statistics tables, and walk each in turn, one store's failure leaving the next walked. Answers counts and store ids only. Admits only the storeStatisticsRollupSweep maintenance principal, at internal origin.",
			Handler:     i.handleRollup,
		},
		// Task 16: the check, the rebuild and the check's state (check.go).
		{
			Name:        "check",
			Description: "Carry every store's check pass on: walk each caught-up store's mirrored orders, lines and refunds again from its floor, compare each fact with what its row gives now by version and by content, write again what differs, and at the end of a pass delete the facts no row answers for. Answers counts and store ids only. Admits only the storeStatisticsCheckSweep maintenance principal, at internal origin.",
			Handler:     i.handleCheck,
		},
		{
			Name:        "rebuild",
			Description: "Rebuild one store's statistics: raise its generation, mark it rebuilding and delete its watermarks and check row, so the walk deletes its facts in batches and refills them, and a check begins when it has caught up. Answers {storeId, rebuildingSince}. For a person who is a cluster owner.",
			Handler:     i.handleRebuild,
			ArgsSchema:  map[string]string{"storeId": "string - the store, bare or v1:shopify:store:<id>"},
		},
		{
			Name:        "checkState",
			Description: "One store's check: when the pass under way began, when the last pass began and ended, when one last ended clean, and the last ended pass's compared, mismatched, repaired and disagreeing counts. For a person who is a cluster owner.",
			Handler:     i.handleCheckState,
			ArgsSchema:  map[string]string{"storeId": "string - the store, bare or v1:shopify:store:<id>"},
		},
	}
}

// errNotThePrincipal is the one refusal, for every caller but the engine's.
var errNotThePrincipal = errors.New("store statistics rollup requires the engine maintenance principal")

// handleRollup runs the rollup for the scheduled automation, and for nobody
// else, and answers one node with its report.
//
// THE PRINCIPAL, AT INTERNAL ORIGIN, AND NOTHING ELSE. The rollup reads every
// store's mirror and spends the database's time on a schedule; a cluster
// owner has no reason to run it by hand. So the check is the automation's own
// maintenance id together with the cluster-owner role that principal carries,
// and the internal origin the automation runner gives the engine's own
// automations: the id with any other role, any other automation's principal,
// a cluster owner by hand, and the principal reached from a client are each
// refused, with one sentence, before anything is read or written.
//
// A SECOND RUN IN THIS PROCESS STANDS DOWN: it answers no node and no error,
// and says so at Info with the time the running one started, so a run that
// holds the node for minute after minute is seen. A run has a deadline of its
// own (runDeadline).
//
// THE REPORT IS COUNTS. The engine's work journal keeps what an automation
// step answers, and the store statistics reads carry a buyer's email on two
// of their shapes, so nothing this answers is drawn from a row but the store
// ids its lists name.
func (i *Integration) handleRollup(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	actor, ok := auth.AccessFromContext(ctx)
	if !ok || actor == nil || actor.UserId != rollupPrincipal || !actor.IsClusterOwner() || !auth.OriginFromContext(ctx).IsInternal() {
		return nil, errNotThePrincipal
	}
	run, standing := i.startRun()
	if run == nil {
		i.logger.Info("Store statistics rollup stood down: a run is still under way on this node",
			"component", "storestats", "startedAt", standing.started.UTC().Format(time.RFC3339Nano))
		return nil, nil
	}

	now := time.Now()
	if i.now != nil {
		now = i.now()
	}
	rep, err := i.runWithin(ctx, run, now)
	// A routine run is the same line every minute, so it is Debug; a store
	// behind, rebuilt or removed is worth a line at Info. Each store's failure
	// has its own Warn (rollup.go).
	level := slog.LevelDebug
	if len(rep.Behind) > 0 || len(rep.Rebuilt) > 0 || len(rep.Removed) > 0 {
		level = slog.LevelInfo
	}
	i.logger.Log(ctx, level, "Store statistics rolled up",
		"component", "storestats", "stores", rep.Stores, "rows", rep.Rows, "facts", rep.Facts,
		"unreadable", rep.Unreadable, "behind", rep.Behind, "rebuilt", rep.Rebuilt, "removed", rep.Removed)
	if err != nil {
		return nil, err
	}

	payload, err := json.Marshal(rep)
	if err != nil {
		return nil, fmt.Errorf("store statistics rollup: encode the report: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        "storeStatisticsRollup",
		Concept:   rollupConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: now.UTC(),
		Payload:   payload,
	}}, nil
}

// startRun takes the node's one run, or answers the run that holds it.
func (i *Integration) startRun() (run, standing *aRun) {
	i.runningMu.Lock()
	defer i.runningMu.Unlock()
	if i.running != nil {
		return nil, i.running
	}
	i.running = &aRun{started: time.Now()}
	return i.running, nil
}

// endRun gives the node's run back, when it is still this one's.
func (i *Integration) endRun(run *aRun) {
	i.runningMu.Lock()
	defer i.runningMu.Unlock()
	if i.running == run {
		i.running = nil
	}
}

// runWithin runs the rollup under its deadline. See runDeadline.
func (i *Integration) runWithin(ctx context.Context, run *aRun, now time.Time) (RollupReport, error) {
	ctx, cancel := context.WithTimeout(ctx, runDeadline)
	type result struct {
		rep RollupReport
		err error
	}
	done := make(chan result, 1)
	go func() {
		// The run's context is cancelled once it has answered, or once it is
		// left behind: never before its deadline.
		defer cancel()
		rep, err := runRollup(ctx, i, now)
		done <- result{rep, err}
	}()
	select {
	case r := <-done:
		i.endRun(run)
		return r.rep, r.err
	case <-ctx.Done():
	}
	select {
	case r := <-done:
		i.endRun(run)
		return r.rep, r.err
	case <-time.After(runUnwind):
	}
	// Left behind: the next minute runs. What the stuck run still writes
	// goes through writeTx like any write, under the lock and the store's
	// generation.
	i.endRun(run)
	i.logger.Warn("Store statistics rollup did not end within its deadline; it was cancelled and the next run will not wait for it",
		"component", "storestats", "deadline", runDeadline.String(), "startedAt", run.started.UTC().Format(time.RFC3339Nano))
	return RollupReport{Rows: map[string]int{}}, fmt.Errorf("store statistics rollup: the run did not end within %s, and was cancelled", runDeadline)
}
