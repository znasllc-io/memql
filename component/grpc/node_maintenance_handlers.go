package memql

import (
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// NodeMaintenanceHandler is the narrow port the operator maintenance
// trigger (memql#1270) drives. app/ wires a concrete implementation that
// funnels into the App's RequestOperatorDrain -- i.e. into the IDENTICAL
// graceful-drain sequence the deploy SIGTERM path runs (memql#1269) --
// rather than re-implementing any drain logic here. component/grpc can't
// import app/ or component/node's lifecycle, so the App satisfies this
// interface from the wiring layer (same pattern as AgentTurnHandler /
// agentPauseHook).
//
// Implementations must be safe for concurrent use and idempotent:
// BeginDrain may race a SIGTERM or a second operator call, and both must
// converge on the one drain already under way.
type NodeMaintenanceHandler interface {
	// NodeID returns the local node's id so the reply confirms which node
	// drained.
	NodeID() string
	// CurrentState returns the node's lifecycle state (lowercase:
	// starting/ready/draining/stopped/unknown).
	CurrentState() string
	// BeginDrain triggers the operator-initiated graceful drain, returning
	// true if THIS call initiated it and false if the node was already
	// draining (a no-op success).
	BeginDrain(reason string) bool
}

// SetNodeMaintenanceHandler installs the operator-maintenance lifecycle
// driver (memql#1270). Wired on mesh binaries during cluster bootstrap;
// non-mesh binaries leave it nil and the NodeMaintenance handler reports
// "unavailable". Mirrors SetAgentTurnHandler: updates both the Server field
// (picked up by the next service construction in Run) and the live service
// reference (so a post-Run install still takes effect).
func (s *Server) SetNodeMaintenanceHandler(h NodeMaintenanceHandler) {
	if s == nil {
		return
	}
	s.nodeMaintenanceHandler = h
	if s.serviceRef != nil && s.serviceRef.svc != nil {
		s.serviceRef.svc.nodeMaintenanceHandler = h
	}
}

// handleNodeMaintenance is the owner/admin-gated landing for the on-demand
// operator maintenance trigger (memql#1270, epic memql#1259 decision #4).
// It puts THIS node into maintenance/Draining on command by funneling into
// the SAME drain mechanism the deploy SIGTERM path uses -- there is no
// separate drain code path. Flow:
//
//  1. Resolve the caller's AccessContext and reject anyone who is not a
//     cluster owner or admin (never an open endpoint).
//  2. Validate the action ("drain" is the only v1 action; Draining is a
//     forward-only lifecycle edge, so there is no "undrain").
//  3. Call the wired NodeMaintenanceHandler.BeginDrain, which triggers the
//     App's operator-drain channel -- app.Run's wait path selects on it and
//     runs the identical Draining -> drain-delay -> in-flight-wait ->
//     Stopped -> Stop-sweep sequence #1269 runs on SIGTERM.
//  4. Reply with ok + the node id + the post-trigger lifecycle state so the
//     ordered-rollout driver can confirm + sequence.
func (s *streamSession) handleNodeMaintenance(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.NodeMaintenanceMsg) error {
	requestId := ""
	if msg != nil {
		requestId = msg.GetRequestId()
	}
	correlate := envelope.GetMessageId()

	send := func(result *memqlv1.NodeMaintenanceResult) error {
		result.RequestId = requestId
		return s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_NodeMaintenanceResult{
				NodeMaintenanceResult: result,
			},
		})
	}

	// Owner/admin gate. Resolve the caller's role from the stream's
	// AccessContext; fail closed when it is missing or below admin.
	ac := s.ensureAccess(s.stream.Context())
	if !isOwnerOrAdmin(ac) {
		if s.logger != nil {
			role := ""
			if ac != nil {
				role = string(ac.Role)
			}
			s.logger.Warn("node maintenance trigger rejected -- caller not owner/admin",
				"role", role, "request_id", requestId)
		}
		return send(&memqlv1.NodeMaintenanceResult{
			Ok:           false,
			ErrorCode:    "permission_denied",
			ErrorMessage: "node maintenance requires a cluster owner or admin",
		})
	}

	// Unavailable when this binary has no maintenance entrypoint (non-mesh
	// builds, or a node whose lifecycle wasn't wired).
	h := s.service.nodeMaintenanceHandler
	if h == nil {
		return send(&memqlv1.NodeMaintenanceResult{
			Ok:           false,
			ErrorCode:    "unavailable",
			ErrorMessage: "this node has no maintenance entrypoint (non-mesh binary)",
		})
	}

	action := strings.TrimSpace(strings.ToLower(msg.GetAction()))
	if action == "" {
		action = "drain"
	}
	if action != "drain" {
		return send(&memqlv1.NodeMaintenanceResult{
			Ok:           false,
			NodeId:       h.NodeID(),
			State:        h.CurrentState(),
			ErrorCode:    "bad_request",
			ErrorMessage: "unknown action " + action + " (only \"drain\" is supported)",
		})
	}

	reason := strings.TrimSpace(msg.GetReason())
	if s.logger != nil {
		s.logger.Warn("operator maintenance drain accepted",
			"node_id", h.NodeID(), "reason", reason, "request_id", requestId)
	}

	initiated := h.BeginDrain(reason)

	return send(&memqlv1.NodeMaintenanceResult{
		Ok:              true,
		AlreadyDraining: !initiated,
		NodeId:          h.NodeID(),
		State:           h.CurrentState(),
	})
}

// isOwnerOrAdmin reports whether the resolved AccessContext is a cluster
// owner or admin -- the gate for operator maintenance (memql#1270).
func isOwnerOrAdmin(ac *auth.AccessContext) bool {
	if ac == nil {
		return false
	}
	return ac.Role == auth.RoleOwner || ac.Role == auth.RoleAdmin
}
