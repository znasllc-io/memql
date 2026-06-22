package memql

import (
	"os"
	"strconv"
	"strings"
)

const (
	defaultMaxResults = 500
	defaultMaxWindow  = 5000
	// defaultListCap is the implicit row cap the engine applies to a
	// list-returning query that arrives WITHOUT an explicit window --
	// no `paginate` and no `sort` directive (epic 5, memql#1965). It is
	// the runtime backstop for the pagination authoring rule: even if an
	// unmarked list query slips past authoring, it can never pull more
	// than this many rows. A query that paginates / sorts states its own
	// window and is unaffected; a query marked `@unbounded("reason")` is
	// rewritten to an explicit paginate and likewise bypasses the cap.
	// Override via MEMORY_ENGINE_DEFAULT_LIST_CAP. Tunable but always
	// clamped to <= MaxResults (it is a tighter default, never a wider
	// one).
	defaultListCap            = 50
	defaultCacheSize          = int64(1024)
	defaultCacheMaxTTLSeconds = int64(0)

	defaultAICacheEnabled    = true
	defaultAICacheMaxSeconds = 60
	maxAICacheSeconds        = 300

	// Agent tool loop: one iteration = one LLM round, during which the
	// model may emit up to maxToolCallsPerIt tool calls. A UI-driving
	// flow (Operator taking over the app) can easily chain 10-15 calls
	// per turn -- describe + request control + nav clicks + form field
	// inputs + highlight + release. Bumped to 40 iterations + 6 calls
	// per iteration = 240 calls/turn headroom. The iteration counter
	// resets on each user message, so this is a per-turn budget, not
	// a conversation-wide one. Cost / latency scale with actual use,
	// not the ceiling.
	// Tool-loop caps apply to BOTH the engine-level AI tool loop
	// (component/memql/si_tool_loop.go) AND the agent-node streaming
	// loop (integrations/agent/streaming.go). Both read the same env
	// var so there is exactly one knob to turn for walkthrough
	// length. Defaults sized for multi-field walkthroughs + upcoming
	// teaching sessions (see docs/planning/teaching-mode.md):
	// one field cycle is roughly 3-4 iterations (narrate + ask +
	// click|type|select). A single Create Agent walkthrough fits in
	// ~40 iterations; a chained "create agent + create space" flow
	// measured at 54 iterations in practice. 120 gives ~2x headroom
	// for longer multi-step chains and future teaching sessions with
	// explanatory pauses. Override via MEMQL_TOOL_LOOP_MAX_ITERATIONS.
	defaultAIToolLoopMaxIterations     = 120
	defaultAIToolLoopMaxToolCallsPerIt = 8
	// Upper bound sanity-checker. Teaching sessions may plausibly
	// climb past 100 iterations when chaining many narrate-ask-click
	// cycles; 200 is the ceiling before we assume the agent is stuck
	// in a loop and bail.
	maxAIToolLoopMaxIterations     = 200
	maxAIToolLoopMaxToolCallsPerIt = 12

	defaultWebhookTimeoutSeconds = 30
	maxWebhookTimeoutSeconds     = 600

	envMaxResults           = "MEMORY_ENGINE_MAX_RESULTS"
	envMaxWindow            = "MEMORY_ENGINE_MAX_WINDOW"
	envDefaultListCap       = "MEMORY_ENGINE_DEFAULT_LIST_CAP"
	envCacheSize            = "MEMORY_ENGINE_CACHE_MAX_ITEMS"
	envCacheMaxTTL          = "CACHE_MAX_TTL"
	envAICacheDefaultEnable = "MEMQL_SI_CACHE_DEFAULT_ENABLED"
	envAICacheMaxSeconds    = "MEMQL_SI_CACHE_MAX_SECONDS"
	// Canonical names for the unified tool-loop knobs. The agent
	// streaming loop in integrations/agent/streaming.go reads the same
	// two env vars so there's exactly one place to tune walkthrough
	// length + per-iteration call fanout.
	envAIToolLoopMaxIterations     = "MEMQL_TOOL_LOOP_MAX_ITERATIONS"
	envAIToolLoopMaxToolCallsPerIt = "MEMQL_TOOL_LOOP_MAX_TOOL_CALLS_PER_ITERATION"
	envWebhookTimeoutSeconds       = "MEMQL_WEBHOOK_TIMEOUT_SECONDS"
	envWebhookAllowedHosts         = "MEMQL_WEBHOOK_ALLOWED_HOSTS"
)

type engineConfig struct {
	MaxResults                  int
	MaxWindow                   int
	DefaultListCap              int
	CacheSize                   int64
	CacheMaxTTLSeconds          int64
	AIToolLoopMaxIterations     int
	AIToolLoopMaxToolCallsPerIt int
	WebhookTimeoutSeconds       int
	WebhookAllowedHosts         []string
}

type aiCacheConfig struct {
	DefaultEnabled bool
	MaxTTLSeconds  int
}

