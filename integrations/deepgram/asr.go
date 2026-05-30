package deepgram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/polyphon"
	"nhooyr.io/websocket"
)

// ASRClient implements polyphon.ASRProvider against Deepgram Nova-3
// streaming transcription. One client is shared across rooms; every
// StartStream opens a fresh WebSocket session.
type ASRClient struct {
	cfg    Config
	logger *slog.Logger
}

// NewASRClient constructs a Deepgram ASR client. APIKey must be set;
// other Config fields fall through to package defaults.
func NewASRClient(cfg Config) (*ASRClient, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	logger := cfg.logger()
	logger.Info("deepgram asr: initialized",
		"model", cfg.ASRModel,
		"language", cfg.Language,
	)
	return &ASRClient{cfg: cfg, logger: logger}, nil
}

// Ping verifies that the configured API key authenticates against
// Deepgram by opening (and immediately closing) a streaming session.
// Used by the startup health check; takes a context for budget control.
func (c *ASRClient) Ping(ctx context.Context) error {
	stream, err := c.StartStream(ctx, polyphon.ASRConfig{SampleRate: asrSampleRate, Language: c.cfg.Language})
	if err != nil {
		return err
	}
	return stream.Close()
}

// Model returns the configured Deepgram ASR model id ("nova-3" by
// default).
func (c *ASRClient) Model() string { return c.cfg.ASRModel }

// Close is a no-op -- per-stream sessions manage their own WebSocket
// lifecycles. Mirrors the openai.ASRClient shape.
func (c *ASRClient) Close() error { return nil }

// StartStream opens a new Nova-3 streaming session and returns a
// polyphon.ASRStream that the bridge agent feeds with PCM16 audio.
// Call Close on the returned stream to terminate the session.
func (c *ASRClient) StartStream(ctx context.Context, config polyphon.ASRConfig) (polyphon.ASRStream, error) {
	streamURL, err := c.cfg.asrStreamURL(config.Language)
	if err != nil {
		return nil, err
	}

	headers := http.Header{}
	headers.Set("Authorization", c.cfg.authHeaderValue())

	conn, _, err := websocket.Dial(ctx, streamURL, &websocket.DialOptions{
		HTTPHeader: headers,
	})
	if err != nil {
		return nil, fmt.Errorf("deepgram asr: connect: %w", err)
	}
	conn.SetReadLimit(512 * 1024)

	s := &deepgramASRStream{
		conn:               conn,
		results:            make(chan polyphon.ASRResult, 16),
		done:               make(chan struct{}),
		logger:             c.logger,
		clientEOUTimeoutMs: c.cfg.ClientEOUTimeoutMs,
	}
	go s.receiveLoop(ctx)
	s.startWatchdog()
	return s, nil
}

// ---------------------------------------------------------------------------
// Stream
// ---------------------------------------------------------------------------

// deepgramASRStream owns one Nova-3 WebSocket session. SendAudio
// writes binary frames; the receive goroutine drains JSON results
// onto the results channel until Close fires.
type deepgramASRStream struct {
	conn   *websocket.Conn
	logger *slog.Logger

	results chan polyphon.ASRResult
	done    chan struct{}

	mu     sync.Mutex
	closed bool

	// firstAudioAt is wall-clock for the first SendAudio after
	// session start (or after the previous final). The receive loop
	// uses it for the per-utterance close-frame logging.
	firstAudioAtMu sync.Mutex
	firstAudioAt   time.Time

	// Accumulator state. Every Results event is treated as an interim
	// update at the polyphon.ASRStream layer; the actual IsFinal=true
	// dispatch fires only on EOU (Deepgram UtteranceEnd OR the
	// client-side watchdog, whichever wins).
	pendingMu sync.Mutex
	// pendingFinalText holds the running transcript of phrases
	// Deepgram has already committed via Results.is_final=true within
	// the current utterance. Cleared on EOU.
	pendingFinalText string
	// lastInterimText holds the most recent uncommitted phrase --
	// fallback when EOU fires before any is_final=true commit (very
	// short utterances Deepgram never finalized).
	lastInterimText string
	// lastChangeAt is the wall-clock when the interim transcript
	// last grew or changed. Driven by handleResults; consumed by the
	// client-EOU watchdog to detect "user is silent, transcript has
	// stabilized."
	lastChangeAt time.Time
	// eouFired guards against double-dispatching the final for one
	// utterance when both the Deepgram UtteranceEnd event and the
	// client watchdog race to trigger EOU.
	eouFired bool

	// clientEOUTimeoutMs (0 = disabled) is the duration of static
	// interim transcript that forces the watchdog to fire EOU. Mirrors
	// Config.ClientEOUTimeoutMs at construction time.
	clientEOUTimeoutMs int
	// watchdog lifecycle channels. watchdogStop is closed on Close()
	// to signal the watchdog goroutine to exit; watchdogDone is closed
	// by the watchdog goroutine when it exits.
	watchdogStop chan struct{}
	watchdogDone chan struct{}
}

