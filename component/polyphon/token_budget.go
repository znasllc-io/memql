package polyphon

// TokenBudget represents the available token space for conversation context.
type TokenBudget struct {
	ContextWindow int // Total context window (e.g., 128000)
	MaxOutput     int // Reserved for response (e.g., 4096)
	SystemPrompt  int // Estimated system prompt tokens (e.g., 2000)
	SafetyMargin  int // Buffer (e.g., 1000)
	Available     int // Computed: ContextWindow - MaxOutput - SystemPrompt - SafetyMargin
}

// CalculateTokenBudget computes the available token budget for conversation history.
func CalculateTokenBudget(contextWindow, maxOutput, systemPromptEst int) TokenBudget {
	safetyMargin := 1000
	available := contextWindow - maxOutput - systemPromptEst - safetyMargin
	if available < 0 {
		available = 0
	}
	return TokenBudget{
		ContextWindow: contextWindow,
		MaxOutput:     maxOutput,
		SystemPrompt:  systemPromptEst,
		SafetyMargin:  safetyMargin,
		Available:     available,
	}
}

// EstimateTokens provides a fast token estimate (characters / 4).
// Within ~20% of actual token count for English text.
func EstimateTokens(text string) int {
	return len(text) / 4
}

// DefaultContextWindows maps known model prefixes to their context window sizes.
// Used as fallback when contextWindow is not specified in the provider config.
// Keep this in sync with providers/v1/openai/*.memql -- the provider file's
// contextWindow param is the source of truth at runtime; this table is
// consulted only when a caller asks for the budget without a loaded
// provider config.
var DefaultContextWindows = map[string]int{
	// GPT-5.4 family (flagship)
	"gpt-5.4":             128000,
	"gpt-5.4-pro":         256000,
	"gpt-5.4-mini":        128000,
	"gpt-5.4-nano":        32000,
	"gpt-5.3-chat-latest": 128000,
	// Reasoning (o-series)
	"o4-mini": 200000,
	// Codex (coding-specialized)
	"gpt-5.3-codex":     256000,
	"gpt-5.1-codex-max": 400000,
	// Search
	"gpt-5-search-api": 128000,
	// Deep research
	"o4-mini-deep-research": 200000,
	"o3-deep-research":      200000,
	// Anthropic
	"claude-opus-4-6":   200000,
	"claude-sonnet-4-6": 200000,
	"claude-haiku-4-5":  200000,
}

// LookupContextWindow returns the context window for a model name.
// First checks the lookup table by exact match, then by prefix match.
// Falls back to 32000 for unknown models (conservative default).
func LookupContextWindow(model string) int {
	if cw, ok := DefaultContextWindows[model]; ok {
		return cw
	}
	// Prefix match for versioned models (e.g., "claude-haiku-4-5-20251001").
	for prefix, cw := range DefaultContextWindows {
		if len(model) > len(prefix) && model[:len(prefix)] == prefix {
			return cw
		}
	}
	return 32000 // Conservative fallback for unknown models.
}

// CompactionLevel determines how aggressively to compact based on budget usage.
type CompactionLevel int

const (
	CompactionNormal     CompactionLevel = iota // Budget < 70%: full context
	CompactionAggressive                        // 70-90%: reduced context
	CompactionEmergency                         // 90-100%: minimal context
	CompactionReset                             // Overflow: start fresh
)

// DetermineCompactionLevel returns the appropriate compaction level based on
// estimated token usage vs available budget.
func DetermineCompactionLevel(estimatedTokens, availableBudget int) CompactionLevel {
	if availableBudget <= 0 {
		return CompactionReset
	}
	ratio := float64(estimatedTokens) / float64(availableBudget)
	switch {
	case ratio >= 1.0:
		return CompactionReset
	case ratio >= 0.9:
		return CompactionEmergency
	case ratio >= 0.7:
		return CompactionAggressive
	default:
		return CompactionNormal
	}
}
