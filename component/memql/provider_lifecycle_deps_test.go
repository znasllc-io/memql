package memql

import (
	"io"
	"log/slog"
	"testing"
)

// mkProviderCfg builds a parsedProviderConfig for the lifecycle
// dependency tests. A literal apiKey keeps newAIProvider off the env.
func mkProviderCfg(name, typ, model, extends string, base, disabled bool) parsedProviderConfig {
	return parsedProviderConfig{
		cfg: &ProviderConfig{
			Name:     name,
			Type:     typ,
			Model:    model,
			Extends:  extends,
			Base:     base,
			Disabled: disabled,
			Auth:     map[string]string{"apiKey": "literal-test-key"},
		},
		origin: "test:" + name,
	}
}

// TestPolicyChainSkipsDisabledPrimary is the Story C (#1081) policy-chain
// acceptance: a policy whose @primary provider is @disabled (and hence
// absent from the registry) still routes via its @fallback. This
// exercises the exact selection the router's chain walk performs --
// first present + Available entry wins -- against a registry seeded the
// way the loader (#1080) leaves it.
func TestPolicyChainSkipsDisabledPrimary(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	reg := newProviderRegistry("")

	registerParsedProviders(logger, reg, []parsedProviderConfig{
		mkProviderCfg("openai", "OpenAI", "", "", true, false),
		// Primary is individually @disabled -> never registered.
		mkProviderCfg("streamPrimary", "", "gpt-primary", "openai", false, true),
		// Fallback is enabled -> registered + available.
		mkProviderCfg("streamFallback", "", "gpt-fallback", "openai", false, false),
	})

	// Disabled primary is absent; enabled fallback is present + available.
	if _, ok := reg.Entry("streamPrimary"); ok {
		t.Error("@disabled @primary 'streamPrimary' should be absent from the registry")
	}
	fb, ok := reg.Entry("streamFallback")
	if !ok {
		t.Fatal("enabled @fallback 'streamFallback' should be registered")
	}
	if !fb.Available {
		t.Error("enabled @fallback 'streamFallback' should be Available for selection")
	}

	// Replicate the router's chain walk: first present + available entry
	// in [primary, fallback] order wins. The disabled primary is skipped,
	// the fallback is selected -- no error, no nil-deref.
	policy := PolicyConfig{Primary: "streamPrimary", Fallbacks: []string{"streamFallback"}}
	chain := policy.ProviderChain()
	if len(chain) != 2 || chain[0] != "streamPrimary" || chain[1] != "streamFallback" {
		t.Fatalf("ProviderChain = %v, want [streamPrimary streamFallback]", chain)
	}
	var selected string
	for _, name := range chain {
		if e, ok := reg.Entry(name); ok && e.Available {
			selected = name
			break
		}
	}
	if selected != "streamFallback" {
		t.Errorf("chain walk selected %q, want streamFallback (primary disabled -> skip -> fallback)", selected)
	}
}

// TestPromptDefaultProviderFallsBackWhenDisabled is the Story C (#1081)
// prompt acceptance: a prompt whose @defaultProvider has been @disabled
// (absent from the registry) resolves to nil via the by-name lookup and
// thus falls through to the engine's default structured provider -- no
// crash, no nil-deref. We assert the registry primitive the fallback in
// generateStructured depends on: by-name lookup of an absent provider
// returns nil, while an enabled provider resolves.
func TestPromptDefaultProviderFallsBackWhenDisabled(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	reg := newProviderRegistry("")

	registerParsedProviders(logger, reg, []parsedProviderConfig{
		mkProviderCfg("openai", "OpenAI", "", "", true, false),
		// A keyless vendor lane the prompt used to point at, now disabled.
		mkProviderCfg("acme", "Acme", "", "", true, true),
		mkProviderCfg("acmeMini", "", "acme-mini", "acme", false, false),
	})

	// The disabled provider (and its propagated child) resolve to nil --
	// the caller (generateStructured) treats nil as "fall back to default".
	if got := reg.ChatStructuredProviderByName("acme"); got != nil {
		t.Error("ChatStructuredProviderByName(disabled 'acme') should be nil so the prompt falls back")
	}
	if got := reg.ChatStructuredProviderByName("acmeMini"); got != nil {
		t.Error("ChatStructuredProviderByName('acmeMini', child of disabled base) should be nil")
	}
	// An unknown name is equally graceful (no panic, nil result).
	if got := reg.ChatStructuredProviderByName("doesNotExist"); got != nil {
		t.Error("ChatStructuredProviderByName(unknown) should be nil, not panic")
	}
}
