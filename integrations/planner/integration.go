// Package planner is the planner-node integration: the home for
// task / Plan lifecycle orchestration.
//
// SCOPE BOUNDARY (read this before adding code):
//
//   - Cognition handles ROUTING + TURN-TAKING decisions: which agent
//     responds to a chat utterance, the conductor's per-turn
//     directive, peer-agent coordination, the AgentForwarder for
//     conversational replies. Cognition does NOT own Plan / Task
//     state machinery.
//
//   - Planner (this package) handles PLAN AND TASK EXECUTION:
//     subscribes to Plan transitions, dispatches the owning agent
//     when a Plan goes from awaitingFeedback -> running, marks
//     Plan terminal status (succeeded with output / failed with
//     errorMessage), enforces token budgets, dispatches container-
//     executor backed Tasks. It uses its own AgentForwarder to ship
//     AgentGenerateTurnMsg to agent peers (parallel to cognition's,
//     not shared -- the two services have independent dispatch
//     reasons and shouldn't share a single forwarder instance).
//
// The split mirrors the docker-compose.cluster.yml topology: the
// planner runs as a separate node-type binary
// (`make build-planner`) and only loads what's relevant to plan
// execution. Putting plan-execution code in cognition means the
// planner binary doesn't have it -- which would be wrong.
package planner

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/events"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/node"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/integrations"
)

// ComponentName is the planner integration's logger / dependency
// component identifier.
const ComponentName common.ComponentName = "plannerIntegration"

// Engine is the narrow MemQL surface the planner integration uses.
// Mirrors the cognition.MemQLEngine pattern -- adapter-based interface
// so the integration doesn't import the full engine package.
type Engine interface {
	Execute(ctx context.Context, query string) (any, error)
	// InvokeAI runs a named prompt against its default provider and
	// returns the assistant response. Used by the Phase 4 Planner
	// Agent loop to drive structured-output decisions.
	InvokeAI(ctx context.Context, templateId string, data map[string]any) (any, error)
	// InvokeAIChatWithFilteredTools renders a prompt and runs a bounded
	// tool-calling loop restricted to the named tool set, returning the
	// model's final assistant text. Used by the trainSpecialist
	// dispatcher to drive the Trainer Agent (webSearch / fetchUrl /
	// writeKnowledgeChunk / markChunkSuperseded / embedChunk).
	InvokeAIChatWithFilteredTools(ctx context.Context, templateId string, data map[string]any, toolNames []string) (string, error)
}

// AgentForwarder is the wire-level interface the planner uses to
// ship AgentGenerateTurnMsg to an agent peer. Same shape as
// cognition's AgentForwarder; satisfied on cluster-mode planner
// binaries by memqlgrpc.AiForwardRouter (separate instance from the
// cognition one -- both routers connect to the same agent peer mesh
// but maintain independent in-flight request tables).
type AgentForwarder interface {
	Forward(
		ctx context.Context,
		requestId string,
		targetType node.NodeType,
		authClaims map[string]string,
		partition string,
		envelope *memqlv1.MemqlClientMessage,
	) (<-chan *memqlv1.MemqlServerMessage, error)

	ForwardContinuation(
		requestId string,
		authClaims map[string]string,
		partition string,
		envelope *memqlv1.MemqlClientMessage,
	) error
}

// ExecutionClaimer is the cross-replica execution gate the plan-execution
// dispatcher consults before forwarding an agent turn (memql#1363). Claim
// returns true when THIS replica should execute and false when another
// replica already owns the (name, dedupKey) claim. Satisfied by
// *automations.ClusterExecutionGuard -- the same DB-PK-backed,
// bounded-fail-open (memql#1142) guard event-triggered automations use; the
// app wiring passes the one instance both share.
type ExecutionClaimer interface {
	Claim(ctx context.Context, name, dedupKey string) bool
}

