package node

import (
	"fmt"
	"sync"

	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

// LifecycleState is the node's explicit operational state in the cluster
// (epic memql#1259 decision #4). It is distinct from common.Lifecycle (which
// is the component start/stop machinery) and from a peer's observed
// NodeHealthStatus: LifecycleState is the SELF-asserted intent of THIS node,
// which is what gets advertised in gossip (NodeHeartbeat.health / PeerInfo.health)
// so peers can route by it -- only deliver new work to Ready peers and route
// AROUND Draining/Stopped immediately instead of after a missed-heartbeat
// timeout.
//
// The legal progression is strictly forward:
//
//	Starting -> Ready -> Draining -> Stopped
//
// A node boots in Starting, flips to Ready once it can actually serve, flips
// to Draining when it begins a graceful shutdown (deploy SIGTERM or operator
// trigger -- the TRIGGERS land in memql#1269 / #1270; this type only provides
// the mechanism), and finally Stopped. There is no backward edge: a node never
// un-drains, and Stopped is terminal. Idempotent self-transitions (X -> X) are
// allowed and are no-ops.
type LifecycleState int

const (
	// LifecycleStarting is the boot state: dependencies are wiring up and the
	// node is NOT yet ready to serve. Readiness reports not-ready.
	LifecycleStarting LifecycleState = iota

	// LifecycleReady means the node is serving normally. Readiness reports
	// ready (subject to the other registered readiness invariants).
	LifecycleReady

	// LifecycleDraining means the node has begun a graceful shutdown: it is
	// still ALIVE (liveness stays 200 on /livez) and finishing in-flight work,
	// but readiness reports NOT-ready so load balancers / k8s de-route new
	// traffic and peers route around it at once. The drain BEHAVIOUR itself
	// (finish in-flight, flush, close) is memql#1269; this state is only the
	// advertised flag.
	LifecycleDraining

	// LifecycleStopped is terminal: the node has finished draining and is
	// going away. Readiness reports not-ready.
	LifecycleStopped
)

// String renders the lifecycle state for logs + the gossip-derived health
// label. Lowercase to match HealthLabel's existing convention.
func (s LifecycleState) String() string {
	switch s {
	case LifecycleStarting:
		return "starting"
	case LifecycleReady:
		return "ready"
	case LifecycleDraining:
		return "draining"
	case LifecycleStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

// Health maps a lifecycle state to the NodeHealthStatus advertised in gossip
// (NodeHeartbeat.health / PeerInfo.health). This is the bridge between the
// self-asserted lifecycle and the wire enum peers already understand, so the
// gossip contract stays backward-compatible -- existing peers keep reading
// NodeHealthStatus, they just now learn DRAINING / STOPPED earlier and more
// explicitly.
//
//   - Starting -> CONNECTING (booting, not yet serving)
//   - Ready    -> HEALTHY    (serving)
//   - Draining -> DRAINING   (de-routed, finishing in-flight)
//   - Stopped  -> STOPPED    (gone)
func (s LifecycleState) Health() nodev1.NodeHealthStatus {
	switch s {
	case LifecycleStarting:
		return nodev1.NodeHealthStatus_NODE_HEALTH_CONNECTING
	case LifecycleReady:
		return nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY
	case LifecycleDraining:
		return nodev1.NodeHealthStatus_NODE_HEALTH_DRAINING
	case LifecycleStopped:
		return nodev1.NodeHealthStatus_NODE_HEALTH_STOPPED
	default:
		return nodev1.NodeHealthStatus_NODE_HEALTH_UNSPECIFIED
	}
}

// legalLifecycleNext lists the forward edges out of each state. A self-edge
// (X -> X) is always legal as an idempotent no-op and is handled in Transition
// without being listed here.
var legalLifecycleNext = map[LifecycleState][]LifecycleState{
	LifecycleStarting: {LifecycleReady, LifecycleDraining, LifecycleStopped},
	LifecycleReady:    {LifecycleDraining, LifecycleStopped},
	LifecycleDraining: {LifecycleStopped},
	LifecycleStopped:  {},
}

// canTransition reports whether from -> to is a legal edge (including the
// idempotent self-edge).
func canTransition(from, to LifecycleState) bool {
	if from == to {
		return true
	}
	for _, next := range legalLifecycleNext[from] {
		if next == to {
			return true
		}
	}
	return false
}

// NodeLifecycle is the concurrency-safe holder for a node's LifecycleState.
// The node is multi-goroutine (heartbeat tickers, the shutdown signal handler,
// the readiness HTTP handler) so every read/write goes through the mutex.
//
// A NodeLifecycle optionally notifies an observer on every ACTUAL state change
// (idempotent self-transitions do not fire it) via SetObserver. The node wires
// this so a flip to Draining immediately re-advertises the new gossip health
// rather than waiting for the next heartbeat tick.
type NodeLifecycle struct {
	mu       sync.RWMutex
	state    LifecycleState
	observer func(old, new LifecycleState)
}

// NewNodeLifecycle returns a lifecycle starting in LifecycleStarting -- the
// correct boot state. The node flips it to Ready once it can serve.
func NewNodeLifecycle() *NodeLifecycle {
	return &NodeLifecycle{state: LifecycleStarting}
}

// SetObserver installs a callback invoked (without the lock held) on every
// actual state change. Passing nil clears it. Thread-safe.
func (l *NodeLifecycle) SetObserver(fn func(old, new LifecycleState)) {
	l.mu.Lock()
	l.observer = fn
	l.mu.Unlock()
}

// State returns the current lifecycle state. Concurrency-safe.
func (l *NodeLifecycle) State() LifecycleState {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.state
}

// Health returns the gossip NodeHealthStatus for the current state.
// Concurrency-safe convenience for the heartbeat builders.
func (l *NodeLifecycle) Health() nodev1.NodeHealthStatus {
	return l.State().Health()
}

// IsDraining reports whether the node has entered Draining (or the terminal
// Stopped). The readiness handler consults this so a draining node reports
// NOT-ready while still alive on /livez (readiness != liveness).
func (l *NodeLifecycle) IsDraining() bool {
	s := l.State()
	return s == LifecycleDraining || s == LifecycleStopped
}

// IsReady reports whether the node is in the Ready state (serving). The
// readiness handler gates on this so a node only reports ready once it has
// actually flipped to Ready and has not begun draining.
func (l *NodeLifecycle) IsReady() bool {
	return l.State() == LifecycleReady
}

// Transition moves the node to the target state, guarding illegal edges. A
// legal forward edge (or an idempotent self-edge) returns nil; an illegal edge
// (e.g. Draining -> Ready, or anything out of Stopped) returns an error and
// leaves the state unchanged. On an ACTUAL change (not a self-edge) the
// observer fires after the lock is released.
func (l *NodeLifecycle) Transition(to LifecycleState) error {
	l.mu.Lock()
	from := l.state
	if !canTransition(from, to) {
		l.mu.Unlock()
		return fmt.Errorf("illegal node lifecycle transition: %s -> %s", from, to)
	}
	changed := from != to
	if changed {
		l.state = to
	}
	observer := l.observer
	l.mu.Unlock()

	if changed && observer != nil {
		observer(from, to)
	}
	return nil
}

// MarkReady transitions to Ready. Convenience wrapper over Transition.
func (l *NodeLifecycle) MarkReady() error { return l.Transition(LifecycleReady) }

// MarkDraining transitions to Draining -- the mechanism the graceful-drain
// (memql#1269) and operator (memql#1270) triggers call. This issue only
// provides the mechanism; the triggers are downstream.
func (l *NodeLifecycle) MarkDraining() error { return l.Transition(LifecycleDraining) }

// MarkStopped transitions to Stopped (terminal). Convenience wrapper.
func (l *NodeLifecycle) MarkStopped() error { return l.Transition(LifecycleStopped) }
