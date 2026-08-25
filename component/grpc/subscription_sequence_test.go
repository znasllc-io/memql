package memql

// The live-data continuity contract on the wire (memql#4536).
//
// A client keeping a list current off graph subscriptions must be able to
// DETECT that it missed something. Before this contract it could not: the
// per-stream event channel is bounded and the bus forwarder drops on full
// with a server-side WARN and nothing else, so a splice-folding consumer
// diverged from the store permanently while every surface still rendered as
// live.
//
// Two fields close it, and these tests pin both plus the ordering property
// the whole subscribe-then-read model rests on.

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/events"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// TestEventNotificationsCarryMonotonicPerStreamSeq pins the counter itself:
// every EventNotification written on one stream is numbered, from 1, with no
// repeats and no holes -- which is the ONLY thing that makes a hole mean
// something.
func TestEventNotificationsCarryMonotonicPerStreamSeq(t *testing.T) {
	s, cs, bus := newSubscribeTestSession(t)

	env := subscribeEnvelope("sub-seq", "v1:cognition:utterance")
	require.NoError(t, s.handleSubscribe(env, env.GetSubscribe()))

	// Three deliveries on top of the subscription-created ack.
	for i := 0; i < 3; i++ {
		bus.PublishSync(events.Event{
			Topic:     "graph.node.created.v1:cognition:utterance",
			Kind:      events.KindNodeCreated,
			Timestamp: time.Now().UTC(),
			Payload:   map[string]any{"id": "utt", "concept": "v1:cognition:utterance"},
		})
		s.handleBusEvent(readBusEvent(t, s.eventChan, 2*time.Second))
	}

	seqs := sentEventSeqs(cs)
	require.Len(t, seqs, 4, "one ack plus three deliveries")
	for i, got := range seqs {
		assert.Equal(t, uint64(i+1), got, "notification %d", i)
	}
}

// TestForwarderOverflowSurfacesAsGapBefore is the whole point of the field:
// force the drop and prove the client is told.
//
// It also pins the SHAPE of the signal -- exactly one notification carries
// gap_before after a burst of drops, because the flag means "since the
// previous delivered notification", not "this stream has dropped something at
// some point".
func TestForwarderOverflowSurfacesAsGapBefore(t *testing.T) {
	s, cs, bus := newSubscribeTestSession(t)
	// One deep: the first publish fills it and every later one is dropped
	// while nothing is draining. This is the real overflow path -- the
	// forwarder's default branch -- not a hand-set flag.
	s.eventChan = make(chan events.Event, 1)

	env := subscribeEnvelope("sub-gap", "v1:cognition:utterance")
	require.NoError(t, s.handleSubscribe(env, env.GetSubscribe()))
	ackCount := len(sentEventSeqs(cs))

	for i := 0; i < 3; i++ {
		bus.PublishSync(events.Event{
			Topic:     "graph.node.created.v1:cognition:utterance",
			Kind:      events.KindNodeCreated,
			Timestamp: time.Now().UTC(),
			Payload:   map[string]any{"id": "utt", "concept": "v1:cognition:utterance"},
		})
	}
	require.True(t, s.eventGapped.Load(), "the forwarder must mark the stream gapped when it drops")

	// Deliver the one event that fit, then one more that arrives after the
	// channel has drained.
	s.handleBusEvent(readBusEvent(t, s.eventChan, 2*time.Second))
	bus.PublishSync(events.Event{
		Topic:     "graph.node.created.v1:cognition:utterance",
		Kind:      events.KindNodeCreated,
		Timestamp: time.Now().UTC(),
		Payload:   map[string]any{"id": "utt-after", "concept": "v1:cognition:utterance"},
	})
	s.handleBusEvent(readBusEvent(t, s.eventChan, 2*time.Second))

	gaps := sentEventGaps(cs)[ackCount:]
	require.Len(t, gaps, 2, "two deliveries after the ack")
	assert.True(t, gaps[0], "the first notification after a drop carries gap_before")
	assert.False(t, gaps[1], "and only that one does -- gap_before is not sticky")
}

// TestSubscribeRegistersOnTheBusBeforeAcking pins the ordering contract that
// makes SUBSCRIBE-THEN-READ race-free (design D2, 2026-08-25).
//
// handleExecuteQuery hands its work to a goroutine and returns; handleSubscribe
// registers on the bus inline. So a client that subscribes and THEN reads
// cannot miss a row: anything written between the two arrives as an event. The
// reverse order can miss one forever, and no amount of client care fixes it.
//
// The assertion is made from INSIDE the send path -- an event published at the
// exact moment the ack is written must already be routed -- so a refactor that
// moves registration into a goroutine breaks this test rather than breaking
// every client quietly.
func TestSubscribeRegistersOnTheBusBeforeAcking(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	bus := events.NewBus()
	t.Cleanup(bus.Close)

	cs := newCaptureStream(t)
	hooked := &ackHookStream{captureStream: cs}
	s := &streamSession{
		service:   &service{logger: logger, eventBus: bus},
		stream:    hooked,
		logger:    logger,
		eventChan: make(chan events.Event, 16),
		closeChan: make(chan struct{}),
	}
	hooked.onAck = func() {
		bus.PublishSync(events.Event{
			Topic:     "graph.node.created.v1:cognition:utterance",
			Kind:      events.KindNodeCreated,
			Timestamp: time.Now().UTC(),
			Payload:   map[string]any{"id": "written-during-the-ack"},
		})
	}

	env := subscribeEnvelope("sub-order", "v1:cognition:utterance")
	require.NoError(t, s.handleSubscribe(env, env.GetSubscribe()))

	select {
	case ev := <-s.eventChan:
		assert.Equal(t, "graph.node.created.v1:cognition:utterance", ev.Topic)
	default:
		t.Fatal("a row written at ack time was not routed: the bus registration " +
			"no longer precedes the ack, so subscribe-then-read can miss a row")
	}
}

