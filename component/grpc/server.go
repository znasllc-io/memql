package memql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	grpcMetadata "google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/bus"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity/verifier"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/memql/sense"
	nodeMetadata "github.com/znasllc-io/memql/component/metadata"
	"github.com/znasllc-io/memql/component/node"
	"github.com/znasllc-io/memql/component/polyphon"
	"github.com/znasllc-io/memql/component/provenance"
	healthsrv "github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/grpctls"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/stt"
)

// contextWithEnvelopeProvenance attaches the envelope's
// MemqlClientMessage.Provenance proto field (set by cross-node
// forwarders) onto ctx so the engine's auto-stamping path uses it
// instead of inferring a default. No-op when the envelope didn't
// carry provenance.
func contextWithEnvelopeProvenance(ctx context.Context, envelope *memqlv1.MemqlClientMessage) context.Context {
	if envelope == nil {
		return ctx
	}
	pb := envelope.GetProvenance()
	if pb == nil {
		return ctx
	}
	p := provenance.Provenance{
		Kind:    provenance.Kind(pb.GetKind()),
		Name:    pb.GetName(),
		Trigger: pb.GetTrigger(),
		Via:     pb.GetVia(),
	}
	if p.IsZero() {
		return ctx
	}
	if err := p.Validate(); err != nil {
		return ctx
	}
	return provenance.ContextWithProvenance(ctx, p)
}

// stampEnvelopeProvenance copies the caller-ctx provenance into the
// envelope before it's serialized + shipped to another node. Called
// by cross-node forwarders (proxyAI). No-op if ctx carries no
// provenance.
func stampEnvelopeProvenance(ctx context.Context, envelope *memqlv1.MemqlClientMessage) {
	if envelope == nil {
		return
	}
	p := provenance.FromContext(ctx)
	if p.IsZero() {
		return
	}
	envelope.Provenance = &memqlv1.Provenance{
		Kind:    string(p.Kind),
		Name:    p.Name,
		Trigger: p.Trigger,
		Via:     p.Via,
	}
}

const (
	ComponentName  = common.ComponentName("memqlGRPCServer")
	lifecycleOrder = 50
	defaultAddress = ":50051"
)

// Server implements the MemQL gRPC surface as a common.Dependency.
type Server struct {
	address           string
	logger            *slog.Logger
	lifecycle         common.Lifecycle
	listener          net.Listener
	grpcServer        *grpc.Server
	engine            *memqlengine.MemQLEngine
	eventBus          *events.Bus
	sttProvider       stt.StreamingProvider
	streamInterceptor grpc.StreamServerInterceptor
	readyCh           chan struct{}
	wiring            *bus.Wiring
	sense             *sense.Service
	scoreEngine       *polyphon.ScoreEngine
	roomProvider      polyphon.RoomProvider
	conceptRegistry   memoryNodes.Registry
	aiForwarder       *AiForwardRouter
	agentReplier      AgentTurnHandler
	agentPauseHook    func(requestId string)
	// nodeMaintenanceHandler drives THIS node's lifecycle for the operator
	// maintenance trigger (memql#1270). Set by app bootstrap on mesh
	// binaries (app wires it to the App's RequestOperatorDrain via a narrow
	// port -- component/grpc can't import app/node-lifecycle). Nil on
	// non-mesh binaries; the NodeMaintenance handler reports "unavailable".
	nodeMaintenanceHandler NodeMaintenanceHandler
	// clientToolRPC serves the substrate-RPC client-tool return channel per
	// agent turn (memql#1265). Set by app bootstrap on agent binaries where the
	// delivery substrate is wired; nil otherwise (legacy ForwardContinuation).
	clientToolRPC *node.ClientToolResultServer
	// deliverySubstrate is the durable streaming substrate (memql#1266). Set by
	// app bootstrap on every mesh node where the substrate is wired; nil on
	// single-node / non-mesh binaries (streaming falls back to the direct
	// forwardedStream push). Producers (agent token streaming, voice audio
	// streaming) StreamSession.Delta to stream:<requestId>; the WS-owning bff
	// SubscribeStreamFrames it back, surviving a mid-stream replica switch.
	deliverySubstrate node.DeliverySubstrate
	// streamNodeID is this node's id, used as the substrate stream producer
	// origin + the bff consumer cursor identity (memql#1266). Set alongside
	// deliverySubstrate.
	streamNodeID    string
	serviceRef      *serviceRef
	extraRegistrars []func(*grpc.Server)
	// tokenVerifier honors RotateAuthMsg on open streams. Set by
	// SetVerifier during app bootstrap; nil on nodes that don't run
	// an identity verifier (rotation responds with
	// error="verifier_unconfigured" instead of crashing).
	tokenVerifier *verifier.Verifier
}

// serviceRef holds the service instance constructed during Run() so
// other components can delegate to it. Populated atomically in
// prepareForRun; read-only afterwards.
type serviceRef struct {
	svc *service
}

// SetAiForwarder installs the BFF-side outbound forwarder. On BFF
// builds the bootstrap phase creates an AiForwardRouter pointing at
// PeerManager and installs it here; AI/voice handlers route every
// aiChat / aiSpeech / aiTranscribe / aiSuggest / listTools / callTool
// request through the forwarder to the matching worker node. Workers
// leave this nil and handle AI requests directly from their local
// providers.
func (s *Server) SetAiForwarder(r *AiForwardRouter) {
	if s == nil {
		return
	}
	s.aiForwarder = r
}

// AiForwardHandler returns a worker-side handler that delegates to the
// internal gRPC service. The service is constructed lazily in Run();
// callers can retrieve this shim at bootstrap time and install it on
// the NodeServer -- the shim resolves to the real handler once Run()
// has populated the service reference. On BFF binaries the handler is
// never invoked because the worker-side dispatch path isn't triggered.
func (s *Server) AiForwardHandler() node.AiForwardHandler {
	if s == nil {
		return nil
	}
	if s.serviceRef == nil {
		s.serviceRef = &serviceRef{}
	}
	return &aiForwardHandlerShim{ref: s.serviceRef}
}

// ClientToolResultFirer returns a node.ClientToolResultFirer that delivers a
// ClientToolResult to this node's parked client-tool waiter (memql#1265). Like
// AiForwardHandler it resolves through the serviceRef so it can be retrieved at
// bootstrap time and handed to the agent's ClientToolResultServer before Run()
// has constructed the service. On non-agent nodes DeliverClientToolResult finds
// no waiter and returns false.
func (s *Server) ClientToolResultFirer() node.ClientToolResultFirer {
	if s == nil {
		return nil
	}
	if s.serviceRef == nil {
		s.serviceRef = &serviceRef{}
	}
	return &clientToolFirerShim{ref: s.serviceRef}
}

// clientToolFirerShim defers resolution of the worker-side service (built in
// Run) so the firer can be wired at bootstrap. A nil/early call returns
// found=false rather than panicking.
type clientToolFirerShim struct {
	ref *serviceRef
}

func (h *clientToolFirerShim) DeliverClientToolResult(p node.ClientToolResultPayload) bool {
	if h == nil || h.ref == nil || h.ref.svc == nil {
		return false
	}
	return h.ref.svc.DeliverClientToolResult(p)
}

// NewServer constructs a gRPC server bound to the provided address.
func NewServer(address string, logger *slog.Logger) *Server {
	addr := strings.TrimSpace(address)
	if addr == "" {
		addr = defaultAddress
	}
	return &Server{
		address: addr,
		logger:  logger,
		readyCh: make(chan struct{}),
	}
}

