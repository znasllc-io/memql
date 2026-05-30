package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/polyphon"
)

// fakeTTS is an in-memory ttsSynthesizer. It records the requests it
// received and emits the configured frames, honoring ctx cancellation
// (barge-in) by stopping mid-stream and recording that it was cut.
type fakeTTS struct {
	mu       sync.Mutex
	requests []string
	voiceIDs []string
	frames   [][]byte
	// blockAfter, when > 0, makes the synthesizer block (until ctx cancel)
	// after emitting that many frames so a test can assert barge-in cut.
	blockAfter int
	cut        bool
}

func (f *fakeTTS) SynthesizePCM(ctx context.Context, text, voiceID string) (<-chan []byte, error) {
	f.mu.Lock()
	f.requests = append(f.requests, text)
	f.voiceIDs = append(f.voiceIDs, voiceID)
	frames := f.frames
	blockAfter := f.blockAfter
	f.mu.Unlock()

	out := make(chan []byte, len(frames)+1)
	go func() {
		defer close(out)
		for i, fr := range frames {
			if blockAfter > 0 && i >= blockAfter {
				// Hold the stream open until cancelled (barge-in).
				<-ctx.Done()
				f.mu.Lock()
				f.cut = true
				f.mu.Unlock()
				return
			}
			select {
			case out <- fr:
			case <-ctx.Done():
				f.mu.Lock()
				f.cut = true
				f.mu.Unlock()
				return
			}
		}
	}()
	return out, nil
}

func (f *fakeTTS) sentRequests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.requests))
	copy(out, f.requests)
	return out
}

func (f *fakeTTS) wasCut() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cut
}

// recordingSink records published frames.
type recordingSink struct {
	mu      sync.Mutex
	frames  [][]byte
	flushes int
}

func (s *recordingSink) WriteFrame(pcm []byte) error {
	s.mu.Lock()
	cp := make([]byte, len(pcm))
	copy(cp, pcm)
	s.frames = append(s.frames, cp)
	s.mu.Unlock()
	return nil
}

func (s *recordingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.frames)
}

func (s *recordingSink) flushed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushes
}

func (s *recordingSink) Flush() {
	s.mu.Lock()
	s.flushes++
	s.mu.Unlock()
}

// newCascadeForTest wires a cascade against a fakeStream-backed client.
func newCascadeForTest(t *testing.T, fs *fakeStream, tts ttsSynthesizer, sink audioSink) *Cascade {
	t.Helper()
	c := newTestClient(t, fs)
	cas := NewCascade(context.Background(), CascadeConfig{
		SpaceID:   "s1",
		GaAgentID: "s1-ga",
		Thread:    memqlv1.VoiceAgentTurnRequest_THREAD_CONTEXT_TEAM,
	}, c, tts, sink, "aura-2-thalia-en", nil)
	t.Cleanup(cas.Close)
	cas.Start()
	return cas
}

// TestCascade_FullLoop_STTToTurnRequestToTTS exercises the end-to-end
// cascade: a final transcript drives a VoiceAgentFinalTranscript + a
// streaming VoiceAgentTurnRequest; the server's TurnComplete reply is
// synthesized via TTS and the PCM frames reach the room sink.
func TestCascade_FullLoop_STTToTurnRequestToTTS(t *testing.T) {
	fs := newFakeStream()
	// On a TurnRequest, reply with a delta + complete correlated to the
	// request envelope's message id (mirrors voice_agent_handlers.go).
	fs.onSend = func(env *memqlv1.MemqlClientMessage) {
		if env.GetVoiceAgentTurnRequest() == nil {
			return
		}
		corr := env.GetMessageId()
		fs.push(&memqlv1.MemqlServerMessage{
			CorrelateTo: corr,
			Payload: &memqlv1.MemqlServerMessage_VoiceAgentTurnDelta{
				VoiceAgentTurnDelta: &memqlv1.VoiceAgentTurnDelta{RequestId: "r1", TextDelta: "Hi "},
			},
		})
		fs.push(&memqlv1.MemqlServerMessage{
			CorrelateTo: corr,
			Payload: &memqlv1.MemqlServerMessage_VoiceAgentTurnComplete{
				VoiceAgentTurnComplete: &memqlv1.VoiceAgentTurnComplete{
					RequestId: "r1", FinalText: "Hi there!", UtteranceId: "u1",
				},
			},
		})
	}
	tts := &fakeTTS{frames: [][]byte{{1, 2}, {3, 4}, {5, 6}}}
	sink := &recordingSink{}
	cas := newCascadeForTest(t, fs, tts, sink)

	// Drive the STT side: onset, interim, final.
	cas.handleASRResult(polyphon.ASRResult{Kind: polyphon.ASRKindSpeechStarted})
	cas.handleASRResult(polyphon.ASRResult{Text: "what is the weather"})
	cas.handleASRResult(polyphon.ASRResult{Text: "what is the weather", IsFinal: true})

	// The TTS reply should be synthesized and published.
	require.Eventually(t, func() bool {
		return len(tts.sentRequests()) == 1 && sink.count() == 3
	}, 2*time.Second, 10*time.Millisecond, "reply should be synthesized + published")

	assert.Equal(t, []string{"Hi there!"}, tts.sentRequests(),
		"TurnComplete final text drives TTS")
	assert.Equal(t, StateListening, cas.Machine().State(),
		"assistant playout complete returns to listening")

	// The wire carried a final transcript + a turn request.
	var sawFinal, sawTurn, sawPartial bool
	for _, env := range fs.sentEnvelopes() {
		switch {
		case env.GetVoiceAgentFinalTranscript() != nil:
			sawFinal = true
			assert.Equal(t, "what is the weather", env.GetVoiceAgentFinalTranscript().GetFinalText())
		case env.GetVoiceAgentTurnRequest() != nil:
			sawTurn = true
			assert.Equal(t, "what is the weather", env.GetVoiceAgentTurnRequest().GetUtteranceText())
		case env.GetVoiceAgentPartialTranscript() != nil:
			sawPartial = true
		}
	}
	assert.True(t, sawFinal, "VoiceAgentFinalTranscript forwarded")
	assert.True(t, sawTurn, "VoiceAgentTurnRequest sent")
	assert.True(t, sawPartial, "VoiceAgentPartialTranscript forwarded for interim")
}