// PlannerIntegration is the integration provider registered on the
// planner node's engine. The lifecycle layer (Start / Stop) hooks
// into the event bus subscriptions; the dispatch logic lives in
// plan_execution.go.
type PlannerIntegration struct {
	// Embed the base Integration so the struct satisfies
	// common.Dependency (Start / Stop / IsRunning / Order / Ready /
	// ComponentName) without re-implementing the lifecycle plumbing.
	// The base Integration's Start kicks off a health-check ticker
	// loop; we extend Start in this package to ALSO subscribe to
	// the event bus before delegating to the base.
	*integrations.Integration

	engine         Engine
	eventBus       *events.Bus
	agentForwarder AgentForwarder
	// dbGetter is the lazy *bun.DB accessor used by the across-account
	// fairness sweep (memql#908) for its bulk pooled read SQL. Optional:
	// when nil, the fairness cron is a no-op.
	dbGetter func() *bun.DB
	// directDBGetter is the lazy *bun.DB accessor for the DIRECT
	// (non-pooled) endpoint, used ONLY by the admission controller for its
	// per-account SESSION-SCOPED advisory locks + running-Plan counts (epic
	// memql#902 / #904, pooling epic memql#1925). TryAdmit / AdmitNext hold
	// pg_advisory_lock across statements (lock -> count -> demote/promote ->
	// unlock), so they must NOT ride the transaction pooler, which recycles
	// the server backend between statements and would drop a held
	// session-scoped lock. Optional: when nil, admission falls back to
	// dbGetter (and DirectBunDB itself falls back to the main pool when
	// DIRECT_DSN is unset), so behavior is unchanged locally / in dev.
	directDBGetter func() *bun.DB
	entitlements   *EntitlementResolver
	admission      *AdmissionController
	agentLoop        *PlannerAgentLoop
	trainDispatch    *TrainSpecialistDispatcher
	embedDispatch    *EmbedDomainItemsDispatcher
	intakeDispatch   *ResponsibilityIntakeDispatcher
	captureDispatch  *AuthoringCaptureDispatcher
	refreshCron      *RefreshCron
	reactiveLoop     *ReactiveLoop
	fairnessCron     *FairnessCron
	strandedWatchdog *StrandedPlanWatchdog
	logger           *slog.Logger
	unsubscribes     []func()
	started          atomic.Bool
	mu               sync.Mutex
	// executing dedups the running-plan executor (handlePlanApprovedForExecution)
	// so one plan can't be dispatched to its agent twice -- e.g. if more than
	// one graph.node.updated event arrives while the plan is in "running".
	// Keyed by planId; cleared when executeApprovedPlan returns. (memql#800)
	//
	// IN-PROCESS ONLY: this map collapses duplicate events within ONE replica.
	// Cross-replica dedup (the plan-approved event reaches every planner
	// replica) is the clusterClaimer's job (memql#1363).
	executing sync.Map
	// clusterClaimer is the cross-replica plan-execution claim (memql#1363).
	// executeApprovedPlan consults it before dispatching the agent turn so
	// exactly one planner replica executes an approved Plan. Optional: when
	// nil (dev / single-binary builds, unit tests) only the in-process dedup
	// above applies -- the pre-#1363 behavior.
	clusterClaimer ExecutionClaimer
}

// PlannerArg is a functional option for NewPlannerIntegration.
type PlannerArg func(*PlannerIntegration)

// WithEngine wires the engine adapter the integration uses for
// queryPlanById + mutationUpdatePlanStatus + insert AI utterance.
func WithEngine(engine Engine) PlannerArg {
	return func(p *PlannerIntegration) { p.engine = engine }
}

// WithEventBus wires the event bus the integration subscribes on
// for graph.node.updated.v1:planner:plan events.
func WithEventBus(bus *events.Bus) PlannerArg {
	return func(p *PlannerIntegration) { p.eventBus = bus }
}

// WithLogger overrides the default logger.
func WithLogger(logger *slog.Logger) PlannerArg {
	return func(p *PlannerIntegration) { p.logger = logger }
}

// WithDBGetter wires the lazy *bun.DB accessor the admission controller
// (epic memql#902 / #904) uses for per-account advisory locks + running-Plan
// counts. Pass a.db.BunDB from app wiring. Optional -- omitting it leaves the
// concurrency gate failing open (no cap enforced), matching the
// default-unlimited contract.
func WithDBGetter(getter func() *bun.DB) PlannerArg {
	return func(p *PlannerIntegration) { p.dbGetter = getter }
}

