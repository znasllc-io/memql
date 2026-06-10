package node

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/core/grpctls"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

const (
	// Reconnection backoff parameters.
	initialBackoff = 1 * time.Second
	maxBackoff     = 30 * time.Second
	backoffFactor  = 2.0

	// sendChCapacity bounds the in-memory outbox for a single peer
	// connection. Messages queued while the outbound gRPC stream is
	// mid-reconnect accumulate here until the new sendLoop drains them.
	// At 1024 a 5-second reconnect window at ~200 events/s still fits
	// without tail-drops; the old 64 overflowed on bursty takeover turns
	// (one GA delegation fans out uiReadState + uiRequestControl +
	// uiClick* + utterance + presence + text:chunk + canvas events,
	// easily >100 across a few seconds).
	sendChCapacity = 1024
)

// peerConnection manages a single gRPC stream to a peer node.
type peerConnection struct {
	mu       sync.Mutex
	nodeId   string
	address  string
	conn     *grpc.ClientConn
	stream   nodev1.NodeService_StreamClient
	sendCh   chan *nodev1.NodeClientMessage
	closed   bool
	cancel   context.CancelFunc
	logger   *slog.Logger
	identity *Identity

	// heartbeatInterval is the cadence at which this connection sends
	// NodeHeartbeat messages to the peer. Zero disables the send ticker.
	heartbeatInterval time.Duration

	// healthFn supplies the NodeHealthStatus to stamp on each outbound
	// heartbeat -- this node's self-asserted lifecycle health (memql#1268).
	// nil means advertise HEALTHY (the pre-lifecycle default), so a
	// connection created without a lifecycle source keeps the old wire
	// behaviour and the gossip contract stays backward-compatible.
	healthFn func() nodev1.NodeHealthStatus
}

// newPeerConnection creates a new outbound connection to a peer.
func newPeerConnection(identity *Identity, nodeId, address string, logger *slog.Logger) *peerConnection {
	return &peerConnection{
		nodeId:            nodeId,
		address:           address,
		sendCh:            make(chan *nodev1.NodeClientMessage, sendChCapacity),
		logger:            logger,
		identity:          identity,
		heartbeatInterval: defaultHeartbeatInterval,
	}
}

// SetHeartbeatInterval overrides the default per-connection heartbeat cadence.
// Must be called before Connect. Non-positive values leave the cadence
// unchanged.
func (pc *peerConnection) SetHeartbeatInterval(d time.Duration) {
	if d <= 0 {
		return
	}
	pc.mu.Lock()
	pc.heartbeatInterval = d
	pc.mu.Unlock()
}

// SetHealthFn installs the source of this node's advertised lifecycle health
// for outbound heartbeats (memql#1268). Typically pm.Lifecycle().Health.
// A nil fn restores the HEALTHY default. Thread-safe.
func (pc *peerConnection) SetHealthFn(fn func() nodev1.NodeHealthStatus) {
	pc.mu.Lock()
	pc.healthFn = fn
	pc.mu.Unlock()
}

