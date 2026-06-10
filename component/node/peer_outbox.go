package node

import (
	"log/slog"
	"sync"
	"time"

	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

const (
	// peerOutboxCapacity bounds the per-peer pending-event buffer that holds
	// EventForward messages destined for a peer whose outbound
	// *peerConnection has not yet been attached (the pre-attach nil window
	// after RegisterMonitored but before AttachConnection -- typical for a
	// freshly-introduced peer after a blue-green cutover). It mirrors
	// sendChCapacity (the connection-level outbox) so a peer that attaches
	// within the buffering window loses nothing the connection itself would
	// have absorbed. Overflow drops the OLDEST queued message (a stale event
	// is the least valuable) with a rate-limited warn.
	peerOutboxCapacity = sendChCapacity

	// peerOutboxTTL is how long a buffered event waits for its peer to
	// attach before it is dropped as stale. A reply that arrives after this
	// window is not worth delivering (the user has moved on), and the bound
	// keeps memory finite if a peer never attaches at all -- the buffer
	// drains by TTL on the next enqueue even with no AttachConnection.
	peerOutboxTTL = 30 * time.Second

	// peerOutboxWarnInterval rate-limits the overflow/expiry warning so a
	// peer that never attaches cannot flood the logs.
	peerOutboxWarnInterval = 10 * time.Second
)

// pendingEvent is a single buffered EventForward awaiting peer attach.
type pendingEvent struct {
	msg      *nodev1.NodeClientMessage
	enqueued time.Time
}

// peerOutbox is a bounded, TTL'd, per-peer FIFO buffer of EventForward
// messages queued while the peer's outbound connection is nil. It is owned by
// PeerManager: EventBridge enqueues into it when forwardToPeers hits a peer
// with Connection==nil, and AttachConnection drains it (copy-under-lock,
// Send-after-release) when the connection finally binds.
//
// Thread-safety: all state is guarded by mu. Drain returns the queued
// messages and clears the buffer under the lock; the CALLER performs the
// actual conn.Send outside any PeerManager lock to avoid lock-ordering
// deadlocks with the send path.
type peerOutbox struct {
	mu      sync.Mutex
	cap     int
	ttl     time.Duration
	pending map[string][]pendingEvent // node_id -> FIFO of buffered events
	now     func() time.Time          // injectable for tests

	// lastWarn rate-limits overflow/expiry warnings per node_id.
	lastWarn map[string]time.Time
	logger   *slog.Logger
}

func newPeerOutbox(capacity int, ttl time.Duration, logger *slog.Logger) *peerOutbox {
	if capacity <= 0 {
		capacity = peerOutboxCapacity
	}
	if ttl <= 0 {
		ttl = peerOutboxTTL
	}
	return &peerOutbox{
		cap:      capacity,
		ttl:      ttl,
		pending:  make(map[string][]pendingEvent),
		now:      time.Now,
		lastWarn: make(map[string]time.Time),
		logger:   logger,
	}
}

// enqueue buffers a message for a not-yet-attached peer. It first drops any
// TTL-expired entries (so a peer that never attaches can't grow the buffer),
// then appends; on cap overflow it drops the OLDEST entry. Returns the number
// of dropped (expired + overflowed) messages so the caller can warn.
func (o *peerOutbox) enqueue(nodeId string, msg *nodev1.NodeClientMessage) {
	if o == nil || nodeId == "" || msg == nil {
		return
	}
	now := o.now()

	o.mu.Lock()
	q := o.pending[nodeId]

	// Drop expired from the front (FIFO, oldest first).
	expired := 0
	for len(q) > 0 && now.Sub(q[0].enqueued) >= o.ttl {
		q = q[1:]
		expired++
	}

	// Append the new event; drop-oldest on overflow.
	overflowed := 0
	q = append(q, pendingEvent{msg: msg, enqueued: now})
	for len(q) > o.cap {
		q = q[1:]
		overflowed++
	}

	if len(q) == 0 {
		delete(o.pending, nodeId)
	} else {
		o.pending[nodeId] = q
	}

	dropped := expired + overflowed
	shouldWarn := false
	if dropped > 0 {
		if last, ok := o.lastWarn[nodeId]; !ok || now.Sub(last) >= peerOutboxWarnInterval {
			o.lastWarn[nodeId] = now
			shouldWarn = true
		}
	}
	depth := len(q)
	o.mu.Unlock()

	if shouldWarn && o.logger != nil {
		o.logger.Warn("peer outbox dropped buffered events",
			"peer_id", nodeId,
			"expired", expired,
			"overflowed", overflowed,
			"depth", depth,
			"cap", o.cap,
		)
	}
}

// drain removes and returns all non-expired buffered messages for nodeId in
// FIFO order, clearing the buffer. TTL-expired entries are discarded. The
// returned slice is safe to Send outside any lock. Caller MUST NOT hold
// PeerManager.mu while Sending (the drain itself only takes o.mu).
func (o *peerOutbox) drain(nodeId string) []*nodev1.NodeClientMessage {
	if o == nil || nodeId == "" {
		return nil
	}
	now := o.now()

	o.mu.Lock()
	q, ok := o.pending[nodeId]
	if ok {
		delete(o.pending, nodeId)
		delete(o.lastWarn, nodeId)
	}
	o.mu.Unlock()

	if len(q) == 0 {
		return nil
	}

	msgs := make([]*nodev1.NodeClientMessage, 0, len(q))
	for _, p := range q {
		if now.Sub(p.enqueued) >= o.ttl {
			continue // stale -- not worth delivering
		}
		msgs = append(msgs, p.msg)
	}
	return msgs
}

// discard drops any buffered messages for nodeId without delivering them.
// Used when a peer is removed entirely so its buffer doesn't linger.
func (o *peerOutbox) discard(nodeId string) {
	if o == nil || nodeId == "" {
		return
	}
	o.mu.Lock()
	delete(o.pending, nodeId)
	delete(o.lastWarn, nodeId)
	o.mu.Unlock()
}

// depth returns the number of buffered messages for nodeId (test helper).
func (o *peerOutbox) depth(nodeId string) int {
	if o == nil {
		return 0
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.pending[nodeId])
}