// WithDirectDBGetter wires the lazy *bun.DB accessor for the DIRECT
// (non-pooled) endpoint that the admission controller uses for its
// session-scoped per-account advisory locks (epic memql#902 / #904, pooling
// epic memql#1925). Pass a.db.DirectBunDB from app wiring: the admission gate
// holds pg_advisory_lock across statements, so it must bypass the transaction
// pooler the bulk pool rides (which would recycle the backend out from under
// the held lock). Optional -- when omitted, the admission controller falls
// back to the WithDBGetter pooled getter; DirectBunDB itself falls back to the
// main pool when DIRECT_DSN is unset, so local / dev behavior is unchanged.
// Only the admission controller uses this; the fairness cron stays on the
// pooled WithDBGetter getter (it does bulk reads, holds no session locks).
func WithDirectDBGetter(getter func() *bun.DB) PlannerArg {
	return func(p *PlannerIntegration) { p.directDBGetter = getter }
}

// WithClusterClaimer wires the cross-replica plan-execution claim
// (memql#1363). Pass the app's ClusterExecutionGuard so the planner's
// plan-execution dispatch shares the automation guard's DB-PK claim table,
// observability counters, and bounded fail-open (memql#1142). Optional --
// omitting it leaves only the in-process dedup (memql#800), which cannot
// collapse duplicates across replicas.
func WithClusterClaimer(claimer ExecutionClaimer) PlannerArg {
	return func(p *PlannerIntegration) { p.clusterClaimer = claimer }
}

// NewPlannerIntegration constructs a PlannerIntegration. Engine and
// eventBus are required; the agent forwarder is installed
// separately via SetAgentForwarder once cluster wiring resolves.
func NewPlannerIntegration(_ context.Context, opts ...PlannerArg) (*PlannerIntegration, error) {
	base, err := integrations.NewIntegration(ComponentName)
	if err != nil {
		return nil, fmt.Errorf("planner integration base: %w", err)
	}
	p := &PlannerIntegration{Integration: base}
	for _, opt := range opts {
		opt(p)
	}
	if p.engine == nil {
		return nil, fmt.Errorf("planner integration: engine is required")
	}
	if p.eventBus == nil {
		return nil, fmt.Errorf("planner integration: event bus is required")
	}
	if p.logger == nil {
		p.logger = slog.Default().With("component", ComponentName)
	}
	// Admission control (epic memql#902): the resolver reads the per-account
	// cap (#903), the controller enforces it race-free (#904). Both are always
	// built; the controller fails open when no dbGetter was wired, so an
	// unconfigured cap is a true no-op.
	p.entitlements = NewEntitlementResolver(p.engine, p.logger)
	// The admission controller holds session-scoped advisory locks across
	// statements, so it resolves its *bun.DB through the DIRECT (non-pooled)
	// getter -- transaction-mode PgBouncer would recycle the backend out from
	// under a held lock (pooling epic memql#1925). Fall back to the pooled
	// getter when no direct getter was wired (tests / older callers);
	// DirectBunDB itself falls back to the main pool when DIRECT_DSN is unset,
	// so behavior is unchanged locally / in dev.
	admissionGetter := p.directDBGetter
	if admissionGetter == nil {
		admissionGetter = p.dbGetter
	}
	p.admission = NewAdmissionController(admissionGetter, p.logger)
	p.agentLoop = NewPlannerAgentLoop(p.engine, p.logger)
	p.trainDispatch = NewTrainSpecialistDispatcher(p.engine, p.logger)
	p.embedDispatch = NewEmbedDomainItemsDispatcher(p.engine, p.logger)
	p.intakeDispatch = NewResponsibilityIntakeDispatcher(p.engine, p.logger)
	// Everyday-task authoring capture (epic memql#1160 / #1161): authors a
	// stored, versioned v1:authoring:bundle for every user-facing one-off task
	// that completes, capturing the reproducible MemQL behind it. Default-on,
	// gated by MEMQL_AUTHORING_CAPTURE_ENABLED; a no-op on a binary whose engine
	// carries no authoring Gate seams.
	p.captureDispatch = NewAuthoringCaptureDispatcher(p.agentLoop, p.engine, p.logger)
	p.refreshCron = NewRefreshCron(p.engine, p.logger)
	p.reactiveLoop = NewReactiveLoop(p.engine, p.logger)
	// Fairness / anti-starvation sweep (memql#908). Needs the same lazy
	// *bun.DB the admission controller uses for its across-account read SQL;
	// disabled by default (MEMQL_TASK_FAIRNESS_ENABLED) and a no-op when no
	// account has a finite cap producing a waiting queue.
	p.fairnessCron = NewFairnessCron(p.engine, p.dbGetter, p.logger)
	// Stranded-plan watchdog (memql#1389): the safety net behind the
	// plan-created event. Re-drives Plans stuck in planning/queued past
	// the strand threshold whose created event was lost (mesh drop /
	// replica crash). Default-on; shares the cross-replica claim guard
	// (memql#1363) so exactly one replica recovers a given Plan. Wired
	// here so the watchdog sees the same agent loop the created-event
	// path uses (the per-plan once-guard is shared -> idempotent).
	p.strandedWatchdog = NewStrandedPlanWatchdog(p.engine, p.agentLoop, p.clusterClaimer, p.logger)
	return p, nil
}

