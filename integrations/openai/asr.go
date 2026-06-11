package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/znasllc-io/memql/component/polyphon"
	"github.com/znasllc-io/memql/integrations/audio"
	"nhooyr.io/websocket"
)

const realtimeBaseURL = "wss://api.openai.com/v1/realtime"

// ASRClient implements polyphon.ASRProvider using the OpenAI Realtime API
// in transcription-only mode (type: "transcription"). This provides streaming
// speech-to-text without bundling an LLM -- the fastest streaming transcription
// available from OpenAI.
type ASRClient struct {
	apiKey string
	model  string
	logger *slog.Logger
}

// NewASRClient creates an OpenAI ASR client.
func NewASRClient(cfg Config) (*ASRClient, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	logger := cfg.logger()
	logger.Info("openai asr: initialized", "model", cfg.ASRModel)

	return &ASRClient{
		apiKey: cfg.APIKey,
		model:  cfg.ASRModel,
		logger: logger,
	}, nil
}

// StartStream begins a streaming transcription session via the OpenAI Realtime
// API WebSocket with type: "transcription".
//
// URL: wss://api.openai.com/v1/realtime?intent=transcription
//
// The ?intent=transcription form is required for transcription-only mode.
// The ?model= form is for realtime *conversation* (and expects an LLM model
// like gpt-4o-realtime-preview). Passing a transcription-only model to the
// ?model= form causes OpenAI to accept the WS upgrade and then immediately
// close the connection -- which surfaces downstream as "use of closed
// network connection" on the next SendAudio write.
//
// The transcription model itself is set inside session.update via
// input_audio_transcription.model (see sendSessionConfig).
//
// Audio from the Polyphon pipeline arrives as PCM16 16kHz mono. This client
// upsamples to 24kHz (OpenAI's native rate), base64-encodes each chunk, and
// sends it as an input_audio_buffer.append message.
//
// Transcription results arrive as conversation.item.input_audio_transcription
// delta (interim) and completed (final) events, mapped to polyphon.ASRResult.
func (c *ASRClient) StartStream(ctx context.Context, config polyphon.ASRConfig) (polyphon.ASRStream, error) {
	url := realtimeBaseURL + "?intent=transcription"

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+c.apiKey)
	headers.Set("OpenAI-Beta", "realtime=v1")

	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: headers,
	})
	if err != nil {
		return nil, fmt.Errorf("openai asr: connect: %w", err)
	}

	// Increase read limit for transcription responses.
	conn.SetReadLimit(512 * 1024) // 512KB

	// Create upsampler: Polyphon 16kHz -> OpenAI 24kHz.
	upsampler, err := audio.NewPCM16Resampler(audio.PolyphonSampleRate, audio.OpenAISampleRate)
	if err != nil {
		conn.Close(websocket.StatusInternalError, "resampler init failed")
		return nil, fmt.Errorf("openai asr: create upsampler: %w", err)
	}

	s := &openaiASRStream{
		conn:      conn,
		upsampler: upsampler,
		model:     c.model,
		results:   make(chan polyphon.ASRResult, 16),
		done:      make(chan struct{}),
		logger:    c.logger,
	}

	// Configure session for transcription-only mode.
	if err := s.sendSessionConfig(ctx, config); err != nil {
		conn.Close(websocket.StatusInternalError, "config failed")
		return nil, fmt.Errorf("openai asr: configure session: %w", err)
	}

	// Start goroutine to receive transcription results.
	go s.receiveLoop(ctx)

	return s, nil
}

// Model returns the configured ASR model name.
func (c *ASRClient) Model() string {
	return c.model
}

// Close is a no-op; individual streams manage their own connections.
func (c *ASRClient) Close() error {
	return nil
}

