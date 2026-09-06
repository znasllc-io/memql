// Package openai implements the OpenAI ASR client behind MemQL's streaming
// transcription path (the AiTranscribeStream* messages). It implements the
// audio.ASRProvider interface, providing cloud-based speech-to-text with no
// GPU requirement.
//
// ASR uses the OpenAI Realtime API in transcription-only mode (type: "transcription"),
// which provides streaming speech-to-text via WebSocket without bundling an LLM.
//
// The provider handles sample rate conversion between MemQL's audio pipeline
// (16kHz) and OpenAI's native format (24kHz) transparently.
//
// The package also shipped a TTS client against /v1/audio/speech, for the
// Polyphon voice transport. Epic memql#4988 retired that transport along with
// the voice and cognition node types, and nothing constructed the client; it
// and its two config knobs (MEMQL_POLYPHON_OPENAI_TTS_MODEL / _TTS_VOICE) are
// gone. The engine's own text-to-speech, AiSpeechMsg, is served by the AI
// provider registry in component/memql/ai_providers.go and never used this.
package openai

import (
	"fmt"
	"log/slog"
)

// Config holds the configuration for the OpenAI ASR provider.
type Config struct {
	// APIKey is the OpenAI API key (MEMQL_AI_OPENAI_API_KEY).
	APIKey string `json:"apiKey"`

	// ASRModel is the transcription model used by the Realtime API in
	// transcription-only mode. Defaults to whisper-1 -- the only
	// transcription model OpenAI enables on every project by default.
	// The newer gpt-4o-{transcribe,mini-transcribe,transcribe-diarize}
	// variants are gated behind project-level model access (visible
	// at platform.openai.com/settings/organization/projects/<id>/limits)
	// and the Realtime session silently fails the FIRST committed
	// utterance with "Project ... does not have access to model" until
	// the project owner enables them. whisper-1 emits final transcripts
	// only -- no streaming partials -- so a client's interim text is
	// suppressed; everything else (final transcript, agent reply) works
	// normally.
	//
	// To upgrade to streaming partials: enable gpt-4o-mini-transcribe
	// (or gpt-4o-transcribe for higher accuracy) on the OpenAI project,
	// then set MEMQL_POLYPHON_OPENAI_ASR_MODEL=gpt-4o-mini-transcribe.
	ASRModel string `json:"asrModel"`

	// Logger for debug output.
	Logger *slog.Logger `json:"-"`
}

const defaultASRModel = "whisper-1"

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		ASRModel: defaultASRModel,
	}
}

// validate checks that required fields are set and applies defaults.
func (c *Config) validate() error {
	if c.APIKey == "" {
		return fmt.Errorf("openai: API key is required")
	}
	if c.ASRModel == "" {
		c.ASRModel = defaultASRModel
	}
	return nil
}

// logger returns the configured logger or a discard logger.
func (c *Config) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.New(slog.DiscardHandler)
}
