package openai

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"github.com/znasllc-io/memql/integrations/audio"
	"nhooyr.io/websocket"
)

// realtime.go is the hand-rolled OpenAI gpt-realtime (speech-to-speech)
// WebSocket client, built per docs/internal/design/voice-453-gpt-realtime-go.md ("hand-roll a
// thin client on nhooyr.io/websocket; do NOT use go-openai"). It is the same
// client shape as asr.go (the in-production transcription-only realtime client)
// with a wider event vocabulary and an audio-out channel.
//
// It is PURE Go and CGO-free: it depends only on encoding/json (realtime_events.go)
// and the CGO-free integrations/audio resampler. The room audio-publish glue
// (room_realtime_voice.go, `//go:build voice`) is the only voice-tagged code;
// this client is unit-tested in the default lane.
//
// Conductor-gate posture (docs/internal/design/voice-432-conductor-response-gate.md, option A):
// the session is configured with turn_detection:null. The model never runs
// input VAD, never auto-commits the input buffer, and never self-creates a
// response. Every response.create is driven explicitly by the caller (the Go
// realtime executor, from the conductor's decision); barge-in is an explicit
// response.cancel + output_audio_buffer.clear.
//
// Concurrency model (spike section 4): one reader goroutine owns conn.Read and
// never blocks on application work; all writes are mutex-guarded so the
// conductor goroutine and the audio-input goroutine can both write safely;
// audio-out frames are pushed onto a bounded channel the room publisher drains.

// realtimeConvBaseURL is the GA conversation-mode endpoint. Note the ?model=
// form (vs asr.go's ?intent=transcription) -- mixing the two makes OpenAI
// accept the upgrade then immediately close the socket (the footgun documented
// in asr.go).
const realtimeConvBaseURL = "wss://api.openai.com/v1/realtime"

// realtimeReadLimit bounds a single server frame. Output-audio deltas are the
// largest frames; 1 MiB is generous for a 24 kHz PCM chunk.
const realtimeReadLimit = 1 << 20

// realtimeAudioOutDepth bounds the decoded-PCM output channel. A slow room
// publisher applies backpressure without blocking the reader goroutine.
const realtimeAudioOutDepth = 64

// realtimeEventDepth bounds the transcript / function-call event channel.
const realtimeEventDepth = 32

// RealtimeClient opens gpt-realtime speech-to-speech sessions. One client per
// voice provider; one *RealtimeSession per room.
type RealtimeClient struct {
	apiKey string
	model  string
	logger *slog.Logger
}

// NewRealtimeClient builds a gpt-realtime client. model is the realtime model
// id (default "gpt-realtime", from MEMQL_REALTIME_MODEL).
func NewRealtimeClient(cfg Config, model string) (*RealtimeClient, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("openai realtime: API key is required")
	}
	if model == "" {
		model = "gpt-realtime-2"
	}
	logger := cfg.logger()
	logger.Info("openai realtime: initialized", "model", model)
	return &RealtimeClient{apiKey: cfg.APIKey, model: model, logger: logger}, nil
}

// RealtimeSession is one live gpt-realtime conversation. It owns the websocket,
// the guarded writer, the input upsampler (16 kHz -> 24 kHz), and the decoded
// audio-out + event channels.
type RealtimeSession struct {
	conn      *websocket.Conn
	upsampler *audio.PCM16Resampler
	logger    *slog.Logger

	audioOut chan []byte
	events   chan RealtimeServerEvent

	writeMu sync.Mutex
	mu      sync.Mutex
	closed  bool
	once    sync.Once
	done    chan struct{}
}

