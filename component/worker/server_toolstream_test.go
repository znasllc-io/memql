package worker

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// fakeWorkerStream is the minimum WorkerService_StreamServer a dispatch needs:
// somewhere for the outbound ToolDispatch to go. Inbound messages are fed to
// the session directly by the test rather than through Recv, because the test
// IS the recv loop -- that is what lets it assert what has arrived at the
// moment the result lands.
type fakeWorkerStream struct {
	mu   sync.Mutex
	sent []*memqlv1.WorkerServerMessage
	ctx  context.Context
}

func (f *fakeWorkerStream) Send(msg *memqlv1.WorkerServerMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, msg)
	return nil
}

func (f *fakeWorkerStream) Recv() (*memqlv1.WorkerClientMessage, error) { return nil, io.EOF }

func (f *fakeWorkerStream) SetHeader(metadata.MD) error  { return nil }
func (f *fakeWorkerStream) SendHeader(metadata.MD) error { return nil }
func (f *fakeWorkerStream) SetTrailer(metadata.MD)       {}
func (f *fakeWorkerStream) Context() context.Context {
	if f.ctx != nil {
		return f.ctx
	}
	return context.Background()
}
func (f *fakeWorkerStream) SendMsg(any) error { return nil }
func (f *fakeWorkerStream) RecvMsg(any) error { return io.EOF }

func newToolStreamTestSession() (*streamSession, *fakeWorkerStream) {
	clock := func() time.Time { return time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC) }
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := newServer(logger, &fakeRegistrationStore{}, NewRegistry(nil, clock), nil, clock, testNodeId)
	stream := &fakeWorkerStream{}
	w := &Worker{RegistrationId: "reg-1", OwnerUserId: "user-1"}
	ctx, cancel := context.WithCancel(context.Background())
	session := newStreamSession(srv, stream, w, ctx, cancel)
	w.SetDispatchFunc(session.dispatch, cancel)
	return session, stream
}

func stdoutChunk(callId, text string) *memqlv1.WorkerClientMessage {
	return &memqlv1.WorkerClientMessage{
		Payload: &memqlv1.WorkerClientMessage_ToolStream{
			ToolStream: &memqlv1.ToolStream{
				CallId:  callId,
				Payload: &memqlv1.ToolStream_StdoutChunk{StdoutChunk: []byte(text)},
			},
		},
	}
}

