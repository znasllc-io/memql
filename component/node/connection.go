package node

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/core/grpctls"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
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

	// remintInitialBackoff / remintMaxBackoff bound the auth-failure re-mint
	// cadence (memql#1521). The FIRST auth rejection re-mints immediately (a
	// key rotation is the expected trigger and we want to recover fast);
	// consecutive rejections back off so a persistently-misconfigured /
	// down identity isn't hammered with mint POSTs.
	remintInitialBackoff = 2 * time.Second
	remintMaxBackoff     = 30 * time.Second

	// defaultReadLivenessFactor is the multiple of the heartbeat interval
	// after which an inbound-silent stream is declared half-dead and torn
	// down so the outer reconnect loop re-establishes it (memql#1388).
	//
	// The peer's NodeService server runs serverHeartbeatLoop, which sends a
	// NodeHeartbeat every heartbeatInterval, so a HEALTHY stream delivers an
	// inbound message at least that often. After a bff blue-green cutover the
	// old-color pod can go HALF-DEAD: the gRPC stream stays ESTABLISHED (no
	// RST/EOF, so stream.Recv() blocks forever) but the parent stops sending
	// server heartbeats AND stops fanning fresh PeerIntros/NodeWelcomes. With
	// no inbound deadline the leaf wedges on that dead parent indefinitely --
	// its plan-created events never leave the node and the routing table
	// decays (no re-advertisement). 4x heartbeats (~20s at the 5s default) is
	// well past normal jitter / a brief GC pause while still catching a
	// silently-dead parent within one strand window.
	defaultReadLivenessFactor = 4
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

	// readLivenessTimeout bounds how long the receive loop may go without an
	// inbound message before the stream is declared half-dead and torn down
	// so the outer reconnect loop re-establishes it (memql#1388). Zero
	// disables the watchdog (kept for tests that drive a stream with no
	// server heartbeat). Defaults to defaultReadLivenessFactor * heartbeat.
	readLivenessTimeout time.Duration

	// healthFn supplies the NodeHealthStatus to stamp on each outbound
	// heartbeat -- this node's self-asserted lifecycle health (memql#1268).
	// nil means advertise HEALTHY (the pre-lifecycle default), so a
	// connection created without a lifecycle source keeps the old wire
	// behaviour and the gossip contract stays backward-compatible.
	healthFn func() nodev1.NodeHealthStatus

	// reauthFn re-acquires a fresh node token after an auth rejection
	// (memql#1521). When identity's signing key rotates, the token this
	// connection presents is rejected with Unauthenticated / "unknown kid"
	// on every dial; without a re-mint the reconnect loop reuses the dead
	// token forever (the stuck loop). When set, the Connect loop calls this
	// on an auth-rejection error to fetch a token signed by the CURRENT key,
	// then retries. nil leaves the legacy reconnect-with-the-same-token
	// behaviour (single-node dev, or an out-of-band MEMQL_NODE_TOKEN that
	// cannot be re-minted). It returns the new token (already stored on the
	// shared Identity) or an error if the re-mint failed.
	reauthFn func(ctx context.Context) (string, error)

	// remintBackoff bounds how often the auth-failure re-mint fires across
	// reconnect attempts so a persistently-rejecting identity isn't hammered.
	// Grows on consecutive auth rejections, resets on a clean connect.
	remintBackoff time.Duration
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

// SetReauthFn installs the auth-failure re-mint hook (memql#1521). When set,
// an Unauthenticated / unknown-kid rejection on a dial triggers a fresh token
// fetch via fn before the next reconnect attempt, instead of looping forever
// on the dead token. Must be called before Connect. A nil fn leaves the legacy
// behaviour (no re-mint). Thread-safe.
func (pc *peerConnection) SetReauthFn(fn func(ctx context.Context) (string, error)) {
	pc.mu.Lock()
	pc.reauthFn = fn
	pc.mu.Unlock()
}

// SetReadLivenessTimeout overrides the inbound-silence deadline after which a
// half-dead stream is torn down for reconnect (memql#1388). A negative value
// disables the watchdog. Zero leaves the field at its current value (so the
// default-from-heartbeat resolution applies). Must be called before Connect.
func (pc *peerConnection) SetReadLivenessTimeout(d time.Duration) {
	pc.mu.Lock()
	pc.readLivenessTimeout = d
	pc.mu.Unlock()
}