// SetAgentForwarder installs the agent-turn forwarder. Called from
// app.cluster after the AiForwardRouter is constructed. Without a
// forwarder, plan-execution dispatches log a warning and skip.
func (p *PlannerIntegration) SetAgentForwarder(fwd AgentForwarder) {
	if p == nil {
		return
	}
	p.agentForwarder = fwd
}

// Start subscribes to Plan-update events and delegates to the
// embedded Integration's lifecycle. Idempotent: the
// CompareAndSwap guard makes the subscription stage run at most
// once across repeated Start calls (the base Integration also
// guards its internal lifecycle separately).
func (p *PlannerIntegration) Start(ctx context.Context) {
	if p == nil {
		return
	}
	if p.started.CompareAndSwap(false, true) {
		p.mu.Lock()
		// Subscribe to Plan transitions. The engine's executeUpdate
		// publishes graph.node.updated AFTER the underlying
		// append-only insert succeeds, so this fires on every Plan
		// status transition. handlePlanApprovedForExecution gates
		// on kind=scopeElevation && status=running and dispatches
		// the agent for the actual work.
		p.unsubscribes = append(p.unsubscribes, p.eventBus.Subscribe(
			"graph.node.updated.v1:planner:plan",
			p.handlePlanApprovedForExecution,
			events.WithSubscriberName("planner:plan-execution"),
		))
		// Planner Agent loop (Phase 4 of the planner-redesign work).
		// Subscribes to plan.created so a brand-new Plan triggers the
		// first agent invocation (decompose -> Plan.phases stamped).
		// Updates are handled by the agent loop's HandlePlanUpdated --
		// log-only for now, deeper re-invoke wiring lands in Phase 4b.
		p.unsubscribes = append(p.unsubscribes, p.eventBus.Subscribe(
			"graph.node.created.v1:planner:plan",
			p.agentLoop.HandlePlanCreated,
			events.WithSubscriberName("planner:agent-loop-created"),
		))
		p.unsubscribes = append(p.unsubscribes, p.eventBus.Subscribe(
			"graph.node.updated.v1:planner:plan",
			p.agentLoop.HandlePlanUpdated,
			events.WithSubscriberName("planner:agent-loop-updated"),
		))
		// produceArtifact completion card (memql#792 / #940): the
		// "Deliverable ready -> Open in Library" notify card now emits from
		// the CoPresent carrier (the emitProduceArtifactDoneCanvasCard
		// automation), not here. mutationCreateCanvasState is copresent-only
		// and the planner core build can't load it, so the old planner-side
		// subscriber failed at runtime with "function not found".
		// Admit-next-on-slot-free (epic memql#902 / #905). When a Plan
		// leaves the running state (succeeded / failed / cancelled / paused
		// / awaitingFeedback / needsAgent) a per-account concurrency slot
		// may have freed; this pulls the longest-waiting (waitingForSlot)
		// Plan(s) for that account into running, FIFO. No-op for accounts
		// with no waiting queue (the common case takes a single indexed
		// probe, no lock).
		p.unsubscribes = append(p.unsubscribes, p.eventBus.Subscribe(
			"graph.node.updated.v1:planner:plan",
			p.handleSlotFreed,
			events.WithSubscriberName("planner:admit-next-on-slot-free"),
		))
		// Preempt-on-pass (epic memql#902 / #906). When a Plan with an
		// in-flight turn goes paused/cancelled, send the cross-node
		// AgentPreemptTurn so the agent's tool loop stops at its next
		// checkpoint (and persists taskState for resume). No-op when the
		// Plan isn't mid-execution here.
		p.unsubscribes = append(p.unsubscribes, p.eventBus.Subscribe(
			"graph.node.updated.v1:planner:plan",
			p.handlePlanPreempt,
			events.WithSubscriberName("planner:preempt-on-pass"),
		))
		// trainSpecialist dispatcher (#644). Claims kind=trainSpecialist
		// Plans -- the agent loop skips that kind so the two don't race.
		// Subscribes to both created + updated so a Plan spawned in
		// 'planning' (cron / stale-signal path) AND one flipped to
		// 'running' (mutationStartPlan) both dispatch; the dispatcher's
		// own claim guard dedups the double-fire.
		p.unsubscribes = append(p.unsubscribes, p.eventBus.Subscribe(
			"graph.node.created.v1:planner:plan",
			p.trainDispatch.HandlePlanCreated,
			events.WithSubscriberName("planner:train-specialist-created"),
		))
		p.unsubscribes = append(p.unsubscribes, p.eventBus.Subscribe(
			"graph.node.updated.v1:planner:plan",
			p.trainDispatch.HandlePlanUpdated,
			events.WithSubscriberName("planner:train-specialist-updated"),
		))
		// embedDomainItems dispatcher (#645). Claims kind=embedDomainItems
		// Plans -- the agent loop skips that kind so the two don't race.
		// Subscribes to both created + updated so a Plan spawned in
		// 'planning' (seed / upload path) AND one flipped to 'running'
		// both dispatch; the dispatcher's own claim guard dedups the
		// double-fire.
		p.unsubscribes = append(p.unsubscribes, p.eventBus.Subscribe(
			"graph.node.created.v1:planner:plan",
			p.embedDispatch.HandlePlanCreated,
			events.WithSubscriberName("planner:embed-domain-items-created"),
		))
		p.unsubscribes = append(p.unsubscribes, p.eventBus.Subscribe(
			"graph.node.updated.v1:planner:plan",
			p.embedDispatch.HandlePlanUpdated,
			events.WithSubscriberName("planner:embed-domain-items-updated"),
		))
		// Event-driven stale-signal refresh (#644). When a domain's
		// staleSignalCount crosses the threshold (Planner Agent bumped
		// it via markKnowledgeDomainStale), spawn an immediate refresh
		// Plan instead of waiting for the cadence backstop.
		p.unsubscribes = append(p.unsubscribes, p.eventBus.Subscribe(
			"graph.node.updated.v1:common:knowledgeDomain",
			p.refreshCron.HandleDomainUpdated,
			events.WithSubscriberName("planner:knowledge-stale-signal"),
		))
		// Responsibility intake (#637). A freshly-authored
		// v1:planner:responsibility (status=draft, intakeStatus='')
		// triggers a lightweight reasoning pass that infers the
		// structured field set and surfaces 0-2 clarifying questions;
		// the updated subscription picks up an answers-folded row to
		// finalize + activate.
		p.unsubscribes = append(p.unsubscribes, p.eventBus.Subscribe(
			"graph.node.created.v1:planner:responsibility",
			p.intakeDispatch.HandleResponsibilityCreated,
			events.WithSubscriberName("planner:responsibility-intake-created"),
		))
		p.unsubscribes = append(p.unsubscribes, p.eventBus.Subscribe(
			"graph.node.updated.v1:planner:responsibility",
			p.intakeDispatch.HandleResponsibilityUpdated,
			events.WithSubscriberName("planner:responsibility-intake-updated"),
		))
		// Everyday-task authoring capture (#1161). A user-facing one-off Plan
		// (produceArtifact / adHocAction) reaching succeeded triggers an async
		// post-hoc capture: design -> emit/Gate-1 -> persist a versioned
		// v1:authoring:bundle -> Gate-2 dry-run. Best-effort + default-on; never
		// affects the already-delivered result.
		p.unsubscribes = append(p.unsubscribes, p.eventBus.Subscribe(
			"graph.node.updated.v1:planner:plan",
			p.captureDispatch.HandlePlanUpdated,
			events.WithSubscriberName("planner:authoring-capture-updated"),
		))
		p.mu.Unlock()
		p.logger.Info("planner integration: subscriptions registered",
			"patterns", []string{
				"graph.node.updated.v1:planner:plan (scope-elevation)",
				"graph.node.created.v1:planner:plan (agent loop)",
				"graph.node.updated.v1:planner:plan (agent loop)",
				"graph.node.created.v1:planner:plan (trainSpecialist)",
				"graph.node.updated.v1:planner:plan (trainSpecialist)",
				"graph.node.created.v1:planner:plan (embedDomainItems)",
				"graph.node.updated.v1:planner:plan (embedDomainItems)",
				"graph.node.created.v1:planner:responsibility (intake)",
				"graph.node.updated.v1:planner:responsibility (intake)",
				"graph.node.updated.v1:planner:plan (authoring capture)",
			},
		)
		// Start the daily-refresh cron poller (#644). Polls
		// queryDueRefreshDomains, does the elapsed-time math Go-side,
		// and spawns trainSpecialist(mode='refresh') Plans -- which the
		// dispatcher above then picks up.
		if p.refreshCron != nil {
			p.refreshCron.Start(ctx)
		}
		// Start the reactive planner loop (#638-#641). Polls
		// queryActiveResponsibilitiesAcrossUsers, does the cron / condition
		// due-check Go-side, routes each due responsibility to an agent,
		// honors it per archetype (Plan vs context injection), and runs the
		// per-user goals x responsibilities convergence step.
		if p.reactiveLoop != nil {
			p.reactiveLoop.Start(ctx)
		}
		if p.fairnessCron != nil {
			p.fairnessCron.Start(ctx)
		}
		// Start the stranded-plan watchdog (memql#1389). Polls for Plans
		// stuck in planning/queued past the strand threshold (created event
		// lost) and re-drives them through the agent loop, cross-replica-
		// claimed so exactly one replica recovers each.
		if p.strandedWatchdog != nil {
			p.strandedWatchdog.Start(ctx)
		}
	}
	// Delegate the rest of the lifecycle (health-check ticker,
	// readyCh close, IsRunning bookkeeping) to the base
	// Integration's Start.
	if p.Integration != nil {
		p.Integration.Start(ctx)
	}
}

// Stop unsubscribes from the event bus and delegates to the base
// Integration's Stop. Called by the dependency lifecycle on
// shutdown.
func (p *PlannerIntegration) Stop(ctx context.Context) {
	if p == nil {
		return
	}
	p.mu.Lock()
	for _, unsub := range p.unsubscribes {
		if unsub != nil {
			unsub()
		}
	}
	p.unsubscribes = nil
	p.mu.Unlock()
	if p.refreshCron != nil {
		p.refreshCron.Stop()
	}
	if p.reactiveLoop != nil {
		p.reactiveLoop.Stop()
	}
	if p.fairnessCron != nil {
		p.fairnessCron.Stop()
	}
	if p.strandedWatchdog != nil {
		p.strandedWatchdog.Stop()
	}
	if p.Integration != nil {
		p.Integration.Stop(ctx)
	}
}

// Suppress the unused-import warning for the common package; it's
// referenced by the constant declaration above (ComponentName is
// of type common.ComponentName) but a future refactor that drops
// the const elsewhere might appear unused without this guard.
var _ = common.ComponentName("planner")
