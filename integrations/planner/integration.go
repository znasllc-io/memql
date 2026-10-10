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
// The split mirrors the deployed cluster topology: the
// planner runs as a separate node-type binary
// (`make planner`) and only loads what's relevant to plan
// execution. Putting plan-execution code in cognition means the
// planner binary doesn't have it -- which would be wrong.
package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
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
	// InvokeAIStructured constrains planning decisions through the normal routed, metered model path.
	InvokeAIStructured(ctx context.Context, templateId string, data map[string]any, schemaName string, schema json.RawMessage, strict bool) (string, error)
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
		principal auth.ForwardedPrincipal,
		envelope *memqlv1.MemqlClientMessage,
	) (<-chan *memqlv1.MemqlServerMessage, error)

	ForwardContinuation(
		requestId string,
		principal auth.ForwardedPrincipal,
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
	// dbGetter is the lazy *bun.DB accessor. The two consumers it had --
	dbGetter        func() *bun.DB
	directDBGetter  func() *bun.DB
	agentLoop       *PlannerAgentLoop
	workGoals       responsibilityGoals
	intakeDispatch  *ResponsibilityIntakeDispatcher
	captureDispatch *AuthoringCaptureDispatcher
	refreshCron     *RefreshCron
	reactiveLoop    *ReactiveLoop
	logger          *slog.Logger
	unsubscribes    []func()
	started         atomic.Bool
	mu              sync.Mutex
	// clusterClaimer is the cross-replica execution claim (memql#1363). Its
	// consumer was executeApprovedPlan, which went with the loop; the option
	// is kept for the same reason dbGetter is.
	clusterClaimer ExecutionClaimer
	workflowClaims workflowClaimer
}

// PlannerArg is a functional option for NewPlannerIntegration.
type PlannerArg func(*PlannerIntegration)

// WithEngine wires the engine adapter the integration uses for
// planById + updatePlanStatus + insert AI utterance.
func WithEngine(engine Engine) PlannerArg {
	return func(p *PlannerIntegration) { p.engine = engine }
}

// WithEventBus wires the event bus the integration subscribes on
// for graph.node.updated.v1:planner:responsibility events.
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
	p.agentLoop = NewPlannerAgentLoop(p.engine, p.logger)
	p.intakeDispatch = NewResponsibilityIntakeDispatcher(p.engine, p.logger)
	// Authoring capture (epic memql#1160, re-pointed at runs in memql#5050):
	// transcribes the tool calls a completed RUN recorded into a stored,
	// versioned v1:authoring:bundle. Default-on, gated by
	// MEMQL_AUTHORING_CAPTURE_ENABLED, and free -- transcription reaches no
	// model.
	p.captureDispatch = NewAuthoringCaptureDispatcher(p.agentLoop, p.engine, p.logger)
	p.refreshCron = NewRefreshCron(p.engine, p.logger)
	p.refreshCron.claims = p.workflowClaims
	p.reactiveLoop = NewReactiveLoop(p.engine, p.logger)
	p.reactiveLoop.claims = p.workflowClaims
	p.reactiveLoop.writeGate = func(ctx context.Context, id string) (func(), error) {
		if p.directDBGetter == nil || p.directDBGetter() == nil {
			return nil, fmt.Errorf("standing directive requires a shared database")
		}
		return memql.AcquireWriteGate(ctx, p.directDBGetter().DB, "agent-directive:"+strings.TrimPrefix(id, "v1:agents:agent:"))
	}
	return p, nil
}

// SetAgentForwarder installs the agent-turn forwarder. Called from
// app.cluster after the AiForwardRouter is constructed. Without a
// forwarder, plan-execution dispatches log a warning and skip.
// SetWorkGoals hands the reactive loop the work spine (memql#5000).
//
// SEPARATE FROM SetCompiler even though both are wired in the same breath and
// take the same object: the compiler is what the work spine asks OF the
// planner, and this is what the planner asks of the work spine. Collapsing
// them would make a node that has one and not the other unrepresentable, and
// the warning that names which half is missing impossible to write.
func (p *PlannerIntegration) SetWorkGoals(g responsibilityGoals) {
	if p == nil {
		return
	}
	// Held on the integration as well as handed to the loop: the refresh
	// cadence and the approved-training gate open goals too (memql#5051), and
	// they are not the reactive loop.
	p.mu.Lock()
	if p.workGoals == nil {
		p.workGoals = g
	}
	p.mu.Unlock()
	if p.reactiveLoop != nil {
		p.reactiveLoop.SetWorkGoals(g)
	}
	if p.agentLoop != nil {
		p.agentLoop.SetWorkGoals(g)
	}
	if p.refreshCron != nil {
		p.refreshCron.SetWorkGoals(g)
	}
}

// workGoalsRef is the goal opener, or nil on a node without the work spine.
func (p *PlannerIntegration) workGoalsRef() responsibilityGoals {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.workGoals
}

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
		// EVERY v1:planner:plan SUBSCRIPTION IS GONE (memql#5052). This block
		// held five: the approved-plan executor, the agent loop's
		// created/updated pair, the admit-next-on-slot-free handler and the
		// preempt-on-pass handler. All of them read a Plan to decide, and
		// there are no Plans.
		//
		// What remains below subscribes to responsibilities and to work runs.
		// Authoring capture (#1161, re-pointed in memql#5050). A RUN reaching
		// succeeded triggers an async post-hoc transcription of the tool calls
		// it recorded into a versioned v1:authoring:bundle. Best-effort +
		// default-on; never affects the already-delivered result, and reaches
		// no model.
		p.unsubscribes = append(p.unsubscribes, p.eventBus.Subscribe(
			"graph.node.updated.v1:work:run",
			p.captureDispatch.HandleRunUpdated,
			events.WithSubscriberName("planner:authoring-capture-updated"),
		))
		p.mu.Unlock()
		p.logger.Info("planner integration: subscriptions registered",
			"patterns", []string{
				"graph.node.created.v1:planner:responsibility (intake)",
				"graph.node.updated.v1:planner:responsibility (intake)",
				"graph.node.updated.v1:work:run (authoring capture)",
			},
		)
		// Refresh and responsibility sweeps run through the DSL scheduler.
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

	if p.Integration != nil {
		p.Integration.Stop(ctx)
	}
}

// Suppress the unused-import warning for the common package; it's
// referenced by the constant declaration above (ComponentName is
// of type common.ComponentName) but a future refactor that drops
// the const elsewhere might appear unused without this guard.
var _ = common.ComponentName("planner")

// WithWorkflowClaims installs durable, fail-closed admission for scheduled workflow effects.
func WithWorkflowClaims(claims workflowClaimer) PlannerArg {
	return func(p *PlannerIntegration) { p.workflowClaims = claims }
}
