package app

// engine_authored.go -- live bootstrap wiring for the owner-scoped
// authored-construct runtime (epic memql#954, issue #961 increment 5 / the #959
// live-glue follow-up).
//
// Increments 1-4 built + tested the engine-side pieces: Gate-3 activation
// (ActivateApprovedBundle), the per-author capability gate, the global kill
// switch, and edit/version impact analysis. This file STANDS THEM UP in a live
// process: it constructs the authored runtime (registry + per-owner scheduler +
// per-automation breaker) during the engine phase and binds:
//
//   - the scheduler's run hook to the live automations Executor, so an
//     activated authored automation actually fires its steps;
//   - the breaker's onTrip to Deactivate + registry-pause, so a misbehaving
//     authored automation fault-isolates (auto-pause, no cascade);
//   - the global kill switch to v1:identity:clusterSettings
//     .authoredAutomationsEnabled, so a single flag halts every node;
//   - the activation audit sink to the identity audit logger.
//
// The cluster owner gate (which node runs an owner's automations) is wired
// later in the cluster phase, where the peer mesh is available; until then the
// single-node default (this node owns every owner) applies.

import (
	"context"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/memql"
)

// wireAuthoredRuntime constructs + wires the authored-construct runtime. Called
// from engineAndBus after the automation scheduler is built (so the step
// registry + event bus exist). Failures are logged and degrade gracefully --
// the core engine keeps serving; only the authored-automation surface is
// affected.
func (a *App) wireAuthoredRuntime() {
	a.authoredRuntime = memql.NewAuthoredRuntimeRegistry()

	// A dedicated Executor for authored runs: same engine + step registry +
	// event bus the core scheduler uses, so authored automations get the same
	// step executors, integrations, and AI providers. It is NOT the core
	// scheduler's executor -- authored runs are driven by the authored
	// scheduler under the author's envelope, kept separate from core.
	authoredExecutor := automations.NewExecutor(automations.ExecutorOptions{
		Logger:       a.Logger,
		Engine:       a.engine,
		EventBus:     a.eventBus,
		StepRegistry: a.stepRegistry,
	})

	// Breaker: a faulting authored automation trips after N consecutive
	// failures; onTrip auto-pauses it (Deactivate stops the trigger; the
	// registry Pause stops resolution) so it fault-isolates without cascading.
	a.authoredBreaker = automations.NewAuthoredBreaker(0, func(info automations.BreakerTripInfo) {
		if a.authoredScheduler != nil {
			a.authoredScheduler.Deactivate(info.Owner, info.Automation)
		}
		if a.authoredRuntime != nil {
			_ = a.authoredRuntime.Pause(info.Owner, "automation", info.Automation)
		}
		a.Logger.Warn("authored automation circuit breaker tripped -- auto-paused",
			"owner", info.Owner, "automation", info.Automation,
			"failures", info.Failures, "lastError", info.LastError)
	})

	scheduler, err := automations.NewAuthoredScheduler(automations.AuthoredSchedulerOptions{
		Logger:   a.Logger,
		Loader:   a.automationLoader,
		EventBus: a.eventBus,
		Breaker:  a.authoredBreaker,
		// Run hook: execute one compiled authored automation under the
		// already-author-scoped ctx the scheduler builds (AuthorContext). The
		// run goes through the live Executor so the steps actually fire.
		Run: func(ctx context.Context, automation *automations.Automation, ev *events.Event) error {
			_, runErr := authoredExecutor.ExecuteWithEvent(ctx, automation, "authored", ev)
			return runErr
		},
		// GlobalGate: the cluster-wide kill switch. Reads
		// v1:identity:clusterSettings.authoredAutomationsEnabled live so a flip
		// halts (or resumes) every node. Defaults to enabled when the row is
		// absent (a fresh cluster has authored automations on).
		GlobalGate: func() bool {
			return a.authoredAutomationsEnabledFlag()
		},
	})
	if err != nil {
		a.Logger.Warn("authored runtime scheduler not wired -- authored automations will not fire",
			"error", err)
		return
	}
	a.authoredScheduler = scheduler

	a.Logger.Info("authored-construct runtime initialized (registry + scheduler + breaker)")

	// Boot re-arm (#1039): re-register every already-active bundle across all
	// owners so their automations fire again after a restart, with no manual
	// re-approval. The scheduler still enforces the global kill switch (#1034)
	// and the cluster owner gate (#1030) at fire time, so a restart neither
	// double-fires nor bypasses governance -- re-arm only re-establishes the
	// registrations. A re-arm failure degrades gracefully (the core engine keeps
	// serving; only the authored surface that failed to restore is affected).
	a.rearmActiveAuthoredBundles()

	// Boot re-hydration of durably-promoted PLAIN constructs (memql#1557): the
	// MCP `promote` op persists a v1:authoring:bundle + construct row pair and
	// registers the construct into the SHARED engine registry so every session
	// can call it. That shared registration is in-memory only; this step
	// recompiles each persisted promoted construct back into e.functions /
	// e.specs on boot so the promotion survives a restart. Core-first +
	// idempotent; automation bundles are untouched (they re-arm above).
	a.rehydratePromotedConstructs()
}