// Start boots the gRPC server.
func (s *Server) Start(ctx context.Context) {
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

// Stop gracefully shuts down the gRPC server.
func (s *Server) Stop(ctx context.Context) {
	_ = s.lifecycle.Stop(ctx, s.logger)
}

// IsRunning reports whether the gRPC server lifecycle is active.
func (s *Server) IsRunning() bool {
	return s.lifecycle.IsRunning()
}

// Ready returns a channel that is closed when the server is ready.
func (s *Server) Ready() <-chan struct{} {
	return s.readyCh
}

// ComponentName identifies the dependency.
func (*Server) ComponentName() common.ComponentName {
	return ComponentName
}

// Order ensures the gRPC server starts after core dependencies.
func (*Server) Order() int {
	return lifecycleOrder
}

// Address returns the configured listen address for the gRPC server.
func (s *Server) Address() string {
	if s == nil {
		return ""
	}
	return s.address
}

func (s *Server) prepareForRun(ctx context.Context) (context.Context, context.CancelFunc, error) {
	listener, err := net.Listen("tcp", s.address)
	if err != nil {
		return ctx, nil, fmt.Errorf("listen on %q: %w", s.address, err)
	}

	s.listener = listener

	if s.streamInterceptor == nil {
		return ctx, nil, fmt.Errorf("authentication interceptor not configured")
	}
	// Message-size limits: gRPC's default cap is 4 MiB, which is too
	// tight for the WorkerService stream. workerComputer.screenshot
	// returns a base64-encoded PNG of the user's screen -- on a
	// retina display that's commonly 5-10 MiB after encoding. The
	// 4 MiB default caused the server to RST_STREAM with
	// INTERNAL_ERROR mid-call (visible to the cockpit as "worker
	// stream ended; will reconnect" and to the agent as
	// errorCode=worker_disconnected). Bump to 32 MiB on both
	// directions; the cockpit's gRPC client has a matching bump in
	// cmd/memql-cockpit/internal/worker/connect.go. 32 MiB covers a
	// 6K screen capture (~28 MiB base64) with headroom for envelope
	// metadata.
	const maxWorkerMessageSize = 32 * 1024 * 1024
	serverOpts := []grpc.ServerOption{
		grpc.StreamInterceptor(s.streamInterceptor),
		grpc.MaxRecvMsgSize(maxWorkerMessageSize),
		grpc.MaxSendMsgSize(maxWorkerMessageSize),
	}
	// TLS opt-in via MEMQL_GRPC_TLS_CERT_FILE + KEY_FILE. When
	// unset, the server stays insecure (the legacy default suitable
	// for deployments behind a TLS-terminating proxy). See
	// docs/internal/design/auth-threat-model.md §6 + component/grpc/tls.go.
	if tlsCfg, err := grpctls.LoadServerTLSConfig(s.logger); err != nil {
		return ctx, nil, fmt.Errorf("grpc server: tls config: %w", err)
	} else if tlsCfg != nil {
		serverOpts = append(serverOpts, grpc.Creds(credentials.NewTLS(tlsCfg)))
	}
	s.grpcServer = grpc.NewServer(serverOpts...)
	identityResolver := auth.NewIdentityResolver(&engineQueryRunner{engine: s.engine}, s.logger)
	svc := &service{
		logger:                 s.logger,
		engine:                 s.engine,
		eventBus:               s.eventBus,
		sttProvider:            s.sttProvider,
		wiring:                 s.wiring,
		sense:                  s.sense,
		scoreEngine:            s.scoreEngine,
		roomProvider:           s.roomProvider,
		conceptRegistry:        s.conceptRegistry,
		identityResolver:       identityResolver,
		aiForwarder:            s.aiForwarder,
		agentReplier:           s.agentReplier,
		nodeMaintenanceHandler: s.nodeMaintenanceHandler,
		clientToolResultServer: s.clientToolRPC,
		deliverySubstrate:      s.deliverySubstrate,
		streamNodeID:           s.streamNodeID,
		verifier:               s.tokenVerifier,
	}
	memqlv1.RegisterMemqlServiceServer(s.grpcServer, svc)
	if s.serviceRef != nil {
		s.serviceRef.svc = svc
	}

	// Subscribe to browser client:tool:response events so a voice-driven
	// client-tool call relayed to the browser (#1420) resolves the local
	// clientToolWaiters waiter on whichever node parked it. No-op when the bus
	// is unset (single-node / non-bus builds).
	svc.startClientToolResponseRelay()

	// Run any caller-supplied registrars (e.g. WorkerService on the
	// agent build). These attach additional gRPC service handlers to
	// the same listener / server.
	for _, reg := range s.extraRegistrars {
		if reg != nil {
			reg(s.grpcServer)
		}
	}

	return ctx, nil, nil
}

// RegisterService queues a registrar function that runs once the
// internal *grpc.Server is constructed in prepareForRun. Used by
// build-tag-gated services (WorkerService, etc.) that need to mount
// onto the same gRPC server but are not constructed inside this
// package.
func (s *Server) RegisterService(register func(*grpc.Server)) {
	if s == nil || register == nil {
		return
	}
	s.extraRegistrars = append(s.extraRegistrars, register)
}

func (s *Server) run(ctx context.Context, markStarted func()) error {
	if s.grpcServer == nil || s.listener == nil {
		return fmt.Errorf("gRPC server not initialized")
	}

	markStarted()
	return s.grpcServer.Serve(s.listener)
}

func (s *Server) cleanup() {
	if s.grpcServer != nil {
		s.grpcServer.GracefulStop()
	}
	if s.listener != nil {
		_ = s.listener.Close()
	}
	s.grpcServer = nil
	s.listener = nil
}

// SetEngine wires the MemQL engine used by the gRPC surface.
func (s *Server) SetEngine(engine *memqlengine.MemQLEngine) {
	if s == nil {
		return
	}
	s.engine = engine
}

// SetEventBus wires the event bus used for publishing and subscribing to events.
func (s *Server) SetEventBus(bus *events.Bus) {
	if s == nil {
		return
	}
	s.eventBus = bus
}

// SetVerifier wires the per-node JWT verifier used by the in-stream
// auth-rotation path (RotateAuthMsg). Set during app bootstrap from
// the same *verifier.Verifier the stream interceptor uses, so a token
// the interceptor would accept at stream-open is also accepted by a
// mid-stream rotate. Nodes that don't run an identity verifier leave
// this nil; RotateAuthMsg comes back with error="verifier_unconfigured"
// instead of crashing the stream.
func (s *Server) SetVerifier(v *verifier.Verifier) {
	if s == nil {
		return
	}
	s.tokenVerifier = v
}

// SetSTTProvider wires the speech-to-text provider used by AI transcription.
func (s *Server) SetSTTProvider(provider stt.StreamingProvider) {
	if s == nil {
		return
	}
	s.sttProvider = provider
}

// SetStreamInterceptor sets a custom stream interceptor for authentication.
// When set, this interceptor will be used instead of the default Authorizer interceptor.
func (s *Server) SetStreamInterceptor(interceptor grpc.StreamServerInterceptor) {
	if s == nil {
		return
	}
	s.streamInterceptor = interceptor
}

// StreamInterceptor returns the currently-configured stream interceptor.
// Used by build-tag-gated wiring (e.g. WorkerService on agent) to wrap
// the existing chain with additional layers without losing the base
// auth interceptor.
func (s *Server) StreamInterceptor() grpc.StreamServerInterceptor {
	if s == nil {
		return nil
	}
	return s.streamInterceptor
}

// SetSense sets the MemQL Sense language service.
func (s *Server) SetSense(svc *sense.Service) {
	if s == nil {
		return
	}
	s.sense = svc
}

// SetScoreEngine sets the Polyphon score engine for voice session management.
func (s *Server) SetScoreEngine(c *polyphon.ScoreEngine) {
	if s == nil {
		return
	}
	s.scoreEngine = c
}

// SetRoomProvider sets the Polyphon room provider for LiveKit token generation.
func (s *Server) SetRoomProvider(rp polyphon.RoomProvider) {
	if s == nil {
		return
	}
	s.roomProvider = rp
}

// SetConceptRegistry sets the concept registry for metadata queries.
func (s *Server) SetConceptRegistry(reg memoryNodes.Registry) {
	if s == nil {
		return
	}
	s.conceptRegistry = reg
}

// EventBus returns the event bus used by the gRPC server.
func (s *Server) EventBus() *events.Bus {
	if s == nil {
		return nil
	}
	return s.eventBus
}

type service struct {
	memqlv1.UnimplementedMemqlServiceServer
	logger           *slog.Logger
	engine           *memqlengine.MemQLEngine
	eventBus         *events.Bus
	sttProvider      stt.StreamingProvider
	wiring           *bus.Wiring
	sense            *sense.Service
	scoreEngine      *polyphon.ScoreEngine
	roomProvider     polyphon.RoomProvider
	conceptRegistry  memoryNodes.Registry
	identityResolver *auth.IdentityResolver
	aiForwarder      *AiForwardRouter   // non-nil on BFF binaries; proxies AI/voice to workers
	verifier         *verifier.Verifier // re-verifies presented tokens for in-stream rotation; nil on no-auth nodes

	// agentReplier handles AgentGenerateTurnMsg on agent nodes. Non-nil
	// only on agent binaries (set via Server.SetAgentReplier during app
	// bootstrap). Other binaries leave this nil; handleAgentGenerateTurn
	// returns an error when the caller lands on the wrong node type.
	agentReplier AgentTurnHandler

	// clientToolResultServer serves the substrate-RPC return channel for an
	// agent turn's client-tool round-trip (memql#1265). Non-nil only on agent
	// binaries where the delivery substrate is wired (set via
	// Server.SetClientToolResultServer during app bootstrap). The agent
	// BeginTurn/EndTurn it around each forwarded turn so a ClientToolResult
	// published by cognition to the turn's logical key fires the local waiter
	// regardless of cognition<->agent connection churn (the #1245 fix). Nil =>
	// the legacy ForwardContinuation path stays in effect.
	clientToolResultServer *node.ClientToolResultServer

	// deliverySubstrate is the durable streaming substrate (memql#1266). Non-nil
	// on every mesh node where the substrate is wired (set via
	// Server.SetDeliverySubstrate during app bootstrap). The token-streaming
	// (handleAiChatStream) + audio-streaming (si_transcribe_stream) paths produce
	// ordered chunks to stream:<requestId> on the producing worker and the
	// WS-owning bff consumes them back, so a streamed turn survives a mid-stream
	// replica switch (replayed from the durable outbox). Nil => the legacy
	// forwardedStream / direct push stays in effect (single-node / non-mesh).
	deliverySubstrate node.DeliverySubstrate
	// streamNodeID is this node's id (memql#1266): the substrate stream producer
	// origin (worker) + the per-replica consumer cursor identity (bff).
	streamNodeID string

	// agentPauseHook flags an in-flight background turn for cooperative
	// preemption (epic memql#902 / #906), keyed by request_id. Injected on
	// agent binaries as agent.RequestPause via Server.SetAgentPauseHook.
	// Nil on other node types; the AgentPreemptTurn handler no-ops when unset.
	agentPauseHook func(requestId string)

	// nodeMaintenanceHandler drives THIS node's lifecycle for the operator
	// maintenance trigger (memql#1270). Non-nil on mesh binaries (wired by
	// app bootstrap to the App's RequestOperatorDrain). Nil elsewhere; the
	// NodeMaintenance handler reports "unavailable" when unset.
	nodeMaintenanceHandler NodeMaintenanceHandler

	// transcribeStreams holds the worker-side state for in-flight
	// streaming transcription sessions, keyed by the caller's
	// request_id. Populated by handleAiTranscribeStreamStart; consumed
	// by Chunk/End; cleared when End finalizes or cancels.
	transcribeStreamsMu sync.Mutex
	transcribeStreams   map[string]*transcribeStream

	// clientToolWaiters dispatches incoming ClientToolResult messages
	// back to the blocked executeClientTool / InvokeClientTool caller
	// keyed by call_id. Service-scoped (not session-scoped) so the
	// result can arrive on a different stream than the one that sent
	// the original ClientToolCall -- this is required for the cluster
	// relay path where Cognition bridges the call via graph events and
	// sends the result back as a fresh AiForwardRequest (which lands
	// on a NEW forwardedStream, not the one that is still waiting on
	// the agent node).
	clientToolMu      sync.Mutex
	clientToolWaiters map[string]chan *memqlv1.ClientToolResult

	// clientToolRelayOnce guards the one-time subscription to browser
	// client:tool:response events that fires the local clientToolWaiters for
	// the voice relay path (#1420). See client_tool_relay_voice.go.
	clientToolRelayOnce sync.Once
}

// engineQueryRunner adapts *memqlengine.MemQLEngine to
// auth.QueryRunner by returning the shaped output payload of each
// query.
type engineQueryRunner struct {
	engine *memqlengine.MemQLEngine
}

func (r *engineQueryRunner) ExecuteShaped(ctx context.Context, query string) (any, error) {
	if r == nil || r.engine == nil {
		return nil, fmt.Errorf("identity resolver: engine unavailable")
	}
	result, err := r.engine.Execute(ctx, query)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, nil
	}
	return result.OutputPayload(), nil
}

func (s *service) Stream(stream memqlv1.MemqlService_StreamServer) error {
	if stream == nil {
		return status.Error(codes.Internal, "stream cannot be nil")
	}

	identity, err := auth.UserIdentityFromContext(stream.Context())
	if err != nil {
		return status.Error(codes.Unauthenticated, "authentication required")
	}

	if s.logger != nil {
		s.logger.Debug("gRPC stream opened", "subject", identity.Subject, "hasEventBus", s.eventBus != nil)
	}

	// Extract transport metadata for the metadata collector.
	requestMeta := extractRequestMeta(stream.Context())

	session := newStreamSession(s, stream, identity)
	session.requestMeta = requestMeta

	// Blue/green drain accounting (#616): count this open MemqlService.Stream
	// so /healthz reports it and the cutover script can keep an OLD-color pod
	// alive until its existing connections drain to 0. Paired decrement in the
	// defer below.
	healthsrv.StreamOpened()

	// Emit session opened event with full identity for user provisioning automation
	s.publishSessionEvent(events.TopicSessionOpened, events.KindSessionOpened, identity)

	defer func() {
		healthsrv.StreamClosed()
		session.shutdown()
		// Emit session closed event
		s.publishSessionEvent(events.TopicSessionClosed, events.KindSessionClosed, identity)
	}()

	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := session.handleMessage(msg); err != nil {
			return err
		}
	}
}

// publishSessionEvent emits a session lifecycle event with full identity information.
func (s *service) publishSessionEvent(topic string, kind events.Kind, identity auth.UserIdentity) {
	if s == nil || s.eventBus == nil {
		return
	}
	event := events.NewEvent(topic, kind, map[string]any{
		"subject":     identity.Subject,
		"email":       identity.Email,
		"firstName":   identity.FirstName,
		"lastName":    identity.LastName,
		"role":        identity.Role,
		"phoneNumber": identity.PhoneNumber,
	})
	s.eventBus.Publish(event)
}

// structuredQueryError wraps a gRPC status error with structured error metadata.
type structuredQueryError struct {
	code     codes.Code
	message  string
	metadata map[string]string
}

func (e *structuredQueryError) Error() string {
	return e.message
}

