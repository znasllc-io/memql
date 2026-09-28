package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/core/grpctls"
	"github.com/znasllc-io/memql/core/id"
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

// peerStream is one transport attempt. Its outbox is never reused on reconnect.
// Work with side effects must not escape into the next attempt after failure.
type peerStream struct {
	done   chan struct{}
	sendCh chan *nodev1.NodeClientMessage
}

// peerConnection manages a single gRPC stream to a peer node.
type peerConnection struct {
	mu       sync.Mutex
	nodeId   string
	address  string
	conn     *grpc.ClientConn
	stream   nodev1.NodeService_StreamClient
	current  *peerStream
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

	// reauthFn answers an auth rejection (memql#1521). When identity's
	// signing key rotates, the token this connection presents is rejected
	// with Unauthenticated / "unknown kid" on every dial; without a new token
	// the reconnect loop reuses the dead one forever (the stuck loop). When
	// set, the Connect loop calls it with the token the rejected dial
	// presented and retries at once with the token it returns. nil leaves the
	// legacy reconnect-with-the-same-token behaviour (single-node dev, or an
	// out-of-band MEMQL_NODE_TOKEN that cannot be re-minted). Production wires
	// Identity.RefreshRejectedBearerToken, which mints only when a mint can
	// help and otherwise returns an error the loop backs off from.
	reauthFn func(ctx context.Context, rejected string) (string, error)

	// presented is the bearer token the latest attempt put on the wire, so a
	// rejection is answered for THAT token and not for whatever the shared
	// Identity holds by then.
	presented string

	// giveUpAfter, when positive, makes Connect return ErrPeerUnreachable
	// after that many consecutive attempts that never received a message
	// from the peer, so the caller can look for a different peer instead of
	// redialling a pod that is gone. Zero redials the same address forever
	// (the default, right for a Service address that always has a live
	// endpoint behind it).
	giveUpAfter int

	// lastAttemptEstablished records whether the latest attempt received at
	// least one message, which is what resets the giveUpAfter count.
	lastAttemptEstablished bool
}

// ErrPeerUnreachable is returned by Connect when a connection configured with
// SetGiveUpAfter has failed that many attempts in a row without the peer
// answering. The address it was dialling is presumed gone.
var ErrPeerUnreachable = errors.New("peer unreachable")

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

// SetReauthFn installs the auth-rejection hook (memql#1521). When set, an
// Unauthenticated / unknown-kid rejection on a dial calls fn with the token
// that dial presented; a token back means retry at once with it, an error
// means back off as for any other failure. Must be called before Connect. A
// nil fn leaves the legacy behaviour (no new token). Thread-safe.
func (pc *peerConnection) SetReauthFn(fn func(ctx context.Context, rejected string) (string, error)) {
	pc.mu.Lock()
	pc.reauthFn = fn
	pc.mu.Unlock()
}

