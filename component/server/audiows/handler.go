// Package audiows provides a WebSocket handler for real-time audio streaming
// and speech-to-text transcription in MemQL spaces.
package audiows

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/env"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/core/logger"
	"github.com/znasllc-io/memql/integrations/stt"
	"nhooyr.io/websocket"
)

// ComponentName identifies this package in logs and environment variable lookups.
const ComponentName = common.ComponentName("audioWebSocket")

const (
	defaultWriteTimeout    = 10 * time.Second
	defaultMaxMessageBytes = 1 * 1024 * 1024 // 1 MiB for audio chunks
	defaultPingInterval    = 30 * time.Second

	// TTS defaults
	defaultTTSVoice      = "nova"
	defaultTTSFormat     = "wav" // WAV for reliable progressive browser decode (each chunk is complete WAV file)
	defaultTTSSampleRate = 24000
)

// NewLogger creates a logger for the audio WebSocket handler.
func NewLogger() *slog.Logger {
	return logger.New(ComponentName, os.Stdout, resolveLoggerLevel())
}

func resolveLoggerLevel() slog.Level {
	reader := env.NewEnvReader("AUDIO_WEBSOCKET")
	for _, key := range []string{"CAPABILITIES_LOGGING_LOG_LEVEL", "LOGGER_LEVEL", "LOG_LEVEL"} {
		if value, ok := reader.String(key); ok {
			if strings.TrimSpace(value) == "" {
				continue
			}
			var level slog.Level
			if err := level.UnmarshalText([]byte(strings.ToLower(value))); err == nil {
				return level
			}
		}
	}
	return slog.LevelInfo
}

// MemQLExecutor provides query and mutation capabilities.
// This interface is satisfied by *memql.MemoryEngine.
type MemQLExecutor interface {
	ExecuteInsert(ctx context.Context, query string) error
}

// Options configure the audio WebSocket handler.
type Options struct {
	Logger          *slog.Logger
	STTProvider     stt.StreamingProvider
	TTSProvider     memql.TTSSIProvider
	Engine          MemQLExecutor
	WriteTimeout    time.Duration
	MaxMessageBytes int64
	PingInterval    time.Duration
}

// Handler handles audio WebSocket connections for real-time transcription and TTS.
type Handler struct {
	logger          *slog.Logger
	sttProvider     stt.StreamingProvider
	ttsProvider     memql.TTSSIProvider
	engine          MemQLExecutor
	writeTimeout    time.Duration
	maxMessageBytes int64
	pingInterval    time.Duration
}

// New constructs an audio WebSocket Handler.
func New(opts Options) (*Handler, error) {
	if opts.STTProvider == nil {
		return nil, fmt.Errorf("audio websocket handler requires an STT provider")
	}

	logger := opts.Logger
	if logger == nil {
		logger = NewLogger()
	}

	// TTS provider is optional - Read Aloud won't work without it
	if opts.TTSProvider == nil {
		logger.Warn("TTS provider not configured - Read Aloud feature will be unavailable")
	}

	return &Handler{
		logger:          logger,
		sttProvider:     opts.STTProvider,
		ttsProvider:     opts.TTSProvider,
		engine:          opts.Engine,
		writeTimeout:    firstDuration(opts.WriteTimeout, defaultWriteTimeout),
		maxMessageBytes: firstPositiveInt64(opts.MaxMessageBytes, defaultMaxMessageBytes),
		pingInterval:    firstDuration(opts.PingInterval, defaultPingInterval),
	}, nil
}

// ServeHTTP upgrades the connection and handles audio streaming.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r == nil || r.Method != http.MethodGet {
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}

	identity, err := auth.UserIdentityFromContext(r.Context())
	if err != nil {
		h.logger.Warn("audio websocket identity missing", "remote", r.RemoteAddr)
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return
	}
	// Allow any authenticated user (role check removed to match /memql/ws behavior)

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
		OriginPatterns:  []string{"*"},
	})
	if err != nil {
		h.logger.Error("audio websocket accept failed", "error", err)
		return
	}
	defer conn.Close(websocket.StatusInternalError, "unexpected error")

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	h.logger.Info("audio websocket connection accepted",
		"subject", identity.Subject,
		"remote", r.RemoteAddr,
	)

	conn.SetReadLimit(h.maxMessageBytes)

	session := &audioSession{
		ctx:          ctx,
		conn:         conn,
		handler:      h,
		identity:     identity,
		writeTimeout: h.writeTimeout,
		streams:      make(map[string]*streamContext),
	}

	if err := session.run(); err != nil && !errors.Is(err, context.Canceled) {
		h.logger.Warn("audio websocket session ended",
			"subject", identity.Subject,
			"error", err,
		)
	} else {
		h.logger.Info("audio websocket session closed",
			"subject", identity.Subject,
		)
	}
}

