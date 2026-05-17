// Package stt provides speech-to-text provider interfaces and implementations
// for real-time audio transcription in MemQL.
package stt

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/env"
	"github.com/znasllc-io/memql/core/logger"
)

// ComponentName identifies this package in logs and environment variable lookups.
const ComponentName = common.ComponentName("sttProvider")

// NewLogger creates a logger for the STT provider using logger.NewLogger.
func NewLogger() *slog.Logger {
	return logger.New(ComponentName, os.Stdout, resolveLoggerLevel())
}

func resolveLoggerLevel() slog.Level {
	reader := env.NewEnvReader("STT_PROVIDER")
	for _, key := range loggerLevelKeys() {
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

func loggerLevelKeys() []string {
	return []string{
		"CAPABILITIES_LOGGING_LOG_LEVEL",
		"LOGGER_LEVEL",
		"LOG_LEVEL",
	}
}

// StreamConfig configures a streaming transcription session.
type StreamConfig struct {
	// Format specifies the audio format: "pcm16", "opus", "webm"
	Format string

	// SampleRate is the audio sample rate in Hz (e.g., 16000, 24000)
	SampleRate int

	// Channels is the number of audio channels (1 for mono, 2 for stereo)
	Channels int

	// LanguageHint provides an optional language code hint (e.g., "en", "es")
	LanguageHint string
}

// StreamingProvider provides real-time streaming transcription capabilities.
type StreamingProvider interface {
	// StartStream begins a new streaming transcription session.
	StartStream(ctx context.Context, config StreamConfig) (StreamingSession, error)

	// Name returns the provider name for logging and tracking.
	Name() string
}

// StreamingSession represents an active streaming transcription session.
type StreamingSession interface {
	// SendAudio sends audio data to the STT service.
	// Audio should be in the format specified during StartStream.
	SendAudio(audio []byte) error

	// Receive returns a channel for receiving transcription events.
	// The channel is closed when the session ends.
	Receive() <-chan TranscriptionResult

	// Finalize closes the stream and returns the final transcription.
	// This should be called when the user stops speaking.
	Finalize(ctx context.Context) (*FinalTranscription, error)

	// Close terminates the session without waiting for final result.
	// Use this when the user cancels the recording.
	Close() error
}

// TranscriptionResult represents an interim or final transcription event.
type TranscriptionResult struct {
	// Text is the transcribed text.
	Text string

	// IsFinal indicates whether this is a final result (true) or interim (false).
	IsFinal bool

	// Confidence is a score from 0.0 to 1.0 indicating transcription confidence.
	Confidence float32

	// Words contains word-level timestamps (typically only for final results).
	Words []WordTimestamp
}

// FinalTranscription is the complete result after a stream ends.
type FinalTranscription struct {
	// Text is the complete transcribed text.
	Text string

	// Words contains word-level timestamps.
	Words []WordTimestamp

	// DurationMS is the total audio duration in milliseconds.
	DurationMS int64

	// Confidence is the overall confidence score.
	Confidence float32

	// Provider is the name of the STT provider used.
	Provider string

	// UtteranceId is set after the utterance is inserted into MemQL.
	UtteranceId string
}

// WordTimestamp provides timing information for individual words.
type WordTimestamp struct {
	// Word is the transcribed word.
	Word string `json:"word"`

	// StartMS is the start time in milliseconds from the beginning of the audio.
	StartMS int `json:"start"`

	// EndMS is the end time in milliseconds from the beginning of the audio.
	EndMS int `json:"end"`

	// Confidence is the confidence score for this word (0.0 to 1.0).
	Confidence float32 `json:"confidence,omitempty"`
}