func (e *structuredQueryError) GRPCStatus() *status.Status {
	return status.New(e.code, e.message)
}

// executeQuery executes a query and returns the result with metadata.
// The clientId parameter is used for optimistic update reconciliation.
func (s *service) executeQuery(ctx context.Context, query string, clientId string, cursor string) (*memqlv1.Result, error) {
	if s.engine == nil {
		return nil, status.Error(codes.Unavailable, "MemQL engine unavailable")
	}

	// Keyset cursor (5.12): thread an opaque inbound continuation cursor onto
	// the context so the engine lifts it onto plan.After and continues via a
	// SQL keyset predicate. Empty cursor is a no-op (offset / first-page path).
	ctx = memqlengine.ContextWithCursor(ctx, cursor)

	execResult, err := s.engine.Execute(ctx, query)
	if err != nil {
		// Wrap error to extract structured information
		wrappedErr := memqlengine.WrapError(err)

		code := classifyEngineError(err)
		msg := strings.TrimSpace(wrappedErr.Error())
		if msg == "" {
			msg = "MemQL query failed."
		}
		if code == codes.Internal {
			if s.logger != nil {
				s.logger.Error("memql query execution failed", "error", err)
			}
			msg = buildEngineErrorMessage(err)
		}

		// Extract structured metadata if available
		var metadata map[string]string
		if se, ok := wrappedErr.(*memqlengine.StructuredError); ok {
			metadata = se.ToMetadata()
			// Use the structured error code for the gRPC code field
			metadata["code"] = string(se.Code)
		}

		return nil, &structuredQueryError{
			code:     code,
			message:  msg,
			metadata: metadata,
		}
	}

	var bundle *memqlv1.GraphBundle
	var data []*structpb.Value
	var protoMeta *memqlv1.ResultMeta
	if execResult != nil {
		// Finalize metadata (calculates took_ms, count, etc.)
		execResult.FinalizeMeta()

		bundle, data, err = execResult.ToAPIResult()
		if err != nil {
			if s.logger != nil {
				s.logger.Error("memql result serialization failed", "error", err)
			}
			return nil, status.Error(codes.Internal, "MemQL engine failed to serialize response")
		}

		// Convert engine metadata to protobuf metadata
		if meta := execResult.GetMeta(); meta != nil {
			protoMeta = &memqlv1.ResultMeta{
				HasMore:  meta.HasMore,
				Cursor:   meta.Cursor,
				TookMs:   meta.TookMs,
				ClientId: clientId,
				ServerId: meta.ServerId,
				Version:  meta.Version,
			}
			// Only set count if it's known (>= 0)
			if meta.Count >= 0 {
				protoMeta.Count = &meta.Count
			}
		}
	}

	return &memqlv1.Result{
		Bundle: bundle,
		Data:   data,
		Meta:   protoMeta,
	}, nil
}

// subscriptionInfo holds subscription metadata.
type subscriptionInfo struct {
	msg *memqlv1.SubscribeMsg
}

type streamSession struct {
	service        *service
	stream         memqlv1.MemqlService_StreamServer
	logger         *slog.Logger
	identity       auth.UserIdentity
	requestMeta    *nodeMetadata.RequestMeta
	sendMu         sync.Mutex
	activeRequests sync.Map
	subscriptions  sync.Map // subscription_id → *subscriptionInfo
	unsubscribers  sync.Map // subscription_id → unsubscribe func()
	eventChan      chan events.Event
	closeChan      chan struct{}

	// access holds the resolved user identity (userId / role /
	// email) for this stream. Loaded lazily on the first message that
	// needs it (ensureAccess). Cached for the lifetime of the stream;
	// clients reconnect to pick up role changes.
	accessMu     sync.Mutex
	access       *auth.AccessContext
	accessLoaded bool

	// uiAskUserRejectCount counts consecutive uiAskUser calls rejected
	// by validateClientToolArgs for missing-options on this session.
	// After the budget is spent we stop rejecting and soft-fallback
	// by injecting default options so the walkthrough never stalls
	// in a reject/retry loop. Reset to zero whenever a uiAskUser call
	// passes validation.
	uiAskUserMu          sync.Mutex
	uiAskUserRejectCount int

	// voiceTurns tracks one active VoiceAgentTurnRequest per
	// (space, gaAgentId). When a second turn request arrives for
	// the same key, the previous one is cancelled so its goroutine
	// completes promptly instead of parking until the 30s wait
	// timeout. Without this, the BFF accumulates orphan turn-reply
	// subscribers that all match against the same
	// `v1:cognition:utterance` events, racing each other and
	// causing the "agent utterance landed in chat but no audio
	// played" symptom (the loser's TurnDelta hits an LLMStream that
	// LK Agents has already advanced past).
	voiceTurnsMu       sync.Mutex
	voiceTurns         map[string]context.CancelFunc
	voiceTurnSentinels map[string]*struct{}

	// voiceAgentSpeakSubMu protects the session-long speak subscriber
	// state. A voice-agent stream binds to one (space, ga_agent_id)
	// via VoiceAgentSessionStart; the subscriber forwards AI reply
	// utterances as VoiceAgentSpeak messages so chat-typed user
	// messages produce audible replies via AgentSession.say(). See
	// startVoiceAgentSpeakSubscriber for the dedup against in-flight
	// TurnRequests.
	voiceAgentSpeakSubMu sync.Mutex
	voiceAgentSpeakStop  func()
	voiceAgentScopeId    string
	voiceAgentGaAgentId  string
	// voiceAgentGaRole is the scoped GA agent's role (e.g. "assistant"),
	// resolved at SessionStart. handleCallTool gates tool execution on it for
	// voice-agent sessions so GA-only (@allowedRoles) tools the model is allowed
	// to SEE (#1467) are also allowed to RUN, instead of being rejected against
	// the voice-agent caller's empty role.
	voiceAgentGaRole string
}

func newStreamSession(svc *service, stream memqlv1.MemqlService_StreamServer, identity auth.UserIdentity) *streamSession {
	sess := &streamSession{
		service:    svc,
		stream:     stream,
		logger:     svc.logger,
		identity:   identity,
		eventChan:  make(chan events.Event, 256),
		closeChan:  make(chan struct{}),
		voiceTurns: make(map[string]context.CancelFunc),
	}

	// Start event forwarding goroutine
	go sess.forwardEvents()

	return sess
}

// forwardEvents listens for events from the event bus and forwards them to the client.
func (s *streamSession) forwardEvents() {
	for {
		select {
		case <-s.closeChan:
			return
		case event, ok := <-s.eventChan:
			if !ok {
				return
			}
			s.handleBusEvent(event)
		}
	}
}

// handleBusEvent converts an events.Event to a proto EventNotification and sends it.
func (s *streamSession) handleBusEvent(event events.Event) {
	if s.logger != nil {
		s.logger.Debug("handleBusEvent received", "topic", event.Topic, "kind", event.Kind.String())
	}

	payload := event.Payload
	if payload == nil {
		payload = make(map[string]any)
	}

	// Add standard event fields
	payload["topic"] = event.Topic
	payload["eventKind"] = event.Kind.String()

	// Add metadata
	for k, v := range event.Metadata {
		payload[k] = v
	}

	structPayload, err := structpb.NewStruct(payload)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("failed to convert event payload", "error", err, "topic", event.Topic)
		}
		return
	}

	// Find matching subscription IDs for this event
	matchCount := 0
	s.subscriptions.Range(func(key, value any) bool {
		subscriptionId, ok := key.(string)
		if !ok {
			return true
		}
		info, ok := value.(*subscriptionInfo)
		if !ok || info == nil || info.msg == nil {
			return true
		}

		// Check if this subscription matches the event topic
		pattern := events.TopicPatternFromSubscriptionKind(info.msg.GetKind(), info.msg.GetFilter())
		if !events.Match(pattern, event.Topic) {
			if s.logger != nil {
				s.logger.Debug("subscription pattern mismatch", "subscriptionId", subscriptionId, "pattern", pattern, "topic", event.Topic)
			}
			return true
		}

		matchCount++
		if s.logger != nil {
			s.logger.Debug("subscription matched, sending event", "subscriptionId", subscriptionId, "pattern", pattern, "topic", event.Topic)
		}

		notification := &memqlv1.EventNotification{
			SubscriptionId: subscriptionId,
			Kind:           events.ToProtoEventKind(event.Kind),
			Ts:             timestamppb.New(event.Timestamp),
			Payload:        structPayload,
		}

		_ = s.sendServerMessage("", &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_Event{
				Event: notification,
			},
		})

		return true
	})
	if s.logger != nil && matchCount == 0 {
		s.logger.Debug("no subscriptions matched event", "topic", event.Topic)
	}
}

// currentAccess returns the AccessContext resolved by ensureAccess for
// this stream, or nil if no message has been dispatched yet (or the
// message didn't trigger an ACL load).
func (s *streamSession) currentAccess() *auth.AccessContext {
	s.accessMu.Lock()
	defer s.accessMu.Unlock()
	return s.access
}

// ensureAccess resolves the caller's user identity (userId / role /
// email) from the database once per stream. On the very first login
// (race with the magic-link verifier's user-row insert), the engine
// may not yet have a user row; we fall back to claims-derived values
// so the session is not locked out. Subsequent streams pick up the
// real user record.
func (s *streamSession) ensureAccess(ctx context.Context) *auth.AccessContext {
	s.accessMu.Lock()
	defer s.accessMu.Unlock()
	if s.accessLoaded {
		return s.access
	}
	s.accessLoaded = true

	resolver := s.service.identityResolver
	claims, _ := auth.ClaimsFromContext(ctx)

	if resolver != nil {
		ac, err := resolver.LoadFromClaims(ctx, claims)
		if err == nil {
			s.access = ac
			return s.access
		}
		if !errors.Is(err, auth.ErrUserNotProvisioned) {
			if s.logger != nil {
				s.logger.Warn("identity resolver: LoadFromClaims failed; using claims fallback",
					"error", err, "subject", s.identity.Subject)
			}
		}
	}
	s.access = auth.FallbackFromClaims(claims)
	return s.access
}