// TestDispatchWithStream_ChunksArriveInOrderBeforeTheResult is the local half
// of memql#4352. Chunks cannot cross a node hop they never reach locally, and
// "reach" means two specific things a forwarder depends on: IN ORDER, and
// BEFORE the result -- a forwarder that received a chunk after the caller had
// its answer would have nowhere to put it.
//
// The ordering assertion is a single log both the callback and the returning
// dispatch append to, rather than two independent counters, because the claim
// is about the sequence and a counter cannot express it.
func TestDispatchWithStream_ChunksArriveInOrderBeforeTheResult(t *testing.T) {
	session, stream := newToolStreamTestSession()
	defer session.cancel()

	var mu sync.Mutex
	var log []string

	done := make(chan *memqlv1.ToolResult, 1)
	go func() {
		res, err := session.worker.DispatchWithStream(
			context.Background(),
			&memqlv1.ToolDispatch{CallId: "call-1"},
			func(chunk *memqlv1.ToolStream) {
				mu.Lock()
				log = append(log, string(chunk.GetStdoutChunk()))
				mu.Unlock()
			},
		)
		if err != nil {
			t.Errorf("dispatch: %v", err)
		}
		mu.Lock()
		log = append(log, "RESULT")
		mu.Unlock()
		done <- res
	}()

	// Wait for the dispatch to be registered -- the ToolDispatch reaching the
	// stream is the observable signal that it is.
	waitForSend(t, stream)

	if err := session.handle(context.Background(), stdoutChunk("call-1", "first"), ""); err != nil {
		t.Fatalf("handle chunk 1: %v", err)
	}
	if err := session.handle(context.Background(), stdoutChunk("call-1", "second"), ""); err != nil {
		t.Fatalf("handle chunk 2: %v", err)
	}
	if err := session.handle(context.Background(), &memqlv1.WorkerClientMessage{
		Payload: &memqlv1.WorkerClientMessage_ToolResult{
			ToolResult: &memqlv1.ToolResult{CallId: "call-1"},
		},
	}, ""); err != nil {
		t.Fatalf("handle result: %v", err)
	}

	select {
	case res := <-done:
		if res == nil || res.GetCallId() != "call-1" {
			t.Fatalf("dispatch returned %+v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("dispatch did not return")
	}

	mu.Lock()
	got := append([]string(nil), log...)
	mu.Unlock()
	want := []string{"first", "second", "RESULT"}
	if len(got) != len(want) {
		t.Fatalf("chunk/result sequence = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("chunk/result sequence = %v, want %v", got, want)
		}
	}
}

// TestHandleToolStream_UnknownCallIdIsDropped: a chunk whose call has already
// been answered, or one for a caller that asked for no chunks, is ordinary
// traffic. It must be dropped, not panic and not error -- the same treatment
// handleToolResult gives a result whose caller has already returned.
func TestHandleToolStream_UnknownCallIdIsDropped(t *testing.T) {
	session, _ := newToolStreamTestSession()
	defer session.cancel()

	if err := session.handle(context.Background(), stdoutChunk("no-such-call", "orphan"), ""); err != nil {
		t.Fatalf("an unknown call id must be dropped, not errored: %v", err)
	}
	// A nil chunk and a blank call id are the other two shapes a malformed
	// worker can send.
	session.handleToolStream(nil)
	session.handleToolStream(&memqlv1.ToolStream{})
}

// TestDispatchWithStream_NilCallbackIsPlainDispatch: Dispatch is
// DispatchWithStream with no callback, and a chunk for that call must be
// dropped rather than reaching a nil function.
func TestDispatchWithStream_NilCallbackIsPlainDispatch(t *testing.T) {
	session, stream := newToolStreamTestSession()
	defer session.cancel()

	done := make(chan *memqlv1.ToolResult, 1)
	go func() {
		res, err := session.worker.Dispatch(context.Background(), &memqlv1.ToolDispatch{CallId: "call-2"})
		if err != nil {
			t.Errorf("dispatch: %v", err)
		}
		done <- res
	}()
	waitForSend(t, stream)

	if err := session.handle(context.Background(), stdoutChunk("call-2", "ignored"), ""); err != nil {
		t.Fatalf("chunk for a callback-less call must be dropped: %v", err)
	}
	if err := session.handle(context.Background(), &memqlv1.WorkerClientMessage{
		Payload: &memqlv1.WorkerClientMessage_ToolResult{
			ToolResult: &memqlv1.ToolResult{CallId: "call-2"},
		},
	}, ""); err != nil {
		t.Fatalf("handle result: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("dispatch did not return")
	}
}

// TestHandleToolResult_RetiresTheChunkSink: once the result has landed the
// caller has its answer, so a chunk the worker sends afterwards must not reach
// the callback.
func TestHandleToolResult_RetiresTheChunkSink(t *testing.T) {
	session, stream := newToolStreamTestSession()
	defer session.cancel()

	var mu sync.Mutex
	var chunks []string

	done := make(chan struct{})
	go func() {
		_, _ = session.worker.DispatchWithStream(
			context.Background(),
			&memqlv1.ToolDispatch{CallId: "call-3"},
			func(chunk *memqlv1.ToolStream) {
				mu.Lock()
				chunks = append(chunks, string(chunk.GetStdoutChunk()))
				mu.Unlock()
			},
		)
		close(done)
	}()
	waitForSend(t, stream)

	_ = session.handle(context.Background(), stdoutChunk("call-3", "before"), "")
	_ = session.handle(context.Background(), &memqlv1.WorkerClientMessage{
		Payload: &memqlv1.WorkerClientMessage_ToolResult{
			ToolResult: &memqlv1.ToolResult{CallId: "call-3"},
		},
	}, "")
	<-done
	_ = session.handle(context.Background(), stdoutChunk("call-3", "after"), "")

	mu.Lock()
	defer mu.Unlock()
	if len(chunks) != 1 || chunks[0] != "before" {
		t.Fatalf("only chunks preceding the result may be delivered, got %v", chunks)
	}
}

// waitForSend blocks until the session has written a message to the stream --
// the observable signal that a dispatch has registered its pending entry and
// its chunk sink.
func waitForSend(t *testing.T, stream *fakeWorkerStream) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stream.mu.Lock()
		n := len(stream.sent)
		stream.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("dispatch never reached the worker stream")
}
