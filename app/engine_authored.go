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
//   - the scheduler's run hook to the live automations executors, so an
//     activated authored automation actually fires its steps -- an event
//     firing claimed once per (automation, event) across replicas;
//   - the breaker's onTrip to the engine's PauseTrippedAutomation, so a
//     misbehaving authored automation fault-isolates (auto-pause, no cascade)
//     on every replica, durably;
//   - the global kill switch to v1:identity:clusterSettings
//     .authoredAutomationsEnabled, so a single flag halts every node;
//   - the activation audit sink to the identity audit logger.
//
// The cluster owner gate (which node runs an owner's SCHEDULED automations) is
// wired later in the cluster phase, where the peer mesh is available; until
// then the single-node default (this node owns every owner) applies. Event
// triggers never consult it: they fire on the node where their event happened.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/emailrules"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/memql"
	routerlib "github.com/znasllc-io/memql/component/router"
	"github.com/znasllc-io/memql/component/routingrules"
	integrationsrouter "github.com/znasllc-io/memql/integrations/router"
)

// wireAuthoredRuntime constructs + wires the authored-construct runtime. Called
// from engineAndBus after the automation scheduler is built (so the step
// registry + event bus exist). Failures are logged and degrade gracefully --
// the core engine keeps serving; only the authored-automation surface is
// affected.
func (a *App) wireAuthoredRuntime() {
	a.authoredRuntime = memql.NewAuthoredRuntimeRegistry()

	// Dedicated executors for authored runs: same engine + step registry +
	// event bus the core scheduler uses, so authored automations get the same
	// step executors, integrations, and AI providers. They are NOT the core
	// scheduler's executors -- authored runs are driven by the authored
	// scheduler under the author's envelope, kept separate from core. An EVENT
	// firing is claimed per (automation, event) through the same cluster guard
	// the core scheduler's event executor claims through, so one event runs an
	// authored automation once however many replicas observe it as local
	// (memql#6022, F2); a scheduled firing is not claimed.
	//
	// THE CEILINGS ARE PER PROCESS. The 10-run concurrency these executors
	// share and the execution budget every run is admitted against
	// (component/automations/budget.go) count this process's runs only, and an
	// event rule fires on whichever replica its write landed on -- so the
	// cluster-wide ceiling is the per-process one times the number of replicas
	// running the authored scheduler.
	runAuthored := automations.NewAuthoredRunner(automations.AuthoredRunnerOptions{
		Logger:       a.Logger,
		Engine:       a.engine,
		EventBus:     a.eventBus,
		StepRegistry: a.stepRegistry,
		ClusterGuard: a.clusterGuard,
	})

	// Breaker: a faulting authored automation trips after N consecutive
	// failures. onTrip pauses it on EVERY replica (memql#6022, F2): the engine
	// stops it here, records the pause on its bundle row -- so neither a
	// reboot nor a reconcile pass arms it again -- and broadcasts the disarm
	// to every other node, which an event rule firing wherever its write
	// lands needs. It runs off the failing run's goroutine: the breaker asks
	// onTrip not to block, and the breaker itself already refuses this
	// automation's next runs here.
	a.authoredBreaker = automations.NewAuthoredBreaker(0, func(info automations.BreakerTripInfo) {
		a.Logger.Warn("authored automation circuit breaker tripped -- pausing it on every replica",
			"owner", info.Owner, "automation", info.Automation,
			"failures", info.Failures, "lastError", info.LastError)
		go a.pauseTrippedAuthoredAutomation(info)
	})

	scheduler, err := automations.NewAuthoredScheduler(automations.AuthoredSchedulerOptions{
		BeforeWriteEngine: a.engine, BeforeWriteRegistry: a.stepRegistry,
		Logger:   a.Logger,
		Loader:   a.automationLoader,
		EventBus: a.eventBus,
		Breaker:  a.authoredBreaker,
		// Run hook: execute one compiled authored automation under the
		// already-author-scoped ctx the scheduler builds (AuthorContext). The
		// run goes through the live executors so the steps actually fire.
		Run: runAuthored,
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

	// Event-email rules (memql#4829) are the FIRST production caller of
	// ActivateApprovedBundle, which shipped with increment 5 and until now had
	// none anywhere in the tree -- so an authored construct took effect at next
	// boot, via the re-arm below, and nothing else. For a capability somebody
	// promotes once that is merely slow; for a rule somebody creates in an app
	// and expects to fire, it is the feature not working, silently.
	//
	// The handoff is here rather than through PluginContext because neither
	// half fits it: ActivateApprovedBundle is a method on the concrete engine,
	// and the deps are assembled from this App's registry, scheduler hooks,
	// catalog promoter and audit sink. It is placed AFTER the registry and
	// scheduler exist and BEFORE the re-arm, so a rule armed by a re-armed
	// bundle finds the seam already wired.
	emailrules.Bind(emailrules.EngineAdapter{Engine: a.engine}, a.AuthoredRuntimeDeps)

	// And the engine gets the same deps, for the OTHER direction (memql#6022,
	// F1): an activation arms only the runtime it is handed -- this node's --
	// and broadcasts authoring.arm so every other replica arms the bundle too;
	// a retire broadcasts authoring.disarm. The engine arms a peer's bundle
	// into these deps. Its subscriber starts later, with the engine
	// (StartAuthoringArmSubscriber in engine.run), so no broadcast is handled
	// before this line; what is activated or retired between the boot re-arm
	// below and that subscription is picked up by the reconcile pass the
	// subscriber runs as soon as it starts, and again every few minutes after.
	a.engine.SetAuthoredRuntimeDeps(a.AuthoredRuntimeDeps)

	// Routing rules (epic memql#5127, design D7) bind through the same seam and
	// for the same reason: a rule an owner adds is a `rule` construct, the
	// authored runtime is what compiles and arms one, and both halves of the
	// handoff -- ActivateApprovedBundle on the concrete engine, and the deps
	// assembled from this App -- are outside PluginContext.
	//
	// The shipped-name check reads the LIVE registries rather than a list, so
	// adding a shipped rule never requires editing anything here.
	routingrules.Bind(
		routingRuleEngine{engine: a.engine},
		a.AuthoredRuntimeDeps,
		routingrules.EngineShippedNames{Engine: a.engine},
	)

	// And the other half of that handoff: the router integration serves the
	// two builtins and cannot import component/routingrules (its module
	// requires the root module at a PUBLISHED version, and the package is
	// newer than any of them), so app/ installs the adapter on it here.
	//
	// A node where the integration did not materialize installs nothing and
	// the capability refuses by name, which is the visible failure rather than
	// the silent one.
	if ri := integrationsrouter.Materialized(); ri != nil {
		ri.SetRuleActivator(routingRuleActivatorAdapter{})
		// The SAME handoff for the same reason, for the evidence fold's
		// renderer (epic memql#5146, D5). The fold hashes the source it
		// renders and a person approves that hash, so the tree must have
		// exactly ONE renderer of this grammar: a second would drift, and the
		// drift is silent because the approval would then carry to text
		// nobody read.
		//
		// The two Form types are field-identical by construction --
		// component/router mirrors component/routingrules.Form because the
		// leaf module cannot import the root one -- and
		// TestEvidenceFormMirrorsTheRoutingRulesForm fails the build if they
		// diverge. This adapter is the one place the mirror is crossed.
		ri.SetRuleRenderer(func(f routerlib.Form) (string, error) {
			return routingrules.GenerateRule(routingrules.Form{
				Name:          f.Name,
				Description:   f.Description,
				When:          f.When,
				Policy:        f.Policy,
				Level:         f.Level,
				Precedence:    f.Precedence,
				OnUnavailable: f.OnUnavailable,
				Excludes:      f.Excludes,
			})
		})
		a.Logger.Info("routing-rule authoring wired: an owner or developer can add a rule at runtime and it survives a restart",
			"component", "router")
	}

	// Boot re-arm (#1039): re-register every already-active bundle across all
	// owners so their automations fire again after a restart, with no manual
	// re-approval. The scheduler still enforces the global kill switch (#1034)
	// at fire time, an event firing still runs once per event (on the node it
	// happened on, claimed across replicas) and a schedule still runs on the
	// owner's node only (#1030), so a restart neither double-fires nor
	// bypasses governance -- re-arm only re-establishes the registrations. A
	// bundle a breaker paused reads paused, not active, so it stays out. A
	// re-arm failure degrades gracefully (the core engine keeps serving; only
	// the authored surface that failed to restore is affected).
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

// pauseTrippedAuthoredAutomation pauses an automation whose breaker tripped on
// every replica: the engine stops it here, records the pause on its bundle row
// and broadcasts the disarm. A pause that cannot be recorded leaves it stopped
// on this node only, which is what the warning says.
func (a *App) pauseTrippedAuthoredAutomation(info automations.BreakerTripInfo) {
	if a.engine == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	trip := memql.AuthoredTrip{
		Owner:      info.Owner,
		Automation: info.Automation,
		// The version the failing runs were armed from: a fix armed before
		// this pause takes the arming lock is not paused for them.
		BundleId: info.BundleId,
		Version:  info.Version,
		Reason:   breakerPauseReason(info),
		// Re-activation resets the breaker; a reset breaker's trip is moot.
		StillTripped: func() bool {
			return a.authoredBreaker != nil && a.authoredBreaker.IsOpen(info.Owner, info.Automation)
		},
	}
	if err := a.engine.PauseTrippedAutomation(ctx, a.AuthoredRuntimeDeps(), trip); err != nil {
		a.Logger.Warn("authored automation paused on this node only -- the pause could not be recorded, so other replicas may still run it",
			"owner", info.Owner, "automation", info.Automation, "error", err)
		return
	}
	a.Logger.Info("authored automation paused on every replica after its circuit breaker tripped",
		"owner", info.Owner, "automation", info.Automation)
}

// breakerPauseReason is the sentence a breaker's pause leaves on the bundle
// row (failureReason): what happened and what to do, then the last failure.
func breakerPauseReason(info automations.BreakerTripInfo) string {
	last := strings.TrimSpace(info.LastError)
	if r := []rune(last); len(r) > 2000 {
		last = string(r[:2000])
	}
	return fmt.Sprintf("Paused automatically after %d failed runs in a row. Fix the cause, then turn it on again. Last error: %s",
		info.Failures, last)
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
	// `staged` is reported alongside `rehydrated` rather than folded into it
	// (memql#3928). A staged construct came back, so it counts as re-hydrated --
	// but it came back OWNER-SCOPED, resolvable for its author and nobody else,
	// and an operator reading "12 re-hydrated" with no split would take that as
	// twelve constructs the cluster can now run.
	a.Logger.Info("durably-promoted constructs re-hydrated on boot",
		"seen", res.Seen, "rehydrated", res.Rehydrated, "staged", res.Staged, "failed", len(res.Failed))
	// A row that did not come back is not callable until its source is
	// corrected and re-promoted, or it is demoted (memql#5426). Each was
	// logged above with its row id, the refusal and the remedy; this line
	// names them all in one place, which is what an operator greps for after
	// an upgrade narrows the language.
	if len(res.Failed) > 0 {
		a.Logger.Warn("durably-promoted constructs did not re-hydrate -- each is logged as \"durable authored construct quarantined at re-hydration\" with its row id and what to do",
			"failed", res.Failed)
	}
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
	res, err := a.engine.Execute(ctx, "clusterSettingsCurrent()")
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

// routingRuleEngine adapts the concrete engine to component/routingrules'
// narrow Engine. It exists for the reason emailrules.EngineAdapter does: the
// package declares the three operations it needs so its dependency reads as
// three named calls rather than as the whole engine, and the adapter is where
// the concrete method set meets that declaration.
type routingRuleEngine struct{ engine *memql.MemQLEngine }

func (r routingRuleEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	return r.engine.Execute(ctx, query)
}

func (r routingRuleEngine) ActivateApprovedBundle(ctx context.Context, owner, bundleId string, deps memql.AuthoredRuntimeDeps) (memql.ActivationResult, error) {
	return r.engine.ActivateApprovedBundle(ctx, owner, bundleId, deps)
}

func (r routingRuleEngine) RetireActiveBundle(ctx context.Context, owner, bundleId string, deps memql.AuthoredRuntimeDeps) error {
	return r.engine.RetireActiveBundle(ctx, owner, bundleId, deps)
}
