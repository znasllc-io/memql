package node

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
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
// (see mutations/v1/cluster/updateNodeHealth.memql). That insert
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

		if err := w.persistHealthTransition(execCtx, query, nodeId, nodeType, address, healthLabel, lastSeenISO); err != nil {
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

// Status-write resilience knobs (#1755). The initial health write for a peer
// can hit a transient DB error during a full-stack roll (e.g. SQLSTATE 53300
// too_many_connections, the #1753 surge); a bounded retry rides over it
// instead of dropping the write.
const (
	statusWriteMaxAttempts  = 3
	statusWriteRetryBackoff = 200 * time.Millisecond
)

// persistHealthTransition upserts a peer's v1:cluster:node health row (#1755).
//
// The normal path is the read-merge updateNodeHealth (preserves
// nodeType/address/capabilities/labels). But that depends on the row already
// existing -- created by the peer's own registerNode at startup. When that
// initial write never committed (the peer's registration, or its first
// transition, hit a transient DB error during a roll), the row is missing and
// EVERY subsequent update fails permanently with "no existing row ... use
// insert()", wedging health tracking for that peer for its whole lifetime.
//
// So when the update reports a missing row we fall back to an insert
// (createNode) carrying this transition's health -- the row self-heals
// instead of staying wedged. Transient DB errors on either call are retried
// with a bounded backoff.
func (w *NodeStatusWriter) persistHealthTransition(ctx context.Context, updateQuery, nodeId, nodeType, address, health, lastSeenISO string) error {
	err := w.execWithRetry(ctx, updateQuery)
	if err == nil || !isMissingRowErr(err) {
		return err
	}

	// Row doesn't exist yet -> insert it carrying this transition's health so
	// the next transition's update finds a row to read-merge.
	createQuery, cerr := buildCreateNodeHealthCall(nodeId, nodeType, address, health, lastSeenISO)
	if cerr != nil {
		return cerr
	}
	w.logger.Warn("status_writer: health row missing; inserting to self-heal",
		"peer_id", nodeId,
		"health", health,
	)
	return w.execWithRetry(ctx, createQuery)
}

// execWithRetry runs a mutation, retrying only on transient DB errors with a
// linear backoff. Non-transient errors (including "no existing row") return
// immediately so the caller can branch on them.
func (w *NodeStatusWriter) execWithRetry(ctx context.Context, query string) error {
	var err error
	for attempt := 1; attempt <= statusWriteMaxAttempts; attempt++ {
		if err = w.engine.Execute(ctx, query); err == nil || !isTransientErr(err) {
			return err
		}
		if attempt == statusWriteMaxAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt) * statusWriteRetryBackoff):
		}
	}
	return err
}

// isMissingRowErr reports whether an update failed because the target row does
// not exist (the engine's update() guard: "no existing row ... use insert()").
func isMissingRowErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no existing row")
}

// isTransientErr reports whether a DB error is worth retrying: connection-slot
// exhaustion (SQLSTATE 53300, the #1753 roll surge that triggers #1755) or a
// transient connection drop / recovery. Deliberately conservative -- a
// logic/validation error (incl. "no existing row") must NOT be retried.
func isTransientErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "53300") || // too_many_connections (the #1753 surge)
		strings.Contains(s, "too many clients already") ||
		strings.Contains(s, "connection refused") ||
		strings.Contains(s, "connection reset") ||
		strings.Contains(s, "the database system is") // starting up / in recovery
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

	rendered, err := renderNodeMutationArgs(args)
	if err != nil {
		return "", err
	}
	return "updateNodeHealth(" + rendered + ")", nil
}

// renderNodeMutationArgs renders a string-valued arg map as the named-args
// invocation body `k: "v", ...` (Story 9 / #2335: the kind-prefixed call form
// `name(k: "v")` drops the legacy object-literal `{...}` wrapper, which the
// parser now rejects). Each value is JSON-encoded for safe escaping; keys
// sorted for a deterministic call string.
func renderNodeMutationArgs(args map[string]string) (string, error) {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		v, err := json.Marshal(args[k])
		if err != nil {
			return "", err
		}
		b.WriteString(k)
		b.WriteString(": ")
		b.Write(v)
	}
	return b.String(), nil
}

// buildCreateNodeHealthCall formats the insert fallback (#1755): when the
// read-merge update reports a missing row, this inserts the v1:cluster:node row
// under the peer's NodeId carrying this transition's health, so health tracking
// self-heals instead of wedging. Shares the createNode mutation the peer's own
// registerNode uses, so the row shape is identical.
func buildCreateNodeHealthCall(nodeId, nodeType, address, health, lastSeenISO string) (string, error) {
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
	rendered, err := renderNodeMutationArgs(args)
	if err != nil {
		return "", err
	}
	return "createNode(" + rendered + ")", nil
}