// SetGiveUpAfter makes Connect return ErrPeerUnreachable after n consecutive
// attempts in which the peer never sent a message. Must be called before
// Connect. A non-positive n redials forever. Thread-safe.
func (pc *peerConnection) SetGiveUpAfter(n int) {
	pc.mu.Lock()
	pc.giveUpAfter = n
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

// SetNodeId records the peer's identity once learned (NodeWelcome on a
// ParentConnector dial). Parent dials start with an empty nodeId because the
// service address (bff-active:50058) is known before the replica's id; without
// this update, "peer connection lost, reconnecting" logs forever show an
// empty peer_id and ops cannot tell which BFF replica flapped.
func (pc *peerConnection) SetNodeId(nodeId string) {
	if pc == nil || nodeId == "" {
		return
	}
	pc.mu.Lock()
	pc.nodeId = nodeId
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
	// unanswered counts consecutive attempts in which the peer never sent a
	// message: a refused dial, a rejected handshake, a pod that is gone.
	unanswered := 0

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
		pc.mu.Lock()
		established := pc.lastAttemptEstablished
		giveUpAfter := pc.giveUpAfter
		pc.mu.Unlock()
		if established {
			unanswered = 0
		} else {
			unanswered++
		}

		// Auth rejection (memql#1521): identity's signing key rotated and the
		// token this connection presents was minted under the OLD key, so the
		// remote NodeServer rejects it with Unauthenticated / "unknown kid"
		// every time. Reconnecting with the SAME token loops forever (the
		// outage). When a re-mint hook is wired, fetch a token signed by the
		// CURRENT key before retrying so the next dial verifies. The Identity
		// mints at most once per nodeRemintMinInterval for the whole process,
		// however many connections are refused, so neither a down identity
		// nor a peer that refuses every token is hammered.
		if isAuthRejection(err) && pc.tryReauth(ctx, err) {
			// A different token to present -- retry IMMEDIATELY: the failure
			// was the credential, not the peer, and a key rotation should
			// heal fast. The re-mint floor on the Identity bounds this path:
			// a token that new is never replaced again, so a peer that keeps
			// refusing it lands on the backoff below.
			backoff = initialBackoff
			continue
		}

		if giveUpAfter > 0 && unanswered >= giveUpAfter {
			return fmt.Errorf("%w: %s did not answer %d consecutive attempts: %w",
				ErrPeerUnreachable, pc.address, unanswered, err)
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

// tryReauth asks the reauth hook (if wired) for a token to present after an
// auth rejection. Returns true when there is a DIFFERENT token to present
// (caller retries immediately), false when there is no hook, or the hook
// declined or failed (caller falls back to the normal reconnect backoff).
// memql#1521.
func (pc *peerConnection) tryReauth(ctx context.Context, cause error) bool {
	pc.mu.Lock()
	fn := pc.reauthFn
	rejected := pc.presented
	pc.mu.Unlock()

	if fn == nil {
		return false
	}

	tok, err := fn(ctx, rejected)
	if err != nil {
		if errors.Is(err, errRemintSuppressed) {
			pc.logger.Warn("node auth rejected a freshly minted token; backing off instead of re-minting",
				"peer_id", pc.nodeId,
				"address", pc.address,
				"error", cause,
			)
			return false
		}
		pc.logger.Warn("node token re-mint failed; will retry on the next reconnect",
			"peer_id", pc.nodeId,
			"address", pc.address,
			"error", err,
			"rejection", cause,
		)
		return false
	}
	if tok == "" || tok == rejected {
		return false
	}

	pc.logger.Info("node auth rejected; retrying with a new node token",
		"peer_id", pc.nodeId,
		"address", pc.address,
		"error", cause,
	)
	return true
}

// connectOnce establishes a single connection attempt.
func (pc *peerConnection) connectOnce(parentCtx context.Context, onMessage func(*nodev1.NodeServerMessage)) error {
	pc.mu.Lock()
	pc.lastAttemptEstablished = false
	pc.mu.Unlock()
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
		pc.endStreamLocked()
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
		// Remember what went on the wire, so a rejection is answered for
		// this token rather than for whatever the Identity holds by then.
		tok := pc.identity.BearerTokenForDial(ctx, pc.logger)
		pc.mu.Lock()
		pc.presented = tok
		pc.mu.Unlock()
		if tok != "" {
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

	// AI work is scoped to this attempt; the general mesh outbox remains
	// reconnectable for its existing users.
	attempt := &peerStream{done: make(chan struct{}), sendCh: make(chan *nodev1.NodeClientMessage, sendChCapacity)}
	pc.mu.Lock()
	if pc.closed {
		pc.mu.Unlock()
		return context.Canceled
	}
	pc.current = attempt
	pc.mu.Unlock()

	// Start send goroutine
	sendDone := make(chan error, 1)
	go func() {
		sendDone <- pc.sendLoop(ctx, stream, attempt.sendCh)
		cancelAttempt()
	}()

	defer func() { cancelAttempt(); <-sendDone }()

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
	answered := false
	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		if !answered {
			// The peer is alive and took us: this attempt does not count
			// toward giveUpAfter, and the count starts over.
			answered = true
			pc.mu.Lock()
			pc.lastAttemptEstablished = true
			pc.mu.Unlock()
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
func (pc *peerConnection) sendLoop(ctx context.Context, stream nodev1.NodeService_StreamClient, scoped <-chan *nodev1.NodeClientMessage) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg := <-scoped:
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := stream.Send(msg); err != nil {
				return err
			}
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

// Send queues a message for sending to the peer, logging a message the outbox
// had no room for.
func (pc *peerConnection) Send(msg *nodev1.NodeClientMessage) {
	if !pc.trySend(msg) && !pc.isClosed() {
		pc.logger.Warn("peer send channel full, dropping message",
			"peer_id", pc.nodeId,
		)
	}
}

// trySend queues a message without blocking and reports whether it was queued.
//
// The non-blocking channel send is performed UNDER pc.mu, paired with Close
// taking the same lock before it closes sendCh. This closes a send-on-closed-
// channel race (panic) and the data race the detector flags: with the
// read-liveness watchdog (memql#1388) a torn-down attempt's heartbeat
// goroutine can still call Send while Close (or the next attempt) runs. The
// critical section is just a non-blocking select, so it never blocks under the
// lock.
func (pc *peerConnection) trySend(msg *nodev1.NodeClientMessage) bool {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	if pc.closed {
		return false
	}

	select {
	case pc.sendCh <- msg:
		return true
	default:
		return false
	}
}

// sendEvent queues one EventForward for the peer without blocking and reports
// whether it was queued. The mesh counts a false as a dropped copy (memql#5338,
// D7) rather than logging each one: a peer that stops reading drops every copy.
func (pc *peerConnection) sendEvent(evt *nodev1.EventForward) bool {
	return pc.trySend(&nodev1.NodeClientMessage{
		MessageId: id.NewShortId(),
		Payload:   &nodev1.NodeClientMessage_EventForward{EventForward: evt},
	})
}

// connected reports whether a stream attempt is live right now, as opposed to
// the connection backing off between attempts. The event path prefers a live
// stream over one that is reconnecting (memql#5338, D2).
func (pc *peerConnection) connected() bool {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	return !pc.closed && pc.current != nil
}

func (pc *peerConnection) isClosed() bool {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	return pc.closed
}

// SendOnStream queues work only on the current transport attempt and returns
// its disconnect signal. expected pins a continuation to its opener's attempt;
// nil starts new work. Failure never queues work for a future reconnect.
func (pc *peerConnection) SendOnStream(msg *nodev1.NodeClientMessage, expected <-chan struct{}) (<-chan struct{}, error) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	attempt := pc.current
	if pc.closed || attempt == nil || (expected != nil && expected != attempt.done) {
		return nil, fmt.Errorf("peer transport unavailable")
	}
	select {
	case attempt.sendCh <- msg:
		return attempt.done, nil
	default:
		return nil, fmt.Errorf("peer transport outbox full")
	}
}

// endStreamLocked notifies all requests bound to the failed attempt, including
// on an explicit Close. The next reconnect receives a fresh queue and signal.
func (pc *peerConnection) endStreamLocked() {
	if pc.current != nil {
		close(pc.current.done)
		pc.current = nil
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
	pc.endStreamLocked()

	if pc.cancel != nil {
		pc.cancel()
	}

	close(pc.sendCh)

	if pc.conn != nil {
		pc.conn.Close()
		pc.conn = nil
	}
}
