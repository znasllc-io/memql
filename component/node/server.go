package node

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"

	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/core/common"
	"google.golang.org/grpc"
)

const (
	NodeServerComponentName = common.ComponentName("nodeServer")
	nodeServerOrder         = 48
	defaultNodeAddress      = ":50052"
)

// NodeServer implements the NodeService gRPC server for inter-node
// communication. It implements common.Dependency.
type NodeServer struct {
	address                  string
	logger                   *slog.Logger
	lifecycle                common.Lifecycle
	listener                 net.Listener
	grpcServer               *grpc.Server
	identity                 *Identity
	peerManager              *PeerManager
	readyCh                  chan struct{}
	queryExecutor            QueryExecutor
	aiForwardHandler         SIForwardHandler
	aiForwardResponse        SIForwardResponseSink
	workbenchForwardHandler  WorkbenchForwardHandler
	workbenchForwardResponse WorkbenchForwardResponseSink
	eventInbound             EventInbound
}

// SetQueryExecutor installs the executor consulted for inbound
// QueryForward messages. Called during bootstrap on nodes that own
// concepts the BFF routes to.
func (s *NodeServer) SetQueryExecutor(e QueryExecutor) {
	if s == nil {
		return
	}
	s.queryExecutor = e
}

// SetAiForwardHandler installs the worker-side AI/voice request handler.
// Called during bootstrap on Voice / Agent binaries; BFF leaves nil.
func (s *NodeServer) SetAiForwardHandler(h SIForwardHandler) {
	if s == nil {
		return
	}
	s.aiForwardHandler = h
}

// SetAiForwardResponseSink installs the BFF-side response sink for
// inbound SIForwardResponse messages received over direct peer-to-peer
// connections the BFF accepts (not just the outbound parent connection;
// some topologies have the BFF on the server side of the NodeService
// handshake). Called during bootstrap on BFF binaries.
func (s *NodeServer) SetAiForwardResponseSink(sink SIForwardResponseSink) {
	if s == nil {
		return
	}
	s.aiForwardResponse = sink
}

// SetEventInbound installs the handler that bridges peer-forwarded events
// onto the local event bus. Every node type needs this -- peer events
// arrive at nodeService.handleEventForward and without an inbound handler
// they are logged and dropped, leaving every local subscriber dark.
// EventBridge satisfies the interface.
func (s *NodeServer) SetEventInbound(h EventInbound) {
	if s == nil {
		return
	}
	s.eventInbound = h
}

// SetWorkbenchForwardHandler installs the workbench-node-side handler
// invoked for inbound WorkbenchForwardRequest messages. Called during
// bootstrap on workbench binaries; other node types leave nil.
func (s *NodeServer) SetWorkbenchForwardHandler(h WorkbenchForwardHandler) {
	if s == nil {
		return
	}
	s.workbenchForwardHandler = h
}

// SetWorkbenchForwardResponseSink installs the agent-side response
// sink for inbound WorkbenchForwardResponse messages received over
// direct peer-to-peer connections. Called during bootstrap on agent
// binaries when cluster-mode workbench forwarding is enabled.
func (s *NodeServer) SetWorkbenchForwardResponseSink(sink WorkbenchForwardResponseSink) {
	if s == nil {
		return
	}
	s.workbenchForwardResponse = sink
}

// NewNodeServer constructs a NodeService gRPC server.
func NewNodeServer(identity *Identity, peerManager *PeerManager, logger *slog.Logger) *NodeServer {
	addr := strings.TrimSpace(identity.Address)
	if addr == "" {
		// Fall back to MEMQL_NODE_SERVICE_ADDRESS env or default
		addr = defaultNodeAddress
	}

	return &NodeServer{
		address:     addr,
		logger:      logger,
		identity:    identity,
		peerManager: peerManager,
		readyCh:     make(chan struct{}),
	}
}

