package stt

import (
	"context"
	"log/slog"
	"testing"
)

func TestOpenAIRealtimeProvider_Name(t *testing.T) {
	p := NewOpenAIRealtimeProvider(nil, slog.New(slog.DiscardHandler))
	if got := p.Name(); got != "openai-realtime" {
		t.Errorf("Name() = %q, want %q", got, "openai-realtime")
	}
}

func TestOpenAIRealtimeProvider_StartStream_NilClient(t *testing.T) {
	p := NewOpenAIRealtimeProvider(nil, slog.New(slog.DiscardHandler))
	_, err := p.StartStream(context.Background(), StreamConfig{})
	if err == nil {
		t.Fatal("expected error when inner client is nil")
	}
}

// Session-level behavior (SendAudio, interim/final forwarding, Finalize,
// Close idempotency) is covered by the shared polyphonASRSession tests in
// polyphon_session_test.go. This file only covers the provider wrapper
// surface; adding duplicate session-level tests here would test the same
// helper twice.

// Compile-time assertion that OpenAIRealtimeProvider satisfies the stt
// provider interface used by the transcribe-stream path.
var _ StreamingProvider = (*OpenAIRealtimeProvider)(nil)
