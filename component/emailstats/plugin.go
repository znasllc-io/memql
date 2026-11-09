package emailstats

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

// The DSL-callable half, declared in dsl/campaigns/builtins.memql and executed
// here: `builtin emailStatisticsRollup`, the five-minute fold of campaign rows
// into email_stat_fact (rollup.go), which only its own automation's
// maintenance principal may run.
//
// # Why it is registered on EVERY node type
//
// The builtin is declared in dsl/campaigns/builtins.memql, which every binary
// loads, and a capability present in the DSL and absent from the registry is a
// boot-time resolution failure. So the plug-in registers everywhere, from
// app/plugins_core.go; the cron leader, wherever it is, runs it.

// integrationName is the plug-in name. Spelled as a string literal in
// RegisterPlugin below as well, because the taxonomy gate scans source for the
// literal; TestThePlugInOffersTheRollupItsBuiltinNames holds the two together.
const integrationName = "emailStats"

// rollupConcept is the concept of the one node the rollup answers with, its
// report. Not a DSL concept: nothing stores it and no client reads it, which
// is how the website statistics rollup answers too.
const rollupConcept = "integration:emailStats:rollup"

// rollupPrincipal is the only caller the rollup admits: the maintenance
// principal its scheduled automation runs under
// (auth.MaintenanceActor("emailStatisticsRollupSweep")).
const rollupPrincipal = "system:maintenance:emailStatisticsRollupSweep"

// Integration exposes the rollup.
type Integration struct {
	engine Engine
	bunDB  func() *bun.DB
	logger *slog.Logger

	// now is the rollup's clock; nil is time.Now. A field so a test runs the
	// builtin at an instant of its own choosing.
	now func() time.Time

	// rollupRunning keeps one rollup per process: a run slower than the
	// five-minute schedule must not have the next one queue up behind it and
	// walk the same pages again. Two replicas need no such guard; see
	// "Multi-node" in rollup.go.
	rollupRunning atomic.Bool
}

// NewIntegration wires the handles. The db getter is resolved at call time,
// not at construction: a node's database handle is not necessarily up when
// plug-ins are materialized.
func NewIntegration(engine Engine, bunDB func() *bun.DB, logger *slog.Logger) *Integration {
	if logger == nil {
		logger = slog.Default()
	}
	return &Integration{engine: engine, bunDB: bunDB, logger: logger}
}

func init() {
	memql.RegisterPlugin("emailStats", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		return NewIntegration(pctx.Engine, pctx.BunDB, pctx.Logger), nil
	})
}

// IntegrationName implements memql.IntegrationProvider.
func (i *Integration) IntegrationName() string { return integrationName }

// Capabilities implements memql.IntegrationProvider.
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "rollup",
			Description: "Fold new and changed campaign deliveries, engagement events and consent events into the email statistics facts, each source walked through the engine from its watermark less five minutes, at most forty pages past it per run, one fact per source record and kind; then delete facts past four hundred days. Admits only the emailStatisticsRollupSweep maintenance principal.",
			Handler:     i.handleRollup,
		},
	}
}

// handleRollup runs the rollup for the scheduled automation, and for nobody
// else, and answers one node with its report.
//
// THE PRINCIPAL AND NOTHING ELSE, the website statistics rollup's rule
// (component/sitetraffic). The rollup reads every organization's campaign
// rows and spends the database's time on a schedule; a cluster owner has no
// reason to run it by hand. So the check is the automation's own maintenance
// id together with the cluster-owner role that principal carries -- the id
// alone with any other role is refused, as is any other automation's
// principal -- and it is made before anything is read.
//
// A failed source is logged here, one Warn naming it, and the run's error is
// returned, so the automation's run says it failed; the next run carries on
// from that source's watermark.
func (i *Integration) handleRollup(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	actor, ok := auth.AccessFromContext(ctx)
	if !ok || actor == nil || actor.UserId != rollupPrincipal || !actor.IsClusterOwner() {
		return nil, fmt.Errorf("email statistics rollup requires the engine maintenance principal")
	}
	if !i.rollupRunning.CompareAndSwap(false, true) {
		return nil, nil
	}
	defer i.rollupRunning.Store(false)

	var db *bun.DB
	if i.bunDB != nil {
		db = i.bunDB()
	}
	now := time.Now()
	if i.now != nil {
		now = i.now()
	}
	rep, err := runRollup(ctx, i.engine, db, now)
	i.logFailures(err)
	// A routine run is the same line every five minutes, so it is Debug; a
	// backfill under way or a retention delete is worth a line at Info. A
	// failure has its own Warn above.
	level := slog.LevelDebug
	if len(rep.Behind) > 0 || rep.Deleted > 0 {
		level = slog.LevelInfo
	}
	i.logger.Log(ctx, level, "Email statistics rolled up",
		"component", "emailstats", "rows", rep.Rows, "facts", rep.Facts,
		"behind", rep.Behind, "deleted", rep.Deleted)
	if err != nil {
		return nil, err
	}

	payload, err := json.Marshal(rep)
	if err != nil {
		return nil, fmt.Errorf("email statistics rollup: encode the report: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        "emailStatisticsRollup",
		Concept:   rollupConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: now.UTC(),
		Payload:   payload,
	}}, nil
}

// logFailures writes one Warn per failure in a run's error: one per source
// whose walk stopped, naming it, and one for a failed retention pass.
func (i *Integration) logFailures(err error) {
	if err == nil {
		return
	}
	failures := []error{err}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		failures = joined.Unwrap()
	}
	for _, f := range failures {
		var src *sourceFailure
		if errors.As(f, &src) {
			i.logger.Warn("Email statistics for a source were not folded; the next run carries on from its watermark",
				"component", "emailstats", "source", src.Source, "error", src.Err)
			continue
		}
		i.logger.Warn("Email statistics rollup failed", "component", "emailstats", "error", f)
	}
}
