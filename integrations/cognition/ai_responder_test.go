package cognition

import (
	"testing"

	"github.com/znasllc-io/memql/component/events"
)

func TestExtractUtteranceFromEventReadsFlattenedSource(t *testing.T) {
	event := events.Event{
		Payload: map[string]any{
			"nodeId":          "utt-1",
			"partitionId":         "space-1",
			"participantId":   "participant-1",
			"text":            "hello",
			"utteranceType":   "speech",
			"source":          map[string]any{"inputMethod": "realtimeVoice", "transcriptOnly": true},
			"participantType": "human",
		},
	}

	got, err := extractUtteranceFromEvent(event)
	if err != nil {
		t.Fatalf("extractUtteranceFromEvent() error = %v", err)
	}

	if got.Source["inputMethod"] != "realtimeVoice" {
		t.Fatalf("expected source.inputMethod=realtimeVoice, got %q", got.Source["inputMethod"])
	}
	if got.Source["transcriptOnly"] != "true" {
		t.Fatalf("expected source.transcriptOnly=true, got %q", got.Source["transcriptOnly"])
	}
}

func TestExtractUtteranceFromEventReadsNestedSource(t *testing.T) {
	event := events.Event{
		Payload: map[string]any{
			"id": "utt-2",
			"payload": map[string]any{
				"partitionId":       "space-2",
				"participantId": "participant-2",
				"text":          "nested payload source",
				"utteranceType": "speech",
				"source": map[string]any{
					"inputMethod": "realtimeVoice",
				},
			},
		},
	}

	got, err := extractUtteranceFromEvent(event)
	if err != nil {
		t.Fatalf("extractUtteranceFromEvent() error = %v", err)
	}

	if got.Source["inputMethod"] != "realtimeVoice" {
		t.Fatalf("expected source.inputMethod=realtimeVoice, got %q", got.Source["inputMethod"])
	}
}

func TestSourceMapFromAnyNilForNonObject(t *testing.T) {
	if got := sourceMapFromAny("not-an-object"); got != nil {
		t.Fatalf("expected nil source map for scalar input, got %#v", got)
	}
}
