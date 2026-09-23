package node

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/core/id"
)

// inboundStream is the PUSH half of a stream a peer opened to this node
// (memql#5338, D1).
//
// Until this existed the server side of NodeService.Stream only ever answered:
// a heartbeat, a welcome, a forward reply, an ack. Events went only where a
// node held an OUTBOUND connection, so a node nobody dials -- the edge, a
// product bff, mcp in the cloud -- heard no broadcast for its whole life. The
// ParentConnector had a handler for a server-pushed EventForward all along; it
// was dead code, because nothing on this side ever sent one.
//
// Three properties, each load-bearing:
//
//   - IT NEVER BLOCKS THE SENDER. The EventBridge forwards from inside the
//     local bus's dispatch, and a gRPC Send blocks on flow control when the
//     peer is slow to read. So a push is queued on a bounded outbox (the same
//     capacity as a dialed connection's) and a copy that does not fit is
//     DROPPED and counted, never waited for.
//   - IT SHARES THE STREAM'S ONE SEND LOCK. Heartbeats and every forward reply
//     already Send on this stream through serializedStream; the drain goroutine
//     sends through the same function, so a push can never interleave with a
//     heartbeat on the wire.
//   - IT DIES WITH THE STREAM. A send error means the stream is gone: the
//     stream stops taking events at once, and the handler that owns it
//     releases it from the peer table when its receive loop ends.
type inboundStream struct {
	peerID string
	send   func(*nodev1.NodeServerMessage) error
	outbox chan *nodev1.NodeServerMessage
	stop   chan struct{}
	logger *slog.Logger

	stopOnce sync.Once
	closed   atomic.Bool

	// lastDropLog rate-limits the drop warning to one line per
	// inboundDropLogEvery per stream, carrying the count since the last line:
	// a stuck peer drops every copy, and one line per copy would bury the log.
	lastDropLog atomic.Int64 // unix nanos
	dropsSince  atomic.Int64
}

const inboundDropLogEvery = 10 * time.Second

// newInboundStream wraps the send function of an accepted stream. Call run on
// its own goroutine; call close when the stream's handler returns.
func newInboundStream(peerID string, send func(*nodev1.NodeServerMessage) error, logger *slog.Logger) *inboundStream {
	if logger == nil {
		logger = slog.Default()
	}
	return &inboundStream{
		peerID: peerID,
		send:   send,
		outbox: make(chan *nodev1.NodeServerMessage, sendChCapacity),
		stop:   make(chan struct{}),
		logger: logger,
	}
}

// run drains the outbox onto the stream until close is called or a send
// fails.
func (s *inboundStream) run() {
	for {
		select {
		case <-s.stop:
			return
		case msg := <-s.outbox:
			if err := s.send(msg); err != nil {
				s.closed.Store(true)
				return
			}
		}
	}
}

// close stops the drain and refuses every later push. Idempotent.
func (s *inboundStream) close() {
	s.closed.Store(true)
	s.stopOnce.Do(func() { close(s.stop) })
}

// open reports whether the stream still takes events.
func (s *inboundStream) open() bool { return !s.closed.Load() }

// sendEvent queues one EventForward for the peer without blocking, and reports
// whether it was queued.
func (s *inboundStream) sendEvent(evt *nodev1.EventForward) bool {
	if !s.open() {
		return false
	}
	msg := &nodev1.NodeServerMessage{
		MessageId: id.NewShortId(),
		Payload:   &nodev1.NodeServerMessage_EventForward{EventForward: evt},
	}
	select {
	case s.outbox <- msg:
		return true
	default:
		s.noteDrop()
		return false
	}
}

func (s *inboundStream) noteDrop() {
	n := s.dropsSince.Add(1)
	now := time.Now().UnixNano()
	last := s.lastDropLog.Load()
	if now-last < int64(inboundDropLogEvery) || !s.lastDropLog.CompareAndSwap(last, now) {
		return
	}
	s.dropsSince.Add(-n)
	s.logger.Warn("mesh: a peer is not reading its stream fast enough; event copies are being dropped",
		"peer_id", s.peerID, "dropped", n, "outbox", cap(s.outbox))
}
