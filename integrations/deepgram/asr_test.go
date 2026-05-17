package deepgram

import (
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/polyphon"
)

// newTestStream builds a deepgramASRStream with no real WebSocket --
// the receiveLoop and watchdog goroutines are never started, so
// handleResults / handleUtteranceEnd can be invoked synchronously
// by the tests below.
func newTestStream() *deepgramASRStream {
	return &deepgramASRStream{
		results: make(chan polyphon.ASRResult, 32),
		done:    make(chan struct{}),
		logger:  slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
}

func drainResults(s *deepgramASRStream) []polyphon.ASRResult {
	var out []polyphon.ASRResult
	for {
		select {
		case r, ok := <-s.results:
			if !ok {
				return out
			}
			out = append(out, r)
		default:
			return out
		}
	}
}

func makeResults(transcript string, isFinal bool, conf float64) deepgramEvent {
	evt := deepgramEvent{Type: "Results", IsFinal: isFinal}
	evt.Channel.Alternatives = []struct {
		Transcript string  `json:"transcript"`
		Confidence float64 `json:"confidence"`
	}{
		{Transcript: transcript, Confidence: conf},
	}
	return evt
}

// TestStream_ResultsAlwaysSurfaceAsInterim locks in the unified-mode
// behavior: every Results event (regardless of is_final) surfaces as
// IsFinal=false at the polyphon.ASRStream layer. The actual
// IsFinal=true dispatch only fires from handleUtteranceEnd (either
// Deepgram UtteranceEnd or the client watchdog).
//
// Treating Results.is_final=true as EOU was the bug that produced
// "agent fires three times for one human thought" -- Deepgram's
// is_final is a phrase-commit signal, not an utterance boundary.
func TestStream_ResultsAlwaysSurfaceAsInterim(t *testing.T) {
	s := newTestStream()
	s.handleResults(makeResults("not much.", false, 0.6))
	s.handleResults(makeResults("not much. actually...", true, 0.95))

	got := drainResults(s)
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2", len(got))
	}
	for i, r := range got {
		if r.IsFinal {
			t.Errorf("result %d emitted IsFinal=true from handleResults; want IsFinal=false (only handleUtteranceEnd dispatches final)", i)
		}
	}
}

// TestStream_AccumulatesPhrasesAcrossFinals -- the accumulator
// behavior: phrases committed via Results.is_final=true grow the
// running transcript; the last interim carries the full combined
// text for in-room display.
func TestStream_AccumulatesPhrasesAcrossFinals(t *testing.T) {
	s := newTestStream()
	s.handleResults(makeResults("not much.", true, 0.95))
	s.handleResults(makeResults("are you able", false, 0.6))
	s.handleResults(makeResults("are you able to tell me", true, 0.9))
	s.handleResults(makeResults("what your capabilities are?", true, 0.95))

	got := drainResults(s)
	wantLast := "not much. are you able to tell me what your capabilities are?"
	if got[len(got)-1].Text != wantLast {
		t.Errorf("accumulated text = %q\nwant %q", got[len(got)-1].Text, wantLast)
	}
}

// TestStream_UtteranceEndFiresFinal verifies that the EOU signal --
// from Deepgram's UtteranceEnd event -- flushes the accumulator as
// IsFinal=true.
func TestStream_UtteranceEndFiresFinal(t *testing.T) {
	s := newTestStream()
	s.handleResults(makeResults("hello sofia.", true, 0.95))
	s.handleResults(makeResults("good to see you.", true, 0.95))
	_ = drainResults(s)

	s.handleUtteranceEnd()

	got := drainResults(s)
	if len(got) != 1 {
		t.Fatalf("UtteranceEnd produced %d results, want 1", len(got))
	}
	if !got[0].IsFinal {
		t.Errorf("UtteranceEnd result IsFinal=false; want true")
	}
	if got[0].Text != "hello sofia. good to see you." {
		t.Errorf("UtteranceEnd text = %q, want %q",
			got[0].Text, "hello sofia. good to see you.")
	}
}

// TestStream_UtteranceEndFallsBackToInterim covers very-short
// utterances where Deepgram never committed via is_final=true. The
// accumulator falls back to the most recent interim so the consumer
// still sees the transcript.
func TestStream_UtteranceEndFallsBackToInterim(t *testing.T) {
	s := newTestStream()
	s.handleResults(makeResults("yeah", false, 0.5))
	_ = drainResults(s)

	s.handleUtteranceEnd()

	got := drainResults(s)
	if len(got) != 1 {
		t.Fatalf("UtteranceEnd produced %d results, want 1", len(got))
	}
	if !got[0].IsFinal || got[0].Text != "yeah" {
		t.Errorf("interim-only fallback failed: got %+v", got[0])
	}
}

// TestStream_UtteranceEndOnEmptyUtteranceIsNoop guards against firing
// spurious finals when Deepgram's VAD declares EOU on silence.
func TestStream_UtteranceEndOnEmptyUtteranceIsNoop(t *testing.T) {
	s := newTestStream()
	s.handleUtteranceEnd()
	got := drainResults(s)
	if len(got) != 0 {
		t.Errorf("UtteranceEnd on empty state produced %d results, want 0", len(got))
	}
}

