package memql

import (
	"io"
	"log/slog"
	"testing"
)

// TestRegisterParsedProviders_DisabledSkipAndPropagation locks the
// provider-lifecycle epic (#1080) loader rules:
//   - a @disabled @base is never registered;
//   - every child that @extends a disabled base is skipped (propagation);
//   - an individually @disabled child is skipped on its own;
//   - enabled peers (base + children) remain registered;
//   - skipped providers do not count toward the registered total.
func TestRegisterParsedProviders_DisabledSkipAndPropagation(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	reg := newProviderRegistry("")

	mk := func(name, typ, model, extends string, base, disabled bool) parsedProviderConfig {
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

	all := []parsedProviderConfig{
		// Enabled vendor lane: base + child.
		mk("openai", "OpenAI", "", "", true, false),
		mk("chatMini", "", "gpt-mini", "openai", false, false),
		// Disabled base + a child that extends it (propagation): both gone.
		mk("google", "Google", "", "", true, true),
		mk("geminiFlash", "", "gemini-flash", "google", false, false),
		// Individually @disabled child of an ENABLED base: just this one gone.
		mk("chatDisabled", "", "gpt-x", "openai", false, true),
	}

	total := registerParsedProviders(logger, reg, all)

	// Enabled base + child stay registered.
	if _, ok := reg.Entry("openai"); !ok {
		t.Error("enabled base 'openai' should be registered")
	}
	if _, ok := reg.Entry("chatMini"); !ok {
		t.Error("enabled child 'chatMini' should be registered")
	}

	// Disabled base is absent from the registry.
	if _, ok := reg.Entry("google"); ok {
		t.Error("@disabled base 'google' should be absent from the registry")
	}
	// Child of a disabled base is absent (propagation) even though it
	// carries no annotation of its own.
	if _, ok := reg.Entry("geminiFlash"); ok {
		t.Error("'geminiFlash' extends disabled base 'google' and should be absent (propagation)")
	}
	// Individually disabled child is absent; its enabled sibling/base is not.
	if _, ok := reg.Entry("chatDisabled"); ok {
		t.Error("individually @disabled child 'chatDisabled' should be absent")
	}

	// Only the enabled base + enabled child count: 2.
	if total != 2 {
		t.Errorf("registered total = %d, want 2 (openai base + chatMini child; disabled lane excluded)", total)
	}
}