// audioSession represents an active audio WebSocket connection.
type audioSession struct {
	ctx          context.Context
	conn         *websocket.Conn
	handler      *Handler
	identity     auth.UserIdentity
	writeTimeout time.Duration

	streamsMu sync.Mutex
	streams   map[string]*streamContext
}

// streamContext represents an active audio stream within a session.
type streamContext struct {
	streamId      string
	spaceId       string
	participantId string
	sttSession    stt.StreamingSession
	startTime     time.Time
}

// run processes messages until the connection closes.
func (s *audioSession) run() error {
	errCh := make(chan error, 2)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.handler.logger.Error("panic in audio readLoop recovered",
					"panic", r,
					"subject", s.identity.Subject,
				)
				errCh <- fmt.Errorf("panic recovered: %v", r)
			}
		}()
		errCh <- s.readLoop()
	}()

	go func() {
		errCh <- s.pingLoop()
	}()

	err := <-errCh
	s.cleanup()
	s.conn.Close(websocket.StatusNormalClosure, "")
	return err
}

// cleanup closes all active streams.
func (s *audioSession) cleanup() {
	s.streamsMu.Lock()
	defer s.streamsMu.Unlock()

	for _, sc := range s.streams {
		if sc.sttSession != nil {
			sc.sttSession.Close()
		}
	}
	s.streams = make(map[string]*streamContext)
}

// readLoop reads and processes incoming messages.
func (s *audioSession) readLoop() error {
	for {
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		default:
		}

		msgType, data, err := s.conn.Read(s.ctx)
		if err != nil {
			return err
		}

		// Handle both text (JSON) and binary messages
		switch msgType {
		case websocket.MessageText:
			if err := s.handleTextMessage(data); err != nil {
				s.handler.logger.Warn("failed to handle message", "error", err)
				// Send error response but don't close connection
				s.sendError("", "message_error", err.Error())
			}
		case websocket.MessageBinary:
			// Binary messages should be raw audio for the current stream
			// For now, we expect audio to be base64-encoded in JSON
			s.handler.logger.Debug("received binary message, ignoring (use JSON)")
		}
	}
}

// handleTextMessage processes a JSON message.
func (s *audioSession) handleTextMessage(data []byte) error {
	var msg AudioMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}

	switch msg.Type {
	case "start":
		return s.handleStart(&msg)
	case "chunk":
		return s.handleChunk(&msg)
	case "end":
		return s.handleEnd(&msg)
	case "synthesize", "tts_synthesize":
		return s.handleSynthesize(data)
	default:
		return fmt.Errorf("unknown message type: %s", msg.Type)
	}
}

// handleStart initiates a new audio stream.
func (s *audioSession) handleStart(msg *AudioMessage) error {
	streamId := msg.StreamId
	if streamId == "" {
		streamId = id.NewShortId()
	}

	s.streamsMu.Lock()
	if _, exists := s.streams[streamId]; exists {
		s.streamsMu.Unlock()
		return fmt.Errorf("stream %s already exists", streamId)
	}
	s.streamsMu.Unlock()

	// Validate required fields
	if msg.SpaceId == "" {
		return fmt.Errorf("spaceId is required")
	}
	if msg.ParticipantId == "" {
		return fmt.Errorf("participantId is required")
	}

	// Start STT session
	config := stt.StreamConfig{
		Format:       firstString(msg.Format, "pcm16"),
		SampleRate:   firstInt(msg.SampleRate, 16000),
		Channels:     firstInt(msg.Channels, 1),
		LanguageHint: msg.LanguageHint,
	}

	sttSession, err := s.handler.sttProvider.StartStream(s.ctx, config)
	if err != nil {
		return fmt.Errorf("failed to start STT stream: %w", err)
	}

	sc := &streamContext{
		streamId:      streamId,
		spaceId:       msg.SpaceId,
		participantId: msg.ParticipantId,
		sttSession:    sttSession,
		startTime:     time.Now(),
	}

	s.streamsMu.Lock()
	s.streams[streamId] = sc
	s.streamsMu.Unlock()

	// Start goroutine to forward transcription results
	go s.forwardTranscriptions(sc)

	s.handler.logger.Info("audio stream started",
		"streamId", streamId,
		"spaceId", msg.SpaceId,
		"participantId", msg.ParticipantId,
		"format", config.Format,
		"sampleRate", config.SampleRate,
	)

	// Send acknowledgment
	return s.sendMessage(&AudioResponse{
		Type:     "started",
		StreamId: streamId,
	})
}

