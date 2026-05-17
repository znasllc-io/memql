//go:build !planner

package app

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/integrations/deepgram"
	openaivoice "github.com/znasllc-io/memql/integrations/openai"
	"github.com/znasllc-io/memql/integrations/stt"
)

// parseEnvIntDefaultZero reads a non-negative integer env var, returning
// 0 when the env is unset, empty, or malformed. 0 lets downstream apply
// the consumer's own default constant.
func parseEnvIntDefaultZero(key string) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// selectSTTProvider chooses the Speech-to-Text provider based on MEMQL_STT_PROVIDER
// and registers the STT integration for DSL-callable transcription.
// Compiled for cognition, agent, and standalone builds.
//
// Provider selection order:
//  1. If MEMQL_STT_PROVIDER is set explicitly, use that value.
//  2. Else if MEMQL_DEEPGRAM_API_KEY is set, default to "deepgram"
//     (Nova-3 streaming WebSocket). Falls through to openai-realtime
//     if the Deepgram health check fails.
//  3. Else default to "openai-realtime" (streaming via the Realtime
//     API -- word-by-word interim results). Falls back to
//     "openai-whisper" (batch) if the Realtime model isn't usable.
//
// This is the Phase 7 default flip in the Deepgram migration:
// Deepgram is now the auto-selected default whenever a key is
// configured; OpenAI is the startup-time fallback. Mid-session
// failover is NOT in scope -- a transient Deepgram error on one call
// surfaces normally and the next call retries Deepgram.
func (a *App) selectSTTProvider() {
	explicit := strings.ToLower(strings.TrimSpace(os.Getenv("MEMQL_STT_PROVIDER")))
	sttProviderName := explicit
	if sttProviderName == "" {
		if strings.TrimSpace(os.Getenv("MEMQL_DEEPGRAM_API_KEY")) != "" {
			sttProviderName = "deepgram"
		} else {
			sttProviderName = "openai-realtime"
		}
	}

	switch sttProviderName {
	case "deepgram":
		if !a.initDeepgramProvider(explicit == "deepgram") && explicit != "deepgram" {
			// Auto-selected deepgram fell through. Emit a
			// structured WARN line matching the
			// v1:identity:auditEvent shape ({category=
			// configuration, action=voice.provider.failover,
			// detail.requested=deepgram, detail.fallback=
			// openai-realtime, outcome=failure, failure_reason=
			// health_check_failed}). Operators can grep this
			// to know a silent fallback fired -- a real
			// db-backed audit-event insertion would need the
			// identity-node's AuditDBSink, which non-identity
			// binaries don't carry.
			a.Logger.Warn("audit",
				"category", "configuration",
				"action", "voice.provider.failover",
				"requested", "deepgram",
				"fallback", "openai-realtime",
				"reason", "health_check_failed",
				"outcome", "failure",
			)
			a.initOpenAIRealtimeProvider("openai-realtime")
		}
	case "openai-realtime", "realtime":
		a.initOpenAIRealtimeProvider(sttProviderName)
	case "openai-whisper", "whisper":
		a.initWhisperProvider(sttProviderName)
	case "openai":
		// Ambiguous legacy alias -- default to the streaming path, which is
		// strictly better UX when the project has Realtime API access.
		a.initOpenAIRealtimeProvider("openai-realtime")
	default:
		a.Logger.Warn("unknown STT provider, audio websocket disabled", "provider", sttProviderName)
	}

	if a.sttProvider != nil {
		sttInteg := stt.NewSTTIntegration(a.sttProvider.(stt.StreamingProvider))
		if err := a.engine.RegisterIntegration(sttInteg); err != nil {
			a.fatal("failed to register stt integration", "error", err)
		}
	}
}