// Connect dials the gpt-realtime conversation endpoint, sends the initial
// session.update from sess (turn_detection:null for the conductor gate, or the
// configured detector for the #478 native path), and starts the reader
// goroutine. On any failure it closes the socket and returns an error so the
// caller can fall back to the cascade.
func (c *RealtimeClient) Connect(ctx context.Context, sess SessionConfig) (*RealtimeSession, error) {
	url := realtimeConvBaseURL + "?model=" + c.model

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+c.apiKey)
	// GA Realtime API: NO `OpenAI-Beta: realtime=v1` header. OpenAI retired the
	// beta surface ("The Realtime Beta API is no longer supported. Please use
	// /v1/realtime for the GA API." -> close 4000 beta_api_shape_disabled); the
	// beta header is what flagged the connection as beta and got it rejected. The
	// GA endpoint (?model=gpt-realtime) authenticates with Bearer alone and reads
	// the GA session shape (type:"realtime", nested audio.input/output).

	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		return nil, fmt.Errorf("openai realtime: connect: %w", err)
	}
	conn.SetReadLimit(realtimeReadLimit)

	// Input upsampler: Polyphon 16 kHz -> OpenAI 24 kHz (same as asr.go).
	upsampler, err := audio.NewPCM16Resampler(audio.PolyphonSampleRate, audio.OpenAISampleRate)
	if err != nil {
		conn.Close(websocket.StatusInternalError, "resampler init failed")
		return nil, fmt.Errorf("openai realtime: create upsampler: %w", err)
	}

	s := &RealtimeSession{
		conn:      conn,
		upsampler: upsampler,
		logger:    c.logger,
		audioOut:  make(chan []byte, realtimeAudioOutDepth),
		events:    make(chan RealtimeServerEvent, realtimeEventDepth),
		done:      make(chan struct{}),
	}

	if err := s.sendSessionUpdate(ctx, sess); err != nil {
		conn.Close(websocket.StatusInternalError, "session config failed")
		return nil, fmt.Errorf("openai realtime: configure session: %w", err)
	}

	go s.receiveLoop(ctx)
	return s, nil
}

// AudioOut returns the decoded PCM16 (24 kHz) output-audio channel. The room
// publisher (#451 / room_realtime_voice.go) drains it; it closes when the
// session ends.
func (s *RealtimeSession) AudioOut() <-chan []byte { return s.audioOut }

// Events returns the transcript / function-call / lifecycle event channel
// (everything except audio frames). It closes when the session ends.
func (s *RealtimeSession) Events() <-chan RealtimeServerEvent { return s.events }

// SendAudio upsamples one PCM16 16 kHz chunk to 24 kHz and appends it to the
// input buffer. Under turn_detection:null this never auto-commits or
// auto-responds (multi-party active-speaker audio path, #433 section 2b).
func (s *RealtimeSession) SendAudio(pcm16k []byte) error {
	upsampled := s.upsampler.Resample(pcm16k)
	if len(upsampled) == 0 {
		return nil
	}
	data, err := encodeInputAudioAppend(upsampled)
	if err != nil {
		return fmt.Errorf("openai realtime: encode audio append: %w", err)
	}
	return s.write(data)
}

// CommitInput commits the buffered input audio as a user conversation item
// (driven from a human final transcript). Does NOT create a response.
func (s *RealtimeSession) CommitInput() error {
	data, err := encodeInputAudioCommit()
	if err != nil {
		return err
	}
	return s.write(data)
}

// InjectItem injects a labeled conversation item (multi-party labeled
// transcript per #433, or a grounding system message per #456) before a
// response is triggered.
func (s *RealtimeSession) InjectItem(item ConversationItem) error {
	data, err := encodeConversationItem(item)
	if err != nil {
		return err
	}
	return s.write(data)
}

// CreateResponse drives one response.create with a per-response instructions
// override (the conductor's rendered directive). This is the conductor gate:
// exactly one CreateResponse per "speak" decision; silence/defer call nothing.
func (s *RealtimeSession) CreateResponse(instructions string) error {
	data, err := encodeResponseCreate(instructions)
	if err != nil {
		return err
	}
	return s.write(data)
}

// CancelResponse cancels the in-flight response and clears the buffered output
// audio so playback stops immediately (barge-in, spike section 3.4).
func (s *RealtimeSession) CancelResponse() error {
	cancel, err := encodeResponseCancel()
	if err != nil {
		return err
	}
	if err := s.write(cancel); err != nil {
		return err
	}
	clear, err := encodeOutputAudioClear()
	if err != nil {
		return err
	}
	return s.write(clear)
}

// SendFunctionResult returns a tool result by call_id (the async function-call
// path, spike section 5; wired by the MCP bridge #458). It does not itself
// create a response -- the caller chains CreateResponse so the model continues
// with the result in context.
func (s *RealtimeSession) SendFunctionResult(callID, output string) error {
	data, err := encodeFunctionCallOutput(callID, output)
	if err != nil {
		return err
	}
	return s.write(data)
}