// SendAudio writes one PCM16 chunk (16kHz mono LE) to the upstream
// session. Deepgram expects raw bytes on a binary frame; no envelope.
func (s *deepgramASRStream) SendAudio(audio []byte) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("deepgram asr: stream closed")
	}
	s.mu.Unlock()

	if len(audio) == 0 {
		return nil
	}

	s.firstAudioAtMu.Lock()
	if s.firstAudioAt.IsZero() {
		s.firstAudioAt = time.Now()
	}
	s.firstAudioAtMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.conn.Write(ctx, websocket.MessageBinary, audio)
}

// Results returns the channel of ASR results -- closed when the
// session ends.
func (s *deepgramASRStream) Results() <-chan polyphon.ASRResult {
	return s.results
}

// Close politely shuts down the session: sends Deepgram's CloseStream
// JSON control message so the server flushes any in-flight transcript,
// then closes the WebSocket. Idempotent.
func (s *deepgramASRStream) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	// Stop the watchdog before we tear down the connection so it
	// can't fire a spurious EOU after Close.
	s.stopWatchdog()

	// Best-effort CloseStream control message. Deepgram flushes the
	// final transcript on receipt; ignoring the error is fine
	// because we close the WebSocket immediately after either way.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	closeMsg := []byte(`{"type":"CloseStream"}`)
	_ = s.conn.Write(ctx, websocket.MessageText, closeMsg)

	closeErr := s.conn.Close(websocket.StatusNormalClosure, "client closing")
	<-s.done
	return closeErr
}

// startWatchdog spawns the client-side EOU watchdog goroutine. No-op
// when clientEOUTimeoutMs <= 0 (operator opt-out via env).
func (s *deepgramASRStream) startWatchdog() {
	if s.clientEOUTimeoutMs <= 0 {
		return
	}
	s.watchdogStop = make(chan struct{})
	s.watchdogDone = make(chan struct{})
	go s.watchdogLoop()
}

func (s *deepgramASRStream) stopWatchdog() {
	if s.watchdogStop == nil {
		return
	}
	close(s.watchdogStop)
	<-s.watchdogDone
	s.watchdogStop = nil
}

// watchdogLoop ticks every watchdogTickMs and fires EOU when the
// interim transcript has been static for clientEOUTimeoutMs. Handles
// Deepgram's "ambient noise keeps VAD active" failure mode where
// UtteranceEnd never fires after the user clearly stopped speaking.
func (s *deepgramASRStream) watchdogLoop() {
	defer close(s.watchdogDone)

	ticker := time.NewTicker(watchdogTickMs * time.Millisecond)
	defer ticker.Stop()
	timeout := time.Duration(s.clientEOUTimeoutMs) * time.Millisecond

	for {
		select {
		case <-s.watchdogStop:
			return
		case <-ticker.C:
			s.pendingMu.Lock()
			hasContent := s.pendingFinalText != "" || s.lastInterimText != ""
			lastChange := s.lastChangeAt
			alreadyFired := s.eouFired
			s.pendingMu.Unlock()

			if alreadyFired || !hasContent || lastChange.IsZero() {
				continue
			}
			if time.Since(lastChange) < timeout {
				continue
			}

			s.logger.Info("voice trace: deepgram event",
				"stage", "deepgram.client_eou_timeout",
				"staticForMs", time.Since(lastChange).Milliseconds(),
				"timeoutMs", s.clientEOUTimeoutMs,
			)
			s.handleUtteranceEnd()
		}
	}
}

// receiveLoop drains JSON messages from the WebSocket and routes
// transcript events onto the results channel. Exits when the
// connection closes; closes the results channel on exit.
func (s *deepgramASRStream) receiveLoop(ctx context.Context) {
	defer close(s.done)
	defer close(s.results)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		_, raw, err := s.conn.Read(ctx)
		if err != nil {
			// Normal close races with our own Close(); only log
			// when something unexpected happened.
			if !s.isClosed() {
				s.logger.Warn("deepgram asr: read error", "error", err)
			}
			return
		}
		s.handleEvent(raw)
	}
}

