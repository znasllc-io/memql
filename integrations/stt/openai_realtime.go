package stt

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/visionarys-io/memql/component/polyphon"
	openaivoice "github.com/visionarys-io/memql/integrations/openai"
)

// OpenAIRealtimeProvider adapts openai.ASRClient (which uses the OpenAI
// Realtime API in transcription-only mode) to the stt.StreamingProvider
// interface used by the aiTranscribeStream path.
//
// Unlike OpenAIWhisperProvider (batch -- one transcription at Finalize),
// this path emits interim deltas as the user speaks, so the frontend can
// render word-by-word transcription.
type OpenAIRealtimeProvider struct {
	inner  *openaivoice.ASRClient
	logger *slog.Logger
}

// NewOpenAIRealtimeProvider wraps an existing openai.ASRClient for use via
// stt.StreamingProvider.
func NewOpenAIRealtimeProvider(asr *openaivoice.ASRClient, logger *slog.Logger) *OpenAIRealtimeProvider {
	if logger == nil {
		logger = NewLogger()
	}
	return &OpenAIRealtimeProvider{inner: asr, logger: logger}
}

// Name returns the provider identifier used in log lines and
// AiTranscribeStreamComplete payloads.
func (p *OpenAIRealtimeProvider) Name() string { return "openai-realtime" }

// StartStream opens a streaming transcription session on the Realtime API
// and returns a shared polyphon-asr-backed session.
func (p *OpenAIRealtimeProvider) StartStream(ctx context.Context, config StreamConfig) (StreamingSession, error) {
	if p.inner == nil {
		return nil, fmt.Errorf("openai realtime asr client not configured")
	}

	asrCfg := polyphon.ASRConfig{
		SampleRate: config.SampleRate,
		Language:   config.LanguageHint,
	}
	if asrCfg.SampleRate == 0 {
		asrCfg.SampleRate = 16000
	}
	if asrCfg.Language == "" {
		// OpenAI Realtime expects ISO-639-1 ("en"); "en-US" works too but
		// the shorter form matches what the Polyphon default uses.
		asrCfg.Language = "en"
	}

	asrStream, err := p.inner.StartStream(ctx, asrCfg)
	if err != nil {
		return nil, fmt.Errorf("openai realtime start stream: %w", err)
	}

	return newPolyphonASRSession(asrStream, "openai-realtime", p.logger), nil
}