// Close tears down the session: stops the reader and closes the socket.
// Idempotent.
func (s *RealtimeSession) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		close(s.done)
		s.conn.Close(websocket.StatusNormalClosure, "session closed")
	})
	return nil
}

func (s *RealtimeSession) sendSessionUpdate(ctx context.Context, sess SessionConfig) error {
	data, err := encodeSessionUpdate(sess)
	if err != nil {
		return err
	}
	return s.conn.Write(ctx, websocket.MessageText, data)
}

// UpdateSession sends a fresh session.update to reconfigure the live session --
// e.g. flipping turn_detection between the conductor-gated null and the #478
// native semantic_vad when a space's human count crosses 1<->2. It goes through
// the guarded write path so it is safe to call concurrently with the audio-input
// and conductor goroutines, and reuses encodeSessionUpdate so the wire shape is
// identical to the connect-time config.
func (s *RealtimeSession) UpdateSession(sess SessionConfig) error {
	data, err := encodeSessionUpdate(sess)
	if err != nil {
		return err
	}
	return s.write(data)
}

// write sends one client event under the guarded writer (spike section 4: the
// conductor goroutine and the audio-input goroutine both write here).
func (s *RealtimeSession) write(data []byte) error {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return fmt.Errorf("openai realtime: session is closed")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.conn.Write(context.Background(), websocket.MessageText, data)
}

// receiveLoop owns conn.Read. It decodes each frame and hands off: audio frames
// onto the bounded audioOut channel (drop-oldest on overflow so a slow consumer
// can never block the reader), everything else onto the events channel. It
// never runs application work, so a long tool call (handled by the executor off
// this goroutine) cannot stall audio.
func (s *RealtimeSession) receiveLoop(ctx context.Context) {
	defer close(s.audioOut)
	defer close(s.events)

	for {
		select {
		case <-s.done:
			return
		case <-ctx.Done():
			return
		default:
		}

		_, data, err := s.conn.Read(ctx)
		if err != nil {
			select {
			case <-s.done:
				// Expected local close.
			default:
				if s.logger != nil {
					s.logger.Warn("openai realtime: stream closed unexpectedly", "error", err)
				}
			}
			return
		}

		ev := parseServerEvent(data)
		s.dispatch(ev)
	}
}

// dispatch routes one decoded event. Audio deltas go to the bounded audioOut
// channel (drop-oldest under backpressure); all other event kinds go to the
// events channel. Unknown / lifecycle events are logged at the asr.go posture.
func (s *RealtimeSession) dispatch(ev RealtimeServerEvent) {
	switch ev.Kind {
	case EventAudioDelta:
		if len(ev.Audio) == 0 {
			return
		}
		s.pushAudio(ev.Audio)
	case EventError:
		if s.logger != nil {
			s.logger.Error("openai realtime: api error", "message", ev.ErrorMessage)
		}
		s.pushEvent(ev)
	case EventUnknown:
		if s.logger != nil {
			s.logger.Debug("openai realtime: unhandled event", "type", ev.Type)
		}
	case EventSessionLifecycle:
		if s.logger != nil {
			s.logger.Info("openai realtime: event", "type", ev.Type)
		}
		s.pushEvent(ev)
	default:
		s.pushEvent(ev)
	}
}

// pushAudio enqueues a decoded PCM frame, dropping the oldest buffered frame
// rather than blocking the reader when the publisher is behind (bounded buffer
// + drop-oldest discipline, spike section 4).
func (s *RealtimeSession) pushAudio(pcm []byte) {
	for {
		select {
		case s.audioOut <- pcm:
			return
		case <-s.done:
			return
		default:
			// Channel full: drop the oldest frame and retry.
			select {
			case <-s.audioOut:
			default:
			}
		}
	}
}

// pushEvent enqueues a non-audio event, dropping it rather than blocking the
// reader if the consumer is wedged (the reader's liveness is sacrosanct).
func (s *RealtimeSession) pushEvent(ev RealtimeServerEvent) {
	select {
	case s.events <- ev:
	case <-s.done:
	default:
		if s.logger != nil {
			s.logger.Warn("openai realtime: event channel full, dropping", "type", ev.Type)
		}
	}
}
