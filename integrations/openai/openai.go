// Package openai implements OpenAI ASR and TTS clients for the Polyphon
// multi-agent voice system. These clients implement the polyphon.ASRProvider
// and polyphon.TTSProvider interfaces, providing cloud-based speech processing
// suitable for development and Cloud Run deployments (no GPU required).
//
// ASR uses the OpenAI Realtime API in transcription-only mode (type: "transcription"),
// which provides streaming speech-to-text via WebSocket without bundling an LLM.
//
// TTS uses the OpenAI /v1/audio/speech HTTP API with PCM16 output format.
//
// Both providers handle sample rate conversion between the Polyphon pipeline
// (16kHz) and OpenAI's native format (24kHz) transparently.
package openai

import (
	"fmt"
	"log/slog"
)

// Config holds the configuration for OpenAI voice providers.
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
	// only -- no streaming partials -- so the orb's interim text is
	// suppressed; everything else (final transcript, agent reply, TTS)
	// works normally.
	//
	// To upgrade to streaming partials: enable gpt-4o-mini-transcribe
	// (or gpt-4o-transcribe for higher accuracy) on the OpenAI project,
	// then set MEMQL_POLYPHON_OPENAI_ASR_MODEL=gpt-4o-mini-transcribe.
	ASRModel string `json:"asrModel"`

	// TTSModel is the text-to-speech model for the /v1/audio/speech
	// HTTP API. Defaults to gpt-4o-mini-tts (TTSModelGPT4oMini) --
	// the newest, lowest-latency, most natural-sounding voice. The
	// constants below name the four currently-supported models so
	// operators can swap via MEMQL_POLYPHON_OPENAI_TTS_MODEL without
	// guessing the string:
	//
	//	TTSModelGPT4oMini  "gpt-4o-mini-tts"  default; cheapest + most natural
	//	TTSModelGPT4o      "gpt-4o-tts"       higher fidelity, slower, costs more
	//	TTSModelTTS1HD     "tts-1-hd"         legacy HD; slower than gpt-4o-mini
	//	TTSModelTTS1       "tts-1"            legacy SD; cheapest legacy fallback
	//
	// All four work with every voice in AvailableVoices() and every
	// response_format (pcm / opus / mp3). Project-level access at
	// platform.openai.com/settings/organization/projects/<id>/limits
	// must include the chosen model or the bridge will fail to
	// synthesize on every turn.
	TTSModel string `json:"ttsModel"`

	// TTSVoice is the default TTS voice (default: "alloy").
	TTSVoice string `json:"ttsVoice"`

	// Logger for debug output.
	Logger *slog.Logger `json:"-"`
}

// Supported OpenAI TTS models. The Config.TTSModel field accepts any
// string, but these are the four the bridge has been tested against
// and the four documented to operators. Set
// MEMQL_POLYPHON_OPENAI_TTS_MODEL=<value> to override the default.
const (
	// TTSModelGPT4oMini is the default -- newest TTS family,
	// cheapest, most natural-sounding voices, lowest latency.
	TTSModelGPT4oMini = "gpt-4o-mini-tts"
	// TTSModelGPT4o is the larger gpt-4o-tts variant. Higher
	// fidelity than mini at the cost of slower synthesis + higher
	// per-token cost. Worth trying if a specific voice on mini
	// sounds clipped or robotic for your content.
	TTSModelGPT4o = "gpt-4o-tts"
	// TTSModelTTS1HD is the legacy HD model. Pre-gpt-4o family,
	// still produces clean audio but at noticeably higher latency
	// than gpt-4o-mini-tts. Useful as a fallback when a project
	// hasn't been granted access to the gpt-4o-* TTS family.
	TTSModelTTS1HD = "tts-1-hd"
	// TTSModelTTS1 is the legacy SD model. Cheapest legacy choice;
	// audibly lower fidelity than tts-1-hd but the most universally
	// available across OpenAI projects.
	TTSModelTTS1 = "tts-1"

	defaultASRModel = "whisper-1"
	defaultTTSModel = TTSModelGPT4oMini
	defaultTTSVoice = "alloy"
)

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		ASRModel: defaultASRModel,
		TTSModel: defaultTTSModel,
		TTSVoice: defaultTTSVoice,
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
	if c.TTSModel == "" {
		c.TTSModel = defaultTTSModel
	}
	if c.TTSVoice == "" {
		c.TTSVoice = defaultTTSVoice
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