// initOpenAIRealtimeProvider wires the OpenAI Realtime API (streaming
// transcription via WebSocket) as the active STT provider. This is the
// current default; Deepgram replaces it as the auto-selected default
// in Phase 7 of the Deepgram migration.
//
// Model resolution: honors MEMQL_OPENAI_REALTIME_MODEL; falls back to
// POLYPHON_OPENAI_ASR_MODEL so a single env var can drive both paths;
// defaults to "whisper-1".
//
// Why whisper-1 is the default (not gpt-4o-transcribe):
// gpt-4o-transcribe gives slightly better quality but requires explicit
// project access on the OpenAI dashboard. Without that access OpenAI
// emits conversation.item.input_audio_transcription.failed with code
// model_not_found, and the UI shows empty transcripts. whisper-1 is
// universally available for every project and IS supported by the
// Realtime API transcription-only mode (becomes streaming in this
// mode, unlike the /audio/transcriptions batch endpoint). Deployments
// that have provisioned gpt-4o-transcribe can opt in via the env var.
func (a *App) initOpenAIRealtimeProvider(name string) {
	openAIKey := strings.TrimSpace(os.Getenv("MEMQL_SI_OPENAI_API_KEY"))
	if openAIKey == "" {
		a.Logger.Info("audio websocket disabled (MEMQL_SI_OPENAI_API_KEY not set for openai-realtime)")
		return
	}

	model := strings.TrimSpace(os.Getenv("MEMQL_OPENAI_REALTIME_MODEL"))
	if model == "" {
		model = strings.TrimSpace(os.Getenv("POLYPHON_OPENAI_ASR_MODEL"))
	}
	if model == "" {
		model = "whisper-1"
	}

	cfg := openaivoice.Config{
		APIKey:   openAIKey,
		ASRModel: model,
		Logger:   a.Logger,
	}
	asr, err := openaivoice.NewASRClient(cfg)
	if err != nil {
		a.Logger.Warn("openai realtime STT init failed; STT disabled", "error", err)
		return
	}

	a.sttProvider = stt.NewOpenAIRealtimeProvider(asr, nil)
	a.Logger.Info("STT provider initialized", "provider", name, "model", model)
}

func (a *App) initWhisperProvider(name string) {
	openAIKey := strings.TrimSpace(os.Getenv("MEMQL_SI_OPENAI_API_KEY"))
	openAIProject := strings.TrimSpace(os.Getenv("MEMQL_SI_OPENAI_PROJECT_ID"))
	if openAIKey == "" {
		a.Logger.Info("audio websocket disabled (MEMQL_SI_OPENAI_API_KEY not set for whisper)")
		return
	}
	a.sttProvider = stt.NewOpenAIWhisperProvider(openAIKey, openAIProject, nil)
	a.Logger.Info("audio websocket using OpenAI Whisper", "provider", name)
}

// initDeepgramProvider constructs the Deepgram STT provider and runs a
// startup health check. Returns true on success. On failure, behavior
// depends on explicitRequest: when the user set MEMQL_STT_PROVIDER=deepgram
// we fail fast; otherwise the caller falls through to openai-realtime
// (this matches the auto-selection pattern Phase 7 introduces).
//
// Model resolution: honors POLYPHON_DEEPGRAM_ASR_MODEL (default "nova-3").
func (a *App) initDeepgramProvider(explicitRequest bool) bool {
	key := strings.TrimSpace(os.Getenv("MEMQL_DEEPGRAM_API_KEY"))
	if key == "" {
		if explicitRequest {
			a.fatal("MEMQL_STT_PROVIDER=deepgram but MEMQL_DEEPGRAM_API_KEY is not set")
		}
		a.Logger.Warn("STT provider 'deepgram' selected but MEMQL_DEEPGRAM_API_KEY not set; falling back")
		return false
	}

	model := strings.TrimSpace(os.Getenv("POLYPHON_DEEPGRAM_ASR_MODEL"))
	if model == "" {
		model = "nova-3"
	}
	language := strings.TrimSpace(os.Getenv("POLYPHON_DEEPGRAM_LANGUAGE"))

	cfg := deepgram.Config{
		APIKey:             key,
		ASRModel:           model,
		Language:           language,
		EndpointingMs:      parseEnvIntDefaultZero(deepgram.EnvEndpointingMs),
		UtteranceEndMs:     parseEnvIntDefaultZero(deepgram.EnvUtteranceEndMs),
		ClientEOUTimeoutMs: parseEnvIntDefaultZero(deepgram.EnvClientEOUTimeoutMs),
		Logger:             a.Logger,
	}
	asr, err := deepgram.NewASRClient(cfg)
	if err != nil {
		if explicitRequest {
			a.fatal("deepgram STT provider init failed", "error", err)
		}
		a.Logger.Warn("deepgram STT init failed; falling back", "error", err)
		return false
	}

	if !a.deepgramHealthCheck(asr, explicitRequest) {
		_ = asr.Close()
		return false
	}

	a.sttProvider = stt.NewDeepgramProvider(asr, nil)
	a.Logger.Info("STT provider initialized", "provider", "deepgram", "model", model)
	return true
}

// deepgramHealthCheck verifies the Deepgram credentials authenticate
// successfully by opening + immediately closing a streaming session
// within a 5s budget. Explicit requests fail fast on failure; auto-
// selected requests log and return false so the caller falls through.
func (a *App) deepgramHealthCheck(asr *deepgram.ASRClient, explicitRequest bool) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := asr.Ping(ctx); err != nil {
		if explicitRequest {
			a.fatal("deepgram STT provider unreachable", "error", err)
		}
		a.Logger.Warn("deepgram unreachable; falling back to openai-realtime", "error", err)
		return false
	}
	return true
}
