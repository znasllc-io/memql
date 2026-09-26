//go:build !planner

package app

import (
	"context"
	"os"
	"strings"

	memqlengine "github.com/znasllc-io/memql/component/memql"
	openaivoice "github.com/znasllc-io/memql/integrations/openai"
	"github.com/znasllc-io/memql/integrations/stt"
)

// selectSTTProvider chooses the Speech-to-Text provider based on MEMQL_STT_PROVIDER
// and registers the STT integration for DSL-callable transcription.
// Compiled for cognition, agent, and standalone builds.
//
// Provider selection order:
//  1. If MEMQL_STT_PROVIDER is set explicitly, use that value.
//  2. Otherwise use the inference router: eligible Fleet audio first, then
//     the federated providers permitted by the configured policy.
func (a *App) selectSTTProvider() {
	explicit := strings.ToLower(strings.TrimSpace(os.Getenv("MEMQL_STT_PROVIDER")))
	sttProviderName := explicit
	if sttProviderName == "" {
		sttProviderName = "router"
	}

	switch sttProviderName {
	case "router":
		a.sttProvider = &stt.RoutedProvider{Transcribe: func(ctx context.Context, wav []byte) (string, error) {
			result, err := a.engine.TranscribeAudio(ctx, memqlengine.FleetAudio{Data: wav, MediaType: "audio/wav"}, "")
			return result.Text, err
		}}
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

// openAIBearerForSTT resolves the credential the transcription paths dial with.
//
// It asks the ENGINE rather than the environment (epic memql#5088). Both STT
// paths used to read a vendor API key out of the process environment, with a
// fallback to its seal-floor spelling; there is no vendor key any more, and the
// credential is a federated bearer the engine's exchanger owns. Going through the engine is
// what keeps transcription and every other OpenAI consumer on this node
// agreeing about whether OpenAI is configured -- a second reader with its own
// idea of that presents as "transcription is broken" long after the change
// that caused it.
//
// The returned function is called PER DIAL and per request, never captured:
// a bearer expires within the hour and a reconnect after that must not present
// the token the first connection opened with.
func (a *App) openAIBearerForSTT() (func(ctx context.Context) (string, error), bool) {
	if a.engine == nil {
		return nil, false
	}
	source, ok := a.engine.OpenAIBearer()
	if !ok {
		return nil, false
	}
	return func(ctx context.Context) (string, error) {
		token, _, err := source.Bearer(ctx)
		return token, err
	}, true
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
	bearer, ok := a.openAIBearerForSTT()
	if !ok {
		// NOT AN ERROR, and the message says which fact it is reporting. A
		// local cluster cannot federate at all -- its OIDC issuer is private --
		// so streaming transcription is off there by design (memql#5088, D6),
		// and a fresh cloud cluster is in the same state until an operator
		// finishes the runbook.
		a.Logger.Info("audio websocket disabled (no OpenAI federation on this cluster)",
			"runbook", "docs/public/operate/auth/openai-federation.md")
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
		Bearer:   bearer,
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
	bearer, ok := a.openAIBearerForSTT()
	if !ok {
		a.Logger.Info("audio websocket disabled (no OpenAI federation on this cluster)",
			"runbook", "docs/public/operate/auth/openai-federation.md")
		return
	}
	openAIProject := strings.TrimSpace(os.Getenv("MEMQL_AI_OPENAI_PROJECT_ID"))
	a.sttProvider = stt.NewOpenAIWhisperProvider(bearer, openAIProject, nil)
	a.Logger.Info("audio websocket using OpenAI Whisper", "provider", name)
}