// Start boots the NodeService gRPC server.
func (s *NodeServer) Start(ctx context.Context) {
	_ = s.lifecycle.Start(ctx, s.logger, common.LifecycleHooks{
		Prepare: s.prepareForRun,
		Run:     s.run,
		OnStarted: func() {
			select {
			case <-s.readyCh:
			default:
				close(s.readyCh)
			}
		},
		OnStop: s.cleanup,
	})
}

// Stop gracefully shuts down the NodeService gRPC server.
func (s *NodeServer) Stop(ctx context.Context) {
	_ = s.lifecycle.Stop(ctx, s.logger)
}

// IsRunning reports whether the server lifecycle is active.
func (s *NodeServer) IsRunning() bool {
	return s.lifecycle.IsRunning()
}

// Ready returns a channel that is closed when the server is ready.
func (s *NodeServer) Ready() <-chan struct{} {
	return s.readyCh
}

// ComponentName identifies the dependency.
func (*NodeServer) ComponentName() common.ComponentName {
	return NodeServerComponentName
}

// Order returns the startup priority.
func (*NodeServer) Order() int {
	return nodeServerOrder
}

// Address returns the configured listen address.
func (s *NodeServer) Address() string {
	if s == nil {
		return ""
	}
	return s.address
}

func (s *NodeServer) prepareForRun(ctx context.Context) (context.Context, context.CancelFunc, error) {
	listener, err := net.Listen("tcp", s.address)
	if err != nil {
		return ctx, nil, fmt.Errorf("node server listen on %q: %w", s.address, err)
	}
	s.listener = listener

	// Message-size limits: gRPC's default cap is 4 MiB. The
	// NodeService stream carries inter-node forwarding traffic,
	// including AgentGenerateTurnDelta envelopes that wrap
	// workerComputer.screenshot results -- a retina-display PNG is
	// 1-3 MiB raw, and once wrapped in the delta + tool-result
	// envelopes it pushes past 4 MiB and the server RST_STREAMs
	// the inter-node connection. Visible to the cockpit as
	// "worker stream ended; will reconnect" because the upstream
	// drop bubbles down: agent receives the screenshot from cockpit
	// fine (workerService is bumped to 32 MiB), then fails to
	// forward it to cognition/bff over nodeService.
	//
	// Bumping both server + client (peerConnection in connection.go)
	// to 32 MiB matches the workerService + memqlService bumps.
	const maxNodeMessageSize = 32 * 1024 * 1024
	s.grpcServer = grpc.NewServer(
		grpc.MaxRecvMsgSize(maxNodeMessageSize),
		grpc.MaxSendMsgSize(maxNodeMessageSize),
	)

	svc := &nodeService{
		logger:                   s.logger,
		identity:                 s.identity,
		peerManager:              s.peerManager,
		queryExecutor:            s.queryExecutor,
		aiForwardHandler:         s.aiForwardHandler,
		aiForwardResponse:        s.aiForwardResponse,
		workbenchForwardHandler:  s.workbenchForwardHandler,
		workbenchForwardResponse: s.workbenchForwardResponse,
		eventInbound:             s.eventInbound,
	}
	nodev1.RegisterNodeServiceServer(s.grpcServer, svc)

	s.logger.Info("node server prepared",
		"address", s.address,
		"node_id", s.identity.ID,
		"node_type", string(s.identity.Type),
	)

	return ctx, nil, nil
}

func (s *NodeServer) run(ctx context.Context, markStarted func()) error {
	if s.grpcServer == nil || s.listener == nil {
		return fmt.Errorf("node server not initialized")
	}

	markStarted()
	return s.grpcServer.Serve(s.listener)
}

func (s *NodeServer) cleanup() {
	if s.grpcServer != nil {
		s.grpcServer.GracefulStop()
	}
	if s.listener != nil {
		_ = s.listener.Close()
	}
	s.grpcServer = nil
	s.listener = nil
}
