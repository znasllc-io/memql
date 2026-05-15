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

	"github.com/visionarys-io/memql/component/events"
	memqlv1 "github.com/visionarys-io/memql/component/grpc/gen"
	"github.com/visionarys-io/memql/component/node"
	"github.com/visionarys-io/memql/core/common"
	"github.com/visionarys-io/memql/integrations"
)

// ComponentName is the planner integration's logger / dependency
// component identifier.
const ComponentName common.ComponentName = "plannerIntegration"

// Engine is the narrow MemQL surface the planner integration uses.
// Mirrors the cognition.MemQLEngine pattern -- adapter-based interface
// so the integration doesn't import the full engine package.
type Engine interface {
	Execute(ctx context.Context, query string) (any, error)
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
	logger         *slog.Logger
	unsubscribes   []func()
	started        atomic.Bool
	mu             sync.Mutex
}

// PlannerArg is a functional option for NewPlannerIntegration.
type PlannerArg func(*PlannerIntegration)

// WithEngine wires the engine adapter the integration uses for
// queryPlanById + mutationUpdatePlanStatus + insert SI utterance.
func WithEngine(engine Engine) PlannerArg {
	return func(p *PlannerIntegration) { p.engine = engine }
}

// WithEventBus wires the event bus the integration subscribes on
// for graph.node.updated.*.v1:planner:plan events.
func WithEventBus(bus *events.Bus) PlannerArg {
	return func(p *PlannerIntegration) { p.eventBus = bus }
}

// WithLogger overrides the default logger.
func WithLogger(logger *slog.Logger) PlannerArg {
	return func(p *PlannerIntegration) { p.logger = logger }
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
			"graph.node.updated.*.v1:planner:plan",
			p.handlePlanApprovedForExecution,
			events.WithSubscriberName("planner:plan-execution"),
		))
		p.mu.Unlock()
		p.logger.Info("planner integration: plan-execution subscription registered",
			"pattern", "graph.node.updated.*.v1:planner:plan",
		)
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
