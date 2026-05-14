package cognition

import (
	"testing"

	"github.com/visionarys-io/memql/component/polyphon"
)

func TestIsTranscriptOnlyRealtimeUtterance(t *testing.T) {
	tests := []struct {
		name   string
		source map[string]string
		want   bool
	}{
		{"nil source", nil, false},
		{"empty source", map[string]string{}, false},
		{"realtimeVoice inputMethod", map[string]string{"inputMethod": "realtimeVoice"}, true},
		{"transcriptOnly true", map[string]string{"transcriptOnly": "true"}, true},
		{"transcriptOnly True (case insensitive)", map[string]string{"transcriptOnly": "True"}, true},
		{"regular text", map[string]string{"inputMethod": "text"}, false},
		{"keyboard input", map[string]string{"inputMethod": "keyboard"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTranscriptOnlyRealtimeUtterance(tt.source); got != tt.want {
				t.Errorf("isTranscriptOnlyRealtimeUtterance(%v) = %v, want %v", tt.source, got, tt.want)
			}
		})
	}
}

func TestMustJSON(t *testing.T) {
	tests := []struct {
		name  string
		input any
		want  string
	}{
		{"nil", nil, "null"},
		{"string", "hello", `"hello"`},
		{"int", 42, "42"},
		{"map", map[string]any{"key": "value"}, `{"key":"value"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mustJSON(tt.input); got != tt.want {
				t.Errorf("mustJSON(%v) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestBuildAgentCandidatesEmpty(t *testing.T) {
	// Without an engine, buildAgentCandidates should return empty candidates for any participants.
	c := &CognitionIntegration{}
	candidates := c.buildAgentCandidates(nil, nil)
	if len(candidates) != 0 {
		t.Errorf("expected 0 candidates for nil participants, got %d", len(candidates))
	}

	candidates = c.buildAgentCandidates(nil, []*participantPayload{})
	if len(candidates) != 0 {
		t.Errorf("expected 0 candidates for empty participants, got %d", len(candidates))
	}
}

func TestCognitionDecisionHasWinner(t *testing.T) {
	// Ensure we correctly use the ScoreDecision.s HasWinner() method.
	noWinner := &polyphon.ScoreDecision{
		Action: "silence",
	}
	if noWinner.HasWinner() {
		t.Error("expected no winner for silence decision")
	}

	withWinner := &polyphon.ScoreDecision{
		Action: "respond",
		Winner: &polyphon.AgentScore{
			AgentId:       "agent-1",
			AgentName:     "Sofia",
			TotalScore:    75.0,
			ShouldRespond: true,
			Reason:        "Direct address",
		},
	}
	if !withWinner.HasWinner() {
		t.Error("expected winner for respond decision")
	}

	// Winner exists but ShouldRespond is false
	belowThreshold := &polyphon.ScoreDecision{
		Action: "respond",
		Winner: &polyphon.AgentScore{
			AgentId:       "agent-1",
			AgentName:     "Sofia",
			TotalScore:    20.0,
			ShouldRespond: false,
			Reason:        "Below threshold",
		},
	}
	if belowThreshold.HasWinner() {
		t.Error("expected no winner when ShouldRespond is false")
	}
}