// handleRotateAuth swaps the bearer on a live stream. Re-verifies the
// presented token through the same verifier the interceptor uses at
// stream-open, then -- on success -- replaces the session's identity
// and reloads the resolved user record atomically.
//
// Semantics:
//   - The stream stays open in EVERY branch. Even on a hard reject,
//     the existing identity remains in effect; the client owns the
//     decision to retry / log out / re-login.
//   - Reply is always a RotateAuthResult correlated to the request's
//     message_id, so the client's SendAndWait pattern unblocks
//     regardless of outcome.
//   - The new identity is durable for the rest of the stream. There
//     is no rollback path -- a successful rotation supersedes the
//     stream-open identity permanently.
func (s *streamSession) handleRotateAuth(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.RotateAuthMsg) error {
	correlationId := envelope.GetMessageId()
	reply := func(ok bool, errCode, errDesc string) error {
		return s.sendServerMessage(correlationId, &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_RotateAuthResult{
				RotateAuthResult: &memqlv1.RotateAuthResult{
					Ok:               ok,
					Error:            errCode,
					ErrorDescription: errDesc,
				},
			},
		})
	}

	v := s.service.verifier
	if v == nil {
		return reply(false, "verifier_unconfigured", "this node was not started with a JWT verifier; cannot honor RotateAuth")
	}
	token := strings.TrimSpace(msg.GetAccessToken())
	if token == "" {
		return reply(false, "invalid_token", "access_token is required")
	}

	// Verify against the same JWKS / issuer / audience the stream
	// interceptor enforces. Pass the stream's context so JWKS refresh
	// gets cancelled if the client hangs up mid-verify.
	vc, err := v.VerifyBearer(s.stream.Context(), token)
	if err != nil {
		if s.logger != nil {
			s.logger.Info("rotate_auth: token verification failed",
				"subject", s.identity.Subject, "error", err)
		}
		return reply(false, "invalid_token", err.Error())
	}

	// Build the new UserIdentity off a synthetic context the verifier's
	// AttachToContext populates -- this routes through the same
	// auth.BuildTokenInfo path the interceptor uses, so the rotated
	// identity matches what a fresh stream would have seen.
	tempCtx := verifier.AttachToContext(context.Background(), vc)
	newIdentity, identityErr := auth.UserIdentityFromContext(tempCtx)
	if identityErr != nil {
		// Should not happen -- VerifyBearer success implies non-empty
		// claims -- but treat it as a hard reject rather than swap to
		// an unknown identity.
		if s.logger != nil {
			s.logger.Warn("rotate_auth: identity build from verified claims failed",
				"subject", s.identity.Subject, "error", identityErr)
		}
		return reply(false, "invalid_token", "verified claims did not yield an identity: "+identityErr.Error())
	}

	// Reload the user context under the lock. If LoadFromClaims errors
	// we DO NOT swap the identity -- leaving the session in a
	// half-rotated state (new identity, old user record) is worse than
	// failing the rotate. The client can retry.
	s.accessMu.Lock()
	defer s.accessMu.Unlock()

	if resolver := s.service.identityResolver; resolver != nil {
		newAccess, err := resolver.LoadFromClaims(s.stream.Context(), vc.ClaimsMap)
		if err != nil {
			if errors.Is(err, auth.ErrUserNotProvisioned) {
				// Same fallback path ensureAccess takes -- the user
				// row may legitimately not exist yet; derive context
				// from the verified claims.
				newAccess = auth.FallbackFromClaims(vc.ClaimsMap)
			} else {
				if s.logger != nil {
					s.logger.Warn("rotate_auth: identity reload failed; rotation rejected",
						"subject", newIdentity.Subject, "error", err)
				}
				return reply(false, "access_load_failed", err.Error())
			}
		}
		s.access = newAccess
		s.accessLoaded = true
	} else {
		// No identity resolver (test paths / no-DB builds). Clear the
		// cache; downstream code that needs an AccessContext will hit
		// ensureAccess and fall through to the claims-derived form.
		s.access = auth.FallbackFromClaims(vc.ClaimsMap)
		s.accessLoaded = true
	}

	prevSubject := s.identity.Subject
	s.identity = newIdentity

	if s.logger != nil {
		s.logger.Info("rotate_auth: identity swapped",
			"previousSubject", prevSubject,
			"newSubject", newIdentity.Subject)
	}
	return reply(true, "", "")
}

func (s *streamSession) handleMessage(envelope *memqlv1.MemqlClientMessage) error {
	if envelope == nil {
		return nil
	}

	payload := envelope.GetPayload()

	switch payload := payload.(type) {
	case *memqlv1.MemqlClientMessage_ClientHello:
		return s.handleClientHello(envelope, payload.ClientHello)
	case *memqlv1.MemqlClientMessage_ExecuteQuery:
		return s.handleExecuteQuery(envelope, payload.ExecuteQuery)
	case *memqlv1.MemqlClientMessage_CancelRequest:
		return s.handleCancelRequest(envelope, payload.CancelRequest)
	case *memqlv1.MemqlClientMessage_Subscribe:
		return s.handleSubscribe(envelope, payload.Subscribe)
	case *memqlv1.MemqlClientMessage_Unsubscribe:
		return s.handleUnsubscribe(envelope, payload.Unsubscribe)
	case *memqlv1.MemqlClientMessage_Ack:
		return nil
	case *memqlv1.MemqlClientMessage_ListTools:
		return s.handleListTools(envelope, payload.ListTools)
	case *memqlv1.MemqlClientMessage_CallTool:
		return s.handleCallTool(envelope, payload.CallTool)
	case *memqlv1.MemqlClientMessage_ClientToolResult:
		return s.handleClientToolResult(envelope, payload.ClientToolResult)
	case *memqlv1.MemqlClientMessage_AiChat:
		return s.handleAiChat(envelope, payload.AiChat)
	case *memqlv1.MemqlClientMessage_AiSpeech:
		return s.handleAiSpeech(envelope, payload.AiSpeech)
	case *memqlv1.MemqlClientMessage_AiTranscribe:
		return s.handleAiTranscribe(envelope, payload.AiTranscribe)
	case *memqlv1.MemqlClientMessage_AiTranscribeStreamStart:
		return s.handleAiTranscribeStreamStart(envelope, payload.AiTranscribeStreamStart)
	case *memqlv1.MemqlClientMessage_AiTranscribeStreamChunk:
		return s.handleAiTranscribeStreamChunk(envelope, payload.AiTranscribeStreamChunk)
	case *memqlv1.MemqlClientMessage_AiTranscribeStreamEnd:
		return s.handleAiTranscribeStreamEnd(envelope, payload.AiTranscribeStreamEnd)
	case *memqlv1.MemqlClientMessage_AiSuggest:
		return s.handleAiSuggest(envelope, payload.AiSuggest)
	// MemQL Sense -- language intelligence
	case *memqlv1.MemqlClientMessage_SenseTokenize:
		return s.handleSenseTokenize(envelope, payload.SenseTokenize)
	case *memqlv1.MemqlClientMessage_SenseComplete:
		return s.handleSenseComplete(envelope, payload.SenseComplete)
	case *memqlv1.MemqlClientMessage_SenseDiagnose:
		return s.handleSenseDiagnose(envelope, payload.SenseDiagnose)
	case *memqlv1.MemqlClientMessage_SenseHover:
		return s.handleSenseHover(envelope, payload.SenseHover)
	case *memqlv1.MemqlClientMessage_SenseSignatureHelp:
		return s.handleSenseSignatureHelp(envelope, payload.SenseSignatureHelp)
	// Pack browser -- read-only DSL pack enumeration (memql#2127 / B1)
	case *memqlv1.MemqlClientMessage_ListPackDomains:
		return s.handleListPackDomains(envelope, payload.ListPackDomains)
	case *memqlv1.MemqlClientMessage_ListPackFiles:
		return s.handleListPackFiles(envelope, payload.ListPackFiles)
	case *memqlv1.MemqlClientMessage_ReadPackFile:
		return s.handleReadPackFile(envelope, payload.ReadPackFile)
	// Polyphon -- multi-agent voice (Phase 3)
	case *memqlv1.MemqlClientMessage_PolyphonRoomToken:
		return s.handlePolyphonRoomToken(envelope, payload.PolyphonRoomToken)
	case *memqlv1.MemqlClientMessage_PolyphonStatus:
		return s.handlePolyphonStatus(envelope, payload.PolyphonStatus)
	case *memqlv1.MemqlClientMessage_PolyphonUtterance:
		return s.handlePolyphonUtterance(envelope, payload.PolyphonUtterance)
	// Access / authorization -- caller's own ACL
	case *memqlv1.MemqlClientMessage_MyAccess:
		return s.handleMyAccess(envelope, payload.MyAccess)
	// In-stream bearer rotation -- swap the session's identity
	// without dropping the stream. Lives next to the
	// other control-plane handlers so long-lived clients (cockpit)
	// can keep the in-flight bearer aligned with their background
	// refresh-token rotation. See handleRotateAuth.
	case *memqlv1.MemqlClientMessage_RotateAuth:
		return s.handleRotateAuth(envelope, payload.RotateAuth)
	// Operator maintenance trigger (memql#1270): owner/admin-gated; puts
	// THIS node into Draining on demand via the same #1269 drain mechanism.
	case *memqlv1.MemqlClientMessage_NodeMaintenance:
		return s.handleNodeMaintenance(envelope, payload.NodeMaintenance)
	// Concepts -- schema metadata (Phase 3)
	case *memqlv1.MemqlClientMessage_ConceptsList:
		return s.handleConceptsList(envelope, payload.ConceptsList)
	case *memqlv1.MemqlClientMessage_ConceptsSubscribe:
		return s.handleConceptsSubscribe(envelope, payload.ConceptsSubscribe)
	// Agent turn generation -- cognition forwards to agent via NodeService.
	case *memqlv1.MemqlClientMessage_AgentGenerateTurn:
		return s.handleAgentGenerateTurn(envelope, payload.AgentGenerateTurn)
	// Guest invites by email
	case *memqlv1.MemqlClientMessage_SendGuestInvite:
		return s.handleSendGuestInvite(envelope, payload.SendGuestInvite)
	case *memqlv1.MemqlClientMessage_ResolveGuestInvite:
		return s.handleResolveGuestInvite(envelope, payload.ResolveGuestInvite)
	case *memqlv1.MemqlClientMessage_JoinSpaceAsGuest:
		return s.handleJoinSpaceAsGuest(envelope, payload.JoinSpaceAsGuest)
	case *memqlv1.MemqlClientMessage_CancelGuestInvite:
		return s.handleCancelGuestInvite(envelope, payload.CancelGuestInvite)
	case *memqlv1.MemqlClientMessage_ResendGuestInviteEmail:
		return s.handleResendGuestInviteEmail(envelope, payload.ResendGuestInviteEmail)
	// Session revocation -- per-device and cross-device sign-out
	case *memqlv1.MemqlClientMessage_RevokeCurrentSession:
		return s.handleRevokeCurrentSession(envelope, payload.RevokeCurrentSession)
	case *memqlv1.MemqlClientMessage_RevokeAllSessions:
		return s.handleRevokeAllSessions(envelope, payload.RevokeAllSessions)
	case *memqlv1.MemqlClientMessage_CreateWorkerToken:
		return s.handleCreateWorkerToken(envelope, payload.CreateWorkerToken)
	case *memqlv1.MemqlClientMessage_RevokeWorkerToken:
		return s.handleRevokeWorkerToken(envelope, payload.RevokeWorkerToken)
	// Realtime voice + video (Initiative C). The Go voice-agent
	// (integrations/voice/agent) speaks these messages while authenticated
	// as a service account. See voice_agent_handlers.go.
	case *memqlv1.MemqlClientMessage_VoiceAgentSessionStart:
		return s.handleVoiceAgentSessionStart(envelope, payload.VoiceAgentSessionStart)
	case *memqlv1.MemqlClientMessage_VoiceAgentSessionEnd:
		return s.handleVoiceAgentSessionEnd(envelope, payload.VoiceAgentSessionEnd)
	case *memqlv1.MemqlClientMessage_VoiceAgentPartialTranscript:
		return s.handleVoiceAgentPartialTranscript(envelope, payload.VoiceAgentPartialTranscript)
	case *memqlv1.MemqlClientMessage_VoiceAgentFinalTranscript:
		return s.handleVoiceAgentFinalTranscript(envelope, payload.VoiceAgentFinalTranscript)
	case *memqlv1.MemqlClientMessage_VoiceAgentTurnRequest:
		return s.handleVoiceAgentTurnRequest(envelope, payload.VoiceAgentTurnRequest)
	case *memqlv1.MemqlClientMessage_VoiceAgentRealtimeOutput:
		return s.handleVoiceAgentRealtimeOutput(envelope, payload.VoiceAgentRealtimeOutput)
	case *memqlv1.MemqlClientMessage_VoiceAgentRealtimeSpeaking:
		return s.handleVoiceAgentRealtimeSpeaking(envelope, payload.VoiceAgentRealtimeSpeaking)
	default:
		if s.logger != nil {
			s.logger.Warn("ignoring unsupported memql client payload", "message_id", envelope.GetMessageId())
		}
		return nil
	}
}

