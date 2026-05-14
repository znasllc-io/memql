// Package deepgram implements ASR and TTS providers for the Polyphon
// voice pipeline backed by Deepgram (Nova-3 streaming WebSocket for
// ASR; Aura-2 streaming WebSocket for TTS, added in Phase 5 of the
// Deepgram migration).
//
// Both clients implement the polyphon.ASRProvider / polyphon.TTSProvider
// interfaces from component/polyphon. The bridge agent picks them up
// via the same provider-selection switch that fronts the OpenAI
// providers; no caller knows which provider is active.
//
// Why Deepgram: Nova-3 is the lowest-latency streaming ASR on the
// market today (sub-300 ms first interim partial in our latency-
// harness baseline). Aura-2 pairs cleanly with the existing sentence-
// streaming LLM->TTS pipeline because it accepts token-by-token input
// streaming (~70% TTS-TTFB reduction relative to chunked HTTP).
//
// Operator-facing knobs (env):
//
//	MEMQL_DEEPGRAM_API_KEY        Deepgram API key. Required.
//	POLYPHON_DEEPGRAM_ASR_MODEL   ASR model (default "nova-3").
//	POLYPHON_DEEPGRAM_TTS_MODEL   TTS model (default "aura-2"; Phase 5).
//	POLYPHON_DEEPGRAM_LANGUAGE    BCP-47 language tag (default "en-US").
//
// Treat MEMQL_DEEPGRAM_API_KEY as a secret -- never log; mount via
// the same secret-management path as MEMQL_SI_OPENAI_API_KEY.
package deepgram

import (
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
)

// Default Deepgram API endpoint. Override only via env in tests; the
// production path resolves to the public api.deepgram.com origin.
const (
	defaultBaseURL  = "wss://api.deepgram.com"
	defaultASRModel = "nova-3"
	// Aura-2 requires a per-voice model id (e.g. "aura-2-thalia-en").
	// There is no plain "aura-2" model -- passing it as the URL
	// `model` parameter gets `INVALID_QUERY_PARAMETER` from Deepgram.
	// thalia-en is the female default Aura-2 voice, matching the
	// "alto" canonical mapping that the catalog uses for the GA.
	defaultTTSModel = "aura-2-thalia-en"
	defaultLanguage = "en-US"

	// asrSampleRate is the linear PCM16 sample rate the Polyphon
	// pipeline publishes (matches what the bridge resamples LiveKit
	// audio down to before forwarding to the ASR provider).
	asrSampleRate = 16000

	// defaultEndpointingMs controls Deepgram's phrase-commit cadence
	// (when `is_final=true` flips inside a Results event). Phrase
	// commits drive interim accumulation -- they're the granularity at
	// which the running transcript stops getting revised. The EOU
	// trigger is separate (utterance_end_ms or the client watchdog
	// below), so this value only affects how often the displayed
	// transcript "snaps" to a stable segment, not the end-of-turn wait.
	defaultEndpointingMs = 300

	// defaultUtteranceEndMs is Deepgram's silence-driven EOU signal.
	// 1000ms is Deepgram's documented minimum. Combined with the
	// client-side watchdog below this is the "no-splits" floor --
	// natural mid-thought pauses under 1s stay merged into one
	// downstream turn.
	defaultUtteranceEndMs = 1000

	// defaultClientEOUTimeoutMs is the client-side watchdog backstop.
	// When Deepgram's VAD gets stuck on ambient mic noise (laptop fan,
	// breath, room sound), it keeps emitting the SAME interim
	// transcript as keepalive for many seconds without ever firing
	// UtteranceEnd or speech_final. Observed in the wild: 11-21 second
	// hangs after the user clearly stopped speaking. The watchdog
	// fires EOU when the interim transcript hasn't changed for this
	// many milliseconds, regardless of what Deepgram is doing. 2500ms
	// is long enough that natural thinking pauses (1-2s) don't trip
	// it but short enough that the ambient-noise hang case stays
	// under 3s. 0 disables the watchdog.
	defaultClientEOUTimeoutMs = 2500

	// watchdogTickMs is how often the client-EOU watchdog checks for
	// interim-text staleness. 500ms gives sub-second resolution on
	// the timeout without burning CPU.
	watchdogTickMs = 500

	// EnvEndpointingMs overrides the per-phrase silence threshold
	// (when Results.is_final flips). Affects display-side phrase
	// snapping; does NOT affect the user's perceived tail latency
	// (that's gated by UtteranceEndMs / ClientEOUTimeoutMs).
	EnvEndpointingMs = "POLYPHON_DEEPGRAM_ENDPOINTING_MS"

	// EnvUtteranceEndMs overrides the silence threshold for
	// Deepgram-driven EOU. Minimum 1000ms (Deepgram's floor). Set
	// to 0 to disable Deepgram's UtteranceEnd and rely entirely on
	// the client watchdog -- not recommended unless you've tuned the
	// watchdog to compensate.
	EnvUtteranceEndMs = "POLYPHON_DEEPGRAM_UTTERANCE_END_MS"

	// EnvClientEOUTimeoutMs overrides the static-transcript watchdog.
	// Lower = faster recovery from Deepgram VAD hangs but more split
	// risk for slow speakers; higher = more tolerance for long
	// thinking pauses but longer worst-case tail when Deepgram is
	// stuck. 0 disables the watchdog entirely.
	EnvClientEOUTimeoutMs = "POLYPHON_DEEPGRAM_CLIENT_EOU_TIMEOUT_MS"
)