// openaiASRStream wraps an OpenAI Realtime API WebSocket transcription session.
type openaiASRStream struct {
	conn      *websocket.Conn
	upsampler *audio.PCM16Resampler
	model     string
	results   chan polyphon.ASRResult
	done      chan struct{}
	logger    *slog.Logger
	once      sync.Once
	mu        sync.Mutex
	closed    bool

	// interimBuf accumulates token-level deltas for the in-progress
	// utterance. The Realtime API's .delta events carry only the newest
	// token (e.g., " sofia"), not the full running transcript -- but our
	// downstream contract is that each interim ASRResult.Text carries the
	// FULL interim transcript so far, so the UI can render it with a
	// simple "replace interimText" rule.
	// Reset on .completed (end of utterance under server_vad).
	interimBuf string
}

// SendAudio sends a chunk of PCM16 16kHz audio to OpenAI for transcription.
// The audio is upsampled to 24kHz and base64-encoded before sending.
func (s *openaiASRStream) SendAudio(audioData []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return fmt.Errorf("stream is closed")
	}

	// Upsample 16kHz -> 24kHz.
	upsampled := s.upsampler.Resample(audioData)
	if len(upsampled) == 0 {
		return nil
	}

	// Base64 encode and send as input_audio_buffer.append.
	msg := map[string]any{
		"type":  "input_audio_buffer.append",
		"audio": base64.StdEncoding.EncodeToString(upsampled),
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal audio message: %w", err)
	}

	ctx := context.Background()
	return s.conn.Write(ctx, websocket.MessageText, data)
}

// Results returns the channel of transcription results.
func (s *openaiASRStream) Results() <-chan polyphon.ASRResult {
	return s.results
}

// Close terminates the ASR stream.
func (s *openaiASRStream) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		close(s.done)
		s.conn.Close(websocket.StatusNormalClosure, "stream closed")
	})
	return nil
}

// sendSessionConfig sends the initial session configuration for transcription-only mode.
//
// Transcription-only sessions use a distinct event type and schema from the
// realtime-conversation sessions. The correct event type for this mode is
// "transcription_session.update" (NOT "session.update"), and the inner session
// object does not carry a "type" field -- the intent is already encoded on
// the WebSocket URL via ?intent=transcription.
//
// Sending "session.update" with "session.type": "transcription" results in
// OpenAI rejecting the config with:
//
//	{"type":"error","error":{"message":"Unknown parameter: 'session.type'."}}
//
// Audio format: the client upsamples to 24kHz PCM16 before send (see
// SendAudio), matching input_audio_format: "pcm16".
//
// Model selection: input_audio_transcription.model carries the transcription
// model (whisper-1, gpt-4o-transcribe, gpt-4o-mini-transcribe). Override via
// MEMQL_OPENAI_REALTIME_MODEL on the voice node or POLYPHON_OPENAI_ASR_MODEL
// on the bridge agent.
func (s *openaiASRStream) sendSessionConfig(ctx context.Context, config polyphon.ASRConfig) error {
	lang := config.Language
	if lang == "" {
		lang = "en"
	}

	// silence_duration_ms is the trailing-silence window the server-side
	// VAD requires before declaring end-of-utterance. 500ms is too
	// aggressive for natural speech with mid-sentence pauses ("just
	// wanna see how... [breath] how it works") -- the VAD splits the
	// thought into two utterances. 800ms was overly conservative and
	// added perceptible end-of-turn lag. 600ms is the sweet spot --
	// long enough for normal conversational pauses (especially the
	// "...uh..." filler), short enough that the user doesn't feel
	// the agent is stalling at end-of-utterance. Operators tune via
	// POLYPHON_OPENAI_VAD_SILENCE_MS.
	silenceMs := 600
	if v := strings.TrimSpace(os.Getenv("POLYPHON_OPENAI_VAD_SILENCE_MS")); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			silenceMs = parsed
		}
	}

	msg := map[string]any{
		"type": "transcription_session.update",
		"session": map[string]any{
			"input_audio_format": "pcm16",
			"input_audio_transcription": map[string]any{
				"model":    s.model,
				"language": lang,
			},
			"turn_detection": map[string]any{
				"type":                "server_vad",
				"threshold":           0.5,
				"prefix_padding_ms":   300,
				"silence_duration_ms": silenceMs,
			},
		},
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	return s.conn.Write(ctx, websocket.MessageText, data)
}