func loadEngineConfigFromEnv() engineConfig {
	cfg := engineConfig{
		MaxResults:                  defaultMaxResults,
		MaxWindow:                   defaultMaxWindow,
		DefaultListCap:              defaultListCap,
		CacheSize:                   defaultCacheSize,
		CacheMaxTTLSeconds:          defaultCacheMaxTTLSeconds,
		AIToolLoopMaxIterations:     defaultAIToolLoopMaxIterations,
		AIToolLoopMaxToolCallsPerIt: defaultAIToolLoopMaxToolCallsPerIt,
		WebhookTimeoutSeconds:       defaultWebhookTimeoutSeconds,
	}

	if value, ok := os.LookupEnv(envMaxResults); ok {
		if parsed, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && parsed > 0 {
			cfg.MaxResults = parsed
		}
	}

	if value, ok := os.LookupEnv(envMaxWindow); ok {
		if parsed, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && parsed > cfg.MaxResults {
			cfg.MaxWindow = parsed
		}
	}

	if value, ok := os.LookupEnv(envDefaultListCap); ok {
		if parsed, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && parsed > 0 {
			cfg.DefaultListCap = parsed
		}
	}
	// The implicit list cap is a TIGHTER default than MaxResults, never a
	// wider one -- clamp it so a misconfigured override can't quietly
	// widen the backstop above the page-size ceiling.
	if cfg.DefaultListCap <= 0 || cfg.DefaultListCap > cfg.MaxResults {
		cfg.DefaultListCap = cfg.MaxResults
	}

	if value, ok := os.LookupEnv(envCacheSize); ok {
		if parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil && parsed > 0 {
			cfg.CacheSize = parsed
		}
	}

	if value, ok := os.LookupEnv(envCacheMaxTTL); ok {
		if parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil && parsed >= 0 {
			cfg.CacheMaxTTLSeconds = parsed
		}
	}

	if value, ok := os.LookupEnv(envAIToolLoopMaxIterations); ok {
		if parsed, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
			cfg.AIToolLoopMaxIterations = clampAIToolLoopMaxIterations(parsed)
		}
	}

	if value, ok := os.LookupEnv(envAIToolLoopMaxToolCallsPerIt); ok {
		if parsed, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
			cfg.AIToolLoopMaxToolCallsPerIt = clampAIToolLoopMaxToolCallsPerIt(parsed)
		}
	}

	cfg.AIToolLoopMaxIterations = clampAIToolLoopMaxIterations(cfg.AIToolLoopMaxIterations)
	cfg.AIToolLoopMaxToolCallsPerIt = clampAIToolLoopMaxToolCallsPerIt(cfg.AIToolLoopMaxToolCallsPerIt)

	if value, ok := os.LookupEnv(envWebhookTimeoutSeconds); ok {
		if parsed, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
			cfg.WebhookTimeoutSeconds = clampWebhookTimeout(parsed)
		}
	}
	cfg.WebhookTimeoutSeconds = clampWebhookTimeout(cfg.WebhookTimeoutSeconds)

	if value, ok := os.LookupEnv(envWebhookAllowedHosts); ok {
		hosts := strings.Split(value, ",")
		for _, h := range hosts {
			h = strings.TrimSpace(h)
			if h != "" {
				cfg.WebhookAllowedHosts = append(cfg.WebhookAllowedHosts, h)
			}
		}
	}

	return cfg
}

func loadAICacheConfigFromEnv() aiCacheConfig {
	cfg := aiCacheConfig{
		DefaultEnabled: defaultAICacheEnabled,
		MaxTTLSeconds:  defaultAICacheMaxSeconds,
	}

	if value, ok := os.LookupEnv(envAICacheDefaultEnable); ok {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "1", "true", "yes", "on":
			cfg.DefaultEnabled = true
		case "0", "false", "no", "off":
			cfg.DefaultEnabled = false
		}
	}

	if value, ok := os.LookupEnv(envAICacheMaxSeconds); ok {
		if parsed, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
			cfg.MaxTTLSeconds = parsed
		}
	}

	cfg.MaxTTLSeconds = clampAICacheSeconds(cfg.MaxTTLSeconds)
	return cfg
}

func clampAICacheSeconds(value int) int {
	if value <= 0 {
		return 0
	}
	if value > maxAICacheSeconds {
		return maxAICacheSeconds
	}
	return value
}

func clampAIToolLoopMaxIterations(value int) int {
	if value <= 0 {
		return defaultAIToolLoopMaxIterations
	}
	if value > maxAIToolLoopMaxIterations {
		return maxAIToolLoopMaxIterations
	}
	return value
}

func clampAIToolLoopMaxToolCallsPerIt(value int) int {
	if value <= 0 {
		return defaultAIToolLoopMaxToolCallsPerIt
	}
	if value > maxAIToolLoopMaxToolCallsPerIt {
		return maxAIToolLoopMaxToolCallsPerIt
	}
	return value
}

func clampWebhookTimeout(value int) int {
	if value <= 0 {
		return defaultWebhookTimeoutSeconds
	}
	if value > maxWebhookTimeoutSeconds {
		return maxWebhookTimeoutSeconds
	}
	return value
}
