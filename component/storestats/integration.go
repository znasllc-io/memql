package storestats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
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
	walkStore    func(ctx context.Context, store map[string]any, generation int64, now time.Time, report *RollupReport) error
	wholesaleFor func(ctx context.Context, storeIDs []string) (map[string]wholesaleState, error)

	// rollupRunning keeps one rollup per process, the email statistics
	// rollup's guard: a run slower than the schedule must not have the next
	// one start beside it and walk the same stores again. Two nodes need no
	// such guard, because every write takes the one lock and checks the
	// store's generation (tx.go).
	rollupRunning atomic.Bool
}

// NewIntegration wires the handles. The db getter is resolved at call time,
// not at construction: a node's database handle is not necessarily up when
// providers are materialized.
func NewIntegration(engine Engine, bunDB func() *bun.DB, logger *slog.Logger) *Integration {
	if logger == nil {
		logger = slog.Default()
	}
	return &Integration{engine: engine, bunDB: bunDB, logger: logger}
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
// and says so at Debug.
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
	if !i.rollupRunning.CompareAndSwap(false, true) {
		i.logger.Debug("Store statistics rollup stood down: another run is under way on this node",
			"component", "storestats")
		return nil, nil
	}
	defer i.rollupRunning.Store(false)

	now := time.Now()
	if i.now != nil {
		now = i.now()
	}
	rep, err := runRollup(ctx, i, now)
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