// rehydratePromotedConstructs recompiles every durably-promoted plain construct
// (memql#1557) back into the shared engine registry on boot. Best-effort: a
// failure is logged, not fatal -- the core engine keeps serving.
func (a *App) rehydratePromotedConstructs() {
	if a.engine == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := a.engine.RehydratePromotedConstructs(ctx)
	if err != nil {
		a.Logger.Warn("durably-promoted construct re-hydration failed -- promoted constructs may not be callable until re-promoted",
			"error", err)
		return
	}
	a.Logger.Info("durably-promoted constructs re-hydrated on boot",
		"seen", res.Seen, "rehydrated", res.Rehydrated, "failed", len(res.Failed))
}

// rearmActiveAuthoredBundles re-registers the persisted active authored bundles
// into the just-constructed runtime + scheduler. Called at the tail of
// wireAuthoredRuntime once the registry + scheduler exist. Best-effort: a
// failure is logged, not fatal.
func (a *App) rearmActiveAuthoredBundles() {
	if a.engine == nil || a.authoredRuntime == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := a.engine.RearmActiveBundles(ctx, a.AuthoredRuntimeDeps())
	if err != nil {
		a.Logger.Warn("authored bundle boot re-arm failed -- active automations may not fire until re-activated",
			"error", err)
		return
	}
	a.Logger.Info("authored bundles re-armed on boot",
		"seen", res.Seen, "rearmed", res.Rearmed, "skipped", res.Skipped, "failed", len(res.FailedIds))
}

// authoredAutomationsEnabledFlag reads the cluster-wide global kill switch from
// v1:identity:clusterSettings. Defaults to TRUE (enabled) when the row or the
// field is absent -- a fresh cluster runs authored automations until an operator
// explicitly halts them. Any read error is treated as enabled-by-default so a
// transient DB hiccup doesn't silently halt the surface (the per-author kill
// switch + per-automation breaker remain as the tighter guards).
func (a *App) authoredAutomationsEnabledFlag() bool {
	if a.engine == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := a.engine.Execute(ctx, "queryClusterSettingsCurrent()")
	if err != nil || res == nil || res.Bundle == nil || len(res.Bundle.Nodes) == 0 {
		return true
	}
	node := res.Bundle.Nodes[0]
	if node == nil || node.GetPayload() == nil {
		return true
	}
	fields := node.GetPayload().GetFields()
	v, ok := fields["authoredAutomationsEnabled"]
	if !ok {
		return true // field absent on an older row -> enabled by default
	}
	return v.GetBoolValue()
}

// authoredAuditSink adapts the identity audit logger to the memql
// AuthoredAuditSink the activation path emits through, so every activation /
// retirement lands on v1:identity:auditEvent.
type authoredAuditSink struct {
	logger identity.AuditLogger
}

func (s authoredAuditSink) EmitAuthoredAudit(ctx context.Context, ev memql.AuthoredAuditEvent) {
	if s.logger == nil {
		return
	}
	s.logger.Log(ctx, identity.AuditEvent{
		OccurredAt:  ev.OccurredAt,
		Category:    identity.AuditCategoryConfiguration,
		Action:      ev.Action,
		ActorUserId: ev.OwnerUserId,
		TargetId:    ev.BundleId,
		Detail:      ev.Detail,
		Outcome:     identity.AuditOutcomeSuccess,
	})
}

// AuthoredRuntimeDeps assembles the dependency bundle the activation path
// (engine.ActivateApprovedBundle) drives: the registry, the scheduler register
// / deregister hooks, the catalog promoter, and the audit sink. Exposed so the
// bundle-activation entry point (and tests) can build it from the live App.
func (a *App) AuthoredRuntimeDeps() memql.AuthoredRuntimeDeps {
	deps := memql.AuthoredRuntimeDeps{
		Registry: a.authoredRuntime,
		Logger:   a.Logger,
	}
	if a.authoredScheduler != nil {
		deps.RegisterHook = a.authoredScheduler.Activate
		deps.DeregisterHook = a.authoredScheduler.Deactivate
	}
	if a.engine != nil {
		deps.PromoteCatalog = a.engine.PromoteConstructToCatalog
		deps.Audit = authoredAuditSink{logger: a.authoredAuditLogger()}
	}
	return deps
}

// authoredAuditLogger builds a slog+DB audit logger over the live engine, the
// same shape app/integrations_identity.go uses.
func (a *App) authoredAuditLogger() identity.AuditLogger {
	return &identity.SlogAuditLogger{
		Logger: a.Logger,
		DB:     &identity.EngineAuditSink{Engine: a.engine, Logger: a.Logger},
	}
}