// TestSubscribeIsDispatchedSynchronously covers the same contract one layer
// out: handleMessage must not hand a Subscribe envelope to a goroutine. When
// it returns, the subscription is live.
func TestSubscribeIsDispatchedSynchronously(t *testing.T) {
	s, _, bus := newSubscribeTestSession(t)

	env := subscribeEnvelope("sub-dispatch", "v1:cognition:utterance")
	require.NoError(t, s.handleMessage(env))

	bus.PublishSync(events.Event{
		Topic:     "graph.node.created.v1:cognition:utterance",
		Kind:      events.KindNodeCreated,
		Timestamp: time.Now().UTC(),
		Payload:   map[string]any{"id": "utt-1"},
	})
	select {
	case <-s.eventChan:
	default:
		t.Fatal("handleMessage returned before the subscription was registered")
	}
}

func subscribeEnvelope(subId, concept string) *memqlv1.MemqlClientMessage {
	return &memqlv1.MemqlClientMessage{
		MessageId: "m-" + subId,
		Payload: &memqlv1.MemqlClientMessage_Subscribe{
			Subscribe: &memqlv1.SubscribeMsg{
				SubscriptionId: subId,
				Kind:           memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_GRAPH_EVENTS,
				Concept:        concept,
				Actions:        []memqlv1.GraphNodeAction{memqlv1.GraphNodeAction_GRAPH_NODE_ACTION_CREATED},
			},
		},
	}
}

func sentEventSeqs(cs *captureStream) []uint64 {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	out := make([]uint64, 0, len(cs.sent))
	for _, m := range cs.sent {
		if ev := m.GetEvent(); ev != nil {
			out = append(out, ev.GetSeq())
		}
	}
	return out
}

func sentEventGaps(cs *captureStream) []bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	out := make([]bool, 0, len(cs.sent))
	for _, m := range cs.sent {
		if ev := m.GetEvent(); ev != nil {
			out = append(out, ev.GetGapBefore())
		}
	}
	return out
}

// ackHookStream runs onAck exactly once, from inside Send, the first time a
// subscription-created ack is written. That is the instant the ordering
// contract is about.
type ackHookStream struct {
	*captureStream
	once  sync.Once
	onAck func()
}

func (a *ackHookStream) Send(msg *memqlv1.MemqlServerMessage) error {
	if ev := msg.GetEvent(); ev != nil {
		if ev.GetPayload().AsMap()["eventType"] == "subscription-created" && a.onAck != nil {
			a.once.Do(a.onAck)
		}
	}
	return a.captureStream.Send(msg)
}

// TestEveryStreamWriteIsClassifiedForEventContinuity keeps the numbering
// contract from decaying the way it would decay naturally: someone adds a
// fifth path that writes to the stream, it never carries an event today, and
// two years later it does.
//
// Every direct `s.stream.Send(` in this package must either stamp through
// stampEventContinuityLocked or be named below with the reason it cannot
// carry an EventNotification. The test reports what it examined, so a pass is
// a claim about the code rather than about the scanner.
func TestEveryStreamWriteIsClassifiedForEventContinuity(t *testing.T) {
	// Functions that write to the stream and provably never write an
	// EventNotification. Each sends exactly one concrete payload type.
	eventFree := map[string]string{
		"closeForwardInflight":          "sends an empty AiChatResult to close a forward inflight",
		"handleAiTranscribeStreamStart": "rawSend carries AiTranscribeStream* frames only",
	}

	fset := token.NewFileSet()
	pkg, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	require.NoError(t, err)

	examined, stamped, exempt := 0, 0, 0
	for _, p := range pkg {
		for _, file := range p.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				body := renderNode(t, fset, fn.Body)
				if !strings.Contains(body, "s.stream.Send(") {
					continue
				}
				examined++
				switch {
				case fn.Name.Name == "sendServerMessage" ||
					strings.Contains(body, "stampEventContinuityLocked"):
					stamped++
				default:
					reason, listed := eventFree[fn.Name.Name]
					assert.True(t, listed,
						"%s writes to the stream without stamping the event sequence "+
							"(memql#4536). Either call stampEventContinuityLocked under "+
							"sendMu, or add it to eventFree with the reason it cannot "+
							"carry an EventNotification.", fn.Name.Name)
					if listed {
						exempt++
						assert.NotEmpty(t, reason)
					}
				}
			}
		}
	}
	t.Logf("stream writers examined=%d stamped=%d exempt=%d", examined, stamped, exempt)
	require.Greater(t, stamped, 0, "the scanner found no stamping writer -- it is not looking at the right tree")
	require.Equal(t, len(eventFree), exempt, "an exemption is listed for a function that no longer writes to the stream")
}

func renderNode(t *testing.T, fset *token.FileSet, n ast.Node) string {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, printer.Fprint(&buf, fset, n))
	return buf.String()
}