// receiveLoop reads transcription events from the OpenAI Realtime API
// and forwards them to the results channel.
func (s *openaiASRStream) receiveLoop(ctx context.Context) {
	defer close(s.results)

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
				// Expected close (local Close() was called), don't log.
			default:
				// Unexpected close -- usually the server rejected the
				// session (bad model, auth scope, rate limit, etc). The
				// OpenAI error reason is carried on the close frame and
				// surfaces here in err.Error(). Log at warn so operators
				// can diagnose without flipping debug.
				s.logger.Warn("openai asr: stream closed unexpectedly", "error", err)
			}
			return
		}

		s.handleEvent(data)
	}
}

// handleEvent parses a Realtime API event and emits ASRResults for transcription events.
func (s *openaiASRStream) handleEvent(data []byte) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		s.logger.Debug("openai asr: failed to parse event", "error", err)
		return
	}

	eventType, _ := raw["type"].(string)

	// Lifecycle visibility -- surface everything except the chatty delta
	// stream at INFO so we can see the shape of a session (session.created,
	// transcription_session.created/.updated, input_audio_buffer.*, etc)
	// without flipping debug mode. This is how we catch silent issues like
	// "session update accepted but buffer committed with no transcript".
	if eventType != "conversation.item.input_audio_transcription.delta" {
		s.logger.Info("openai asr: event", "type", eventType)
	}

	switch eventType {
	case "input_audio_buffer.speech_started":
		// Server-VAD voice-activity onset. Surfaced as a text-less
		// ASRKindSpeechStarted result -- the turn-taking machine uses it
		// to enter human-turn and raise a barge-in candidate while the
		// assistant has the floor (the onset contract the cascade's
		// turn-taking machine consumes, see turntaking.go).
		s.results <- polyphon.ASRResult{Kind: polyphon.ASRKindSpeechStarted}

	case "conversation.item.input_audio_transcription.delta":
		// Interim transcription result -- append the token to the running
		// buffer and emit the full accumulated text. Matches the
		// downstream contract documented on interimBuf.
		delta, _ := raw["delta"].(string)
		if delta == "" {
			return
		}
		s.mu.Lock()
		s.interimBuf += delta
		text := s.interimBuf
		s.mu.Unlock()
		s.results <- polyphon.ASRResult{
			Text:       text,
			IsFinal:    false,
			Confidence: 0.0, // Interim results don't include confidence.
		}

	case "conversation.item.input_audio_transcription.completed":
		// Final transcription result. Reset the interim accumulator so
		// the next utterance starts clean. OpenAI may also return a
		// slightly normalized / punctuation-corrected transcript here
		// vs the raw delta stream -- we trust this one.
		transcript, _ := raw["transcript"].(string)
		s.mu.Lock()
		s.interimBuf = ""
		s.mu.Unlock()
		if transcript != "" {
			s.results <- polyphon.ASRResult{
				Text:       transcript,
				IsFinal:    true,
				Confidence: 1.0, // Final results are high-confidence.
			}
		}

	case "conversation.item.input_audio_transcription.failed":
		// Per-utterance transcription failure. Carries an error object
		// like {type, code, message}. Until we surface this to the
		// caller, at least log the reason so it's diagnosable.
		if errObj, ok := raw["error"].(map[string]any); ok {
			msg, _ := errObj["message"].(string)
			code, _ := errObj["code"].(string)
			etype, _ := errObj["type"].(string)
			s.logger.Error("openai asr: transcription failed",
				"message", msg, "code", code, "type", etype)
		} else {
			s.logger.Error("openai asr: transcription failed (no error body)")
		}
		// Reset the interim buffer so the next utterance starts clean.
		s.mu.Lock()
		s.interimBuf = ""
		s.mu.Unlock()

	case "error":
		if errObj, ok := raw["error"].(map[string]any); ok {
			msg, _ := errObj["message"].(string)
			s.logger.Error("openai asr: api error", "message", msg)
		}
	}
}