// handleChunk processes an audio chunk.
func (s *audioSession) handleChunk(msg *AudioMessage) error {
	if msg.StreamId == "" {
		return fmt.Errorf("streamId is required")
	}

	s.streamsMu.Lock()
	sc, exists := s.streams[msg.StreamId]
	s.streamsMu.Unlock()

	if !exists {
		return fmt.Errorf("stream %s not found", msg.StreamId)
	}

	// Decode base64 audio
	audio, err := base64.StdEncoding.DecodeString(msg.Audio)
	if err != nil {
		return fmt.Errorf("invalid base64 audio: %w", err)
	}

	// Forward to STT provider
	if err := sc.sttSession.SendAudio(audio); err != nil {
		return fmt.Errorf("failed to send audio: %w", err)
	}

	return nil
}

// handleEnd finalizes an audio stream.
func (s *audioSession) handleEnd(msg *AudioMessage) error {
	if msg.StreamId == "" {
		return fmt.Errorf("streamId is required")
	}

	s.streamsMu.Lock()
	sc, exists := s.streams[msg.StreamId]
	if exists {
		delete(s.streams, msg.StreamId)
	}
	s.streamsMu.Unlock()

	if !exists {
		return fmt.Errorf("stream %s not found", msg.StreamId)
	}

	if msg.Cancelled {
		// User cancelled - just close the session
		sc.sttSession.Close()
		s.handler.logger.Info("audio stream cancelled", "streamId", msg.StreamId)
		return s.sendMessage(&AudioResponse{
			Type:      "ended",
			StreamId:  msg.StreamId,
			Cancelled: true,
		})
	}

	// Finalize and get transcript
	final, err := sc.sttSession.Finalize(s.ctx)
	if err != nil {
		return fmt.Errorf("failed to finalize stream: %w", err)
	}

	s.handler.logger.Info("audio stream finalized",
		"streamId", msg.StreamId,
		"text", truncate(final.Text, 50),
		"durationMs", final.DurationMS,
	)

	// Insert utterance if we have text
	var utteranceId string
	if strings.TrimSpace(final.Text) != "" && s.handler.engine != nil {
		utteranceId, err = s.insertUtterance(sc, final)
		if err != nil {
			s.handler.logger.Error("failed to insert utterance",
				"streamId", msg.StreamId,
				"error", err,
			)
			// Continue anyway - send the transcription result
		} else {
			s.handler.logger.Info("utterance created",
				"streamId", msg.StreamId,
				"utteranceId", utteranceId,
			)
		}
	}

	// Send final transcription
	return s.sendMessage(&AudioResponse{
		Type:        "transcription",
		StreamId:    msg.StreamId,
		Text:        final.Text,
		IsFinal:     true,
		Confidence:  final.Confidence,
		Words:       final.Words,
		UtteranceId: utteranceId,
		DurationMS:  final.DurationMS,
	})
}

// forwardTranscriptions forwards interim transcription results to the client.
func (s *audioSession) forwardTranscriptions(sc *streamContext) {
	for result := range sc.sttSession.Receive() {
		// Only forward interim results (final is sent in handleEnd)
		if !result.IsFinal {
			s.sendMessage(&AudioResponse{
				Type:       "transcription",
				StreamId:   sc.streamId,
				Text:       result.Text,
				IsFinal:    false,
				Confidence: result.Confidence,
			})
		}
	}
}