func (s *deepgramASRStream) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// deepgramEnvelope carries the discriminator field that every
// Deepgram WebSocket frame includes. We peek at `type` first and then
// unmarshal into the per-event-type struct -- Deepgram's JSON shapes
// diverge across event types (notably `channel` is an object in
// `Results` events but an int array in `UtteranceEnd` events), so a
// single shared struct fails to unmarshal whichever variant it didn't
// model.
type deepgramEnvelope struct {
	Type string `json:"type"`
}

// deepgramEvent is the Results-event payload (the only event type
// whose body we currently care about beyond the discriminator).
type deepgramEvent struct {
	Type    string `json:"type"`
	IsFinal bool   `json:"is_final"`
	Channel struct {
		Alternatives []struct {
			Transcript string  `json:"transcript"`
			Confidence float64 `json:"confidence"`
		} `json:"alternatives"`
	} `json:"channel"`
}

func (s *deepgramASRStream) handleEvent(raw []byte) {
	var env deepgramEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		s.logger.Debug("deepgram asr: ignoring non-json frame", "bytes", len(raw))
		return
	}

	switch env.Type {
	case "Results":
		var evt deepgramEvent
		if err := json.Unmarshal(raw, &evt); err != nil {
			// `channel` is an object on Results; if Deepgram ever
			// reshapes this, fail loud so we notice in logs.
			s.logger.Warn("deepgram asr: parse Results event", "error", err)
			return
		}
		// Diagnostic: log every Results event so we can see exactly
		// when Deepgram is emitting interims vs phrase commits, with
		// what text and confidence. Lets us tell apart "user spoke
		// for 8 seconds with pauses" from "phantom early interim
		// from ambient noise set utteranceStart too soon."
		s.logResultsEvent(evt)
		s.handleResults(evt)
	case "UtteranceEnd":
		// Actual end-of-utterance signal from Deepgram's VAD.
		// THIS is when we surface the accumulated transcript as a
		// downstream "final" -- triggers the bridge to post the
		// utterance to memQL + cognition to route. Drives the
		// utterance_end_ms knob on the WebSocket URL (default 1000ms
		// after speech stops).
		s.logger.Info("voice trace: deepgram event",
			"stage", "deepgram.utterance_end",
		)
		s.handleUtteranceEnd()
	case "SpeechStarted":
		// VAD says voice activity just began. The wall-clock between
		// this and the first non-empty Results event tells us
		// Deepgram's first-partial latency. Surface it as a
		// speech-started ASRResult (additive Kind discriminator, see
		// docs/voice/452-turntaking-orchestration-go.md step 1) so the
		// turn-taking machine can drive human-turn entry + barge-in
		// onset. Transcript-only consumers ignore the kind and skip the
		// empty-text result, so this is backward-compatible.
		s.logger.Info("voice trace: deepgram event",
			"stage", "deepgram.speech_started",
		)
		s.dispatch(polyphon.ASRResult{Kind: polyphon.ASRKindSpeechStarted})
	case "Metadata":
		s.logger.Debug("deepgram asr: vad event", "type", env.Type)
	default:
		s.logger.Debug("deepgram asr: unknown event", "type", env.Type)
	}
}

// logResultsEvent emits one structured log line per Deepgram Results
// frame. Surfaces text + is_final + confidence so the voice-trace
// makefile target lets us reconstruct the full per-utterance event
// timeline (when first interim arrived, when phrases committed,
// confidence distribution, etc.).
//
// Truncates very long transcripts to keep the log line readable. The
// untruncated transcript is what reaches the downstream consumer; this
// is just for the trace.
func (s *deepgramASRStream) logResultsEvent(evt deepgramEvent) {
	if len(evt.Channel.Alternatives) == 0 {
		return
	}
	alt := evt.Channel.Alternatives[0]
	if alt.Transcript == "" {
		return
	}
	stage := "deepgram.results.interim"
	if evt.IsFinal {
		stage = "deepgram.results.final"
	}
	text := alt.Transcript
	const maxLen = 120
	if len(text) > maxLen {
		text = text[:maxLen] + "…"
	}
	s.logger.Info("voice trace: deepgram event",
		"stage", stage,
		"isFinal", evt.IsFinal,
		"confidence", alt.Confidence,
		"chars", len(alt.Transcript),
		"text", text,
	)
}

