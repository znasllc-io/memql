package memql

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/node"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestAiForwardRouter_NoPeerAvailable asserts we surface a clear
// "no X node available" error instead of hanging when no peer of the
// target type is registered yet.
func TestAiForwardRouter_NoPeerAvailable(t *testing.T) {
	identity := &node.Identity{ID: "test-bff", Type: node.NodeTypeBFF}
	peerMgr := node.NewPeerManager(identity, testLogger())
	router := NewAiForwardRouter(peerMgr, testLogger())

	_, err := router.Forward(nil, "req-1", node.NodeTypeVoice,
		map[string]string{"sub": "alice"},
		"default",
		&memqlv1.MemqlClientMessage{MessageId: "req-1"},
	)
	if err == nil {
		t.Fatal("expected error when no voice peer is available, got nil")
	}
	want := "no voice node available"
	if got := err.Error(); got != want {
		t.Errorf("error message: want %q, got %q", want, got)
	}
}

// TestAiForwardRouter_DispatchRoutesByRequestId asserts a response
// delivered via Dispatch reaches the correct in-flight request's
// channel and that the channel closes on done=true.
func TestAiForwardRouter_DispatchRoutesByRequestId(t *testing.T) {
	identity := &node.Identity{ID: "test-bff", Type: node.NodeTypeBFF}
	peerMgr := node.NewPeerManager(identity, testLogger())
	router := NewAiForwardRouter(peerMgr, testLogger())

	// Seed an in-flight entry directly so we can exercise Dispatch
	// without round-tripping through Forward (which needs a real peer
	// and connection; that's the integration path, not this unit
	// test's concern).
	ch := make(chan *memqlv1.MemqlServerMessage, 4)
	router.mu.Lock()
	router.inflight["req-42"] = &inflightEntry{respCh: ch}
	router.mu.Unlock()

	// Build a server message that represents a final chat result.
	serverMsg := &memqlv1.MemqlServerMessage{
		MessageId:   "resp-1",
		CorrelateTo: "req-42",
		Payload: &memqlv1.MemqlServerMessage_SIChatResult{
			SIChatResult: &memqlv1.SIChatResult{
				RequestId: "req-42",
				Message: &memqlv1.SIChatMessage{
					Role:    "assistant",
					Content: "hello",
				},
			},
		},
	}
	raw, err := proto.Marshal(serverMsg)
	if err != nil {
		t.Fatalf("marshal server msg: %v", err)
	}

	router.Dispatch(&nodev1.SIForwardResponse{
		RequestId:      "req-42",
		MemqlServerMsg: raw,
		Done:           true,
	})

	select {
	case got := <-ch:
		if got == nil {
			t.Fatal("channel delivered nil message")
		}
		if got.GetSIChatResult().GetMessage().GetContent() != "hello" {
			t.Errorf("unexpected content: %q", got.GetSIChatResult().GetMessage().GetContent())
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timed out waiting for dispatched message")
	}

	// Done=true should close the channel so the relay goroutine can
	// finish its range loop.
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("channel still open after done=true response")
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("channel not closed after done=true")
	}

	// Inflight entry should be gone.
	router.mu.Lock()
	_, stillThere := router.inflight["req-42"]
	router.mu.Unlock()
	if stillThere {
		t.Error("inflight entry not cleaned up after done=true")
	}
}

// TestAiForwardRouter_DispatchOrphanedResponse asserts that a response
// with an unknown request_id is dropped silently (the sender's
// cancellation raced with delivery) without panicking or blocking.
func TestAiForwardRouter_DispatchOrphanedResponse(t *testing.T) {
	identity := &node.Identity{ID: "test-bff", Type: node.NodeTypeBFF}
	peerMgr := node.NewPeerManager(identity, testLogger())
	router := NewAiForwardRouter(peerMgr, testLogger())

	// No inflight entry. Dispatch should be a no-op.
	router.Dispatch(&nodev1.SIForwardResponse{
		RequestId: "does-not-exist",
		Done:      true,
	})
	// If we got here without panicking the test passes.
}