func (s *streamSession) handleClientHello(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.ClientHello) error {
	if msg == nil {
		return nil
	}
	response := &memqlv1.ServerHello{
		NodeId:  string(ComponentName),
		Version: "v1",
	}
	return s.sendServerMessage(envelope.GetMessageId(), &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_ServerHello{
			ServerHello: response,
		},
	})
}

func (s *streamSession) handleExecuteQuery(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.ExecuteQueryMsg) error {
	if msg == nil {
		return s.sendQueryError("", envelope.GetMessageId(), codes.InvalidArgument, "query request missing")
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())
	query := strings.TrimSpace(msg.GetQuery())
	if query == "" {
		return s.sendQueryError(requestId, envelope.GetMessageId(), codes.InvalidArgument, "query cannot be empty")
	}

	// Authorization runs per-row inside the DSL (see
	// docs/public/operate/auth/per-row-authz-audit.md). Any authenticated role may
	// reach this handler; query/mutation bodies enforce ownership.

	ctx, cancel := context.WithCancel(s.stream.Context())

	// Enrich context with request metadata for the metadata collector.
	ctx = s.enrichContextWithMetadata(ctx, envelope)

	// Re-hydrate cross-node provenance: when this envelope arrived via
	// proxyAI / AiForward (BFF -> worker), stampEnvelopeProvenance put
	// the caller's provenance on envelope.Metadata. We attach it to ctx
	// so engine.Execute uses it instead of stamping a fresh default.
	ctx = contextWithEnvelopeProvenance(ctx, envelope)

	// Resolve + attach the caller's AccessContext so the engine can bind
	// actor.* references in DSL filters/bodies (e.g. currentUser's
	// `id==actor.userId`, or `createdBy: actor.userId` on mutations). The
	// verifier interceptor only stamps claims onto the stream context;
	// ensureAccess converts them to a full AccessContext (cached per
	// stream). Without this the engine's resolveActorPath sees no
	// AccessContext, actor.userId resolves to "", and every self-scoped
	// query/mutation silently no-ops (zero rows). See memql#216.
	ctx = auth.ContextWithAccess(ctx, s.ensureAccess(ctx))

	s.activeRequests.Store(requestId, cancel)

	// Extract clientId for optimistic update reconciliation
	// First check the proto field, then fall back to variables struct
	clientId := msg.GetClientId()
	if clientId == "" {
		if vars := msg.GetVariables(); vars != nil {
			if v, ok := vars.GetFields()["clientId"]; ok {
				clientId = v.GetStringValue()
			}
		}
	}

	go func() {
		defer cancel()
		defer s.activeRequests.Delete(requestId)

		result, err := s.service.executeQuery(ctx, query, clientId, msg.GetCursor())
		if err != nil {
			// Extract structured metadata if available
			var metadata map[string]string
			if sqe, ok := err.(*structuredQueryError); ok {
				metadata = sqe.metadata
			}
			s.sendQueryErrorWithMetadata(requestId, envelope.GetMessageId(), status.Code(err), status.Convert(err).Message(), metadata)
			return
		}
		chunk := &memqlv1.QueryResultChunk{
			RequestId: requestId,
			Result:    result,
			Done:      true,
		}
		_ = s.sendServerMessage(envelope.GetMessageId(), &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_QueryResult{
				QueryResult: chunk,
			},
		})
	}()

	return nil
}

func (s *streamSession) handleCancelRequest(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.CancelRequestMsg) error {
	if msg == nil {
		return s.sendQueryError("", envelope.GetMessageId(), codes.InvalidArgument, "cancel request missing")
	}
	requestId := strings.TrimSpace(msg.GetRequestId())
	if requestId == "" {
		return s.sendQueryError("", envelope.GetMessageId(), codes.InvalidArgument, "cancel request must include request_id")
	}

	if cancel, ok := s.activeRequests.LoadAndDelete(requestId); ok {
		if fn, ok := cancel.(context.CancelFunc); ok && fn != nil {
			fn()
		}
		s.sendEventNotification(envelope.GetMessageId(), requestId, "request-cancelled", map[string]any{
			"requestId": requestId,
			"status":    "cancelled",
		})
		return nil
	}

	return s.sendEventNotification(envelope.GetMessageId(), requestId, "request-missing", map[string]any{
		"requestId": requestId,
		"status":    "not_found",
	})
}

func (s *streamSession) handleSubscribe(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.SubscribeMsg) error {
	if msg == nil {
		return s.sendEventNotification(envelope.GetMessageId(), "", "subscription-error", map[string]any{
			"status":  "error",
			"message": "subscription payload missing",
		})
	}
	if s.logger != nil {
		s.logger.Info("handleSubscribe called", "filter", msg.GetFilter(), "kind", msg.GetKind().String())
	}
	subscriptionId := strings.TrimSpace(msg.GetSubscriptionId())
	if subscriptionId == "" {
		subscriptionId = id.NewShortId()
	}

	// Store the subscription info for matching
	info := &subscriptionInfo{
		msg: msg,
	}
	s.subscriptions.Store(subscriptionId, info)

	// Register with the event bus if available
	if s.service.eventBus != nil {
		pattern := events.TopicPatternFromSubscriptionKind(msg.GetKind(), msg.GetFilter())
		unsubscribe := s.service.eventBus.Subscribe(pattern, func(event events.Event) {
			select {
			case s.eventChan <- event:
			default:
				// Channel full, drop event
				if s.logger != nil {
					s.logger.Warn("event channel full, dropping event",
						"subscription_id", subscriptionId,
						"topic", event.Topic,
					)
				}
			}
		})
		s.unsubscribers.Store(subscriptionId, unsubscribe)

		if s.logger != nil {
			s.logger.Info("subscription registered with event bus",
				"subscription_id", subscriptionId,
				"pattern", pattern,
				"kind", msg.GetKind().String(),
			)
		}
	}

	responsePayload := map[string]any{
		"status":           "subscribed",
		"subscriptionId":   subscriptionId,
		"subscriptionKind": msg.GetKind().String(),
		"filter":           msg.GetFilter(),
	}

	return s.sendEventNotification(envelope.GetMessageId(), subscriptionId, "subscription-created", responsePayload)
}

func (s *streamSession) handleUnsubscribe(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.UnsubscribeMsg) error {
	if msg == nil {
		return s.sendEventNotification(envelope.GetMessageId(), "", "subscription-error", map[string]any{
			"status":  "error",
			"message": "unsubscribe payload missing",
		})
	}
	subscriptionId := strings.TrimSpace(msg.GetSubscriptionId())
	if subscriptionId == "" {
		return s.sendEventNotification(envelope.GetMessageId(), "", "subscription-error", map[string]any{
			"status":  "error",
			"message": "subscription_id required",
		})
	}

	// Call the unsubscribe function if it exists
	if unsubFn, ok := s.unsubscribers.LoadAndDelete(subscriptionId); ok {
		if fn, ok := unsubFn.(func()); ok && fn != nil {
			fn()
		}
	}

	s.subscriptions.Delete(subscriptionId)
	return s.sendEventNotification(envelope.GetMessageId(), subscriptionId, "subscription-removed", map[string]any{
		"status":         "unsubscribed",
		"subscriptionId": subscriptionId,
	})
}

// handleListTools returns the list of available MCP tools.
func (s *streamSession) handleListTools(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.ListToolsMsg) error {
	if msg == nil {
		return s.sendToolError(envelope.GetMessageId(), "", "list_tools request missing")
	}

	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())

	if s.shouldProxyAI(nodeTargetForListTools()) {
		// Thread the voice session's tool-scope context across the proxy hop
		// (#1448). ListTools is forwarded to the agent node, whose session has
		// no local voiceAgentScopeId / voiceAgentGaAgentId (those bind only on
		// the bff session that received VoiceAgentSessionStart). Without this
		// the receiving node resolves no scope and fails open to the full
		// 517-tool registry. Stamp the bound scope onto the message (which is
		// the same object the envelope carries) so the agent node scopes the
		// surface to the GA's tool list. No-op for non-voice callers (both
		// fields empty), preserving their unscoped behaviour.
		s.stampVoiceAgentScopeOnListTools(msg)
		return s.proxyAI(envelope, requestId, nodeTargetForListTools())
	}

	if s.service.engine == nil {
		return s.sendToolError(envelope.GetMessageId(), requestId, "MemQL engine unavailable")
	}

	registry := s.service.engine.Tools()
	var toolDefs []*memqlv1.ToolDefinition

	if registry != nil {
		callerRole := callerRoleFromMetadata(envelope)
		tools := registry.List()
		// Voice-agent sessions get the GA agent's tool surface (#1419) -- the
		// same slug-expanded list the text loop binds -- not the global
		// registry. `scoped` reports whether the GA's surface was resolved
		// AUTHORITATIVELY: when true we restrict the exposed list to agentScope
		// EXACTLY, even when agentScope is empty (a GA with no tools is a real
		// answer, NOT a reason to dump the 517-tool registry -- #1442). When
		// false (any non-voice caller, or a genuine lookup error -- WARN'd at
		// the lookup site) we keep the unscoped behaviour / fail open.
		// Resolve scope from the threaded request context first (the proxied
		// agent-node receiver path -- #1448), falling back to local session
		// state for non-proxied callers.
		agentScope, scopedRole, scoped := s.voiceAgentScopedToolNamesForRequest(msg.GetVoiceAgentScopeId(), msg.GetVoiceAgentGaAgentId())
		// When the surface is scoped to a specific GA agent, gate tools against
		// THAT agent's role, not the voice-agent caller's role. The voice-agent
		// service presents no role (empty -> "specialist" default), but the tools
		// are being exposed FOR the GA, an "assistant"-role agent. GA-only tools
		// (@allowedRoles("assistant"): produceArtifact + the operator/uiClick
		// surface) are correctly scoped IN but were then role-filtered OUT against
		// the empty caller role -- the bug that made the realtime model unable to
		// delegate work or drive the UI despite the skills resolving fine.
		roleForGate := callerRole
		if scoped && scopedRole != "" {
			roleForGate = scopedRole
		}
		toolDefs = make([]*memqlv1.ToolDefinition, 0, len(tools))
		for _, tool := range tools {
			if !tool.IsAllowedForRole(roleForGate) {
				continue
			}
			if scoped {
				if _, ok := agentScope[tool.Name]; !ok {
					continue
				}
			}
			toolDefs = append(toolDefs, &memqlv1.ToolDefinition{
				Name:            tool.Name,
				Description:     tool.Description,
				InputSchema:     string(tool.InputSchema),
				ClientExecution: tool.ClientExecution,
				Scopes:          append([]string(nil), tool.Scopes...),
			})
		}
	}

	result := &memqlv1.ListToolsResult{
		RequestId: requestId,
		Tools:     toolDefs,
	}

	return s.sendServerMessage(envelope.GetMessageId(), &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_ListToolsResult{
			ListToolsResult: result,
		},
	})
}

