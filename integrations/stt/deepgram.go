package stt

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/znasllc-io/memql/component/polyphon"
	"github.com/znasllc-io/memql/integrations/deepgram"
)

// DeepgramProvider adapts deepgram.ASRClient (Nova-3 streaming
// WebSocket) to the stt.StreamingProvider interface used by the
// aiTranscribeStream gRPC path. Mirrors OpenAIRealtimeProvider; the
// only differences are the provider name in log lines + the
// Final-transcription payload, and the underlying client.
type DeepgramProvider struct {
	inner  *deepgram.ASRClient
	logger *slog.Logger
}

// NewDeepgramProvider wraps an existing deepgram.ASRClient for use
// via stt.StreamingProvider.
func NewDeepgramProvider(asr *deepgram.ASRClient, logger *slog.Logger) *DeepgramProvider {
	if logger == nil {
		logger = NewLogger()
	}
	return &DeepgramProvider{inner: asr, logger: logger}
}

// Name returns the provider identifier used in log lines and
// AiTranscribeStreamComplete payloads.
func (p *DeepgramProvider) Name() string { return "deepgram" }

// StartStream opens a streaming transcription session against
// Deepgram Nova-3 and returns the shared polyphon-asr-backed session.
func (p *DeepgramProvider) StartStream(ctx context.Context, config StreamConfig) (StreamingSession, error) {
	if p.inner == nil {
		return nil, fmt.Errorf("deepgram asr client not configured")
	}

	asrCfg := polyphon.ASRConfig{
		SampleRate: config.SampleRate,
		Language:   config.LanguageHint,
	}
	if asrCfg.SampleRate == 0 {
		asrCfg.SampleRate = 16000
	}
	// Deepgram accepts BCP-47 tags (en-US, es-MX, ...). The polyphon
	// default of "en-US" matches what the Bridge Agent passes; the
	// empty-language fall-through keeps the default sane when the
	// gRPC stream caller doesn't specify one.
	if asrCfg.Language == "" {
		asrCfg.Language = "en-US"
	}

	asrStream, err := p.inner.StartStream(ctx, asrCfg)
	if err != nil {
		return nil, fmt.Errorf("deepgram start stream: %w", err)
	}

	return newPolyphonASRSession(asrStream, "deepgram", p.logger), nil
}
