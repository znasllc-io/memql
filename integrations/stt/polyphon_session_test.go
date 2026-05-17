package stt

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/polyphon"
)

// mockASRStream implements polyphon.ASRStream for tests, letting us control
// the exact sequence of ASRResults the session sees without needing a real
// upstream (gRPC or WebSocket) connection.
type mockASRStream struct {
	mu       sync.Mutex
	results  chan polyphon.ASRResult
	sent     [][]byte
	closed   bool
	closeErr error
}

func newMockASRStream(buffer int) *mockASRStream {
	return &mockASRStream{results: make(chan polyphon.ASRResult, buffer)}
}

func (m *mockASRStream) SendAudio(audio []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, audio)
	return nil
}

func (m *mockASRStream) Results() <-chan polyphon.ASRResult { return m.results }

func (m *mockASRStream) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	close(m.results)
	return m.closeErr
}

func (m *mockASRStream) audioChunks() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent)
}

// newTestSession wires a polyphonASRSession directly to a mock upstream
// stream, mirroring what a provider's StartStream builds at runtime.
func newTestSession(stream polyphon.ASRStream) *polyphonASRSession {
	return newPolyphonASRSession(stream, "openai-realtime", slog.New(slog.DiscardHandler))
}

func TestSession_ForwardsInterimAndFinal(t *testing.T) {
	mock := newMockASRStream(4)
	mock.results <- polyphon.ASRResult{Text: "hey sof", IsFinal: false, Confidence: 0.6}
	mock.results <- polyphon.ASRResult{Text: "hey sofia", IsFinal: true, Confidence: 0.95}
	_ = mock.Close() // closes upstream results channel; forwardResults will drain and close downstream

	sess := newTestSession(mock)

	var got []TranscriptionResult
	for r := range sess.Receive() {
		got = append(got, r)
	}

	if len(got) != 2 {
		t.Fatalf("expected 2 results, got %d", len(got))
	}
	if got[0].IsFinal {
		t.Error("first result should be interim")
	}
	if got[0].Text != "hey sof" {
		t.Errorf("first text = %q, want %q", got[0].Text, "hey sof")
	}
	if !got[1].IsFinal {
		t.Error("second result should be final")
	}
	if got[1].Text != "hey sofia" {
		t.Errorf("second text = %q, want %q", got[1].Text, "hey sofia")
	}
	// polyphon uses float64 confidence; we downcast to float32.
	if got[1].Confidence < 0.9 {
		t.Errorf("second confidence = %f, want > 0.9", got[1].Confidence)
	}
}

// TestSession_Finalize_AccumulatesMultipleFinals locks in the contract
// that multiple IsFinal=true results (one per utterance) accumulate into the
// full session transcript. The previous behavior (returning only the most
// recent final) was a bug: when the user paused mid-recording and then spoke
// again, the UI would drop the earlier utterance.
func TestSession_Finalize_AccumulatesMultipleFinals(t *testing.T) {
	mock := newMockASRStream(4)
	mock.results <- polyphon.ASRResult{Text: "first segment", IsFinal: true, Confidence: 0.9}
	mock.results <- polyphon.ASRResult{Text: "second segment final", IsFinal: true, Confidence: 0.92}

	sess := newTestSession(mock)

	// Drain the results channel on a side goroutine so forwardResults can
	// complete its sends (the channel has finite capacity).
	drained := make(chan struct{})
	go func() {
		for range sess.Receive() {
		}
		close(drained)
	}()

	final, err := sess.Finalize(context.Background())
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if final == nil {
		t.Fatal("expected non-nil FinalTranscription")
	}
	want := "first segment second segment final"
	if final.Text != want {
		t.Errorf("Finalize.Text = %q, want %q", final.Text, want)
	}
	if final.Provider != "openai-realtime" {
		t.Errorf("Finalize.Provider = %q, want %q", final.Provider, "openai-realtime")
	}

	<-drained
}

// TestPolyphonSession_AccumulatesAcrossUtterances exercises the full pause/
// resume flow: utterance 1 streams deltas + final, user pauses, utterance 2
// streams deltas + final. Each emission to the consumer must carry the full
// running transcript (finals-so-far joined with the current interim), not
// just the latest utterance.
func TestPolyphonSession_AccumulatesAcrossUtterances(t *testing.T) {
	mock := newMockASRStream(16)

	// Utterance 1: two interim deltas + final.
	mock.results <- polyphon.ASRResult{Text: "hey", IsFinal: false}
	mock.results <- polyphon.ASRResult{Text: "hey there", IsFinal: false}
	mock.results <- polyphon.ASRResult{Text: "Hey, there.", IsFinal: true, Confidence: 0.95}

	// Utterance 2: two interim deltas + final.
	mock.results <- polyphon.ASRResult{Text: "how", IsFinal: false}
	mock.results <- polyphon.ASRResult{Text: "how are you", IsFinal: false}
	mock.results <- polyphon.ASRResult{Text: "How are you?", IsFinal: true, Confidence: 0.96}

	_ = mock.Close()

	sess := newTestSession(mock)
	var got []TranscriptionResult
	for r := range sess.Receive() {
		got = append(got, r)
	}

	want := []struct {
		text    string
		isFinal bool
	}{
		{"hey", false},
		{"hey there", false},
		{"Hey, there.", true},
		// Utterance 2 interim deltas must carry the session prefix so the UI
		// sees the transcript grow, not reset.
		{"Hey, there. how", false},
		{"Hey, there. how are you", false},
		{"Hey, there. How are you?", true},
	}

	if len(got) != len(want) {
		t.Fatalf("got %d results, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Text != w.text {
			t.Errorf("result %d: Text = %q, want %q", i, got[i].Text, w.text)
		}
		if got[i].IsFinal != w.isFinal {
			t.Errorf("result %d: IsFinal = %v, want %v", i, got[i].IsFinal, w.isFinal)
		}
	}

	// Finalize should return the full accumulated session transcript.
	final, err := sess.Finalize(context.Background())
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if final.Text != "Hey, there. How are you?" {
		t.Errorf("Finalize.Text = %q, want %q", final.Text, "Hey, there. How are you?")
	}
}

func TestSession_Finalize_EmptyWhenNoFinalArrived(t *testing.T) {
	mock := newMockASRStream(4)
	mock.results <- polyphon.ASRResult{Text: "interim only", IsFinal: false, Confidence: 0.5}

	sess := newTestSession(mock)

	go func() {
		for range sess.Receive() {
		}
	}()

	final, err := sess.Finalize(context.Background())
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if final.Text != "" {
		t.Errorf("Finalize.Text = %q, want empty string", final.Text)
	}
}

func TestSession_Close_Idempotent(t *testing.T) {
	mock := newMockASRStream(1)
	sess := newTestSession(mock)

	if err := sess.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestSession_SendAudio_ForwardsToUpstream(t *testing.T) {
	mock := newMockASRStream(1)
	sess := newTestSession(mock)
	defer sess.Close()

	if err := sess.SendAudio([]byte{0x01, 0x02, 0x03}); err != nil {
		t.Fatalf("SendAudio: %v", err)
	}
	if got := mock.audioChunks(); got != 1 {
		t.Errorf("upstream chunks = %d, want 1", got)
	}
}