// TestCascade_Speak_DrivesTTS exercises the unsolicited VoiceAgentSpeak
// path (conductor push) -> TTS -> room.
func TestCascade_Speak_DrivesTTS(t *testing.T) {
	fs := newFakeStream()
	tts := &fakeTTS{frames: [][]byte{{9, 9}, {8, 8}}}
	sink := &recordingSink{}
	cas := newCascadeForTest(t, fs, tts, sink)

	ok := cas.Speak(SpeakDirective{Text: "Unsolicited reply", UtteranceID: "u9"})
	assert.True(t, ok)

	require.Eventually(t, func() bool {
		return sink.count() == 2
	}, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, []string{"Unsolicited reply"}, tts.sentRequests())

	// Empty speak text is rejected.
	assert.False(t, cas.Speak(SpeakDirective{Text: "   "}))
}

// TestCascade_BargeIn_CutsTTS verifies a human onset during assistant
// playout cancels the in-flight TTS synthesis.
func TestCascade_BargeIn_CutsTTS(t *testing.T) {
	fs := newFakeStream()
	// One frame, then block until cancelled.
	tts := &fakeTTS{frames: [][]byte{{1}, {2}, {3}}, blockAfter: 1}
	sink := &recordingSink{}
	cas := newCascadeForTest(t, fs, tts, sink)

	require.True(t, cas.Speak(SpeakDirective{Text: "a very long answer"}))
	require.Eventually(t, func() bool {
		return sink.count() >= 1
	}, 2*time.Second, 10*time.Millisecond, "playout started")
	require.Equal(t, StateAssistantTurn, cas.Machine().State())

	// Human onset -> barge-in -> TTS cut.
	cas.handleASRResult(polyphon.ASRResult{Kind: polyphon.ASRKindSpeechStarted})
	assert.Equal(t, StateHumanTurn, cas.Machine().State())
	require.Eventually(t, func() bool {
		return tts.wasCut()
	}, 2*time.Second, 10*time.Millisecond, "barge-in must cancel in-flight TTS")
	assert.GreaterOrEqual(t, sink.flushed(), 1, "barge-in must flush the publish buffer")
}

// TestCascade_SuppressedReply_NoTTS verifies that when cognition produces
// no reply (suppressed by the conductor/classifier), nothing is spoken.
func TestCascade_SuppressedReply_NoTTS(t *testing.T) {
	fs := newFakeStream()
	// TurnRequest gets a TurnComplete with empty final text (suppressed).
	fs.onSend = func(env *memqlv1.MemqlClientMessage) {
		if env.GetVoiceAgentTurnRequest() == nil {
			return
		}
		fs.push(&memqlv1.MemqlServerMessage{
			CorrelateTo: env.GetMessageId(),
			Payload: &memqlv1.MemqlServerMessage_VoiceAgentTurnComplete{
				VoiceAgentTurnComplete: &memqlv1.VoiceAgentTurnComplete{RequestId: "r1", FinalText: ""},
			},
		})
	}
	tts := &fakeTTS{frames: [][]byte{{1}}}
	sink := &recordingSink{}
	cas := newCascadeForTest(t, fs, tts, sink)

	cas.handleASRResult(polyphon.ASRResult{Text: "mm hmm", IsFinal: true})

	// Give the turn goroutine time to process the empty complete.
	require.Eventually(t, func() bool {
		for _, env := range fs.sentEnvelopes() {
			if env.GetVoiceAgentTurnRequest() != nil {
				return true
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond)
	time.Sleep(50 * time.Millisecond)

	assert.Empty(t, tts.sentRequests(), "suppressed reply must not synthesize")
	assert.Equal(t, StateListening, cas.Machine().State())
}