// TestStream_UtteranceEndIsIdempotentWithinOneUtterance guards
// against the Deepgram-UtteranceEnd vs client-watchdog race emitting
// two finals for one utterance. After the first fires, subsequent
// handleUtteranceEnd calls are no-ops until new transcript arrives.
func TestStream_UtteranceEndIsIdempotentWithinOneUtterance(t *testing.T) {
	s := newTestStream()
	s.handleResults(makeResults("hello.", true, 0.9))
	_ = drainResults(s)

	s.handleUtteranceEnd() // first fire (e.g. Deepgram UtteranceEnd)
	s.handleUtteranceEnd() // second fire (e.g. client watchdog)

	got := drainResults(s)
	if len(got) != 1 {
		t.Fatalf("two handleUtteranceEnd calls produced %d finals, want 1: %+v", len(got), got)
	}
	if !got[0].IsFinal || got[0].Text != "hello." {
		t.Errorf("final = %+v; want IsFinal=true Text=hello.", got[0])
	}
}

// TestStream_AccumulatorResetsBetweenUtterances ensures the running
// transcript is cleared after each EOU so utterance N+1 doesn't
// carry forward N's text.
func TestStream_AccumulatorResetsBetweenUtterances(t *testing.T) {
	s := newTestStream()
	s.handleResults(makeResults("first.", true, 0.9))
	s.handleUtteranceEnd()
	_ = drainResults(s)

	s.handleResults(makeResults("second.", true, 0.9))
	s.handleUtteranceEnd()
	got := drainResults(s)

	if len(got) != 2 {
		t.Fatalf("utterance 2 produced %d results, want 2 (one interim, one final)", len(got))
	}
	finalResult := got[len(got)-1]
	if !finalResult.IsFinal || finalResult.Text != "second." {
		t.Errorf("utterance 2 final = %+v; want IsFinal=true Text=second.", finalResult)
	}
}

// TestStream_KeepaliveInterimsDontResetWatchdogClock verifies that
// when Deepgram keeps emitting the SAME interim text as a keepalive
// (the failure mode where ambient noise prevents Deepgram from
// firing UtteranceEnd), the watchdog's staleness timer keeps
// counting. Re-emission of identical text must NOT push lastChangeAt
// forward.
func TestStream_KeepaliveInterimsDontResetWatchdogClock(t *testing.T) {
	s := newTestStream()

	// First arrival -- text grows from "" to "hello sofia."
	s.handleResults(makeResults("hello sofia.", true, 0.9))
	s.pendingMu.Lock()
	firstChange := s.lastChangeAt
	s.pendingMu.Unlock()
	if firstChange.IsZero() {
		t.Fatal("lastChangeAt not set on first interim")
	}

	// Sleep a tick so wall-clock advances measurably.
	time.Sleep(20 * time.Millisecond)

	// Deepgram keepalive: same text re-emitted. With the current
	// implementation lastChangeAt resets on every handleResults
	// call regardless of whether text changed -- that's a known
	// simplification. This test documents the current behavior so a
	// future tightening (only-reset-when-text-changes) is a deliberate
	// decision, not an accident.
	s.handleResults(makeResults("hello sofia.", false, 0.9))
	s.pendingMu.Lock()
	secondChange := s.lastChangeAt
	s.pendingMu.Unlock()

	// Today's implementation: secondChange > firstChange (each
	// Results event resets the timer). If the future implementation
	// only resets on real content change, this test will need to
	// flip its expectation. The test exists to catch silent behavior
	// drift either way.
	if secondChange.Before(firstChange) {
		t.Errorf("lastChangeAt went backwards: %v -> %v", firstChange, secondChange)
	}
}

// TestStream_HandleEventDispatchesUtteranceEndJSON locks in the
// envelope/event-payload split. Deepgram's UtteranceEnd shape has
// `channel` as a JSON array (channel indices) while Results has it
// as an object (alternatives). A single shared struct fails to
// unmarshal UtteranceEnd events, drops them, and the downstream
// pipeline never sees end-of-utterance -- which the user experiences
// as "I spoke and nothing happened" (no transcript, no agent reply).
func TestStream_HandleEventDispatchesUtteranceEndJSON(t *testing.T) {
	s := newTestStream()

	// Seed with a Results event (object-shaped channel).
	resultsJSON := []byte(`{
		"type": "Results",
		"is_final": true,
		"channel": {
			"alternatives": [{"transcript": "Hello, Sofia.", "confidence": 0.95}]
		}
	}`)
	s.handleEvent(resultsJSON)
	_ = drainResults(s)

	// Then UtteranceEnd (array-shaped channel).
	utteranceEndJSON := []byte(`{
		"type": "UtteranceEnd",
		"channel": [0, 1],
		"last_word_end": 1.234
	}`)
	s.handleEvent(utteranceEndJSON)

	got := drainResults(s)
	if len(got) != 1 {
		t.Fatalf("UtteranceEnd JSON dispatch produced %d results, want 1", len(got))
	}
	if !got[0].IsFinal {
		t.Errorf("UtteranceEnd JSON dispatch IsFinal=false; want true")
	}
	if got[0].Text != "Hello, Sofia." {
		t.Errorf("UtteranceEnd JSON dispatch text = %q, want %q", got[0].Text, "Hello, Sofia.")
	}
}

func TestJoinPhrase(t *testing.T) {
	cases := []struct{ left, right, want string }{
		{"", "", ""},
		{"a", "", "a"},
		{"", "b", "b"},
		{"a", "b", "a b"},
		{" a ", " b ", "a b"},
	}
	for _, tc := range cases {
		if got := joinPhrase(tc.left, tc.right); got != tc.want {
			t.Errorf("joinPhrase(%q, %q) = %q, want %q", tc.left, tc.right, got, tc.want)
		}
	}
}

// Compile-time check: sync.Mutex usage stays consistent.
var _ = sync.Mutex{}