// handleCallTool executes an MCP tool and returns the result.
func (s *streamSession) handleCallTool(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.CallToolMsg) error {
	if msg == nil {
		return s.sendToolError(envelope.GetMessageId(), "", "call_tool request missing")
	}

	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())
	toolName := strings.TrimSpace(msg.GetName())

	if toolName == "" {
		return s.sendToolError(envelope.GetMessageId(), requestId, "tool name is required")
	}

	if s.shouldProxyAI(nodeTargetForCallTool()) {
		// Thread the voice-agent execution context across the proxy hop (mirror
		// of stampVoiceAgentScopeOnListTools #1448). CallTool runs on the agent
		// node, whose session has no local voiceAgentScopeId / voiceAgentGaRole;
		// without this the agent-node role gate sees the empty caller role and
		// rejects GA-only tools (uiClick/operator, produceArtifact), and the
		// client-tool relay has no space to scope to.
		s.stampVoiceAgentScopeOnCallTool(msg)
		return s.proxyAI(envelope, requestId, nodeTargetForCallTool())
	}

	// Allow any authenticated user to call tools (user, manager, admin, owner)
	// This enables frontend client apps to work with memql-users role

	if s.service.engine == nil {
		return s.sendCallToolResult(envelope.GetMessageId(), requestId, nil, true, "MemQL engine unavailable")
	}

	registry := s.service.engine.Tools()
	if registry == nil {
		return s.sendCallToolResult(envelope.GetMessageId(), requestId, nil, true, "no tools available")
	}

	tool, err := registry.Get(toolName)
	if err != nil {
		return s.sendCallToolResult(envelope.GetMessageId(), requestId, nil, true, fmt.Sprintf("tool %q not found", toolName))
	}

	// Role gate: tools with AllowedRoles reject callers outside the allowed set.
	callerRole := callerRoleFromMetadata(envelope)
	// Voice-agent sessions present no caller role (empty -> "specialist"); gate
	// EXECUTION on the scoped GA agent's role (assistant), the same role the
	// ListTools scope gate uses (#1467). Without this, a GA-only tool the model
	// was allowed to SEE (produceArtifact, the operator/uiClick Takeover surface)
	// is rejected when it actually CALLS it -- the "not allowed for caller role"
	// failure Sofia surfaces after acknowledging the action.
	//
	// On the agent node (the proxied receiver), the GA role arrives THREADED on
	// the CallToolMsg (the bff stamped it -- handleCallTool runs on the agent
	// node, whose local voiceAgentGaRole is empty). Prefer the threaded value;
	// fall back to the local session role for in-process / non-proxied callers.
	if r := strings.TrimSpace(msg.GetVoiceAgentGaRole()); r != "" {
		callerRole = r
	} else {
		s.voiceAgentSpeakSubMu.Lock()
		gaRole := s.voiceAgentGaRole
		s.voiceAgentSpeakSubMu.Unlock()
		if gaRole != "" {
			callerRole = gaRole
		}
	}
	if !tool.IsAllowedForRole(callerRole) {
		return s.sendCallToolResult(envelope.GetMessageId(), requestId, nil, true, fmt.Sprintf("tool %q is not allowed for caller role %q", toolName, callerRole))
	}

	ctx := s.stream.Context()
	// Re-hydrate cross-node provenance for tool calls that arrived via
	// proxyAI / AiForward. Any row this tool writes via the engine will
	// stamp the originating caller's provenance instead of a fresh
	// per-tool default.
	ctx = contextWithEnvelopeProvenance(ctx, envelope)
	// Auto-inject the voice-path tool defaults (#1503). On the realtime
	// voice CallTool proxy hop the agent-node session has no local voice
	// scope -- the bff threads it on the CallToolMsg. The engine's central
	// auto-injection (applyToolDefaults in ExecuteTool) stamps the
	// `@autoInjected` partitionId / agentId / ownerUserId fields from
	// ToolDefaultsFromContext, so set them here for any @autoInjected tool
	// the voice model can reach (produceArtifact, requestComputerUseScope,
	// editDocument, ...). Resolution is at the default layer, not per-tool,
	// so adding a new @autoInjected voice tool needs no change here.
	ctx = s.contextWithVoiceCallToolDefaults(ctx, msg)
	args := msg.GetArguments()

	// Client-executed tool: the body runs in a browser, not on the engine.
	//
	// Direct browser session: emit ClientToolCall on THIS stream, park until
	// the browser answers with a ClientToolResult (or timeout).
	//
	// Voice-agent session (#1420): the calling stream is the voice-agent
	// process -- it has NO browser, so emitting the ClientToolCall here would
	// only time out (the pre-#1420 behaviour). Instead route through the
	// cross-node client-tool relay: emit a v1:cognition:client:tool:request
	// graph node scoped to the voice space so the browser's space subscription
	// dispatches it, and park on the service waiter until the matching
	// client:tool:response lands (relayClientToolToBrowser, fired by the
	// service's client:tool:response subscription). This is what lets the
	// realtime voice model drive the browser ("change the app appearance").
	if tool.ClientExecution {
		// Voice space for the relay: local session (in-process) first, else the
		// value threaded on the CallToolMsg (the agent-node proxied receiver, whose
		// local voiceAgentScopeId is empty).
		voiceScopeId := strings.TrimSpace(s.voiceAgentScopeId)
		if voiceScopeId == "" {
			voiceScopeId = strings.TrimSpace(msg.GetVoiceAgentScopeId())
		}
		if partitionId := voiceScopeId; partitionId != "" {
			argsJSON := "{}"
			if args != nil {
				if b, err := json.Marshal(args.AsMap()); err == nil {
					argsJSON = string(b)
				}
			}
			// The voice bridge self-mirrors the call via its own log sink
			// (NewLogMirrorSink), so no mirrorRealtimeToolCall here -- it is a
			// no-op for voice-agent sessions by design (avoids a double-log).
			content, execErr := s.relayClientToolToBrowser(ctx, tool.Name, argsJSON, partitionId)
			if execErr != nil {
				// Surface a clean, user-legible message for the fast-fail
				// browser-unreachable case (#1834) so voice doesn't speak a
				// bare "it timed out"; genuine browser errors pass through.
				return s.sendCallToolResult(envelope.GetMessageId(), requestId, nil, true, userFacingClientToolError(execErr))
			}
			return s.sendCallToolResult(envelope.GetMessageId(), requestId, content, false, "")
		}
		content, execErr := s.executeClientTool(ctx, tool.Name, args, envelope.GetMessageId())
		if execErr != nil {
			return s.sendCallToolResult(envelope.GetMessageId(), requestId, nil, true, userFacingClientToolError(execErr))
		}
		return s.sendCallToolResult(envelope.GetMessageId(), requestId, content, false, "")
	}

	// Non-client-execution path still delegates to the engine. Attach
	// this session as the ClientToolInvoker and the caller role so any
	// nested engine.ExecuteTool calls for client_execution tools
	// (e.g. recipes calling primitives via ctx.dispatch) work from
	// here too. Redundant for the common case but keeps a single
	// invariant: every engine.ExecuteTool reached from a stream has
	// the session available on context.
	ctx = memqlengine.WithClientToolInvoker(ctx, s)
	if callerRole != "" {
		ctx = memqlengine.WithActingAgentRole(ctx, callerRole)
	}
	result, execErr := s.executeTool(ctx, s.service.engine, tool, args)
	if execErr != nil {
		// Mirror the failed call too, matching the relay bridge (copresent#158).
		s.mirrorRealtimeToolCall(callerRole, tool.Name, args, execErr.Error(), true)
		return s.sendCallToolResult(envelope.GetMessageId(), requestId, nil, true, execErr.Error())
	}

	// Awareness breadcrumb for the direct browser path's low-risk read tools
	// (copresent#158). No-op for voice-agent streams + non-allowlisted tools.
	s.mirrorRealtimeToolCall(callerRole, tool.Name, args, flattenToolResultContent(result), false)
	return s.sendCallToolResult(envelope.GetMessageId(), requestId, result, false, "")
}

// handleClientToolResult is invoked when the connected client (direct
// browser session, or the cluster relay arriving as an AiForwardRequest
// continuation) returns the result of a ClientToolCall we emitted
// earlier. Looks up the service-scoped waiter by call_id and delivers
// the result so the parked tool call can return on whichever session
// originally registered it.
func (s *streamSession) handleClientToolResult(_ *memqlv1.MemqlClientMessage, msg *memqlv1.ClientToolResult) error {
	if msg == nil || s.service == nil {
		return nil
	}
	callId := msg.GetCallId()
	if callId == "" {
		return nil
	}
	s.service.clientToolMu.Lock()
	ch, ok := s.service.clientToolWaiters[callId]
	if ok {
		delete(s.service.clientToolWaiters, callId)
	}
	s.service.clientToolMu.Unlock()
	if !ok {
		if s.logger != nil {
			s.logger.Debug("client tool result for unknown call_id (timed out or duplicate)",
				"component", ComponentName,
				"call_id", callId)
		}
		return nil
	}
	select {
	case ch <- msg:
	default:
	}
	return nil
}

// DeliverClientToolResult fires the service-scoped client-tool waiter for a
// result that arrived over the substrate-RPC return channel (memql#1265). It
// implements node.ClientToolResultFirer so the agent's ClientToolResultServer
// can hand a decoded result straight to the parked waiter, independent of any
// streamSession. Reuses the exact same dispatch as handleClientToolResult (the
// legacy ForwardContinuation landing) -- the only difference is the transport
// that carried the result here. Returns whether a live waiter was found+fired
// so the RPC reply can tell cognition the result actually landed.
func (s *service) DeliverClientToolResult(p node.ClientToolResultPayload) bool {
	if s == nil || p.CallID == "" {
		return false
	}
	result := &memqlv1.ClientToolResult{
		CallId:       p.CallID,
		Content:      parseClientToolContentJSON(p.ContentJSON),
		IsError:      p.IsError,
		ErrorMessage: p.ErrorMessage,
	}
	s.clientToolMu.Lock()
	ch, ok := s.clientToolWaiters[p.CallID]
	if ok {
		delete(s.clientToolWaiters, p.CallID)
	}
	s.clientToolMu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- result:
	default:
	}
	return true
}

// parseClientToolContentJSON decodes the browser's JSON-encoded tool-result
// content array into []*ToolResultContent. Returns nil on empty/invalid input so
// the agent sees an empty result rather than a malformed reply. Mirrors
// cognition's parseToolResultContent (the producer side) so the substrate-RPC
// path and the legacy relay path decode identically.
func parseClientToolContentJSON(jsonStr string) []*memqlv1.ToolResultContent {
	jsonStr = strings.TrimSpace(jsonStr)
	if jsonStr == "" || jsonStr == "null" {
		return nil
	}
	var items []map[string]any
	if err := json.Unmarshal([]byte(jsonStr), &items); err != nil {
		return nil
	}
	out := make([]*memqlv1.ToolResultContent, 0, len(items))
	for _, item := range items {
		typeStr, _ := item["type"].(string)
		text, _ := item["text"].(string)
		mimeType, _ := item["mimeType"].(string)
		data, _ := item["data"].(string)
		uri, _ := item["uri"].(string)
		out = append(out, &memqlv1.ToolResultContent{
			Type:     typeStr,
			Text:     text,
			MimeType: mimeType,
			Data:     data,
			Uri:      uri,
		})
	}
	return out
}

