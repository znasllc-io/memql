package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/safety"
	"github.com/znasllc-io/memql/core/common"
)

// Env knobs for the LLM classifier. Read at NewClassifier time;
// changing them requires a restart. Names + defaults documented
// here so the app-boot wiring + ops runbook share one source.
const (
	// EnvTimeoutMs bounds each LLM call. On timeout the classifier
	// returns an error; the gate's per-surface fail-closed/open
	// posture then decides what to do (computer-use fail-closed in
	// enforce mode; everything else proceeds).
	EnvTimeoutMs = "MEMQL_SAFETY_LLM_TIMEOUT_MS"
	// EnvCacheTTLSeconds is how long a verdict stays cached. 0
	// disables caching entirely. The dominant evictor.
	EnvCacheTTLSeconds = "MEMQL_SAFETY_LLM_CACHE_TTL_SECONDS"
	// EnvCacheMaxEntries is the soft upper bound on cache size.
	// Eviction is expired-first; on overflow with no expired
	// entries, new entries are dropped (the LLM re-classifies on
	// the next miss). 0 = unbounded (not recommended).
	EnvCacheMaxEntries = "MEMQL_SAFETY_LLM_CACHE_MAX_ENTRIES"
)

const (
	defaultTimeoutMs       = 5_000
	defaultCacheTTLSeconds = 3600 // 1 hour
	defaultCacheMaxEntries = 10_000
)

// Options configure NewClassifier. Zero values pick the defaults
// (which read from env). Tests inject everything; production reads
// from env via app boot.
type Options struct {
	// Provider is the structured-output SI provider. Required.
	Provider common.ChatStructuredProvider
	// Timeout bounds the per-call LLM round-trip. 0 = read from
	// EnvTimeoutMs (default 5s).
	Timeout time.Duration
	// CacheTTL is how long a verdict stays cached. 0 = read from
	// EnvCacheTTLSeconds. Negative disables caching.
	CacheTTL time.Duration
	// CacheMaxEntries soft-bounds the cache. 0 = read from
	// EnvCacheMaxEntries.
	CacheMaxEntries int
	// Now is the time source the cache uses. nil = time.Now.
	// Injectable for tests.
	Now func() time.Time
}

// Classifier is a safety.Classifier that asks an LLM provider to
// judge actions the rule layer didn't decide. Slot it into a
// safety.ChainClassifier AFTER the rule classifier (#228) -- the
// rules short-circuit the obvious cases in microseconds and only
// what they escalate (ErrNoClassifierOpinion) reaches the model.
//
// Concurrency: safe for concurrent use. The cache is internally
// synchronised; the provider is invoked once per cache miss.
type Classifier struct {
	provider common.ChatStructuredProvider
	timeout  time.Duration
	cache    *verdictCache
}

// NewClassifier constructs a Classifier from the given Options.
// Returns an error when Provider is nil -- the package can't reach
// the SI surface without one.
func NewClassifier(opts Options) (*Classifier, error) {
	if opts.Provider == nil {
		return nil, errors.New("safety/llm: Provider is required")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = time.Duration(envIntDefault(EnvTimeoutMs, defaultTimeoutMs)) * time.Millisecond
	}
	ttl := opts.CacheTTL
	if ttl == 0 {
		ttl = time.Duration(envIntDefault(EnvCacheTTLSeconds, defaultCacheTTLSeconds)) * time.Second
	}
	maxEntries := opts.CacheMaxEntries
	if maxEntries == 0 {
		maxEntries = envIntDefault(EnvCacheMaxEntries, defaultCacheMaxEntries)
	}
	cache := newVerdictCache(ttl, maxEntries)
	if opts.Now != nil {
		cache.now = opts.Now
	}
	return &Classifier{
		provider: opts.Provider,
		timeout:  timeout,
		cache:    cache,
	}, nil
}

// modelVerdict is the parsed strict-JSON shape the provider returns.
// Mirrors classifierSchemaJSON; the field types are forgiving (we
// re-validate / clamp on the way out) so a slightly-off model
// response doesn't crash the dispatch path.
type modelVerdict struct {
	Tier       string   `json:"tier"`
	Categories []string `json:"categories"`
	Reason     string   `json:"reason"`
	Confidence float64  `json:"confidence"`
}

// Classify implements safety.Classifier. Cache lookup short-circuits
// the LLM call; on miss, calls the provider with a bounded timeout
// and caches the verdict. Any provider error (network / timeout /
// schema violation) propagates so the gate's per-surface
// fail-closed semantics can apply.
func (c *Classifier) Classify(ctx context.Context, desc safety.ActionDescriptor) (safety.Classification, error) {
	start := time.Now()
	key := descriptorCacheKey(desc)

	if cls, ok := c.cache.Get(key); ok {
		// Cache hit: stamp the latency as the in-process lookup
		// cost so observability sees how cheap hits are.
		cls.LatencyMs = sinceMs(start)
		return cls, nil
	}

	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	messages := []common.ChatMessage{
		{Role: "system", Content: classifierSystemPrompt},
		{Role: "user", Content: renderUserPrompt(desc)},
	}
	raw, err := c.provider.CallChatStructured(callCtx, messages, classifierSchema)
	if err != nil {
		return safety.Classification{LatencyMs: sinceMs(start), Source: safety.SourceModel},
			fmt.Errorf("safety/llm: provider call failed: %w", err)
	}

	var v modelVerdict
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return safety.Classification{LatencyMs: sinceMs(start), Source: safety.SourceModel},
			fmt.Errorf("safety/llm: provider returned non-conforming JSON: %w", err)
	}

	cls := classificationFromModel(v)
	cls.Source = safety.SourceModel
	cls.RuleID = "model.classify_v1"
	cls.LatencyMs = sinceMs(start)
	c.cache.Put(key, cls)
	return cls, nil
}

// classificationFromModel maps the provider's JSON shape into a
// safety.Classification. Tier + Categories are validated against the
// enums; an unknown value falls through to a defensive default
// rather than a hard error -- the model already produced a verdict
// the strict-mode provider accepted, so unknowns are rare. Confidence
// is clamped to [0, 1].
func classificationFromModel(v modelVerdict) safety.Classification {
	tier, ok := safety.ParseRiskTier(strings.ToLower(strings.TrimSpace(v.Tier)))
	if !ok {
		tier = safety.TierNone
	}
	cats := make([]safety.Category, 0, len(v.Categories))
	for _, c := range v.Categories {
		c = strings.ToLower(strings.TrimSpace(c))
		if c == "" {
			continue
		}
		// Trust the schema's enum to have caught invalid values.
		cats = append(cats, safety.Category(c))
	}
	conf := v.Confidence
	switch {
	case conf < 0:
		conf = 0
	case conf > 1:
		conf = 1
	}
	reason := strings.TrimSpace(v.Reason)
	if len(reason) > 200 {
		reason = reason[:200]
	}
	return safety.Classification{
		Tier:       tier,
		Categories: cats,
		Reason:     reason,
		Confidence: conf,
	}
}

// envIntDefault reads an env var as an int with the given default.
// Returns the default on missing / unparseable / negative values
// (negative would be nonsensical for any of our knobs).
func envIntDefault(name string, def int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return n
}

func sinceMs(t time.Time) int64 {
	d := time.Since(t)
	if d < 0 {
		return 0
	}
	return d.Milliseconds()
}
