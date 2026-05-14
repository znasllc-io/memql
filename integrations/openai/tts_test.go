package openai

import (
	"context"
	"testing"

	"github.com/visionarys-io/memql/component/polyphon"
)

// Compile-time interface compliance check.
var _ polyphon.TTSProvider = (*TTSClient)(nil)

func TestNewTTSClient_MissingAPIKey(t *testing.T) {
	cfg := DefaultConfig()
	// APIKey intentionally left empty.

	_, err := NewTTSClient(cfg)
	if err == nil {
		t.Fatal("expected error for missing API key, got nil")
	}
}

func TestNewTTSClient_DefaultModel(t *testing.T) {
	cfg := DefaultConfig()
	cfg.APIKey = "test-key"

	client, err := NewTTSClient(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client.model != defaultTTSModel {
		t.Errorf("expected default model %q, got %q", defaultTTSModel, client.model)
	}
	if client.voice != defaultTTSVoice {
		t.Errorf("expected default voice %q, got %q", defaultTTSVoice, client.voice)
	}
}

func TestNewTTSClient_CustomConfig(t *testing.T) {
	cfg := Config{
		APIKey:   "test-key",
		TTSModel: "tts-1-hd",
		TTSVoice: "nova",
	}

	client, err := NewTTSClient(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client.model != "tts-1-hd" {
		t.Errorf("expected model tts-1-hd, got %s", client.model)
	}
	if client.voice != "nova" {
		t.Errorf("expected voice nova, got %s", client.voice)
	}
}

func TestTTSClient_AvailableVoices(t *testing.T) {
	cfg := DefaultConfig()
	cfg.APIKey = "test-key"

	client, err := NewTTSClient(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	voices, err := client.AvailableVoices(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(voices) != 6 {
		t.Errorf("expected 6 voices, got %d", len(voices))
	}

	// Verify expected voices are present.
	expected := map[string]bool{"alloy": false, "nova": false, "coral": false, "sage": false, "echo": false, "shimmer": false}
	for _, v := range voices {
		if _, ok := expected[v.ID]; ok {
			expected[v.ID] = true
		}
	}
	for name, found := range expected {
		if !found {
			t.Errorf("expected voice %q not found", name)
		}
	}
}

func TestTTSClient_SynthesizeEmptyText(t *testing.T) {
	cfg := DefaultConfig()
	cfg.APIKey = "test-key"

	client, err := NewTTSClient(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = client.Synthesize(context.Background(), polyphon.TTSConfig{Text: ""})
	if err == nil {
		t.Fatal("expected error for empty text, got nil")
	}
}
