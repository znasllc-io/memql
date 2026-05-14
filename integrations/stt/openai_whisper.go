package stt

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sashabaranov/go-openai"
)

// projectHeaderClient wraps an HTTP client to inject the OpenAI-Project header.
type projectHeaderClient struct {
	inner     openai.HTTPDoer
	projectId string
}

func (c *projectHeaderClient) Do(req *http.Request) (*http.Response, error) {
	req.Header.Set("OpenAI-Project", c.projectId)
	return c.inner.Do(req)
}

const (
	// defaultWhisperModel is the OpenAI transcription model used when
	// MEMQL_WHISPER_MODEL is unset. We default to "whisper-1" because
	// "gpt-4o-transcribe" paraphrases longer audio into summaries
	// instead of transcribing verbatim and is more aggressive about
	// pruning low-confidence leading syllables (causing first words to
	// be dropped on short utterances). whisper-1 is older but
	// transcribes verbatim -- the correct behavior for a "capture what
	// the user said" use case. Override via MEMQL_WHISPER_MODEL to A/B
	// test or opt into gpt-4o-transcribe for specific deployments.
	defaultWhisperModel = "whisper-1"
)

// resolveWhisperModel returns the OpenAI transcription model to use.
// Honors MEMQL_WHISPER_MODEL when set (trimmed, case-preserved);
// otherwise returns defaultWhisperModel.
func resolveWhisperModel() string {
	if m := strings.TrimSpace(os.Getenv("MEMQL_WHISPER_MODEL")); m != "" {
		return m
	}
	return defaultWhisperModel
}

// OpenAIWhisperProvider implements StreamingProvider using OpenAI's Whisper API.
// Note: Whisper API is batch-based (not real-time streaming). Audio is buffered
// during the session and transcribed when Finalize() is called.
type OpenAIWhisperProvider struct {
	client *openai.Client
	logger *slog.Logger
}

// NewOpenAIWhisperProvider creates a new OpenAI Whisper STT provider.
// The apiKey should be the same OpenAI API key used for other OpenAI services.
// The projectId is optional; when set it is sent as the OpenAI-Project header.
func NewOpenAIWhisperProvider(apiKey, projectId string, logger *slog.Logger) *OpenAIWhisperProvider {
	if logger == nil {
		logger = NewLogger()
	}

	config := openai.DefaultConfig(apiKey)
	if projectId != "" {
		config.HTTPClient = &projectHeaderClient{
			inner:     config.HTTPClient,
			projectId: projectId,
		}
	}
	client := openai.NewClientWithConfig(config)

	return &OpenAIWhisperProvider{
		client: client,
		logger: logger,
	}
}

// NewOpenAIWhisperProviderWithClient creates a provider using an existing OpenAI client.
// This allows reusing a client that's already configured (e.g., from the AI provider registry).
func NewOpenAIWhisperProviderWithClient(client *openai.Client, logger *slog.Logger) *OpenAIWhisperProvider {
	if logger == nil {
		logger = NewLogger()
	}
	return &OpenAIWhisperProvider{
		client: client,
		logger: logger,
	}
}

// Name returns the provider identifier.
func (p *OpenAIWhisperProvider) Name() string {
	return "openai-whisper"
}

// StartStream begins a new audio buffering session.
// Since Whisper is batch-based, audio is accumulated until Finalize() is called.
func (p *OpenAIWhisperProvider) StartStream(ctx context.Context, config StreamConfig) (StreamingSession, error) {
	if p.client == nil {
		return nil, fmt.Errorf("openai client not configured")
	}

	session := &whisperSession{
		provider:    p,
		config:      config,
		audioBuffer: bytes.NewBuffer(nil),
		results:     make(chan TranscriptionResult, 10),
		done:        make(chan struct{}),
		startTime:   time.Now(),
		logger:      p.logger,
	}

	p.logger.Info("whisper session started",
		"format", config.Format,
		"sampleRate", config.SampleRate,
		"language", config.LanguageHint,
	)

	return session, nil
}

// whisperSession represents an active Whisper transcription session.
// It buffers all audio until Finalize() is called.
type whisperSession struct {
	provider    *OpenAIWhisperProvider
	config      StreamConfig
	audioBuffer *bytes.Buffer
	results     chan TranscriptionResult
	done        chan struct{}
	startTime   time.Time
	logger      *slog.Logger

	mu     sync.Mutex
	closed bool
}

// SendAudio buffers audio data for later transcription.
func (s *whisperSession) SendAudio(audio []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return fmt.Errorf("session closed")
	}

	_, err := s.audioBuffer.Write(audio)
	return err
}

// Receive returns a channel for transcription results.
// For Whisper, interim results are not available - only final result on Finalize().
func (s *whisperSession) Receive() <-chan TranscriptionResult {
	return s.results
}

