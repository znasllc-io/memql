package polyphon

import (
	"math"
	"testing"
)

func TestPatternBasedPrediction(t *testing.T) {
	tests := []struct {
		name           string
		phase          ConversationPhase
		wantTopics     bool
		wantNextAgent  bool
	}{
		{"greeting phase anticipates introductions", PhaseGreeting, true, true},
		{"exploration phase anticipates domain questions", PhaseExploration, true, true},
		{"task phase anticipates follow-ups", PhaseTask, true, true},
		{"follow_up phase anticipates new topics", PhaseFollowUp, true, true},
		{"winding_down phase suggests summarize", PhaseWindingDown, false, true},
	}

	candidates := []AgentCandidate{
		{ID: "a1", Name: "Sofia"},
		{ID: "a2", Name: "Rex"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := NewSession("space-pred-1")
			session.StateMachine = NewConversationStateMachine()
			session.StateMachine.mu.Lock()
			session.StateMachine.phase = tt.phase
			session.StateMachine.mu.Unlock()

			state := patternBasedPrediction(session, candidates)
			if state == nil {
				t.Fatal("expected non-nil PredictiveState")
			}
			if state.Confidence != 0.4 {
				t.Errorf("expected confidence 0.4, got %.2f", state.Confidence)
			}
			if tt.wantTopics && len(state.AnticipatedTopics) == 0 {
				t.Error("expected anticipated topics")
			}
			if tt.wantNextAgent && state.LikelyNextAgent == "" {
				t.Error("expected likely next agent")
			}
		})
	}
}

func TestPatternBasedPrediction_LikelyAgentFewestTurns(t *testing.T) {
	session := NewSession("space-pred-2")
	session.StateMachine = NewConversationStateMachine()
	session.mu.Lock()
	session.AgentTurnCounts = map[string]int{"a1": 5, "a2": 1}
	session.mu.Unlock()

	candidates := []AgentCandidate{
		{ID: "a1", Name: "Sofia"},
		{ID: "a2", Name: "Rex"},
	}

	state := patternBasedPrediction(session, candidates)
	if state.LikelyNextAgent != "Rex" {
		t.Errorf("expected Rex (fewest turns), got %s", state.LikelyNextAgent)
	}
}

func TestParsePredictionResult(t *testing.T) {
	t.Run("nil returns nil", func(t *testing.T) {
		state, err := parsePredictionResult(nil)
		if err != nil || state != nil {
			t.Errorf("expected nil/nil, got %v/%v", state, err)
		}
	})

	t.Run("valid JSON string", func(t *testing.T) {
		input := `{"anticipatedTopics":["billing","support"],"likelyNextAgent":"Rex","confidence":0.85,"conversationPhase":"task"}`
		state, err := parsePredictionResult(input)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(state.AnticipatedTopics) != 2 {
			t.Errorf("expected 2 topics, got %d", len(state.AnticipatedTopics))
		}
		if state.LikelyNextAgent != "Rex" {
			t.Errorf("expected Rex, got %s", state.LikelyNextAgent)
		}
		if state.Confidence != 0.85 {
			t.Errorf("expected confidence 0.85, got %.2f", state.Confidence)
		}
	})

	t.Run("markdown-fenced JSON", func(t *testing.T) {
		input := "```json\n{\"likelyNextAgent\":\"Sofia\",\"confidence\":0.7}\n```"
		state, err := parsePredictionResult(input)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if state.LikelyNextAgent != "Sofia" {
			t.Errorf("expected Sofia, got %s", state.LikelyNextAgent)
		}
	})

	t.Run("map input", func(t *testing.T) {
		input := map[string]any{
			"likelyNextAgent": "Kai",
			"confidence":      0.6,
			"anticipatedTopics": []any{"design"},
		}
		state, err := parsePredictionResult(input)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if state.LikelyNextAgent != "Kai" {
			t.Errorf("expected Kai, got %s", state.LikelyNextAgent)
		}
	})

	t.Run("invalid type returns error", func(t *testing.T) {
		_, err := parsePredictionResult(42)
		if err == nil {
			t.Error("expected error for int input")
		}
	})

	t.Run("invalid JSON string returns error", func(t *testing.T) {
		_, err := parsePredictionResult("not json")
		if err == nil {
			t.Error("expected error for invalid JSON")
		}
	})
}

func TestCosineSimilarityF32(t *testing.T) {
	tests := []struct {
		name string
		a, b []float32
		want float64
		tol  float64
	}{
		{"identical vectors", []float32{1, 0, 0}, []float32{1, 0, 0}, 1.0, 0.01},
		{"orthogonal vectors", []float32{1, 0, 0}, []float32{0, 1, 0}, 0.0, 0.01},
		{"similar vectors", []float32{1, 1, 0}, []float32{1, 0, 0}, 0.707, 0.01},
		{"empty vectors", []float32{}, []float32{}, 0.0, 0.01},
		{"different lengths", []float32{1, 0}, []float32{1, 0, 0}, 0.0, 0.01},
		{"zero vector a", []float32{0, 0, 0}, []float32{1, 0, 0}, 0.0, 0.01},
		{"zero vector b", []float32{1, 0, 0}, []float32{0, 0, 0}, 0.0, 0.01},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cosineSimilarityF32(tt.a, tt.b)
			if math.Abs(got-tt.want) > tt.tol {
				t.Errorf("cosineSimilarityF32: got %.4f, want %.4f", got, tt.want)
			}
		})
	}
}

func TestPhasePatternLibrary_DetectPhase(t *testing.T) {
	t.Run("empty embedding returns empty phase", func(t *testing.T) {
		lib := NewPhasePatternLibrary()
		phase, confidence := lib.DetectPhase(nil)
		if phase != "" || confidence != 0 {
			t.Errorf("expected empty phase/0, got %s/%.2f", phase, confidence)
		}
	})

	t.Run("uninitialised patterns return empty (no embeddings)", func(t *testing.T) {
		lib := NewPhasePatternLibrary()
		// Patterns exist but have no embeddings yet.
		phase, confidence := lib.DetectPhase([]float32{1, 0, 0})
		if phase != "" || confidence != 0 {
			t.Errorf("expected empty when patterns have no embeddings, got %s/%.2f", phase, confidence)
		}
	})
}