// Config holds the connection configuration for the Deepgram clients.
type Config struct {
	// APIKey is the Deepgram API key. Required.
	APIKey string `json:"apiKey"`

	// ASRModel is the Deepgram ASR model id (default "nova-3").
	ASRModel string `json:"asrModel,omitempty"`

	// TTSModel is the Deepgram TTS model id (default "aura-2").
	TTSModel string `json:"ttsModel,omitempty"`

	// Language is the BCP-47 language tag for transcription (default
	// "en-US"). Per-call overrides flow through polyphon.ASRConfig.
	Language string `json:"language,omitempty"`

	// EndpointingMs is Deepgram's phrase-commit silence threshold
	// (the `endpointing` URL parameter). 0 -> defaultEndpointingMs.
	// Affects the granularity of interim accumulator commits, not
	// the perceived tail latency.
	EndpointingMs int `json:"endpointingMs,omitempty"`

	// UtteranceEndMs is Deepgram's VAD-driven EOU silence threshold
	// (the `utterance_end_ms` URL parameter; minimum 1000ms per
	// Deepgram). 0 disables Deepgram's UtteranceEnd entirely. Default
	// 1000ms unless the env override sets it.
	UtteranceEndMs int `json:"utteranceEndMs,omitempty"`

	// ClientEOUTimeoutMs is the client-side watchdog timeout. When
	// the interim transcript stays unchanged for this many ms, the
	// stream force-fires EOU -- handles Deepgram's "ambient noise
	// keeps VAD active" failure mode where UtteranceEnd hangs for
	// many seconds after the user actually stopped speaking. 0
	// disables the watchdog. Default: defaultClientEOUTimeoutMs.
	ClientEOUTimeoutMs int `json:"clientEOUTimeoutMs,omitempty"`

	// BaseURL overrides wss://api.deepgram.com. Only set in tests --
	// production runs should leave this empty.
	BaseURL string `json:"baseUrl,omitempty"`

	// Logger is the per-client logger. nil falls back to a discard
	// logger.
	Logger *slog.Logger `json:"-"`
}

// validate ensures the required fields are present and applies the
// defaults that downstream code expects.
func (c *Config) validate() error {
	if strings.TrimSpace(c.APIKey) == "" {
		return fmt.Errorf("deepgram: APIKey is required")
	}
	if strings.TrimSpace(c.ASRModel) == "" {
		c.ASRModel = defaultASRModel
	}
	if strings.TrimSpace(c.TTSModel) == "" {
		c.TTSModel = defaultTTSModel
	}
	if strings.TrimSpace(c.Language) == "" {
		c.Language = defaultLanguage
	}
	if strings.TrimSpace(c.BaseURL) == "" {
		c.BaseURL = defaultBaseURL
	}
	if c.EndpointingMs <= 0 {
		c.EndpointingMs = defaultEndpointingMs
	}
	if c.UtteranceEndMs < 0 {
		c.UtteranceEndMs = 0
	} else if c.UtteranceEndMs == 0 {
		c.UtteranceEndMs = defaultUtteranceEndMs
	}
	if c.ClientEOUTimeoutMs < 0 {
		c.ClientEOUTimeoutMs = 0
	} else if c.ClientEOUTimeoutMs == 0 {
		c.ClientEOUTimeoutMs = defaultClientEOUTimeoutMs
	}
	return nil
}

// logger returns the configured logger or a no-op fallback.
func (c *Config) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// asrStreamURL builds the wss:// URL for the Nova-3 streaming
// transcription endpoint, baking the per-stream parameters into the
// query string. Deepgram reads model + language + encoding + sample
// rate from the URL, not from a session-update message, so they have
// to be set on the dial.
//
// language defaults to the client's configured language when empty.
func (c *Config) asrStreamURL(language string) (string, error) {
	if err := c.validate(); err != nil {
		return "", err
	}

	lang := strings.TrimSpace(language)
	if lang == "" {
		lang = c.Language
	}

	base, err := url.Parse(c.BaseURL)
	if err != nil {
		return "", fmt.Errorf("deepgram: parse base url %q: %w", c.BaseURL, err)
	}
	base.Path = "/v1/listen"

	q := base.Query()
	q.Set("model", c.ASRModel)
	q.Set("language", lang)
	q.Set("encoding", "linear16")
	q.Set("sample_rate", strconv.Itoa(asrSampleRate))
	q.Set("channels", "1")
	q.Set("interim_results", "true")
	q.Set("vad_events", "true")
	q.Set("smart_format", "true")
	q.Set("endpointing", strconv.Itoa(c.EndpointingMs))
	if c.UtteranceEndMs > 0 {
		// Drives Deepgram's UtteranceEnd event. Minimum 1000ms per
		// Deepgram. Set to 0 in Config to disable and rely entirely
		// on the client-side watchdog (not recommended).
		q.Set("utterance_end_ms", strconv.Itoa(c.UtteranceEndMs))
	}
	base.RawQuery = q.Encode()

	return base.String(), nil
}

// authHeaderValue returns the value to use in the Authorization
// header. Deepgram expects "Token <key>" (NOT "Bearer <key>") --
// using Bearer silently downgrades to anonymous and the WebSocket
// handshake succeeds but every audio frame returns 401.
func (c *Config) authHeaderValue() string {
	return "Token " + c.APIKey
}