// handleSynthesize processes a TTS synthesis request.
func (s *audioSession) handleSynthesize(data []byte) error {
	var msg SynthesizeMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return fmt.Errorf("invalid synthesize JSON: %w", err)
	}

	// Validate TTS provider is available
	if s.handler.ttsProvider == nil {
		return fmt.Errorf("TTS not available: provider not configured")
	}

	// Validate required fields
	if msg.Text == "" {
		return fmt.Errorf("text is required for synthesis")
	}
	if msg.RequestId == "" {
		return fmt.Errorf("requestId is required for synthesis")
	}

	// Use defaults if not provided
	voice := firstString(msg.Voice, defaultTTSVoice)
	format := firstString(msg.Format, defaultTTSFormat)
	sampleRate := firstInt(msg.SampleRate, defaultTTSSampleRate)

	s.handler.logger.Info("TTS synthesis requested",
		"requestId", msg.RequestId,
		"voice", voice,
		"format", format,
		"textLen", len(msg.Text),
	)

	// Start streaming synthesis in background
	go s.streamTTS(msg.RequestId, msg.SpaceId, msg.ParticipantId, msg.Text, voice, format, sampleRate)

	return nil
}

// streamTTS performs TTS synthesis and streams audio chunks to the client.
func (s *audioSession) streamTTS(requestId, spaceId, participantId, text, voice, format string, sampleRate int) {
	// Send tts_started message
	if err := s.sendTTSStarted(&TTSStartedMessage{
		Type:          "tts_started",
		RequestId:     requestId,
		Format:        format,
		SampleRate:    sampleRate,
		SpaceId:       spaceId,
		ParticipantId: participantId,
		Text:          text,
	}); err != nil {
		s.handler.logger.Error("failed to send TTS started",
			"requestId", requestId,
			"error", err,
		)
		return
	}

	chunkCh, err := s.handler.ttsProvider.SynthesizeStream(s.ctx, text, voice)
	if err != nil {
		s.handler.logger.Error("TTS synthesis stream error",
			"requestId", requestId,
			"error", err,
		)
		s.sendTTSEnded(&TTSEndedMessage{
			Type:          "tts_ended",
			RequestId:     requestId,
			SpaceId:       spaceId,
			ParticipantId: participantId,
			Error:         err.Error(),
		})
		return
	}

	for chunk := range chunkCh {
		if chunk.Error != nil {
			s.handler.logger.Error("TTS synthesis error",
				"requestId", requestId,
				"error", chunk.Error,
			)
			s.sendTTSEnded(&TTSEndedMessage{
				Type:          "tts_ended",
				RequestId:     requestId,
				SpaceId:       spaceId,
				ParticipantId: participantId,
				Error:         chunk.Error.Error(),
			})
			return
		}

		// Send TTS chunk to client
		if err := s.sendTTSChunk(&TTSChunkMessage{
			Type:          "tts_chunk",
			RequestId:     requestId,
			Audio:         base64.StdEncoding.EncodeToString(chunk.Audio),
			Format:        format,
			SampleRate:    sampleRate,
			Sequence:      chunk.Sequence,
			Done:          chunk.Done,
			SpaceId:       spaceId,
			ParticipantId: participantId,
			Text:          text,
		}); err != nil {
			s.handler.logger.Error("failed to send TTS chunk",
				"requestId", requestId,
				"sequence", chunk.Sequence,
				"error", err,
			)
			s.sendTTSEnded(&TTSEndedMessage{
				Type:          "tts_ended",
				RequestId:     requestId,
				SpaceId:       spaceId,
				ParticipantId: participantId,
				Error:         err.Error(),
			})
			return
		}
	}

	// Send tts_ended message on successful completion
	s.sendTTSEnded(&TTSEndedMessage{
		Type:          "tts_ended",
		RequestId:     requestId,
		SpaceId:       spaceId,
		ParticipantId: participantId,
	})

	s.handler.logger.Debug("TTS stream complete", "requestId", requestId)
}

// sendTTSStarted sends a TTS started message to the client.
func (s *audioSession) sendTTSStarted(msg *TTSStartedMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(s.ctx, s.writeTimeout)
	defer cancel()

	return s.conn.Write(ctx, websocket.MessageText, data)
}

