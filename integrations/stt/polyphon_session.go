package stt

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/polyphon"
)

// polyphonASRSession adapts a polyphon.ASRStream to stt.StreamingSession.
// It is shared by every STT provider that wraps a polyphon.ASRProvider --
// OpenAI Realtime today, Deepgram Nova-3 in Stage 2 of the Deepgram
// migration, and any future streaming provider -- since the bridge logic
// between the two interfaces is identical.
//
// Multi-utterance accumulation: streaming providers emit per-utterance
// transcripts -- each IsFinal=true result carries only the text of the
// utterance that just ended, not the running session transcript. Without
// accumulation here, when the speaker pauses mid-recording and then
// resumes, the UI renders only the latest utterance and the previous text
// disappears. The session buffer concatenates each final into sessionText
// and emits interim deltas as sessionText + current_interim, so consumers
// can keep a simple "replace interimText on every delta" rule and still
// see a growing transcript across pauses.
type polyphonASRSession struct {
	stream   polyphon.ASRStream
	logger   *slog.Logger
	provider string
	results  chan TranscriptionResult

	startedAt time.Time
	pumpDone  chan struct{}

	mu     sync.Mutex
	closed bool
	// sessionText accumulates the final transcripts of every completed
	// utterance within this session, separated by a single space. It's
	// the authoritative running transcript returned by Finalize().
	sessionText string
}

// newPolyphonASRSession wires a polyphon.ASRStream into a stt.StreamingSession
// and starts the forwarder goroutine. The provider name flows into the final
// transcription payload and the drop-log line.
func newPolyphonASRSession(stream polyphon.ASRStream, provider string, logger *slog.Logger) *polyphonASRSession {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	s := &polyphonASRSession{
		stream:    stream,
		logger:    logger,
		provider:  provider,
		results:   make(chan TranscriptionResult, 16),
		startedAt: time.Now(),
		pumpDone:  make(chan struct{}),
	}
	go s.forwardResults(stream.Results())
	return s
}

// SendAudio forwards an audio chunk to the upstream provider.
func (s *polyphonASRSession) SendAudio(audio []byte) error {
	return s.stream.SendAudio(audio)
}

// Receive returns the channel of interim + final transcription results.
// The gRPC layer drains this and emits SITranscribeStreamDelta per item.
func (s *polyphonASRSession) Receive() <-chan TranscriptionResult {
	return s.results
}

// Finalize signals end-of-audio upstream, waits briefly for the forwarder
// goroutine to drain, and returns the accumulated session transcript.
func (s *polyphonASRSession) Finalize(ctx context.Context) (*FinalTranscription, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return s.finalResult(), nil
	}
	s.closed = true
	s.mu.Unlock()

	// Closing the upstream stream is the provider-specific EOS signal
	// (for OpenAI this closes the WebSocket; Deepgram's WebSocket likewise
	// gets a close-frame in Stage 2). Either way, forwardResults will
	// exit when the upstream channel closes.
	if err := s.stream.Close(); err != nil {
		s.logger.Warn("polyphon-asr: close stream", "provider", s.provider, "error", err)
	}

	// Bound the drain so a stuck upstream doesn't hang the request.
	select {
	case <-s.pumpDone:
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		s.logger.Warn("polyphon-asr: forwardResults did not drain within 3s",
			"provider", s.provider)
	}

	return s.finalResult(), nil
}

// Close terminates the session without emitting further messages.
func (s *polyphonASRSession) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	return s.stream.Close()
}

func (s *polyphonASRSession) finalResult() *FinalTranscription {
	s.mu.Lock()
	text := s.sessionText
	s.mu.Unlock()
	return &FinalTranscription{
		Text:       text,
		DurationMS: time.Since(s.startedAt).Milliseconds(),
		Provider:   s.provider,
	}
}

// joinText concatenates two non-empty transcript fragments with a single
// space, trimming whitespace so the join never produces double spaces or a
// leading space (OpenAI deltas can start with a space, for example).
func joinText(prefix, suffix string) string {
	suffix = strings.TrimSpace(suffix)
	if suffix == "" {
		return prefix
	}
	if prefix == "" {
		return suffix
	}
	return prefix + " " + suffix
}

// forwardResults reads ASRResult from the polyphon stream and translates each
// into a TranscriptionResult on the stt session's results channel.
//
// For interim results: emit sessionText + current-utterance-interim. For
// final results: commit the utterance into sessionText and emit the full
// running transcript. Exits when the upstream channel closes.
func (s *polyphonASRSession) forwardResults(in <-chan polyphon.ASRResult) {
	defer close(s.results)
	defer close(s.pumpDone)

	for r := range in {
		text := strings.TrimSpace(r.Text)
		if text == "" {
			continue
		}

		s.mu.Lock()
		var out string
		if r.IsFinal {
			s.sessionText = joinText(s.sessionText, text)
			out = s.sessionText
		} else {
			out = joinText(s.sessionText, text)
		}
		s.mu.Unlock()

		select {
		case s.results <- TranscriptionResult{
			Text:       out,
			IsFinal:    r.IsFinal,
			Confidence: float32(r.Confidence),
		}:
		default:
			s.logger.Debug("polyphon-asr: results channel full, dropping interim",
				"provider", s.provider)
		}
	}
}
