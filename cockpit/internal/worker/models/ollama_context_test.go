package models

import (
	"encoding/json"
	"math"
	"testing"
)

func TestOllamaContextLengthRejectsUnrepresentableCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		want  int
	}{
		{"decoded whole tokens", float64(131072), 131072},
		{"preserved json number", json.Number("131072"), 131072},
		{"fractional tokens", 131072.5, 0},
		{"negative", float64(-1), 0},
		{"nan", math.NaN(), 0},
		{"infinity", math.Inf(1), 0},
		{"rounded signed limit", float64(math.MaxInt64), 0},
		{"huge magnitude", 1e30, 0},
		{"json overflow", json.Number("9223372036854775808"), 0},
		{"json fractional", json.Number("131072.5"), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ollamaContextLength(map[string]any{"model.context_length": tc.value}); got != tc.want {
				t.Fatalf("context length = %d, want %d", got, tc.want)
			}
		})
	}
}