// sendTTSChunk sends a TTS audio chunk to the client.
func (s *audioSession) sendTTSChunk(msg *TTSChunkMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(s.ctx, s.writeTimeout)
	defer cancel()

	return s.conn.Write(ctx, websocket.MessageText, data)
}

// sendTTSEnded sends a TTS ended message to the client.
func (s *audioSession) sendTTSEnded(msg *TTSEndedMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(s.ctx, s.writeTimeout)
	defer cancel()

	return s.conn.Write(ctx, websocket.MessageText, data)
}

// SynthesizeAuto generates and streams TTS audio for server-initiated responses.
// This is called by the Turn State engine or AI responder when auto-TTS is needed.
func (s *audioSession) SynthesizeAuto(spaceId, participantId, text, voice string) error {
	if s.handler.ttsProvider == nil {
		return fmt.Errorf("TTS not available: provider not configured")
	}

	requestId := id.NewShortId()

	// Use defaults if not provided
	if voice == "" {
		voice = defaultTTSVoice
	}

	s.handler.logger.Info("Auto-TTS synthesis initiated",
		"requestId", requestId,
		"spaceId", spaceId,
		"participantId", participantId,
		"voice", voice,
		"textLen", len(text),
	)

	// Stream in background
	go s.streamTTS(requestId, spaceId, participantId, text, voice, defaultTTSFormat, defaultTTSSampleRate)

	return nil
}

// insertUtterance creates a v1:cognition:utterance record.
func (s *audioSession) insertUtterance(sc *streamContext, final *stt.FinalTranscription) (string, error) {
	utteranceId := fmt.Sprintf("utt-voice-%d", time.Now().UnixNano())

	// Build timestamps JSON
	timestampsJSON := "null"
	if len(final.Words) > 0 {
		wordsData, err := json.Marshal(map[string]any{
			"words": final.Words,
		})
		if err == nil {
			timestampsJSON = string(wordsData)
		}
	}

	// Escape the text for JSON
	textJSON, err := json.Marshal(final.Text)
	if err != nil {
		return "", fmt.Errorf("failed to marshal text: %w", err)
	}

	query := fmt.Sprintf(`insert("%s", id="%s", payload={
		"spaceId": "%s",
		"participantId": "%s",
		"participantType": "human",
		"utteranceType": "speech",
		"text": %s,
		"duration": %d,
		"timestamps": %s,
		"source": {
			"inputMethod": "stt",
			"outputMethod": "text",
			"sttProvider": "%s"
		}
	})`,
		memoryNodes.ConceptCognitionUtterance,
		utteranceId,
		sc.spaceId,
		sc.participantId,
		string(textJSON),
		final.DurationMS,
		timestampsJSON,
		final.Provider,
	)

	err = s.handler.engine.ExecuteInsert(s.ctx, query)
	if err != nil {
		return "", err
	}

	return utteranceId, nil
}

// sendMessage sends a JSON message to the client.
func (s *audioSession) sendMessage(msg *AudioResponse) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(s.ctx, s.writeTimeout)
	defer cancel()

	return s.conn.Write(ctx, websocket.MessageText, data)
}

// sendError sends an error message to the client.
func (s *audioSession) sendError(streamId, code, message string) error {
	return s.sendMessage(&AudioResponse{
		Type:     "error",
		StreamId: streamId,
		Error: &AudioError{
			Code:    code,
			Message: message,
		},
	})
}

// pingLoop sends periodic pings to keep the connection alive.
func (s *audioSession) pingLoop() error {
	if s.handler.pingInterval <= 0 {
		<-s.ctx.Done()
		return s.ctx.Err()
	}

	ticker := time.NewTicker(s.handler.pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
			err := s.conn.Ping(ctx)
			cancel()
			if err != nil {
				return fmt.Errorf("ping failed: %w", err)
			}
		}
	}
}

// Helper functions

func firstDuration(input, fallback time.Duration) time.Duration {
	if input <= 0 {
		return fallback
	}
	return input
}

func firstPositiveInt64(input, fallback int64) int64 {
	if input <= 0 {
		return fallback
	}
	return input
}

func firstString(input, fallback string) string {
	if input == "" {
		return fallback
	}
	return input
}

func firstInt(input, fallback int) int {
	if input <= 0 {
		return fallback
	}
	return input
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