// TestAiForwardRouter_HasInflight reports membership for both open and
// closed request ids. Used by the streaming transcription handlers to
// distinguish "never started" from "already closed" on Chunk / End.
func TestAiForwardRouter_HasInflight(t *testing.T) {
	identity := &node.Identity{ID: "test-bff", Type: node.NodeTypeBFF}
	peerMgr := node.NewPeerManager(identity, testLogger())
	router := NewAiForwardRouter(peerMgr, testLogger())

	if router.HasInflight("req-open") {
		t.Fatal("expected no inflight before registration")
	}

	router.mu.Lock()
	router.inflight["req-open"] = &inflightEntry{respCh: make(chan *memqlv1.MemqlServerMessage, 1)}
	router.mu.Unlock()

	if !router.HasInflight("req-open") {
		t.Error("expected inflight present after registration")
	}

	router.cleanupInflight("req-open")

	if router.HasInflight("req-open") {
		t.Error("expected inflight removed after cleanup")
	}
}

// TestAiForwardRouter_ForwardContinuation_NoInflight ensures callers get
// a clear error when they try to send a continuation envelope for a
// stream that was never opened.
func TestAiForwardRouter_ForwardContinuation_NoInflight(t *testing.T) {
	identity := &node.Identity{ID: "test-bff", Type: node.NodeTypeBFF}
	peerMgr := node.NewPeerManager(identity, testLogger())
	router := NewAiForwardRouter(peerMgr, testLogger())

	err := router.ForwardContinuation(
		"never-opened",
		map[string]string{"sub": "alice"},
		"default",
		&memqlv1.MemqlClientMessage{MessageId: "chunk-1"},
	)
	if err == nil {
		t.Fatal("expected error when no inflight exists")
	}
}

// TestAiForwardRouter_ForwardContinuation_RequiresRequestId rejects
// empty request_ids so we can't accidentally address "the only open
// stream" or similar.
func TestAiForwardRouter_ForwardContinuation_RequiresRequestId(t *testing.T) {
	identity := &node.Identity{ID: "test-bff", Type: node.NodeTypeBFF}
	peerMgr := node.NewPeerManager(identity, testLogger())
	router := NewAiForwardRouter(peerMgr, testLogger())

	err := router.ForwardContinuation(
		"   ",
		nil,
		"",
		&memqlv1.MemqlClientMessage{},
	)
	if err == nil {
		t.Fatal("expected error when request_id is blank")
	}
}

// TestAiForwardRouter_ForwardContinuation_PeerWithoutConnection covers
// the case where the selected peer has disconnected between Start and
// the next continuation: we should error rather than nil-panic.
func TestAiForwardRouter_ForwardContinuation_PeerWithoutConnection(t *testing.T) {
	identity := &node.Identity{ID: "test-bff", Type: node.NodeTypeBFF}
	peerMgr := node.NewPeerManager(identity, testLogger())
	router := NewAiForwardRouter(peerMgr, testLogger())

	// Seed an inflight with a peer entry that has no Connection set
	// (mirrors what happens if the worker's connection dropped after
	// Start succeeded).
	router.mu.Lock()
	router.inflight["req-disconnected"] = &inflightEntry{
		respCh: make(chan *memqlv1.MemqlServerMessage, 1),
		peer:   &node.PeerEntry{Info: &nodev1.PeerInfo{NodeId: "voice-x"}},
	}
	router.mu.Unlock()

	err := router.ForwardContinuation(
		"req-disconnected",
		nil,
		"",
		&memqlv1.MemqlClientMessage{},
	)
	if err == nil {
		t.Fatal("expected error when peer has no active connection")
	}
}

// TestIsTerminalServerPayload_StreamComplete verifies the terminal
// marker includes SITranscribeStreamComplete so the BFF's inflight
// closes on the stream's final message.
func TestIsTerminalServerPayload_StreamComplete(t *testing.T) {
	complete := &memqlv1.MemqlServerMessage_SITranscribeStreamComplete{}
	if !isTerminalServerPayload(complete) {
		t.Error("SITranscribeStreamComplete should be terminal")
	}

	// Delta must remain non-terminal (many Deltas flow before Complete).
	delta := &memqlv1.MemqlServerMessage_SITranscribeStreamDelta{}
	if isTerminalServerPayload(delta) {
		t.Error("SITranscribeStreamDelta must not be terminal")
	}
}
