package openai

import (
	"log/slog"
	"testing"

	"github.com/znasllc-io/memql/component/polyphon"
)

// Compile-time interface compliance check.
var _ polyphon.ASRProvider = (*ASRClient)(nil)

func TestNewASRClient_MissingAPIKey(t *testing.T) {
	cfg := DefaultConfig()
	// APIKey intentionally left empty.

	_, err := NewASRClient(cfg)
	if err == nil {
		t.Fatal("expected error for missing API key, got nil")
	}
}

func TestNewASRClient_DefaultModel(t *testing.T) {
	cfg := DefaultConfig()
	cfg.APIKey = "test-key"

	client, err := NewASRClient(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client.model != defaultASRModel {
		t.Errorf("expected default model %q, got %q", defaultASRModel, client.model)
	}
}

func TestNewASRClient_CustomModel(t *testing.T) {
	cfg := DefaultConfig()
	cfg.APIKey = "test-key"
	cfg.ASRModel = "gpt-audio-mini"

	client, err := NewASRClient(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client.model != "gpt-audio-mini" {
		t.Errorf("expected model gpt-audio-mini, got %s", client.model)
	}
}

// TestHandleEvent_AccumulatesInterimDeltas locks in the contract that each
// interim ASRResult.Text carries the FULL accumulated transcript so far,
// not just the newest token. This is the semantics the frontend depends on
// -- see the interimBuf comment on openaiASRStream.
func TestHandleEvent_AccumulatesInterimDeltas(t *testing.T) {
	s := &openaiASRStream{
		results: make(chan polyphon.ASRResult, 16),
		logger:  slog.New(slog.DiscardHandler),
	}

	// Simulate the Realtime API sending 4 token-level deltas for one utterance.
	deltas := []string{
		`{"type":"conversation.item.input_audio_transcription.delta","delta":"the"}`,
		`{"type":"conversation.item.input_audio_transcription.delta","delta":" quick"}`,
		`{"type":"conversation.item.input_audio_transcription.delta","delta":" brown"}`,
		`{"type":"conversation.item.input_audio_transcription.delta","delta":" fox"}`,
	}
	for _, d := range deltas {
		s.handleEvent([]byte(d))
	}

	// Drain -- we expect 4 interim results with monotonically-growing text.
	want := []string{"the", "the quick", "the quick brown", "the quick brown fox"}
	for i, w := range want {
		select {
		case r := <-s.results:
			if r.IsFinal {
				t.Errorf("delta %d: expected interim, got final", i)
			}
			if r.Text != w {
				t.Errorf("delta %d: Text = %q, want %q", i, r.Text, w)
			}
		default:
			t.Fatalf("delta %d: no result emitted", i)
		}
	}
}

// TestHandleEvent_CompletedResetsInterim verifies the accumulator resets
// between utterances (server_vad emits one .completed per utterance under
// transcription-only mode).
func TestHandleEvent_CompletedResetsInterim(t *testing.T) {
	s := &openaiASRStream{
		results: make(chan polyphon.ASRResult, 16),
		logger:  slog.New(slog.DiscardHandler),
	}

	// Utterance 1: two deltas + completed.
	s.handleEvent([]byte(`{"type":"conversation.item.input_audio_transcription.delta","delta":"hey"}`))
	s.handleEvent([]byte(`{"type":"conversation.item.input_audio_transcription.delta","delta":" sofia"}`))
	s.handleEvent([]byte(`{"type":"conversation.item.input_audio_transcription.completed","transcript":"Hey, Sofia."}`))

	// Drain utterance 1 results.
	for range 3 {
		<-s.results
	}

	// Utterance 2: must start from empty buffer, not continue from utterance 1.
	s.handleEvent([]byte(`{"type":"conversation.item.input_audio_transcription.delta","delta":"what"}`))

	select {
	case r := <-s.results:
		if r.Text != "what" {
			t.Errorf("after completed, new utterance delta Text = %q, want %q (accumulator did not reset)", r.Text, "what")
		}
	default:
		t.Fatal("expected interim result after completed reset")
	}
}

// TestHandleEvent_FinalUsesCompletedTranscript verifies that the terminal
// .completed event emits the normalized transcript (not the raw delta
// concatenation), since OpenAI sometimes applies punctuation / corrections.
func TestHandleEvent_FinalUsesCompletedTranscript(t *testing.T) {
	s := &openaiASRStream{
		results: make(chan polyphon.ASRResult, 16),
		logger:  slog.New(slog.DiscardHandler),
	}

	s.handleEvent([]byte(`{"type":"conversation.item.input_audio_transcription.delta","delta":"hey sofia"}`))
	<-s.results // drain interim
	s.handleEvent([]byte(`{"type":"conversation.item.input_audio_transcription.completed","transcript":"Hey, Sofia!"}`))

	select {
	case r := <-s.results:
		if !r.IsFinal {
			t.Error("expected IsFinal=true on completed event")
		}
		if r.Text != "Hey, Sofia!" {
			t.Errorf("final Text = %q, want %q (should use completed.transcript, not interim buffer)", r.Text, "Hey, Sofia!")
		}
	default:
		t.Fatal("expected final result on completed")
	}
}
