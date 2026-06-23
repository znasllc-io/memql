package agent

import (
	"context"
	"log/slog"
	"strings"
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
		PartitionID:   "s1",
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
	cas.handleASRResult("", polyphon.ASRResult{Kind: polyphon.ASRKindSpeechStarted})
	cas.handleASRResult("", polyphon.ASRResult{Text: "what is the weather"})
	cas.handleASRResult("", polyphon.ASRResult{Text: "what is the weather", IsFinal: true})

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
	cas.handleASRResult("", polyphon.ASRResult{Kind: polyphon.ASRKindSpeechStarted})
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

	cas.handleASRResult("", polyphon.ASRResult{Text: "mm hmm", IsFinal: true})

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

// newCascadeWithConfig builds a cascade against a fakeStream-backed client
// with an explicit CascadeConfig + logger (the #1403 speaker-attribution
// tests need control over SpeakerUserID and log capture).
func newCascadeWithConfig(t *testing.T, fs *fakeStream, cfg CascadeConfig, logger *slog.Logger) *Cascade {
	t.Helper()
	c := newTestClient(t, fs)
	cas := NewCascade(context.Background(), cfg, c, &fakeTTS{}, &recordingSink{}, "aura-2-thalia-en", logger)
	t.Cleanup(cas.Close)
	cas.Start()
	return cas
}

// awaitFinalAndTurn waits until the wire has carried both a
// VoiceAgentFinalTranscript and a VoiceAgentTurnRequest, then returns them.
func awaitFinalAndTurn(t *testing.T, fs *fakeStream) (*memqlv1.VoiceAgentFinalTranscript, *memqlv1.VoiceAgentTurnRequest) {
	t.Helper()
	var final *memqlv1.VoiceAgentFinalTranscript
	var turn *memqlv1.VoiceAgentTurnRequest
	require.Eventually(t, func() bool {
		final, turn = nil, nil
		for _, env := range fs.sentEnvelopes() {
			if f := env.GetVoiceAgentFinalTranscript(); f != nil {
				final = f
			}
			if tr := env.GetVoiceAgentTurnRequest(); tr != nil {
				turn = tr
			}
		}
		return final != nil && turn != nil
	}, 2*time.Second, 10*time.Millisecond, "final transcript + turn request must reach the wire")
	return final, turn
}

// TestCascade_PerTrackSpeakerAttribution pins the #1403 primary fix: the
// participant identity carried with the per-track ASR results is stamped as
// speaker_user_id on the partial, the final transcript, AND the turn
// request -- and wins over the configured 1:1 fallback.
func TestCascade_PerTrackSpeakerAttribution(t *testing.T) {
	const track = "standard:v1:cognition:participant:alice"
	fs := newFakeStream()
	cas := newCascadeWithConfig(t, fs, CascadeConfig{
		PartitionID:       "s1",
		GaAgentID:     "s1-ga",
		SpeakerUserID: "fallback-speaker",
		Thread:        memqlv1.VoiceAgentTurnRequest_THREAD_CONTEXT_TEAM,
	}, nil)

	cas.handleASRResult(track, polyphon.ASRResult{Kind: polyphon.ASRKindSpeechStarted})
	cas.handleASRResult(track, polyphon.ASRResult{Text: "hello"})
	cas.handleASRResult(track, polyphon.ASRResult{Text: "hello there", IsFinal: true})

	final, turn := awaitFinalAndTurn(t, fs)
	assert.Equal(t, track, final.GetSpeakerUserId(),
		"final transcript must carry the per-track speaker, not the config fallback")
	assert.Equal(t, track, turn.GetSpeakerUserId(),
		"turn request must carry the per-track speaker")
	for _, env := range fs.sentEnvelopes() {
		if p := env.GetVoiceAgentPartialTranscript(); p != nil {
			assert.Equal(t, track, p.GetSpeakerUserId(),
				"partials must carry the per-track speaker too")
		}
	}
}

// TestCascade_SpeakerFallsBackToConfig pins the 1:1 wiring contract: with no
// per-track identity, CascadeConfig.SpeakerUserID is stamped on the wire.
func TestCascade_SpeakerFallsBackToConfig(t *testing.T) {
	fs := newFakeStream()
	cas := newCascadeWithConfig(t, fs, CascadeConfig{
		PartitionID:       "s1",
		GaAgentID:     "s1-ga",
		SpeakerUserID: "cfg-speaker",
		Thread:        memqlv1.VoiceAgentTurnRequest_THREAD_CONTEXT_TEAM,
	}, nil)

	cas.handleASRResult("", polyphon.ASRResult{Text: "hi", IsFinal: true})

	final, turn := awaitFinalAndTurn(t, fs)
	assert.Equal(t, "cfg-speaker", final.GetSpeakerUserId())
	assert.Equal(t, "cfg-speaker", turn.GetSpeakerUserId())
}

// TestCascade_EmptySpeakerStillSends pins the documented server contract:
// with neither a per-track identity nor a configured fallback, the final
// transcript is still SENT with an empty speaker (the server resolves the
// space's single active human) rather than being dropped client-side.
func TestCascade_EmptySpeakerStillSends(t *testing.T) {
	fs := newFakeStream()
	cas := newCascadeWithConfig(t, fs, CascadeConfig{
		PartitionID:   "s1",
		GaAgentID: "s1-ga",
		Thread:    memqlv1.VoiceAgentTurnRequest_THREAD_CONTEXT_TEAM,
	}, nil)

	cas.handleASRResult("", polyphon.ASRResult{Text: "hi", IsFinal: true})

	final, turn := awaitFinalAndTurn(t, fs)
	assert.Empty(t, final.GetSpeakerUserId())
	assert.Empty(t, turn.GetSpeakerUserId())
}

// syncLogBuffer is a goroutine-safe io.Writer for capturing slog output.
type syncLogBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestCascade_FinalAckRejectionLogged pins the #1403 no-more-silent-failures
// fix: a server FinalAck with success=false is consumed and WARN-logged
// instead of being discarded.
func TestCascade_FinalAckRejectionLogged(t *testing.T) {
	buf := &syncLogBuffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	fs := newFakeStream()
	fs.onSend = func(env *memqlv1.MemqlClientMessage) {
		if env.GetVoiceAgentFinalTranscript() == nil {
			return
		}
		fs.push(&memqlv1.MemqlServerMessage{
			CorrelateTo: env.GetMessageId(),
			Payload: &memqlv1.MemqlServerMessage_VoiceAgentFinalAck{
				VoiceAgentFinalAck: &memqlv1.VoiceAgentFinalAck{
					RequestId:    "r1",
					Success:      false,
					ErrorCode:    "speaker_unresolvable",
					ErrorMessage: "2 active human participants",
				},
			},
		})
	}
	cas := newCascadeWithConfig(t, fs, CascadeConfig{
		PartitionID:   "s1",
		GaAgentID: "s1-ga",
		Thread:    memqlv1.VoiceAgentTurnRequest_THREAD_CONTEXT_TEAM,
	}, logger)

	cas.handleASRResult("", polyphon.ASRResult{Text: "hi", IsFinal: true})

	require.Eventually(t, func() bool {
		out := buf.String()
		return strings.Contains(out, "voice-agent final transcript rejected") &&
			strings.Contains(out, "speaker_unresolvable")
	}, 2*time.Second, 10*time.Millisecond, "rejected FinalAck must be WARN-logged")
}

// TestLogFinalTranscriptReply_Variants pins the reply classifier directly:
// QueryError rejections and successful acks log at the right levels.
func TestLogFinalTranscriptReply_Variants(t *testing.T) {
	buf := &syncLogBuffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	logFinalTranscriptReply(logger, "cascade", "s1", &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_QueryError{
			QueryError: &memqlv1.QueryErrorMsg{
				RequestId: "r2",
				Error:     &memqlv1.QueryError{Code: "INVALID_ARGUMENT", Message: "partitionId, speakerUserId, finalText are required"},
			},
		},
	})
	assert.Contains(t, buf.String(), "voice-agent final transcript rejected")
	assert.Contains(t, buf.String(), "INVALID_ARGUMENT")

	logFinalTranscriptReply(logger, "cascade", "s1", &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_VoiceAgentFinalAck{
			VoiceAgentFinalAck: &memqlv1.VoiceAgentFinalAck{RequestId: "r3", Success: true, UtteranceId: "u1"},
		},
	})
	assert.Contains(t, buf.String(), "voice-agent final transcript acked")
}
