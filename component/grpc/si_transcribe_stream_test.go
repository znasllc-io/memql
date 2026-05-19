package memql

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/integrations/stt"
)

// fakeStreamingSession implements stt.StreamingSession for tests.
// It exposes a writable results channel so the test can simulate
// provider output, and records SendAudio / Finalize / Close calls.
type fakeStreamingSession struct {
	mu         sync.Mutex
	audioBytes [][]byte
	results    chan stt.TranscriptionResult
	finalText  string
	duration   int64
	closed     bool
	finalized  bool
}

func newFakeStreamingSession(finalText string) *fakeStreamingSession {
	return &fakeStreamingSession{
		results:   make(chan stt.TranscriptionResult, 16),
		finalText: finalText,
		duration:  123,
	}
}

func (f *fakeStreamingSession) SendAudio(audio []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return io.ErrClosedPipe
	}
	cp := make([]byte, len(audio))
	copy(cp, audio)
	f.audioBytes = append(f.audioBytes, cp)
	return nil
}

func (f *fakeStreamingSession) Receive() <-chan stt.TranscriptionResult {
	return f.results
}

func (f *fakeStreamingSession) Finalize(_ context.Context) (*stt.FinalTranscription, error) {
	f.mu.Lock()
	f.finalized = true
	f.closed = true
	f.mu.Unlock()
	close(f.results)
	return &stt.FinalTranscription{
		Text:       f.finalText,
		DurationMS: f.duration,
		Provider:   "fake",
	}, nil
}

func (f *fakeStreamingSession) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	close(f.results)
	return nil
}

// capturingSend is an ordered recorder of MemqlServerMessages that the
// transcribeStream would have sent to the client.
type capturingSend struct {
	mu   sync.Mutex
	msgs []*memqlv1.MemqlServerMessage
}

func (c *capturingSend) Send(m *memqlv1.MemqlServerMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, m)
	return nil
}

func (c *capturingSend) snapshot() []*memqlv1.MemqlServerMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*memqlv1.MemqlServerMessage, len(c.msgs))
	copy(out, c.msgs)
	return out
}

func TestTranscribeStream_PumpDedupesConsecutiveIdenticalText(t *testing.T) {
	cap := &capturingSend{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sess := newFakeStreamingSession("hello world final")
	ts := &transcribeStream{
		requestId:  "req-1",
		correlate:  "corr-1",
		provider:   "fake",
		sttSession: sess,
		send:       cap.Send,
		ctx:        ctx,
		cancel:     cancel,
		done:       make(chan struct{}),
		startTime:  time.Now(),
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	go ts.pumpDeltas(logger)

	// Emit three results, two of which are identical in text. Only the
	// distinct ones should flow to the client.
	sess.results <- stt.TranscriptionResult{Text: "hello", Confidence: 0.9}
	sess.results <- stt.TranscriptionResult{Text: "hello"} // dup -- drop
	sess.results <- stt.TranscriptionResult{Text: "hello world", Confidence: 0.95}

	// Close the results channel so the pump exits.
	close(sess.results)
	<-ts.done

	msgs := cap.snapshot()
	if len(msgs) != 2 {
		t.Fatalf("expected 2 deltas, got %d (%v)", len(msgs), msgs)
	}

	d1 := msgs[0].GetSITranscribeStreamDelta()
	if d1 == nil || d1.GetText() != "hello" {
		t.Errorf("first delta: want text=\"hello\", got %+v", d1)
	}
	d2 := msgs[1].GetSITranscribeStreamDelta()
	if d2 == nil || d2.GetText() != "hello world" {
		t.Errorf("second delta: want text=\"hello world\", got %+v", d2)
	}

	// correlate_to should track the original envelope.MessageId.
	for i, m := range msgs {
		if m.CorrelateTo != "corr-1" {
			t.Errorf("msg %d correlate_to: want %q, got %q", i, "corr-1", m.CorrelateTo)
		}
	}
}

func TestTranscribeStream_PumpSkipsEmptyText(t *testing.T) {
	cap := &capturingSend{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sess := newFakeStreamingSession("")
	ts := &transcribeStream{
		requestId:  "req-2",
		correlate:  "corr-2",
		sttSession: sess,
		send:       cap.Send,
		ctx:        ctx,
		cancel:     cancel,
		done:       make(chan struct{}),
		startTime:  time.Now(),
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	go ts.pumpDeltas(logger)

	sess.results <- stt.TranscriptionResult{Text: ""}
	sess.results <- stt.TranscriptionResult{Text: "   "}
	sess.results <- stt.TranscriptionResult{Text: "finally"}
	close(sess.results)
	<-ts.done

	msgs := cap.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 delta, got %d", len(msgs))
	}
	if got := msgs[0].GetSITranscribeStreamDelta().GetText(); got != "finally" {
		t.Errorf("want text=\"finally\", got %q", got)
	}
}

func TestTranscribeStream_CloseSilentlyIdempotent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sess := newFakeStreamingSession("done")
	ts := &transcribeStream{
		requestId:  "req-3",
		sttSession: sess,
		ctx:        ctx,
		cancel:     cancel,
		done:       make(chan struct{}),
		startTime:  time.Now(),
	}

	ts.closeSilently()
	ts.closeSilently() // must not panic / double-close the provider session
}

func TestServiceTranscribeStreamRegistry(t *testing.T) {
	s := &service{}

	if s.lookupTranscribeStream("x") != nil {
		t.Fatal("expected nil on lookup of unregistered id")
	}

	ts := &transcribeStream{requestId: "req-7"}
	s.registerTranscribeStream("req-7", ts)

	if got := s.lookupTranscribeStream("req-7"); got != ts {
		t.Error("registered stream not retrievable via lookup")
	}

	got := s.unregisterTranscribeStream("req-7")
	if got != ts {
		t.Errorf("unregister returned wrong entry: got %p, want %p", got, ts)
	}
	if s.lookupTranscribeStream("req-7") != nil {
		t.Error("entry still present after unregister")
	}
	if s.unregisterTranscribeStream("req-7") != nil {
		t.Error("unregister of absent entry should return nil")
	}
}

func TestPickHelpers(t *testing.T) {
	if got := pickNonEmpty("given", "fallback"); got != "given" {
		t.Errorf("pickNonEmpty(\"given\",...): got %q", got)
	}
	if got := pickNonEmpty("", "fallback"); got != "fallback" {
		t.Errorf("pickNonEmpty(\"\",\"fallback\"): got %q", got)
	}
	if got := pickNonEmpty("  ", "fallback"); got != "fallback" {
		t.Errorf("pickNonEmpty(whitespace): got %q", got)
	}

	if got := pickPositiveInt(42, 100); got != 42 {
		t.Errorf("pickPositiveInt(42,...): got %d", got)
	}
	if got := pickPositiveInt(0, 100); got != 100 {
		t.Errorf("pickPositiveInt(0,100): got %d", got)
	}
	if got := pickPositiveInt(-5, 100); got != 100 {
		t.Errorf("pickPositiveInt(-5,100): got %d", got)
	}
}
