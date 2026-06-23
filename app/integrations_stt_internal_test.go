//go:build !planner

package app

import "testing"

// The STT bootstrap must resolve the OpenAI key through the same
// prefix-elision chain as the provider auth resolver: the prefixed
// MEMQL_AI_OPENAI_API_KEY wins, the bare MEMQL_OPENAI_API_KEY (the form the
// genesis envelope seeds) is the fallback (#1371).
func TestOpenAIKeyFromEnvFallbackChain(t *testing.T) {
	cases := []struct {
		name     string
		prefixed string
		bare     string
		want     string
	}{
		{"prefixed wins over bare", "sk-prefixed", "sk-bare", "sk-prefixed"},
		{"bare form is the fallback", "", "sk-bare", "sk-bare"},
		{"whitespace-only prefixed falls back", "   ", "sk-bare", "sk-bare"},
		{"neither set", "", "", ""},
		{"values are trimmed", "", "  sk-bare  ", "sk-bare"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MEMQL_AI_OPENAI_API_KEY", tc.prefixed)
			t.Setenv("MEMQL_OPENAI_API_KEY", tc.bare)
			if got := openAIKeyFromEnv(); got != tc.want {
				t.Fatalf("openAIKeyFromEnv() = %q, want %q", got, tc.want)
			}
		})
	}
}