// resolvedReadLivenessTimeout returns the effective inbound-silence deadline:
// an explicit override if set, otherwise defaultReadLivenessFactor x the
// heartbeat interval. A negative override (or a non-positive heartbeat with no
// override) disables the watchdog by returning <= 0.
func (pc *peerConnection) resolvedReadLivenessTimeout() time.Duration {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if pc.readLivenessTimeout != 0 {
		return pc.readLivenessTimeout
	}
	if pc.heartbeatInterval <= 0 {
		return 0
	}
	return time.Duration(defaultReadLivenessFactor) * pc.heartbeatInterval
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

		// Auth rejection (memql#1521): identity's signing key rotated and the
		// token this connection presents was minted under the OLD key, so the
		// remote NodeServer rejects it with Unauthenticated / "unknown kid"
		// every time. Reconnecting with the SAME token loops forever (the
		// outage). When a re-mint hook is wired, fetch a token signed by the
		// CURRENT key before retrying so the next dial verifies. The re-mint
		// itself backs off across consecutive rejections so a down /
		// misconfigured identity isn't hammered.
		if isAuthRejection(err) {
			if pc.tryReauth(ctx, err) {
				// Fresh token obtained -- retry IMMEDIATELY (no reconnect
				// backoff): the previous failure was purely an expired
				// credential, not an unreachable peer, and we want to recover
				// from the key rotation fast.
				backoff = initialBackoff
				continue
			}
			// No re-mint hook, or the re-mint failed (already backed off
			// inside tryReauth). Fall through to the normal reconnect backoff
			// and try again -- a transient identity blip will clear.
		} else {
			// A non-auth failure means the credential is (probably) fine; reset
			// the re-mint backoff so the next genuine key rotation re-mints
			// promptly again.
			pc.resetRemintBackoff()
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

// isAuthRejection reports whether err is a node-auth rejection that a token
// re-mint could fix (memql#1521): the remote NodeServer's class-pin
// interceptor returns codes.Unauthenticated for an unknown-kid / expired /
// invalid token (see component/node/auth.go). We also match the message
// substrings so a rejection surfaced without a gRPC status (wrapped error,
// non-status transport) is still recognised. A codes.PermissionDenied (e.g.
// "node service requires a node-class token") is NOT included -- that is a
// class/binding problem a fresh same-class token would not fix.
func isAuthRejection(err error) bool {
	if err == nil {
		return false
	}
	if st, ok := status.FromError(err); ok && st.Code() == codes.Unauthenticated {
		return true
	}
	msg := err.Error()
	for _, needle := range []string{"unknown kid", "invalid or expired token", "Unauthenticated"} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// tryReauth runs the re-mint hook (if wired) after an auth rejection, applying
// a per-connection backoff so consecutive rejections don't hammer identity.
// Returns true when a fresh token was obtained (caller retries immediately),
// false when there is no hook or the re-mint failed (caller falls back to the
// normal reconnect backoff). memql#1521.
func (pc *peerConnection) tryReauth(ctx context.Context, cause error) bool {
	pc.mu.Lock()
	fn := pc.reauthFn
	wait := pc.remintBackoff
	pc.mu.Unlock()

	if fn == nil {
		return false
	}

	// Backoff BEFORE re-minting on a repeat rejection (wait==0 the first time,
	// so the initial re-mint after a key rotation fires immediately).
	if wait > 0 {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(wait):
		}
	}

	pc.logger.Warn("node auth rejected; re-minting node token before reconnect",
		"peer_id", pc.nodeId,
		"address", pc.address,
		"error", cause,
	)

	_, err := fn(ctx)
	if err != nil {
		// Grow the backoff so a persistently-failing identity isn't hammered.
		pc.bumpRemintBackoff()
		pc.logger.Warn("node token re-mint failed; will retry on the next reconnect",
			"peer_id", pc.nodeId,
			"address", pc.address,
			"error", err,
		)
		return false
	}

	pc.resetRemintBackoff()
	pc.logger.Info("node token re-minted after auth rejection; retrying connection",
		"peer_id", pc.nodeId,
		"address", pc.address,
	)
	return true
}

func (pc *peerConnection) bumpRemintBackoff() {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if pc.remintBackoff <= 0 {
		pc.remintBackoff = remintInitialBackoff
		return
	}
	pc.remintBackoff = time.Duration(float64(pc.remintBackoff) * backoffFactor)
	if pc.remintBackoff > remintMaxBackoff {
		pc.remintBackoff = remintMaxBackoff
	}
}

func (pc *peerConnection) resetRemintBackoff() {
	pc.mu.Lock()
	pc.remintBackoff = 0
	pc.mu.Unlock()
}

// connectOnce establishes a single connection attempt.
func (pc *peerConnection) connectOnce(parentCtx context.Context, onMessage func(*nodev1.NodeServerMessage)) error {
	// Per-attempt context so the read-liveness watchdog (memql#1388) can tear
	// down a half-dead stream by cancelling it, which unblocks stream.Recv()
	// and returns control to the outer reconnect loop. Cancelling this does
	// NOT cancel the parent (the supervising loop keeps re-dialing).
	ctx, cancelAttempt := context.WithCancel(parentCtx)
	defer cancelAttempt()
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
	if pc.identity != nil {
		// Read under the token lock: the re-mint path (memql#1521) writes
		// BearerToken at runtime, so a bare field read here would race it.
		if tok := pc.identity.BearerTokenValue(); tok != "" {
			streamCtx = metadata.AppendToOutgoingContext(streamCtx,
				"authorization", "Bearer "+tok)
		}
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

	// Read-liveness watchdog (memql#1388). A healthy peer streams a server
	// heartbeat every heartbeatInterval, so a live stream is never inbound-
	// silent for long. A half-dead parent (a draining old-color bff after a
	// blue-green cutover) can leave the gRPC stream ESTABLISHED -- so
	// stream.Recv() blocks forever and connectOnce never returns -- while it
	// has stopped emitting heartbeats and fresh PeerIntros/NodeWelcomes. The
	// watchdog declares the stream dead after readLivenessTimeout of inbound
	// silence and cancels the per-attempt context, which unblocks Recv() and
	// drops us into the outer reconnect loop for a fresh handshake (a new
	// NodeWelcome snapshot re-advertises the peer set, healing the routing
	// table). recvActivity is pinged on every inbound message to reset it.
	recvActivity := make(chan struct{}, 1)
	if d := pc.resolvedReadLivenessTimeout(); d > 0 {
		go pc.readLivenessWatchdog(ctx, cancelAttempt, d, recvActivity)
	}

	// Receive loop (blocks until stream ends or context cancelled)
	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		// Reset the inbound-silence deadline. Non-blocking: a full channel
		// already signals "saw activity since the last watchdog tick".
		select {
		case recvActivity <- struct{}{}:
		default:
		}
		if onMessage != nil {
			onMessage(msg)
		}
	}
}

// readLivenessWatchdog cancels the per-attempt context when no inbound message
// has arrived within timeout, tearing down a half-dead stream (memql#1388).
// It exits when the attempt context is cancelled (normal stream end / Close /
// its own fire).
func (pc *peerConnection) readLivenessWatchdog(ctx context.Context, cancelAttempt context.CancelFunc, timeout time.Duration, activity <-chan struct{}) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-activity:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(timeout)
		case <-timer.C:
			pc.logger.Warn("peer stream inbound-silent past read-liveness deadline; tearing down for reconnect",
				"peer_id", pc.nodeId,
				"address", pc.address,
				"timeout", timeout,
			)
			cancelAttempt()
			return
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
//
// The non-blocking channel send is performed UNDER pc.mu, paired with Close
// taking the same lock before it closes sendCh. This closes a send-on-closed-
// channel race (panic) and the data race the detector flags: with the
// read-liveness watchdog (memql#1388) a torn-down attempt's heartbeat
// goroutine can still call Send while Close (or the next attempt) runs. The
// critical section is just a non-blocking select, so it never blocks under the
// lock.
func (pc *peerConnection) Send(msg *nodev1.NodeClientMessage) {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	if pc.closed {
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
