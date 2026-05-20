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
	case NodeTypeCognition:
		return &CognitionBootstrap{}
	case NodeTypeAgent:
		return &AgentBootstrap{}
	case NodeTypePlanner:
		return &PlannerBootstrap{}
	case NodeTypeBFF:
		return &BFFBootstrap{}
	case NodeTypeVoice:
		return &VoiceBootstrap{}
	case NodeTypeWorkbench:
		return &WorkbenchBootstrap{}
	default:
		// Default to BFF.
		return &BFFBootstrap{}
	}
}

// DiscoverPeerAddress queries the DB for a healthy peer to connect to.
// If the identity already has a parent address (from MEMQL_PARENT_ADDRESS),
// this is a no-op. Otherwise it queries v1:cluster:node for any healthy
// node whose address is non-empty and not this node's own address, and
// sets identity.ParentAddress to the first match.
//
// This uses DB-based peer discovery to find mesh peers.
// The first node in a fresh cluster finds no peers, starts as the mesh
// root, and waits for others to discover it via their own DB query.
func DiscoverPeerAddress(ctx BootstrapContext) {
	if ctx.Identity == nil || ctx.Identity.ParentAddress != "" {
		return // already configured via env var
	}
	if ctx.Engine == nil {
		return // no engine to query (shouldn't happen in normal bootstrap)
	}

	result, err := ctx.Engine.Execute(context.Background(), "concept==v1:cluster:node")
	if err != nil {
		if ctx.Logger != nil {
			ctx.Logger.Info("peer discovery: no existing peers found (first node or empty cluster)",
				"error", err)
		}
		return
	}
	if result == nil || result.Bundle == nil || len(result.Bundle.Nodes) == 0 {
		if ctx.Logger != nil {
			ctx.Logger.Info("peer discovery: no existing peers in DB (this is the first node)")
		}
		return
	}

	selfAddr := ctx.Identity.Address
	selfId := ctx.Identity.ID
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
		if addr == "" || addr == selfAddr {
			continue
		}
		// Accept healthy or connecting nodes.
		if health == "healthy" || health == "connecting" || health == "" {
			ctx.Identity.ParentAddress = addr
			if ctx.Logger != nil {
				ctx.Logger.Info("peer discovery: discovered peer from DB",
					"peer_address", addr,
					"peer_id", nodeId)
			}
			return
		}
	}

	if ctx.Logger != nil {
		ctx.Logger.Info("peer discovery: no healthy peers found (this node will accept inbound connections)")
	}
}
