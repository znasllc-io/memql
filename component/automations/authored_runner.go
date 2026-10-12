package automations

// authored_runner.go -- what runs one authored automation's firing
// (memql#6022, F2).
//
// The authored scheduler decides WHERE a firing runs (authored_scheduler.go):
// an event trigger on the node where its event happened, a schedule on the
// owner's node. This file decides HOW it runs, and it is two executors, split
// exactly as the core scheduler splits its own:
//
//   - EVENT firings go through an executor that claims each run per
//     (automation, event) -- per-process dedup plus the cross-replica
//     ClusterGuard, keyed on the run's initial chain head (the automation's
//     identity, owner included, and a deterministic fingerprint of its
//     triggering event: topic, kind, payload without the version clock, and
//     causal correlation). Firing on the origin node is the primary guarantee
//     that one event runs an automation once; the claim is the backstop that
//     holds even when one event is somehow observed as local on two replicas.
//     The key is the event's CONTENT, as it is for every core event
//     automation: two events with identical content inside the dedup windows
//     (10 minutes in process, the claim's retention across replicas) run once.
//   - SCHEDULED firings have no event to claim by: every tick of a cron entry
//     carries the same (empty) trigger, so a claim would admit the first tick
//     and refuse every later one. They keep the owner gate as their once-only
//     rule and run every time they are due, as they always did.
//
// The two share ONE concurrency ceiling (10 runs at a time, the default every
// authored run had before the split), so splitting them changes no limit.
//
// That ceiling, and the execution budget every run is admitted against
// (sharedAutomationBudget, budget.go), are PER PROCESS. Event rules now fire
// on whichever replica their write landed on, so the cluster-wide ceiling is
// the per-process one multiplied by the number of replicas running the
// authored scheduler.

import (
	"context"
	"log/slog"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
)

// AuthoredRunnerOptions configures the runner the authored scheduler fires
// through.
type AuthoredRunnerOptions struct {
	Logger       *slog.Logger
	Engine       *memql.MemQLEngine
	EventBus     *events.Bus
	StepRegistry StepExecutorRegistry
	// ClusterGuard claims each EVENT firing per (automation, event) in the
	// shared database, so one event runs an authored automation once however
	// many replicas observe it as local. nil = no cross-replica claim (a single
	// node; tests); the per-process claim still applies. Typically the
	// *ClusterExecutionGuard the core scheduler's event executor claims through.
	ClusterGuard executionClaimer
}

// NewAuthoredRunner builds the RunAutomationFunc the app hands the authored
// scheduler: event firings through the claiming executor, scheduled firings
// through the plain one.
func NewAuthoredRunner(opts AuthoredRunnerOptions) RunAutomationFunc {
	onEvent := NewExecutor(ExecutorOptions{
		Logger:               opts.Logger,
		Engine:               opts.Engine,
		EventBus:             opts.EventBus,
		StepRegistry:         opts.StepRegistry,
		ChainTrackingEnabled: true, // the claim key is the run's initial chain head
		DedupEnabled:         true,
		ClusterGuard:         opts.ClusterGuard,
	})
	onSchedule := NewExecutor(ExecutorOptions{
		Logger:       opts.Logger,
		Engine:       opts.Engine,
		EventBus:     opts.EventBus,
		StepRegistry: opts.StepRegistry,
	})
	onSchedule.concurrencySem = onEvent.concurrencySem
	return func(ctx context.Context, automation *Automation, triggeringEvent *events.Event) error {
		executor := onSchedule
		if triggeringEvent != nil {
			executor = onEvent
		}
		_, err := executor.ExecuteWithEvent(ctx, automation, "authored", triggeringEvent)
		return err
	}
}
