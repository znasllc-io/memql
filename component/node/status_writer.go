package node

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

// EngineExecutor is the narrow interface the NodeStatusWriter needs on the
// MemQL engine: the ability to execute a MemQL query/mutation string. The
// return value is deliberately opaque; status transitions don't consume
// the result. A concrete implementation wraps memql.MemQLEngine (see
// bootstrap files) or is stubbed in tests.
type EngineExecutor interface {
	Execute(ctx context.Context, query string) error
}

// NodeStatusWriter bridges PeerManager status transitions to the
// v1:cluster:node concept store. It is installed as PeerManager's
// StatusChangeHandler in the bootstrap phase.
//
// On every transition it schedules a MemQL `updateNodeHealth` mutation
// (see mutations/v1/cluster/mutationUpdateNodeHealth.memql). That insert
// shares its `id` with the peer's NodeId so the concept records a
// time-series of health changes for that node. The engine's existing CDC
// path emits graph.node.created.<partition>.v1:cluster:node, which the
// CLI subscribes to for live topology updates.
type NodeStatusWriter struct {
	engine EngineExecutor
	logger *slog.Logger

	// self identifies this local node so the writer can also persist its
	// own lifecycle transitions (not just peer transitions). Optional.
	self *Identity
}

// NewNodeStatusWriter constructs a writer bound to the given engine.
// Passing a nil engine returns a writer whose Handle is a no-op -- useful
// in unit tests that exercise PeerManager without a DB.
func NewNodeStatusWriter(engine EngineExecutor, self *Identity, logger *slog.Logger) *NodeStatusWriter {
	return &NodeStatusWriter{
		engine: engine,
		logger: logger,
		self:   self,
	}
}

// Handle implements StatusChangeHandler. It is invoked synchronously from
// PeerManager; we dispatch the actual engine.Execute on a fresh goroutine
// so we never block the peer-manager tick.
func (w *NodeStatusWriter) Handle(ctx context.Context, peer *nodev1.PeerInfo, old, new nodev1.NodeHealthStatus, lastSeen time.Time) {
	if w == nil || w.engine == nil || peer == nil {
		return
	}

	// Snapshot values we need on the async goroutine.
	nodeId := peer.NodeId
	nodeType := peer.NodeType
	address := peer.Address
	healthLabel := HealthLabel(new)
	lastSeenISO := lastSeen.UTC().Format(time.RFC3339)

	// Run on its own goroutine. Use context.Background to avoid inheriting
	// the possibly-already-cancelled caller context (a liveness tick's
	// context is the PeerManager lifecycle context, which is fine, but a
	// test caller may pass something shorter).
	go func() {
		query, err := buildUpdateNodeHealthCall(nodeId, nodeType, address, healthLabel, lastSeenISO)
		if err != nil {
			w.logger.Error("status_writer: failed to build mutation call",
				"peer_id", nodeId,
				"error", err,
			)
			return
		}

		execCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		// Inject a system actor so the engine's mutation path accepts the
		// write. The executor's mutationActor helper requires a non-empty
		// actor; this is equivalent to how the registerNode automation's
		// writes are attributed during startup.
		execCtx = auth.ContextWithToken(execCtx, &auth.TokenInfo{
			Subject: "system:node-status-writer",
		})

		if err := w.engine.Execute(execCtx, query); err != nil {
			w.logger.Error("status_writer: failed to persist health transition",
				"peer_id", nodeId,
				"from", old.String(),
				"to", new.String(),
				"error", err,
			)
			return
		}

		w.logger.Info("status_writer: persisted health transition",
			"peer_id", nodeId,
			"from", old.String(),
			"to", new.String(),
		)
	}()
}

// HealthLabel converts a NodeHealthStatus enum value to the lowercase
// string form stored in the v1:cluster:node concept's `health` enum.
// The proto enum is the source of truth; callers must go through this
// function rather than hard-coding strings elsewhere.
func HealthLabel(s nodev1.NodeHealthStatus) string {
	switch s {
	case nodev1.NodeHealthStatus_NODE_HEALTH_CONNECTING:
		return "connecting"
	case nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY:
		return "healthy"
	case nodev1.NodeHealthStatus_NODE_HEALTH_DEGRADED:
		return "degraded"
	case nodev1.NodeHealthStatus_NODE_HEALTH_DRAINING:
		return "draining"
	case nodev1.NodeHealthStatus_NODE_HEALTH_OFFLINE:
		return "offline"
	case nodev1.NodeHealthStatus_NODE_HEALTH_STOPPED:
		return "stopped"
	default:
		return "connecting"
	}
}

// ParseHealthLabel maps the lowercase concept enum form back to the proto
// enum. Unknown values return CONNECTING (safe transient state).
func ParseHealthLabel(s string) nodev1.NodeHealthStatus {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "connecting":
		return nodev1.NodeHealthStatus_NODE_HEALTH_CONNECTING
	case "healthy", "online":
		return nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY
	case "degraded":
		return nodev1.NodeHealthStatus_NODE_HEALTH_DEGRADED
	case "draining":
		return nodev1.NodeHealthStatus_NODE_HEALTH_DRAINING
	case "offline", "down":
		return nodev1.NodeHealthStatus_NODE_HEALTH_OFFLINE
	case "stopped":
		return nodev1.NodeHealthStatus_NODE_HEALTH_STOPPED
	default:
		return nodev1.NodeHealthStatus_NODE_HEALTH_CONNECTING
	}
}

// buildUpdateNodeHealthCall formats the MemQL mutation function invocation
// that persists a health transition. Exported via a package-private helper
// so status_writer_test.go can assert on the generated string without
// spinning up the engine.
func buildUpdateNodeHealthCall(nodeId, nodeType, address, health, lastSeenISO string) (string, error) {
	if strings.TrimSpace(nodeId) == "" {
		return "", fmt.Errorf("status_writer: nodeId is required")
	}
	if strings.TrimSpace(nodeType) == "" {
		return "", fmt.Errorf("status_writer: nodeType is required")
	}
	if strings.TrimSpace(health) == "" {
		return "", fmt.Errorf("status_writer: health is required")
	}

	args := map[string]string{
		"id":       nodeId,
		"nodeType": nodeType,
		"address":  address,
		"health":   health,
		"lastSeen": lastSeenISO,
	}

	// Serialize as JSON for safe escaping of values that may contain
	// quotes or special chars. MemQL mutation function args accept a JSON
	// object literal as the positional argument (same syntax the engine
	// parses for other mutation calls).
	body, err := json.Marshal(args)
	if err != nil {
		return "", err
	}
	return "mutationUpdateNodeHealth(" + string(body) + ")", nil
}