// executeClientTool emits a ClientToolCall envelope on the stream and
// blocks until the client responds with a matching ClientToolResult or
// the timeout expires. The agent microservice can call this exactly like
// executeTool; the difference is that the tool body runs in the browser.
func (s *streamSession) executeClientTool(ctx context.Context, toolName string, args *structpb.Struct, turnId string) ([]*memqlv1.ToolResultContent, error) {
	callId := id.NewShortId()
	argsJSON := "{}"
	if args != nil {
		if bytes, err := json.Marshal(args.AsMap()); err == nil {
			argsJSON = string(bytes)
		}
	}

	// Register the waiter before sending so we never miss a fast reply.
	ch := s.registerClientToolWaiter(callId)
	defer s.unregisterClientToolWaiter(callId)

	call := &memqlv1.ClientToolCall{
		CallId:        callId,
		TurnId:        turnId,
		AgentId:       memqlengine.ActingAgentIdFromContext(ctx),
		ToolName:      toolName,
		ArgumentsJson: argsJSON,
		TimeoutMs:     clientToolTimeoutMs(toolName),
	}
	if s.logger != nil {
		s.logger.Info("client-tool waiter registered",
			"component", ComponentName, "path", "browser",
			"call_id", callId, "tool", toolName,
			"timeout_ms", clientToolWaitTimeout(toolName).Milliseconds())
	}
	if err := s.sendServerMessage("", &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_ClientToolCall{
			ClientToolCall: call,
		},
	}); err != nil {
		return nil, fmt.Errorf("send client tool call: %w", err)
	}

	result, outcome, elapsed := awaitClientToolResult(ctx, ch, toolName)
	if s.logger != nil {
		s.logger.Info("client-tool waiter resolved",
			"component", ComponentName, "path", "browser",
			"call_id", callId, "tool", toolName,
			"outcome", string(outcome), "elapsed_ms", elapsed.Milliseconds())
	}
	switch outcome {
	case clientToolCancelled:
		return nil, ctx.Err()
	case clientToolTimedOut:
		// Browser gone / unreachable: fail fast with the typed, user-legible
		// error so the caller surfaces a clear message (#1834).
		return nil, clientToolUnreachableError(toolName, elapsed)
	}
	if result.GetIsError() {
		errMsg := result.GetErrorMessage()
		if errMsg == "" {
			errMsg = "client tool execution failed"
		}
		return nil, errors.New(errMsg)
	}
	return result.GetContent(), nil
}

func (s *streamSession) registerClientToolWaiter(callId string) chan *memqlv1.ClientToolResult {
	ch := make(chan *memqlv1.ClientToolResult, 1)
	s.service.clientToolMu.Lock()
	defer s.service.clientToolMu.Unlock()
	if s.service.clientToolWaiters == nil {
		s.service.clientToolWaiters = make(map[string]chan *memqlv1.ClientToolResult)
	}
	s.service.clientToolWaiters[callId] = ch
	return ch
}

func (s *streamSession) unregisterClientToolWaiter(callId string) {
	s.service.clientToolMu.Lock()
	defer s.service.clientToolMu.Unlock()
	delete(s.service.clientToolWaiters, callId)
}

// InvokeClientTool implements memqlengine.ClientToolInvoker. It lets the
// engine's ExecuteTool path round-trip a client_execution tool through
// this stream session -- i.e., whenever the agent loop (or cognition's
// streaming tool loop) calls engine.ExecuteToolByName for a tool whose
// implementation lives in the browser, the call lands here via the
// invoker carried on context (see WithClientToolInvoker in
// handleAgentGenerateTurn and handleCallTool below).
//
// Role gate: the engine already checked Tool.IsAllowedForRole before
// calling us, so we trust and pass through here -- no redundant check.
func (s *streamSession) InvokeClientTool(
	ctx context.Context,
	toolName string,
	argsJSON string,
	_ string,
) (*memqlengine.ToolCallResult, error) {
	callId := id.NewShortId()
	if argsJSON == "" {
		argsJSON = "{}"
	}

	// Enforce tool-specific argument contracts the agent keeps
	// forgetting. For uiAskUser, we give the agent up to
	// uiAskUserRejectBudget chances to self-correct (each rejection
	// feeds the retry-instructional error back to the model as a
	// tool-result). After the budget is spent we soft-fallback by
	// injecting default options -- the walkthrough keeps moving
	// with a rendered card, even if the agent never figured out how
	// to populate options itself. Any valid uiAskUser call resets
	// the counter.
	if adjusted, err := s.guardClientToolArgs(toolName, argsJSON); err != nil {
		return nil, err
	} else {
		argsJSON = adjusted
	}

	ch := s.registerClientToolWaiter(callId)
	defer s.unregisterClientToolWaiter(callId)

	call := &memqlv1.ClientToolCall{
		CallId:        callId,
		AgentId:       memqlengine.ActingAgentIdFromContext(ctx),
		ToolName:      toolName,
		ArgumentsJson: argsJSON,
		TimeoutMs:     clientToolTimeoutMs(toolName),
	}
	if s.logger != nil {
		s.logger.Info("client-tool waiter registered",
			"component", ComponentName, "path", "agent",
			"call_id", callId, "tool", toolName,
			"timeout_ms", clientToolWaitTimeout(toolName).Milliseconds())
	}
	if err := s.sendServerMessage("", &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_ClientToolCall{
			ClientToolCall: call,
		},
	}); err != nil {
		return nil, fmt.Errorf("send client tool call: %w", err)
	}

	result, outcome, elapsed := awaitClientToolResult(ctx, ch, toolName)
	if s.logger != nil {
		s.logger.Info("client-tool waiter resolved",
			"component", ComponentName, "path", "agent",
			"call_id", callId, "tool", toolName,
			"outcome", string(outcome), "elapsed_ms", elapsed.Milliseconds())
	}
	switch outcome {
	case clientToolCancelled:
		return nil, ctx.Err()
	case clientToolTimedOut:
		if toolName == "uiAskUser" {
			// uiAskUser blocks on a human, not the browser handshake. An
			// unanswered ask is "user dismissed", not "browser unreachable":
			// return a synthesised empty-answer result the agent reads and
			// releases control gracefully -- never the fast-fail typed error.
			return synthesiseTimeoutResult(toolName, clientToolWaitTimeout(toolName)), nil
		}
		// Interactive UI tool: the browser is gone / unreachable. Fail fast
		// with the typed, user-legible error so the agent loop surfaces a
		// clear message instead of stalling (#1834).
		return nil, clientToolUnreachableError(toolName, elapsed)
	}

	// Responded: the browser returned a result.
	if result.GetIsError() {
		errMsg := result.GetErrorMessage()
		if errMsg == "" {
			errMsg = "client tool execution failed"
		}
		return nil, errors.New(errMsg)
	}
	out := &memqlengine.ToolCallResult{
		Content: make([]memqlengine.ToolResultContent, 0, len(result.GetContent())),
	}
	for _, c := range result.GetContent() {
		out.Content = append(out.Content, memqlengine.ToolResultContent{
			Type:     c.GetType(),
			Text:     c.GetText(),
			MimeType: c.GetMimeType(),
			Data:     c.GetData(),
			URI:      c.GetUri(),
		})
	}
	return out, nil
}

// synthesiseTimeoutResult builds the result the agent sees when a
// client tool didn't respond in time. The agent prompt treats an
// empty uiAskUser answer as "user dismissed" and should release
// control gracefully rather than keep retrying.
func synthesiseTimeoutResult(toolName string, timeout time.Duration) *memqlengine.ToolCallResult {
	// Match the shape ordinary success responses use so the agent's
	// tool-result parser reads this as just another JSON payload
	// ("type":"json", text=JSON.stringify(data)). See
	// copresent/src/lib/operator/clientToolHost.ts toToolResultContent.
	if toolName == "uiAskUser" {
		return &memqlengine.ToolCallResult{
			Content: []memqlengine.ToolResultContent{{
				Type:     "json",
				MimeType: "application/json",
				Text:     fmt.Sprintf(`{"answer":"","fromOption":false,"timedOut":true,"timeoutMs":%d}`, timeout.Milliseconds()),
			}},
		}
	}
	return &memqlengine.ToolCallResult{
		Content: []memqlengine.ToolResultContent{{
			Type:     "json",
			MimeType: "application/json",
			Text:     fmt.Sprintf(`{"timedOut":true,"timeoutMs":%d,"tool":%q}`, timeout.Milliseconds(), toolName),
		}},
	}
}

// uiAskUserRejectBudget is the number of times we reject a
// missing-options uiAskUser call before giving up and injecting
// default options so the walkthrough keeps moving. Each rejection
// costs one tool-loop iteration on the agent side (the model sees
// the instructional error and retries); after the budget we decide
// the agent is genuinely stuck on this question and would rather
// see *some* card than none.
const uiAskUserRejectBudget = 3

// uiAskUserSoftFallbackOptions are the options we inject when the
// retry budget is spent. Deliberately generic so they work for any
// question: "Pick one for me" delegates back to the agent (it can
// reasonably interpret that as "make up a value and proceed"), and
// "Skip this question" lets the user bail out without abandoning
// the whole takeover. allowFreeForm=true means the user can still
// type a specific answer.
var uiAskUserSoftFallbackOptions = []string{"Pick one for me", "Skip this question"}

