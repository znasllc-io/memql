package tools

import (
	"math"
	"testing"
)

func TestArgumentIntegerKeepsSafeDefaultOnOverflow(t *testing.T) {
	const fallback = 100
	for _, tc := range []struct {
		name  string
		value any
		want  int
	}{
		{"decoded count", float64(42), 42},
		{"fraction retains truncation", 42.75, 42},
		{"integer", 42, 42},
		{"signed integer", int64(42), 42},
		{"negative remains caller policy", float64(-1), -1},
		{"positive overflow", 1e30, fallback},
		{"negative overflow", -1e30, fallback},
		{"rounded int limit", float64(math.MaxInt), fallback},
		{"nan", math.NaN(), fallback},
		{"infinity", math.Inf(1), fallback},
		{"unknown", "42", fallback},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := argInt(map[string]any{"limit": tc.value}, "limit", fallback); got != tc.want {
				t.Fatalf("decoded argument = %d, want %d", got, tc.want)
			}
		})
	}
	if got := argInt(nil, "limit", fallback); got != fallback {
		t.Fatalf("missing argument = %d", got)
	}
}
