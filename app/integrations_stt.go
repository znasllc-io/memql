//go:build !planner

package app

import (
	"os"
	"strings"

	openaivoice "github.com/znasllc-io/memql/integrations/openai"
	"github.com/znasllc-io/memql/integrations/stt"
)

// selectSTTProvider chooses the Speech-to-Text provider based on MEMQL_STT_PROVIDER
// and registers the STT integration for DSL-callable transcription.
// Compiled for cognition, agent, and standalone builds.
//
// Provider selection order:
//  1. If MEMQL_STT_PROVIDER is set explicitly, use that value.
//  2. Else default to "openai-realtime" (streaming via the Realtime
//     API -- word-by-word interim results). Falls back to
//     "openai-whisper" (batch) if the Realtime model isn't usable.
func (a *App) selectSTTProvider() {
	explicit := strings.ToLower(strings.TrimSpace(os.Getenv("MEMQL_STT_PROVIDER")))
	sttProviderName := explicit
	if sttProviderName == "" {
		sttProviderName = "openai-realtime"
	}

	switch sttProviderName {
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

// openAIKeyFromEnv resolves the OpenAI API key for the STT bootstrap,
// trying MEMQL_AI_OPENAI_API_KEY first and falling back to the bare
// MEMQL_OPENAI_API_KEY -- the same prefix-elision chain the provider auth
// resolver (component/memql/ai_providers.go, authConceptLookupNames)
// and integrations/openairealtime use. The genesis envelope seeds the
// bare form, so without the fallback the voice node boots with the
// audio websocket silently disabled (#1371).
func openAIKeyFromEnv() string {
	if key := strings.TrimSpace(os.Getenv("MEMQL_AI_OPENAI_API_KEY")); key != "" {
		return key
	}
	return strings.TrimSpace(os.Getenv("MEMQL_OPENAI_API_KEY"))
}

// initOpenAIRealtimeProvider wires the OpenAI Realtime API (streaming
// transcription via WebSocket) as the active STT provider -- the default.
//
// Model resolution: honors MEMQL_OPENAI_REALTIME_MODEL; falls back to
// MEMQL_POLYPHON_OPENAI_ASR_MODEL so a single env var can drive both paths;
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
	openAIKey := openAIKeyFromEnv()
	if openAIKey == "" {
		a.Logger.Info("audio websocket disabled (neither MEMQL_AI_OPENAI_API_KEY nor MEMQL_OPENAI_API_KEY set for openai-realtime)")
		return
	}

	model := strings.TrimSpace(os.Getenv("MEMQL_OPENAI_REALTIME_MODEL"))
	if model == "" {
		model = strings.TrimSpace(os.Getenv("MEMQL_POLYPHON_OPENAI_ASR_MODEL"))
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
	openAIKey := openAIKeyFromEnv()
	openAIProject := strings.TrimSpace(os.Getenv("MEMQL_AI_OPENAI_PROJECT_ID"))
	if openAIKey == "" {
		a.Logger.Info("audio websocket disabled (neither MEMQL_AI_OPENAI_API_KEY nor MEMQL_OPENAI_API_KEY set for whisper)")
		return
	}
	a.sttProvider = stt.NewOpenAIWhisperProvider(openAIKey, openAIProject, nil)
	a.Logger.Info("audio websocket using OpenAI Whisper", "provider", name)
}
