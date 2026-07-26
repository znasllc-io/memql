package node

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"time"

	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/core/common"
	"google.golang.org/grpc"
)

const (
	NodeServerComponentName = common.ComponentName("nodeServer")
	nodeServerOrder         = 48
	defaultNodeAddress      = ":50052"

	// nodeServerGracefulStopTimeout bounds how long shutdown waits for
	// grpcServer.GracefulStop before forcing grpcServer.Stop (#1119).
	// GracefulStop blocks until every in-flight RPC returns, and the
	// NodeService.Stream is a long-lived inter-node stream that does NOT
	// end on its own -- so an unbounded GracefulStop consumed the entire
	// shared shutdown budget, starving every later dependency's Stop
	// (they then failed with "context deadline exceeded") and leaving the
	// dying pod a registered-but-dead mesh peer for the full 30-45s grace
	// (the source of the "skipped peers (nil Connection)" flood). Bounding
	// it to a few seconds + a forceful fallback makes the node leave the
	// mesh promptly and frees the budget for the rest of the sweep.
	nodeServerGracefulStopTimeout = 5 * time.Second
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
	accessResolver           ForwardedAccessResolver
	aiForwardHandler         AiForwardHandler
	aiForwardResponse        AiForwardResponseSink
	workbenchForwardHandler  WorkbenchForwardHandler
	workbenchForwardResponse WorkbenchForwardResponseSink
	eventInbound             EventInbound
	// authInterceptor is the class="node" JWT enforcement interceptor
	// (#105). Wired by app/cluster.go from the per-binary verifier;
	// nil when the verifier isn't configured (single-node dev),
	// which leaves NodeService.Stream unauthenticated -- the
	// single-node binary doesn't open inter-node streams either way.
	authInterceptor grpc.StreamServerInterceptor
}

// SetAuthInterceptor installs the class="node" JWT enforcement
// stream interceptor on NodeService.Stream. Call after construction
// but BEFORE Start (the interceptor is read once at prepareForRun
// when the gRPC server is created). Passing nil leaves the
// interceptor unwired for single-node binaries. See
// NodeClassStreamInterceptor + #105.
func (s *NodeServer) SetAuthInterceptor(i grpc.StreamServerInterceptor) {
	if s == nil {
		return
	}
	s.authInterceptor = i
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

// SetIdentityResolver installs the database-backed resolver that turns a
// forwarded query's claims into the caller's AccessContext. Must be wired
// wherever SetQueryExecutor is: without it forwarded queries are refused
// rather than executed on peer-asserted authority (memql#2814).
func (s *NodeServer) SetIdentityResolver(r ForwardedAccessResolver) {
	if s == nil {
		return
	}
	s.accessResolver = r
}

// SetAiForwardHandler installs the worker-side AI/voice request handler.
// Called during bootstrap on Voice / Agent binaries; BFF leaves nil.
func (s *NodeServer) SetAiForwardHandler(h AiForwardHandler) {
	if s == nil {
		return
	}
	s.aiForwardHandler = h
}

// SetAiForwardResponseSink installs the BFF-side response sink for
// inbound AiForwardResponse messages received over direct peer-to-peer
// connections the BFF accepts (not just the outbound parent connection;
// some topologies have the BFF on the server side of the NodeService
// handshake). Called during bootstrap on BFF binaries.
func (s *NodeServer) SetAiForwardResponseSink(sink AiForwardResponseSink) {
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
	// Bind the NodeService listener to MEMQL_NODE_SERVICE_ADDRESS (e.g.
	// ":50061" = all interfaces). The advertised address
	// (identity.Address / MEMQL_NODE_ADDRESS) is how PEERS reach this node;
	// in k8s that's a Service DNS name resolving to a Service VIP, which is
	// NOT bindable -- listening must use the explicit bind address. (In
	// docker the service name resolved to the container's own IP, so the
	// old identity.Address bind happened to work there.)
	addr := strings.TrimSpace(os.Getenv("MEMQL_NODE_SERVICE_ADDRESS"))
	if addr == "" {
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
	serverOpts := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(maxNodeMessageSize),
		grpc.MaxSendMsgSize(maxNodeMessageSize),
	}
	if s.authInterceptor != nil {
		// class="node" enforcement (#105). When unset, the binary
		// runs single-node mode (no inter-node traffic), so
		// installing nothing is fine.
		serverOpts = append(serverOpts, grpc.StreamInterceptor(s.authInterceptor))
		s.logger.Info("node server: auth interceptor enabled (class=node required)")
	}
	s.grpcServer = grpc.NewServer(serverOpts...)

	svc := &nodeService{
		logger:                   s.logger,
		identity:                 s.identity,
		peerManager:              s.peerManager,
		queryExecutor:            s.queryExecutor,
		accessResolver:           s.accessResolver,
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
	// Capture the server + listener into locals: the OnStop hook (cleanup)
	// nils the struct fields, and the Serve goroutine below reads them at
	// execution time -- which can race after cleanup on a fast Start/Stop.
	// Locals make the goroutine and stopGRPC immune to that nil-out.
	srv := s.grpcServer
	lis := s.listener
	if srv == nil || lis == nil {
		return fmt.Errorf("node server not initialized")
	}

	// Serve blocks until the server stops, so run it in a goroutine and
	// let this lifecycle loop own the ctx-driven shutdown (#1119). Without
	// this, Serve ignored ctx cancellation entirely: the only GracefulStop
	// lived in the OnStop hook, which never ran because the Run hook never
	// returned -- so Stop just timed out the shared shutdown budget.
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(lis) }()

	markStarted()

	select {
	case err := <-serveErr:
		// Server exited on its own (listener error, etc.).
		return err
	case <-ctx.Done():
		s.stopGRPC(srv)
		return nil
	}
}

// stopGRPC bounds GracefulStop with a forceful Stop fallback so a
// long-lived inter-node stream can't wedge shutdown (#1119). Safe to call
// once from the run loop on ctx cancellation. Takes the server explicitly
// (not s.grpcServer) so it can't race the cleanup hook's nil-out.
func (s *NodeServer) stopGRPC(srv *grpc.Server) {
	if srv == nil {
		return
	}

	stopped := make(chan struct{})
	go func() {
		srv.GracefulStop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(nodeServerGracefulStopTimeout):
		// Inter-node streams didn't drain in time; force them closed so
		// the node leaves the mesh now instead of lingering as a dead peer.
		s.logger.Warn("node server: graceful stop timed out; forcing stop",
			"timeout", nodeServerGracefulStopTimeout.String())
		srv.Stop()
		<-stopped
	}
}

func (s *NodeServer) cleanup() {
	// run() already stopped the gRPC server (which closes the listener) via
	// stopGRPC on ctx cancellation; this hook just clears references. The
	// listener.Close is retained as a defensive no-op for the rare path
	// where Serve returned before any client connected.
	if s.listener != nil {
		_ = s.listener.Close()
	}
	s.grpcServer = nil
	s.listener = nil
}