// guardClientToolArgs enforces per-tool argument contracts we need
// the agent to respect. Today it handles just uiAskUser's "options
// must be populated" rule. Returns the argsJSON to forward (possibly
// rewritten with injected defaults when the retry budget is spent)
// and an error when the call should be rejected outright -- the
// agent tool loop treats the error as a tool-call failure and feeds
// the instructional message back to the model for the retry.
func (s *streamSession) guardClientToolArgs(toolName, argsJSON string) (string, error) {
	if toolName != "uiAskUser" {
		return argsJSON, nil
	}
	var parsed struct {
		Question      string   `json:"question"`
		Options       []string `json:"options"`
		AllowFreeForm *bool    `json:"allowFreeForm,omitempty"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &parsed); err != nil {
		return argsJSON, nil
	}
	if strings.TrimSpace(parsed.Question) == "" {
		return "", fmt.Errorf(`uiAskUser requires a non-empty "question" string`)
	}
	valid := 0
	for _, opt := range parsed.Options {
		if strings.TrimSpace(opt) != "" {
			valid++
		}
	}
	if valid >= 2 {
		// Call is well-formed. Reset the reject counter so a future
		// bad call gets the full retry budget again.
		s.uiAskUserMu.Lock()
		s.uiAskUserRejectCount = 0
		s.uiAskUserMu.Unlock()
		return argsJSON, nil
	}

	// Call is under-specified. Decide whether to reject (forcing the
	// model to retry with options) or soft-fallback (injecting
	// defaults so the card still renders).
	s.uiAskUserMu.Lock()
	s.uiAskUserRejectCount++
	count := s.uiAskUserRejectCount
	s.uiAskUserMu.Unlock()

	if count <= uiAskUserRejectBudget {
		if s.logger != nil {
			s.logger.Warn("uiAskUser rejected: missing options",
				"attempt", count,
				"budget", uiAskUserRejectBudget,
				"validOptions", valid,
			)
		}
		return "", fmt.Errorf(
			`uiAskUser requires an "options" array with 2 or 3 concrete, actionable suggestions. Got %d. Retry this call with options populated -- e.g. for a Name field: ["Pick a random name for me","Zeus","Aria"]; for a Role field: ["IT Support","Assistant","Customer Service"]. If the answer is genuinely open-ended, include one "Pick for me"/"You decide" option plus two concrete examples. The prompt card renders these as click-to-pick pills above the free-form input -- without them the user gets a bare text input, which is the failure mode this validation is guarding against`,
			valid,
		)
	}

	// Budget exhausted. Inject default options + preserve whatever
	// the agent did send (question text, allowFreeForm) so the card
	// still renders usefully. Reset the counter so the next mistake
	// gets a fresh budget.
	if s.logger != nil {
		s.logger.Warn("uiAskUser soft-fallback: reject budget exhausted, injecting default options",
			"attempts", count,
			"budget", uiAskUserRejectBudget,
			"question", parsed.Question,
		)
	}
	s.uiAskUserMu.Lock()
	s.uiAskUserRejectCount = 0
	s.uiAskUserMu.Unlock()

	// Rebuild the JSON with our default options merged in. Preserve
	// the question as-is; preserve allowFreeForm if set (default true).
	patched := map[string]any{
		"question": parsed.Question,
		"options":  uiAskUserSoftFallbackOptions,
	}
	if parsed.AllowFreeForm != nil {
		patched["allowFreeForm"] = *parsed.AllowFreeForm
	} else {
		patched["allowFreeForm"] = true
	}
	rewritten, err := json.Marshal(patched)
	if err != nil {
		// Can't realistically fail with these inputs, but fall back
		// to a conservative hand-built JSON rather than rejecting.
		return argsJSON, nil
	}
	return string(rewritten), nil
}

// clientToolTimeoutMs is the ClientToolCall.TimeoutMs stamped on the wire so
// the cognition relay's TTL sweeper knows the call's declared deadline. It
// mirrors the in-process wait (clientToolWaitTimeout): the fast interactive
// ceiling for programmatic UI tools (#1834), the longer human-input ceiling
// for uiAskUser. Note the cognition relay still clamps its correlation
// lifetime up to pendingClientToolCallTTL (60s), so the 60s backstop is
// preserved regardless of this value.
func clientToolTimeoutMs(toolName string) int32 {
	return int32(clientToolWaitTimeout(toolName).Milliseconds())
}

// callerRoleFromMetadata pulls the caller's agent role (if any) from
// envelope metadata. Cognition attaches this when forwarding a
// CallToolMsg on behalf of an agent. Empty string means "no declared
// role" -- Tool.IsAllowedForRole treats that as "specialist".
func callerRoleFromMetadata(envelope *memqlv1.MemqlClientMessage) string {
	if envelope == nil {
		return ""
	}
	md := envelope.GetMetadata()
	if md == nil {
		return ""
	}
	return strings.TrimSpace(md["agent_role"])
}

// executeTool runs a tool and returns the result content.
func (s *streamSession) executeTool(ctx context.Context, engine *memqlengine.MemQLEngine, tool *memqlengine.Tool, args *structpb.Struct) ([]*memqlv1.ToolResultContent, error) {
	if tool == nil {
		return nil, fmt.Errorf("tool is nil")
	}

	// If no handler is defined, return tool info as a text response
	if tool.Handler == nil {
		return []*memqlv1.ToolResultContent{
			{
				Type: "text",
				Text: fmt.Sprintf("Tool %q has no handler defined", tool.Name),
			},
		}, nil
	}

	handler := tool.Handler

	switch strings.ToLower(handler.Type) {
	case "query":
		toolResult, err := engine.ExecuteTool(ctx, tool, argsAsMap(args))
		if err != nil {
			return nil, err
		}
		return toolResultToProto(toolResult), nil
	case "function":
		toolResult, err := engine.ExecuteTool(ctx, tool, argsAsMap(args))
		if err != nil {
			return nil, err
		}
		return toolResultToProto(toolResult), nil
	case "webhook":
		// Webhook execution would require HTTP client - not implemented in this version
		toolResult, err := engine.ExecuteTool(ctx, tool, argsAsMap(args))
		if err != nil {
			return nil, err
		}
		return toolResultToProto(toolResult), nil
	default:
		return nil, fmt.Errorf("unknown handler type %q", handler.Type)
	}
}

func argsAsMap(args *structpb.Struct) map[string]any {
	if args == nil {
		return nil
	}
	m := args.AsMap()
	if len(m) == 0 {
		return nil
	}
	return m
}

func toolResultToProto(result *memqlengine.ToolCallResult) []*memqlv1.ToolResultContent {
	if result == nil || len(result.Content) == 0 {
		return nil
	}
	out := make([]*memqlv1.ToolResultContent, 0, len(result.Content))
	for _, c := range result.Content {
		out = append(out, &memqlv1.ToolResultContent{
			Type:     c.Type,
			Text:     c.Text,
			MimeType: c.MimeType,
			Data:     c.Data,
			Uri:      c.URI,
		})
	}
	return out
}

// sendToolError sends a tool error as an event notification.
func (s *streamSession) sendToolError(correlate, requestId, message string) error {
	return s.sendCallToolResult(correlate, requestId, nil, true, message)
}

// sendCallToolResult sends a tool execution result.
func (s *streamSession) sendCallToolResult(correlate, requestId string, content []*memqlv1.ToolResultContent, isError bool, errorMessage string) error {
	if isError && len(content) == 0 {
		content = []*memqlv1.ToolResultContent{
			{
				Type: "text",
				Text: errorMessage,
			},
		}
	}

	result := &memqlv1.CallToolResult{
		RequestId: requestId,
		Content:   content,
		IsError:   isError,
	}

	return s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_CallToolResult{
			CallToolResult: result,
		},
	})
}

func (s *streamSession) sendQueryError(requestId, correlate string, code codes.Code, message string) error {
	return s.sendQueryErrorWithMetadata(requestId, correlate, code, message, nil)
}

func (s *streamSession) sendQueryErrorWithMetadata(requestId, correlate string, code codes.Code, message string, metadata map[string]string) error {
	if requestId == "" {
		requestId = correlate
	}
	if message == "" {
		message = code.String()
	}
	errMsg := &memqlv1.QueryErrorMsg{
		RequestId: requestId,
		Error: &memqlv1.QueryError{
			Code:     code.String(),
			Message:  message,
			Metadata: metadata,
		},
	}
	return s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_QueryError{
			QueryError: errMsg,
		},
	})
}

func (s *streamSession) sendEventNotification(correlate, subscriptionId, eventType string, payload map[string]any) error {
	now := timestamppb.Now()
	if payload == nil {
		payload = make(map[string]any)
	}
	if strings.TrimSpace(eventType) != "" {
		payload["eventType"] = eventType
	}
	structPayload, _ := structpb.NewStruct(payload)
	event := &memqlv1.EventNotification{
		SubscriptionId: subscriptionId,
		Kind:           memqlv1.EventKind_EVENT_KIND_MESSAGE,
		Ts:             now,
		Payload:        structPayload,
	}
	return s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_Event{
			Event: event,
		},
	})
}

// sendServerMessage writes a MemqlServerMessage to the stream. Callers
// construct the message with the Payload oneof variant already set; the
// function stamps a fresh MessageId and the caller-supplied CorrelateTo
// and hands it to stream.Send. The Go compiler enforces that Payload is
// a valid oneof variant (the generated isMemqlServerMessage_Payload
// interface), so there is no per-type allow-list to maintain and new
// oneof variants "just work" the moment the proto is regenerated.
//
// Historical note: an earlier version of this function took `any` and
// ran a hand-maintained type switch on the payload, falling through to
// `default: return nil` for unlisted types. Adding a new oneof variant
// to the proto without also adding it to the switch silently dropped
// every send on that type, surfacing as mysterious 30-second frontend
// timeouts. Passing the full message eliminates that footgun by
// leaning on the Go type system.
func (s *streamSession) sendServerMessage(correlate string, msg *memqlv1.MemqlServerMessage) error {
	if msg == nil {
		return nil
	}
	msg.MessageId = id.NewShortId()
	msg.CorrelateTo = s.safeCorrelate(correlate)

	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.stream.Send(msg)
}

func (s *streamSession) normalizeRequestId(envelope *memqlv1.MemqlClientMessage, requestId string) string {
	requestId = strings.TrimSpace(requestId)
	if requestId != "" {
		return requestId
	}
	if envelope != nil {
		if trimmed := strings.TrimSpace(envelope.GetMessageId()); trimmed != "" {
			return trimmed
		}
	}
	return id.NewShortId()
}

func (s *streamSession) safeCorrelate(messageId string) string {
	if trimmed := strings.TrimSpace(messageId); trimmed != "" {
		return trimmed
	}
	return ""
}

func (s *streamSession) shutdown() {
	// Signal the event forwarding goroutine to stop
	close(s.closeChan)

	// Cancel all active requests
	s.activeRequests.Range(func(key, value any) bool {
		if cancel, ok := value.(context.CancelFunc); ok && cancel != nil {
			cancel()
		}
		return true
	})

	// Unsubscribe all subscriptions from the event bus
	s.unsubscribers.Range(func(key, value any) bool {
		if fn, ok := value.(func()); ok && fn != nil {
			fn()
		}
		return true
	})

	// Tear down the voice-agent session-long speak subscriber (no-op
	// for non-voice-agent streams).
	s.stopVoiceAgentSpeakSubscriber()
}

func classifyEngineError(err error) codes.Code {
	switch {
	case errors.Is(err, memqlengine.ErrInvalidQuerySyntax),
		errors.Is(err, memqlengine.ErrEmptyQuery):
		return codes.InvalidArgument
	case errors.Is(err, memqlengine.ErrEngineNotInitialized):
		return codes.FailedPrecondition
	default:
		return codes.Internal
	}
}

func isAtLeastAdmin(role string) bool {
	r := userRoleFromString(role)
	return r == memqlv1.UserRole_USER_ROLE_OWNER || r == memqlv1.UserRole_USER_ROLE_ADMIN
}

func userRoleFromString(role string) memqlv1.UserRole {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "owner":
		return memqlv1.UserRole_USER_ROLE_OWNER
	case "admin":
		return memqlv1.UserRole_USER_ROLE_ADMIN
	case "writer":
		return memqlv1.UserRole_USER_ROLE_WRITER
	case "reader":
		return memqlv1.UserRole_USER_ROLE_READER
	default:
		return memqlv1.UserRole_USER_ROLE_UNSPECIFIED
	}
}

func buildEngineErrorMessage(err error) string {
	const maxRunes = 512
	base := "MemQL engine failed to execute query."
	if err == nil {
		return base
	}
	msg := strings.Join(strings.Fields(err.Error()), " ")
	if msg == "" {
		return base
	}
	runes := []rune(msg)
	if len(runes) > maxRunes {
		msg = string(runes[:maxRunes]) + "…"
	}
	return fmt.Sprintf("%s Details: %s", base, msg)
}

func bearerTokenFromMetadata(md grpcMetadata.MD) (string, error) {
	if md == nil {
		return "", errors.New("authorization header missing")
	}
	values := md.Get("authorization")
	if len(values) == 0 {
		values = md.Get("Authorization")
	}
	if len(values) == 0 {
		return "", errors.New("authorization header missing")
	}
	return parseBearerToken(values[0])
}

func parseBearerToken(header string) (string, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return "", errors.New("authorization header missing")
	}
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", errors.New("invalid authorization header format")
	}
	token := strings.TrimSpace(parts[1])
	if token == "" {
		return "", errors.New("bearer token missing")
	}
	return token, nil
}

func cloneClaims(claims map[string]any) map[string]any {
	if claims == nil {
		return nil
	}
	cloned := make(map[string]any, len(claims))
	for key, value := range claims {
		cloned[key] = value
	}
	return cloned
}