// Connect establishes the gRPC connection and starts the send/receive loops.
// It blocks until the context is cancelled or the connection is closed.
func (pc *peerConnection) Connect(ctx context.Context, onMessage func(*nodev1.NodeServerMessage)) error {
	ctx, cancel := context.WithCancel(ctx)
	pc.mu.Lock()
	pc.cancel = cancel
	pc.mu.Unlock()

	defer cancel()

	backoff := initialBackoff

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		err := pc.connectOnce(ctx, onMessage)
		if err == nil || ctx.Err() != nil {
			return err
		}

		pc.logger.Warn("peer connection lost, reconnecting",
			"peer_id", pc.nodeId,
			"address", pc.address,
			"error", err,
			"backoff", backoff,
		)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}

		backoff = time.Duration(float64(backoff) * backoffFactor)
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// connectOnce establishes a single connection attempt.
func (pc *peerConnection) connectOnce(ctx context.Context, onMessage func(*nodev1.NodeServerMessage)) error {
	// Message-size limits: see component/node/server.go for the
	// rationale -- screenshot-bearing AgentGenerateTurnDelta
	// envelopes exceed gRPC's default 4 MiB cap, RST_STREAM tears
	// down the inter-node connection, and the cockpit sees the
	// drop bubble back as "worker stream ended; will reconnect".
	// 32 MiB matches the server side and the workerService /
	// memqlService limits.
	const maxNodeMessageSize = 32 * 1024 * 1024
	// Match the server-side TLS posture: if this node has a server
	// cert configured (MEMQL_GRPC_TLS_CERT_FILE), the inter-node
	// dial enables TLS too (ServerName pinned to the peer's
	// dial-address host) so the mesh authenticates symmetrically.
	// When unset, fall back to insecure -- the legacy default
	// suitable for clusters behind a TLS-terminating proxy. See
	// component/grpc/tls.go.
	tlsCfg, err := grpctls.LoadClientTLSConfig(pc.address, pc.logger)
	if err != nil {
		return fmt.Errorf("node.connect: tls config: %w", err)
	}
	transportCreds := insecure.NewCredentials()
	if tlsCfg != nil {
		transportCreds = credentials.NewTLS(tlsCfg)
	}
	conn, err := grpc.NewClient(pc.address,
		grpc.WithTransportCredentials(transportCreds),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(maxNodeMessageSize),
			grpc.MaxCallSendMsgSize(maxNodeMessageSize),
		),
	)
	if err != nil {
		return err
	}

	pc.mu.Lock()
	pc.conn = conn
	pc.mu.Unlock()

	defer func() {
		pc.mu.Lock()
		pc.conn = nil
		pc.stream = nil
		pc.mu.Unlock()
		conn.Close()
	}()

	client := nodev1.NewNodeServiceClient(conn)
	// Attach the class="node" bearer token to outbound metadata so
	// the remote NodeServer's class-pin interceptor can verify. When
	// the local Identity has no BearerToken (single-node dev /
	// clusters not yet rolled onto node tokens) the context passes
	// through unchanged and the legacy "any peer can NodeHello"
	// behavior holds end-to-end. See #105.
	streamCtx := ctx
	if pc.identity != nil && pc.identity.BearerToken != "" {
		streamCtx = metadata.AppendToOutgoingContext(streamCtx,
			"authorization", "Bearer "+pc.identity.BearerToken)
	}
	stream, err := client.Stream(streamCtx)
	if err != nil {
		return err
	}

	pc.mu.Lock()
	pc.stream = stream
	pc.mu.Unlock()

	// Send NodeHello
	hello := &nodev1.NodeClientMessage{
		Payload: &nodev1.NodeClientMessage_NodeHello{
			NodeHello: &nodev1.NodeHello{
				NodeId:   pc.identity.ID,
				NodeType: string(pc.identity.Type),
				Version:  pc.identity.Version,
				Address:  pc.identity.Address,
				ParentId: pc.identity.ParentAddress,
				Labels:   pc.identity.Labels,
			},
		},
	}
	if err := stream.Send(hello); err != nil {
		return err
	}

	// Start send goroutine
	sendDone := make(chan error, 1)
	go func() {
		sendDone <- pc.sendLoop(ctx, stream)
	}()

	// Start heartbeat ticker. Sends periodic NodeHeartbeat messages so the
	// peer's PeerManager can track our liveness. Cancelled when the stream
	// context is done.
	pc.mu.Lock()
	hbInterval := pc.heartbeatInterval
	pc.mu.Unlock()
	if hbInterval > 0 {
		go pc.heartbeatLoop(ctx, hbInterval)
	}

	// Receive loop (blocks until stream ends or context cancelled)
	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		if onMessage != nil {
			onMessage(msg)
		}
	}
}

// heartbeatLoop sends a NodeHeartbeat on the configured interval for the
// lifetime of the stream. It queues messages via the send loop (pc.Send)
// rather than writing to the stream directly to keep serialization in one
// goroutine.
func (pc *peerConnection) heartbeatLoop(ctx context.Context, interval time.Duration) {
	// Send one immediately so the peer learns our liveness before the first
	// tick elapses.
	pc.sendHeartbeatMessage()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pc.sendHeartbeatMessage()
		}
	}
}

func (pc *peerConnection) sendHeartbeatMessage() {
	pc.mu.Lock()
	fn := pc.healthFn
	pc.mu.Unlock()

	// Advertise this node's self-asserted lifecycle health (memql#1268) so a
	// Draining node is routed around at once. Default to HEALTHY when no
	// lifecycle source is wired (backward-compatible).
	health := nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY
	if fn != nil {
		if h := fn(); h != nodev1.NodeHealthStatus_NODE_HEALTH_UNSPECIFIED {
			health = h
		}
	}
	pc.Send(buildHeartbeatMessage(health))
}

// sendLoop drains the send channel and writes to the stream.
func (pc *peerConnection) sendLoop(ctx context.Context, stream nodev1.NodeService_StreamClient) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg, ok := <-pc.sendCh:
			if !ok {
				return nil
			}
			if err := stream.Send(msg); err != nil {
				return err
			}
		}
	}
}

// Send queues a message for sending to the peer.
func (pc *peerConnection) Send(msg *nodev1.NodeClientMessage) {
	pc.mu.Lock()
	closed := pc.closed
	pc.mu.Unlock()

	if closed {
		return
	}

	select {
	case pc.sendCh <- msg:
	default:
		pc.logger.Warn("peer send channel full, dropping message",
			"peer_id", pc.nodeId,
		)
	}
}

// Close shuts down the connection.
func (pc *peerConnection) Close() {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	if pc.closed {
		return
	}
	pc.closed = true

	if pc.cancel != nil {
		pc.cancel()
	}

	close(pc.sendCh)

	if pc.conn != nil {
		pc.conn.Close()
		pc.conn = nil
	}
}
