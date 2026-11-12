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

// A RUN HAS A BUDGET, AND A HARD DEADLINE ABOVE IT.
//
// runBudget paces a run: once it is spent, the run takes up no new store and
// ends normally (rollup.go, "One long store does not starve the others"). It
// is the schedule's period, a minute, less a margin of ten seconds for the
// store under way to finish its own work and the run to log, so the next
// minute's tick seldom finds the run still going; when it does, that tick
// stands down at Info.
//
// runDeadline is a safety bound for a run that hangs -- a read that never
// answers, a seam deaf to its context -- not the pacing. It cancels the run's
// context, and a run that has not answered runUnwind later is left behind,
// its hold released, so the next minute runs. It sits above the budget plus
// the most one store's turn can take from the limits the walk and the
// housekeeping keep (storeTurnBound), so a store under way is never cut off
// by it. Variables so a test can make a run outlast them.
var (
	runBudget   = 50 * time.Second
	runDeadline = runBudget + storeTurnBound() + time.Minute
	runUnwind   = 5 * time.Second
)

// The limits a store's turn is bounded by, beyond those walk.go and store.go
// name.
const (
	// statementBound is the longest one statement can take: pgdriver's client
	// read deadline, ten seconds, which ends a statement before or when the
	// request pool's eight-second statement timeout does.
	statementBound = 10 * time.Second
	// walkSourcesPerStore are the sources a store's turn walks: orders, lines
	// and refunds (walk.go). A task that adds a walk to a store's turn -- the
	// wholesale pass, the check -- raises this or adds its own term below.
	walkSourcesPerStore = 3
	// foldStatementsPerPage is an allowance for the statements one page's
	// write transaction makes -- the lock, the generation, the facts, their
	// derivation, the watermark and the walked mark -- with room over. The
	// fold sets the real count here when it lands.
	foldStatementsPerPage = 8
	// housekeepingStatements are a store's own statements outside its walk:
	// its row (two), what is kept about it (eight), its generation (one), the
	// end of a rebuild (three), and its floor's read and write (four), with
	// room over.
	housekeepingStatements = 24
)

// storeTurnBound is the most one store's turn can take, from its limits:
//
//	engine reads:  floorProbes (14) + the shop and the verdict (2)
//	               + walkSourcesPerStore (3) x maxReadsPerRun (400)       = 1,216
//	statements:    walkSourcesPerStore (3) x maxReadsPerRun (400)
//	                 x foldStatementsPerPage (8)                          = 9,600
//	               + purgeBudget (20) x (the lock, the purging read, one
//	                 delete for each of the 4 fact tables, the clear: 7)  =   140
//	               + housekeepingStatements                               =    24
//	                                                              total  10,980
//
// each at most statementBound (10 s): 109,800 s, about 30.5 hours. The
// verdict is one builtin call, counted as one read, though it reads more. It
// is not what a turn takes: a read is milliseconds, and a page that outlasts
// a read ends its source for the run (walk.go). It is the bound the run's
// hard deadline must sit above so that it never cuts off a store the limits
// still allow.
func storeTurnBound() time.Duration {
	reads := floorProbes + 2 + walkSourcesPerStore*maxReadsPerRun
	statements := walkSourcesPerStore*maxReadsPerRun*foldStatementsPerPage +
		purgeBudget*(3+len(factTables)) + housekeepingStatements
	return time.Duration(reads+statements) * statementBound
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
