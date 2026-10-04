package node

import (
	"context"
	"log/slog"

	"github.com/znasllc-io/memql/component/bus"
	"github.com/znasllc-io/memql/component/events"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// BootstrapContext provides the shared dependencies that all node bootstraps need.
type BootstrapContext struct {
	Identity *Identity
	Logger   *slog.Logger
	EventBus *events.Bus
	Engine   *memqlengine.MemQLEngine
	Version  string
	Wiring   *bus.Wiring // Channel-based communication wiring (optional)
}

// engineExecutorAdapter satisfies EngineExecutor by delegating to a
// *memqlengine.MemQLEngine, discarding the *ExecuteResult value that
// status transitions do not consume.
type engineExecutorAdapter struct {
	engine *memqlengine.MemQLEngine
}

func (a *engineExecutorAdapter) Execute(ctx context.Context, query string) error {
	if a == nil || a.engine == nil {
		return nil
	}
	_, err := a.engine.Execute(ctx, query)
	return err
}

// installStatusWriter constructs a NodeStatusWriter bound to the engine and
// wires it as the PeerManager's status-change handler. If the engine is
// nil (e.g. in tests) the writer is still constructed but its Handle is a
// no-op.
func installStatusWriter(pm *PeerManager, ctx BootstrapContext) *NodeStatusWriter {
	var exec EngineExecutor
	if ctx.Engine != nil {
		exec = &engineExecutorAdapter{engine: ctx.Engine}
	}
	writer := NewNodeStatusWriter(exec, ctx.Identity, ctx.Logger)
	pm.SetStatusChangeHandler(writer.Handle)
	return writer
}

// NodeBootstrap defines the strategy for creating dependencies per node type.
// Each node type provides its own implementation that only wires the
// components relevant to that node's purpose.
type NodeBootstrap interface {
	// NodeDependencies returns the node-specific dependencies that should
	// be added to the dependency list. Core dependencies (database, engine,
	// event bus) are created by main.go and passed via BootstrapContext.
	NodeDependencies(ctx BootstrapContext) ([]common.Dependency, error)

	// Description returns a human-readable description of this bootstrap.
	Description() string
}

// BootstrapFor returns the appropriate NodeBootstrap for the given node type.
func BootstrapFor(nodeType NodeType) NodeBootstrap {
	switch nodeType {
	case NodeTypeAgent:
		return &AgentBootstrap{}
	case NodeTypePlanner:
		return &PlannerBootstrap{}
	case NodeTypeBFF:
		return &BFFBootstrap{}
	case NodeTypeWorkbench:
		return &WorkbenchBootstrap{}
	case NodeTypeMCP:
		return &MCPBootstrap{}
	default:
		// Default to BFF.
		return &BFFBootstrap{}
	}
}

// DiscoverPeerAddress queries the DB for a healthy peer to connect to.
// If the identity already has a parent address (from MEMQL_PARENT_ADDRESS),
// this is a no-op. Otherwise it reads the cluster topology for any healthy
// node whose address is non-empty and not this node's own address, and
// that relays mesh events (not identity), and sets identity.ParentAddress to
// the first match.
//
// This uses DB-based peer discovery to find mesh peers.
// The first node in a fresh cluster finds no peers, starts as the mesh
// root, and waits for others to discover it via their own DB query.
//
// A DISCOVERED parent is a pod address, and a pod is replaced on every
// rollout. So when discovery supplies the address it also leaves the
// Identity a way to discover again, which the ParentConnector uses once the
// parent stops answering (ErrPeerUnreachable) instead of redialling a pod that
// is gone. On 2026-09-28 a bff redialled the address of an edge pod its
// rollout had replaced every 30 seconds for as long as it ran. An address
// from MEMQL_PARENT_ADDRESS is a Service that always routes to a live pod, so
// it gets no resolver and is redialled as before.
func DiscoverPeerAddress(ctx BootstrapContext) {
	if ctx.Identity == nil || ctx.Identity.ParentAddress != "" {
		return // already configured via env var
	}
	if ctx.Engine == nil {
		return // no engine to query (shouldn't happen in normal bootstrap)
	}

	addr, peerId, err := discoverParentAddress(context.Background(), ctx.Engine, ctx.Identity, "")
	if err != nil {
		if ctx.Logger != nil {
			ctx.Logger.Info("peer discovery: no existing peers found (first node or empty cluster)",
				"error", err)
		}
		return
	}
	logger := ctx.Logger
	if addr == "" {
		// The mesh root: no parent now, and no ParentConnector to lose one.
		if logger != nil {
			logger.Info("peer discovery: no healthy peers found (this node will accept inbound connections)")
		}
		return
	}
	engine := ctx.Engine
	identity := ctx.Identity
	ctx.Identity.ParentAddress = addr
	ctx.Identity.parentResolver = func(rctx context.Context, exclude string) (string, bool) {
		next, _, err := discoverParentAddress(rctx, engine, identity, exclude)
		if err != nil {
			if logger != nil {
				logger.Warn("peer discovery: re-resolving the parent failed", "error", err)
			}
			return "", false
		}
		return next, next != ""
	}
	if logger != nil {
		logger.Info("peer discovery: discovered peer from DB",
			"peer_address", addr,
			"peer_id", peerId)
	}
}

// parentTopologyQuery is the topology read parent discovery makes: the LATEST
// row per node, nodes already marked stopped left out. The raw
// `concept==v1:cluster:node` scan it replaces returns historical versions
// (capped, see WorkerDialer.discoverFromDB), so a pod that has since gone
// offline could still look healthy to it.
const parentTopologyQuery = "query staleClusterNodes()"

// topologyReader is the one engine call parent discovery makes.
type topologyReader interface {
	Execute(ctx context.Context, query string) (*memqlengine.ExecuteResult, error)
}

// discoverParentAddress returns the NodeService address of a peer this node
// can take as its parent, or "" when the topology offers none. It skips this
// node itself, `exclude` (the address that just stopped answering), a node
// that relays no mesh events, and any node not healthy or connecting.
func discoverParentAddress(ctx context.Context, engine topologyReader, identity *Identity, exclude string) (addr, peerId string, err error) {
	result, err := engine.Execute(ctx, parentTopologyQuery)
	if err != nil {
		return "", "", err
	}
	if result == nil || result.Bundle == nil {
		return "", "", nil
	}

	selfAddr := identity.Address
	selfId := identity.ID
	for _, n := range result.Bundle.Nodes {
		nodeId := n.GetId()
		if nodeId == selfId || nodeId == "v1:cluster:node:"+selfId {
			continue
		}
		// Parse payload for address and health.
		payloadStruct := n.GetPayload()
		if payloadStruct == nil {
			continue
		}
		fields := payloadStruct.GetFields()
		addr := fields["address"].GetStringValue()
		health := fields["health"].GetStringValue()
		if addr == "" || addr == selfAddr || addr == exclude {
			continue
		}
		// NEVER A NODE THAT RELAYS NOTHING (memql#5338, D8). A parent is often
		// a node's only stream to the mesh -- mcp's, in the cloud -- and
		// identity takes no mesh events, so it relays none: a node parented on
		// it would hear identity's own events and nothing else, an island by
		// construction. Every other node type relays.
		if !takesMeshEvents(NodeType(fields["nodeType"].GetStringValue())) {
			continue
		}
		// Accept healthy or connecting nodes.
		if health == "healthy" || health == "connecting" || health == "" {
			return addr, nodeId, nil
		}
	}
	return "", "", nil
}
