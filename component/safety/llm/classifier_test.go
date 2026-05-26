package llm

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/safety"
	"github.com/znasllc-io/memql/core/common"
)

// fakeProvider is a deterministic ChatStructuredProvider for tests.
// Returns the canned `respond` body (raw JSON the model would
// emit), or `err` if non-nil. Counts calls so cache-hit tests can
// assert "the provider was NOT consulted on repeat."
type fakeProvider struct {
	mu      sync.Mutex
	respond string
	err     error
	calls   int
	last    []common.ChatMessage
}

func (p *fakeProvider) CallChatStructured(_ context.Context, messages []common.ChatMessage, _ common.StructuredSchema) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.last = messages
	if p.err != nil {
		return "", p.err
	}
	return p.respond, nil
}

func (p *fakeProvider) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func newClassifierWithProvider(t *testing.T, p common.ChatStructuredProvider) *Classifier {
	t.Helper()
	c, err := NewClassifier(Options{
		Provider:        p,
		Timeout:         500 * time.Millisecond,
		CacheTTL:        time.Hour,
		CacheMaxEntries: 100,
	})
	if err != nil {
		t.Fatalf("NewClassifier: %v", err)
	}
	return c
}

func TestNewClassifierRequiresProvider(t *testing.T) {
	if _, err := NewClassifier(Options{}); err == nil {
		t.Error("NewClassifier should reject nil provider")
	}
}

func TestClassifyHappyPath(t *testing.T) {
	p := &fakeProvider{respond: `{
        "tier": "high",
        "categories": ["destructive", "privilege_escalation"],
        "reason": "rm -rf of a system path",
        "confidence": 0.92
    }`}
	c := newClassifierWithProvider(t, p)
	desc := safety.NewExecAction(safety.SurfaceComputerUseHeadless, "rm -rf /etc", safety.CallerContext{})
	cls, err := c.Classify(context.Background(), desc)
	if err != nil {
		t.Fatalf("classify err: %v", err)
	}
	if cls.Tier != safety.TierHigh {
		t.Errorf("tier: %v", cls.Tier)
	}
	if cls.Source != safety.SourceModel {
		t.Errorf("source: %v", cls.Source)
	}
	if cls.RuleID == "" {
		t.Errorf("rule_id should be stamped for telemetry, got empty")
	}
	if cls.Confidence != 0.92 {
		t.Errorf("confidence: %v", cls.Confidence)
	}
	if len(cls.Categories) != 2 {
		t.Errorf("categories: %v", cls.Categories)
	}
}

func TestClassifyCacheShortCircuitsProvider(t *testing.T) {
	p := &fakeProvider{respond: `{"tier":"low","categories":[],"reason":"ls is benign","confidence":1.0}`}
	c := newClassifierWithProvider(t, p)
	desc := safety.NewExecAction(safety.SurfaceWorkbench, "ls", safety.CallerContext{})

	// First call: provider is consulted.
	if _, err := c.Classify(context.Background(), desc); err != nil {
		t.Fatal(err)
	}
	// Second + third call with identical descriptor: cache hit, no
	// provider call.
	if _, err := c.Classify(context.Background(), desc); err != nil {
		t.Fatal(err)
	}
	cls, err := c.Classify(context.Background(), desc)
	if err != nil {
		t.Fatal(err)
	}
	if p.Calls() != 1 {
		t.Errorf("expected exactly 1 provider call (cache should short-circuit), got %d", p.Calls())
	}
	if cls.Source != safety.SourceCache {
		t.Errorf("repeated lookup should report Source=SourceCache, got %v", cls.Source)
	}
}

func TestClassifyProviderErrorPropagates(t *testing.T) {
	boom := errors.New("provider unreachable")
	p := &fakeProvider{err: boom}
	c := newClassifierWithProvider(t, p)
	_, err := c.Classify(context.Background(),
		safety.NewExecAction(safety.SurfaceWorkbench, "ls", safety.CallerContext{}))
	if !errors.Is(err, boom) {
		t.Errorf("expected wrapped provider error, got %v", err)
	}
}

func TestClassifyNonConformingJSONReturnsError(t *testing.T) {
	p := &fakeProvider{respond: `not a json object`}
	c := newClassifierWithProvider(t, p)
	_, err := c.Classify(context.Background(),
		safety.NewExecAction(safety.SurfaceWorkbench, "ls", safety.CallerContext{}))
	if err == nil || !strings.Contains(err.Error(), "non-conforming JSON") {
		t.Errorf("expected non-conforming JSON error, got %v", err)
	}
}