// Finalize transcribes the buffered audio using OpenAI Whisper API.
func (s *whisperSession) Finalize(ctx context.Context) (*FinalTranscription, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, fmt.Errorf("session already closed")
	}
	s.closed = true
	audioData := s.audioBuffer.Bytes()
	s.mu.Unlock()

	close(s.done)
	// Note: s.results is closed via defer to ensure it happens after any sends
	defer close(s.results)

	duration := time.Since(s.startTime)

	if len(audioData) == 0 {
		s.logger.Debug("no audio data to transcribe")
		return &FinalTranscription{
			Text:       "",
			DurationMS: duration.Milliseconds(),
			Provider:   "openai-whisper",
		}, nil
	}

	// Convert audio format to a file-like representation
	// Whisper API accepts various formats, we'll send as WAV
	audioFile := s.prepareAudioFile(audioData)

	// Build transcription request
	req := openai.AudioRequest{
		Model:    resolveWhisperModel(),
		Reader:   bytes.NewReader(audioFile),
		FilePath: s.getFilePath(),
	}

	// Add language hint if provided (normalize to ISO-639-1 format)
	if s.config.LanguageHint != "" {
		normalizedLang := normalizeLanguageCode(s.config.LanguageHint)
		req.Language = normalizedLang
		s.logger.Debug("language hint applied",
			"original", s.config.LanguageHint,
			"normalized", normalizedLang,
		)
	}

	// Use JSON format for structured response (verbose_json not supported by gpt-4o-transcribe)
	req.Format = openai.AudioResponseFormatJSON

	s.logger.Debug("calling whisper API",
		"audioSize", len(audioData),
		"language", s.config.LanguageHint,
	)

	resp, err := s.provider.client.CreateTranscription(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("whisper transcription failed: %w", err)
	}

	// Extract word timestamps
	var words []WordTimestamp
	for _, word := range resp.Words {
		words = append(words, WordTimestamp{
			Word:    word.Word,
			StartMS: int(word.Start * 1000),
			EndMS:   int(word.End * 1000),
		})
	}

	text := strings.TrimSpace(resp.Text)

	s.logger.Info("whisper transcription complete",
		"text", truncateText(text, 50),
		"wordCount", len(words),
		"durationMs", duration.Milliseconds(),
	)

	// Send final result to results channel (for any listeners)
	select {
	case s.results <- TranscriptionResult{
		Text:    text,
		IsFinal: true,
		Words:   words,
	}:
	default:
	}

	return &FinalTranscription{
		Text:       text,
		Words:      words,
		DurationMS: duration.Milliseconds(),
		Provider:   "openai-whisper",
	}, nil
}

// Close terminates the session without transcribing.
func (s *whisperSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil
	}
	s.closed = true

	close(s.done)
	close(s.results)

	s.logger.Debug("whisper session closed without transcription")
	return nil
}

// prepareAudioFile converts raw audio data to a format Whisper can accept.
// For PCM16 audio, we wrap it in a minimal WAV header.
func (s *whisperSession) prepareAudioFile(audioData []byte) []byte {
	format := strings.ToLower(s.config.Format)

	switch format {
	case "pcm16", "linear16":
		// Wrap PCM16 in WAV header
		return createWAVFile(audioData, s.config.SampleRate, s.config.Channels)
	case "wav":
		// Already WAV format
		return audioData
	case "mp3", "mp4", "mpeg", "mpga", "m4a", "webm", "ogg":
		// These formats are natively supported by Whisper
		return audioData
	default:
		// Assume PCM16 and wrap in WAV
		return createWAVFile(audioData, s.config.SampleRate, s.config.Channels)
	}
}

// getFilePath returns a filename with appropriate extension for the audio format.
func (s *whisperSession) getFilePath() string {
	format := strings.ToLower(s.config.Format)

	switch format {
	case "pcm16", "linear16", "wav":
		return "audio.wav"
	case "mp3":
		return "audio.mp3"
	case "mp4", "m4a":
		return "audio.m4a"
	case "webm":
		return "audio.webm"
	case "ogg":
		return "audio.ogg"
	default:
		return "audio.wav"
	}
}

// createWAVFile wraps raw PCM16 audio data in a WAV file header.
func createWAVFile(pcmData []byte, sampleRate, channels int) []byte {
	if sampleRate <= 0 {
		sampleRate = 16000
	}
	if channels <= 0 {
		channels = 1
	}

	bitsPerSample := 16
	byteRate := sampleRate * channels * bitsPerSample / 8
	blockAlign := channels * bitsPerSample / 8
	dataSize := len(pcmData)
	fileSize := 36 + dataSize

	buf := bytes.NewBuffer(make([]byte, 0, 44+dataSize))

	// RIFF header
	buf.WriteString("RIFF")
	buf.Write(uint32ToBytes(uint32(fileSize)))
	buf.WriteString("WAVE")

	// fmt subchunk
	buf.WriteString("fmt ")
	buf.Write(uint32ToBytes(16))                    // Subchunk1Size (16 for PCM)
	buf.Write(uint16ToBytes(1))                     // AudioFormat (1 for PCM)
	buf.Write(uint16ToBytes(uint16(channels)))      // NumChannels
	buf.Write(uint32ToBytes(uint32(sampleRate)))    // SampleRate
	buf.Write(uint32ToBytes(uint32(byteRate)))      // ByteRate
	buf.Write(uint16ToBytes(uint16(blockAlign)))    // BlockAlign
	buf.Write(uint16ToBytes(uint16(bitsPerSample))) // BitsPerSample

	// data subchunk
	buf.WriteString("data")
	buf.Write(uint32ToBytes(uint32(dataSize)))
	buf.Write(pcmData)

	return buf.Bytes()
}

func uint32ToBytes(v uint32) []byte {
	return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}
}

func uint16ToBytes(v uint16) []byte {
	return []byte{byte(v), byte(v >> 8)}
}

func truncateText(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// normalizeLanguageCode converts a language code to ISO-639-1 format.
// OpenAI Whisper requires ISO-639-1 codes (e.g., "en", "es", "fr").
// This function strips region suffixes like "en-US" -> "en", "es-MX" -> "es".
func normalizeLanguageCode(lang string) string {
	lang = strings.TrimSpace(lang)
	if lang == "" {
		return ""
	}

	// Handle both "en-US" and "en_US" formats
	if idx := strings.IndexAny(lang, "-_"); idx > 0 {
		return strings.ToLower(lang[:idx])
	}

	return strings.ToLower(lang)
}