func (s *deepgramASRStream) handleResults(evt deepgramEvent) {
	if len(evt.Channel.Alternatives) == 0 {
		return
	}
	alt := evt.Channel.Alternatives[0]
	if alt.Transcript == "" {
		return
	}

	// Phrase commits accumulate into pendingFinalText; non-final
	// interims update lastInterimText. Nothing downstream sees
	// IsFinal=true until handleUtteranceEnd fires (either from
	// Deepgram's UtteranceEnd event or from the client watchdog).
	//
	// We update lastChangeAt only when the COMBINED text actually
	// gains new content. Deepgram emits identical keepalive interims
	// every second or so when stuck on ambient noise; those mustn't
	// reset the watchdog's staleness counter.
	s.pendingMu.Lock()
	var combined string
	if evt.IsFinal {
		s.pendingFinalText = joinPhrase(s.pendingFinalText, alt.Transcript)
		s.lastInterimText = ""
		combined = s.pendingFinalText
	} else {
		s.lastInterimText = alt.Transcript
		combined = joinPhrase(s.pendingFinalText, alt.Transcript)
	}
	// Only reset the staleness timer when the running transcript
	// actually moved. Re-emission of the same text by Deepgram's
	// keepalive doesn't count as user activity.
	if !s.eouFired {
		s.lastChangeAt = time.Now()
	}
	s.pendingMu.Unlock()

	s.dispatch(polyphon.ASRResult{
		Text:       combined,
		IsFinal:    false,
		Confidence: alt.Confidence,
	})
}

// handleUtteranceEnd flushes the accumulated transcript as IsFinal=true
// and resets per-utterance state. Called from two paths:
//
//   - Deepgram's UtteranceEnd event (VAD-driven, fires after
//     utterance_end_ms silence).
//   - The client-EOU watchdog when interim transcript stays static
//     for clientEOUTimeoutMs (handles Deepgram VAD-hang failures).
//
// Idempotent: eouFired guards against the two paths racing each other
// and emitting two finals for one utterance.
func (s *deepgramASRStream) handleUtteranceEnd() {
	s.pendingMu.Lock()
	if s.eouFired {
		s.pendingMu.Unlock()
		return
	}
	// Use the committed text when present; fall back to the most
	// recent interim when EOU fires before any phrase committed
	// (very short or noisy utterance).
	text := s.pendingFinalText
	if text == "" {
		text = s.lastInterimText
	}
	if text == "" {
		// Empty utterance (silence-only VAD event). Don't fire EOU;
		// leave eouFired false so the next real utterance starts
		// fresh.
		s.pendingMu.Unlock()
		return
	}
	// Snapshot + reset under the lock so a concurrent watchdog tick
	// can't race the Deepgram-driven path.
	s.eouFired = true
	s.pendingFinalText = ""
	s.lastInterimText = ""
	s.lastChangeAt = time.Time{}
	s.pendingMu.Unlock()

	// Reset per-utterance first-audio marker so the harness
	// instrumentation measures the next utterance's first-partial
	// latency from its own first audio frame.
	s.firstAudioAtMu.Lock()
	s.firstAudioAt = time.Time{}
	s.firstAudioAtMu.Unlock()

	s.dispatch(polyphon.ASRResult{
		Text:    text,
		IsFinal: true,
	})

	// Clear eouFired AFTER dispatch so the watchdog can detect a
	// fresh utterance starting (lastChangeAt will set again on the
	// next handleResults call).
	s.pendingMu.Lock()
	s.eouFired = false
	s.pendingMu.Unlock()
}

func (s *deepgramASRStream) dispatch(r polyphon.ASRResult) {
	select {
	case s.results <- r:
	default:
		// Drop-oldest behavior is not allowed by polyphon.ASRStream
		// (the consumer expects every result). If this fires, the
		// downstream consumer is too slow -- log and back off.
		s.logger.Warn("deepgram asr: result channel full; dropping",
			"isFinal", r.IsFinal,
			"chars", len(r.Text),
		)
	}
}

// joinPhrase concatenates two text segments with a single space,
// tolerating empty inputs on either side.
func joinPhrase(left, right string) string {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	switch {
	case left == "":
		return right
	case right == "":
		return left
	default:
		return left + " " + right
	}
}