func TestClassifyClampsConfidenceAndTrimsReason(t *testing.T) {
	long := strings.Repeat("x", 500)
	p := &fakeProvider{respond: `{"tier":"low","categories":[],"reason":"` + long + `","confidence":1.7}`}
	c := newClassifierWithProvider(t, p)
	cls, err := c.Classify(context.Background(),
		safety.NewExecAction(safety.SurfaceWorkbench, "ls", safety.CallerContext{}))
	if err != nil {
		t.Fatal(err)
	}
	if cls.Confidence != 1.0 {
		t.Errorf("confidence should clamp to 1.0, got %v", cls.Confidence)
	}
	if len(cls.Reason) != 200 {
		t.Errorf("reason should clamp to 200 chars, got len=%d", len(cls.Reason))
	}
}

func TestClassifyUnknownTierFallsToNone(t *testing.T) {
	// strict-mode providers should refuse this; but if a provider
	// is misconfigured / non-strict and emits an out-of-enum tier
	// we don't want to crash. Defensive fallback.
	p := &fakeProvider{respond: `{"tier":"severe","categories":[],"reason":"x","confidence":0.5}`}
	c := newClassifierWithProvider(t, p)
	cls, err := c.Classify(context.Background(),
		safety.NewExecAction(safety.SurfaceWorkbench, "ls", safety.CallerContext{}))
	if err != nil {
		t.Fatal(err)
	}
	if cls.Tier != safety.TierNone {
		t.Errorf("unknown tier should fall back to None, got %v", cls.Tier)
	}
}

func TestClassifyPromptCarriesRedactedPayload(t *testing.T) {
	p := &fakeProvider{respond: `{"tier":"none","categories":[],"reason":"ok","confidence":1.0}`}
	c := newClassifierWithProvider(t, p)
	// Tool descriptor with a credential-shaped arg -- must NOT
	// reach the prompt in cleartext.
	desc := safety.NewToolAction(safety.SurfaceToolWebhook, "callX", map[string]any{
		"apiKey": "secret-token-xyz",
	}, safety.CallerContext{})
	if _, err := c.Classify(context.Background(), desc); err != nil {
		t.Fatal(err)
	}
	for _, m := range p.last {
		if strings.Contains(m.Content, "secret-token-xyz") {
			t.Errorf("classifier prompt leaked a secret: %q", m.Content)
		}
	}
}

func TestClassifyTimeoutSurfacesError(t *testing.T) {
	// A provider that blocks forever exercises the per-call
	// context.WithTimeout in Classify.
	p := &slowProvider{}
	c, err := NewClassifier(Options{Provider: p, Timeout: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Classify(context.Background(),
		safety.NewExecAction(safety.SurfaceWorkbench, "ls", safety.CallerContext{}))
	if err == nil {
		t.Error("expected timeout error from blocked provider")
	}
}

// slowProvider blocks until ctx is canceled.
type slowProvider struct{}

func (slowProvider) CallChatStructured(ctx context.Context, _ []common.ChatMessage, _ common.StructuredSchema) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

func TestNewClassifierClampsNegativeMaxEntries(t *testing.T) {
	// A negative CacheMaxEntries would otherwise pass through to
	// verdictCache's "<=0 means unbounded" branch and silently
	// disable the soft cap. Confirm it clamps to the default.
	p := &fakeProvider{respond: `{"tier":"none","categories":[],"reason":"x","confidence":0.5}`}
	c, err := NewClassifier(Options{Provider: p, CacheMaxEntries: -1, CacheTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if c.cache.maxEntries != defaultCacheMaxEntries {
		t.Errorf("negative max should clamp to %d, got %d", defaultCacheMaxEntries, c.cache.maxEntries)
	}
}

func TestEnvIntDefault(t *testing.T) {
	// Missing -> default.
	t.Setenv("UNSET_VAR_FOR_TEST", "")
	if got := envIntDefault("UNSET_VAR_FOR_TEST", 42); got != 42 {
		t.Errorf("missing: got %d, want 42", got)
	}
	// Valid -> parsed.
	t.Setenv("INT_VAR", "7")
	if got := envIntDefault("INT_VAR", 42); got != 7 {
		t.Errorf("valid: got %d, want 7", got)
	}
	// Bad value -> default.
	t.Setenv("BAD_VAR", "xxx")
	if got := envIntDefault("BAD_VAR", 42); got != 42 {
		t.Errorf("bad: got %d, want 42", got)
	}
	// Negative -> default (none of our knobs accept negatives).
	t.Setenv("NEG_VAR", "-1")
	if got := envIntDefault("NEG_VAR", 42); got != 42 {
		t.Errorf("negative: got %d, want 42", got)
	}
}
