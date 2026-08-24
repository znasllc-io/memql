package memql

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// Keyless boot is quiet: one INFO line, not a warning per provider
// (epic memql#4440, task memql#4442, design sections D2 and D3).
//
// WHAT IS ACTUALLY BEING DEFENDED. Not a log format -- an operator's ability
// to believe this component's warnings. After memql#4441 the ordinary install
// supplies no AI provider key at all, so "no provider resolved" is the state a
// correctly-installed cluster boots in. Emitting one WARN per provider in that
// state means a dozen warnings on every node on every boot about nothing being
// wrong, which is how a component teaches people to filter it out.
//
// The risk the design named (D3) is the inverse: quieting could hide a
// MISCONFIGURED provider. That is why the rule keys off whether ANY provider
// resolved rather than off a flag -- and why TestPartialConfigStillWarns
// exists, because it is the half that can silently rot.

// allLevelsHandler records EVERY record, unlike the package's captureHandler
// which is WARN-and-above (dsl_load_test.go). That difference is the point:
// this epic moved detail DOWN to DEBUG and put a summary at INFO, so a handler
// that cannot see below WARN would report the quieting as a total silence and
// pass a change that had simply deleted the guidance.
//
// It reuses the package's capturedRecord shape so the two capture helpers stay
// comparable.
type allLevelsHandler struct {
	mu      sync.Mutex
	records []capturedRecord
}

func (h *allLevelsHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *allLevelsHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := map[string]any{}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, capturedRecord{Level: r.Level, Message: r.Message, Attrs: attrs})
	return nil
}

func (h *allLevelsHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *allLevelsHandler) WithGroup(string) slog.Handler      { return h }

func (h *allLevelsHandler) at(level slog.Level) []capturedRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []capturedRecord
	for _, r := range h.records {
		if r.Level == level {
			out = append(out, r)
		}
	}
	return out
}

// providerWarns returns only the WARNs this loader emits about availability --
// not every WARN in the run, so an unrelated warning cannot make a test about
// provider noise fail for the wrong reason.
func (h *allLevelsHandler) providerWarns() []capturedRecord {
	var out []capturedRecord
	for _, r := range h.at(slog.LevelWarn) {
		if strings.Contains(r.Message, "registered as unavailable") {
			out = append(out, r)
		}
	}
	return out
}

// unresolvable builds a provider whose auth placeholder names an env var that
// is not set -- the exact shape a keyless cluster produces, rather than a
// synthetic failure. `${...}` is what resolveAuthPlaceholders looks for.
func unresolvable(name, extends string, base bool) parsedProviderConfig {
	cfg := &ProviderConfig{
		Name:    name,
		Extends: extends,
		Base:    base,
		Auth:    map[string]string{"apiKey": "${MEMQL_AI_TEST_KEY_DEFINITELY_NOT_SET_4440}"},
	}
	if base {
		cfg.Type = "OpenAI"
	} else {
		cfg.Model = name + "-model"
	}
	return parsedProviderConfig{cfg: cfg, origin: "test:" + name}
}

// resolvable builds a provider that both resolves its auth AND constructs a
// client -- a literal key needs no env, and "OpenAI" is a real dispatch case
// whose constructor makes no network call. Both halves are required: auth
// resolution alone leaves Available=false, which is the unavailable state this
// helper exists to be the opposite of.
func resolvable(name, extends string) parsedProviderConfig {
	return parsedProviderConfig{
		cfg: &ProviderConfig{
			Name:    name,
			Type:    "OpenAI",
			Extends: extends,
			Model:   name + "-model",
			Auth:    map[string]string{"apiKey": "literal-test-key"},
		},
		origin: "test:" + name,
	}
}

func TestKeylessBootIsOneInfoLine(t *testing.T) {
	// NOTHING RESOLVED -- a freshly installed cluster, by design.
	h := &allLevelsHandler{}
	logger := slog.New(h)
	reg := newProviderRegistry("")

	registerParsedProviders(logger, reg, []parsedProviderConfig{
		unresolvable("openai", "", true),
		unresolvable("chatA", "openai", false),
		unresolvable("chatB", "openai", false),
		unresolvable("chatC", "openai", false),
	})

	warns := h.providerWarns()
	if len(warns) != 0 {
		t.Errorf("keyless boot emitted %d provider WARNs; it must emit none: %+v", len(warns), warns)
	}

	var summaries []capturedRecord
	for _, r := range h.at(slog.LevelInfo) {
		if r.Message == KeylessBootSummary {
			summaries = append(summaries, r)
		}
	}
	if len(summaries) != 1 {
		t.Fatalf("expected exactly ONE keyless summary line at INFO, got %d", len(summaries))
	}
	// The count is an attribute rather than baked into the sentence, so a
	// structured-log consumer can read it without parsing prose.
	// slog widens ints, so compare through a formatted value rather than
	// against an untyped 3 -- which compares false against int64(3) and turns
	// this into a test of slog's boxing.
	if got := fmt.Sprint(summaries[0].Attrs["unavailable"]); got != "3" {
		t.Errorf("summary reported unavailable=%v; the three children are unavailable", got)
	}
	// THE GUIDANCE HALF. Quieting that does not say where to go has traded a
	// wall of warnings for silence.
	if !strings.Contains(KeylessBootSummary, "Settings -> AI providers") {
		t.Errorf("the summary does not tell the operator where to configure providers: %q", KeylessBootSummary)
	}

	// The detail is not lost -- it moved to DEBUG.
	var debugDetail int
	for _, r := range h.at(slog.LevelDebug) {
		if strings.Contains(r.Message, "registered as unavailable") {
			debugDetail++
		}
	}
	if debugDetail != 3 {
		t.Errorf("per-provider detail should move to DEBUG, got %d records", debugDetail)
	}
}

func TestKeylessBootStillRegistersEveryProvider(t *testing.T) {
	// Quieter, not emptier. Selection needs the entries present-and-unavailable
	// so `providerAuthStatus` and `provider-auth check` can say WHY, and so a
	// later reload has something to re-resolve.
	h := &allLevelsHandler{}
	reg := newProviderRegistry("")
	total := registerParsedProviders(slog.New(h), reg, []parsedProviderConfig{
		unresolvable("openai", "", true),
		unresolvable("chatA", "openai", false),
	})
	if total != 2 {
		t.Fatalf("expected base + child registered, got %d", total)
	}
	entry, ok := reg.Entry("chatA")
	if !ok {
		t.Fatal("an unresolvable provider must still be REGISTERED, as unavailable")
	}
	if entry.Available {
		t.Error("a provider whose key does not resolve must not be available")
	}
	if entry.err == nil {
		t.Error("the entry must carry why it is unavailable, or nothing can report it")
	}
}

func TestPartialConfigStillWarns(t *testing.T) {
	// THE HALF-CONFIGURED STATE, which is the one worth shouting about:
	// somebody seeded a credential and it did not take. This is the assertion
	// that keeps D3's risk from materialising -- if this ever goes green while
	// the WARNs are gone, quieting has started hiding real misconfiguration.
	h := &allLevelsHandler{}
	reg := newProviderRegistry("")

	registerParsedProviders(slog.New(h), reg, []parsedProviderConfig{
		unresolvable("openai", "", true),
		resolvable("chatGood", "openai"),
		unresolvable("chatBad", "openai", false),
	})

	warns := h.providerWarns()
	if len(warns) != 1 {
		t.Fatalf("a half-configured node must WARN per unavailable provider; got %d: %+v", len(warns), warns)
	}
	if got := warns[0].Attrs["provider"]; got != "chatBad" {
		t.Errorf("the WARN names %v; it must name the provider that failed", got)
	}

	for _, r := range h.at(slog.LevelInfo) {
		if r.Message == KeylessBootSummary {
			t.Error("a node with a working provider must not claim AI providers are not configured")
		}
	}
}

func TestFullyConfiguredNodeSaysNothing(t *testing.T) {
	h := &allLevelsHandler{}
	reg := newProviderRegistry("")
	registerParsedProviders(slog.New(h), reg, []parsedProviderConfig{
		resolvable("chatGood", ""),
	})
	if n := len(h.providerWarns()); n != 0 {
		t.Errorf("a fully configured node emitted %d availability WARNs", n)
	}
	for _, r := range h.at(slog.LevelInfo) {
		if r.Message == KeylessBootSummary {
			t.Error("a fully configured node claimed AI providers are not configured")
		}
	}
}

func TestDisabledProvidersDoNotCountAsUnconfigured(t *testing.T) {
	// A @disabled lane is OFF, not broken. It resolves no auth and registers
	// nothing (#1080), so it must not appear in the unavailable tally either --
	// otherwise a cluster with one working provider and one deliberately
	// disabled vendor would read as half-configured and warn about a lane
	// somebody turned off on purpose.
	h := &allLevelsHandler{}
	reg := newProviderRegistry("")

	disabledBase := unresolvable("acme", "", true)
	disabledBase.cfg.Disabled = true

	registerParsedProviders(slog.New(h), reg, []parsedProviderConfig{
		disabledBase,
		unresolvable("acmeChat", "acme", false),
		resolvable("chatGood", ""),
	})

	if n := len(h.providerWarns()); n != 0 {
		t.Errorf("a disabled lane produced %d availability WARNs; it must produce none", n)
	}
}

// TestRealTreeBootsKeylessAndQuiet is the boot-level assertion behind D2's
// "keyless boot is a first-class state": it walks the REAL provider tree the
// engine loads, with no credential reachable through any of the three
// resolution tiers, and holds the whole outcome to account.
//
// AGAINST THE REAL TREE, not fixtures, because the claim is about what a node
// prints on a freshly installed cluster -- and the number of providers, their
// vendors, and which of them share a base are all facts of
// dsl/providers/providers.memql. A fixture would let that file grow a provider
// that warns on its own without this noticing.
func TestRealTreeBootsKeylessAndQuiet(t *testing.T) {
	// ALL THREE TIERS CLOSED. Auth resolves globalSecret -> globalVariable ->
	// OS env, so blanking the environment alone would leave two tiers open and
	// the test would pass or fail on whatever a previous test left in the
	// package-level resolvers.
	prevSecret, prevVariable := systemSecretResolver, systemVariableResolver
	SetSystemSecretResolver(nil)
	SetSystemVariableResolver(nil)
	t.Cleanup(func() {
		SetSystemSecretResolver(prevSecret)
		SetSystemVariableResolver(prevVariable)
	})

	// Every name the resolver could try, blanked for the duration. t.Setenv
	// restores them, and an empty value reads as absent (the resolver trims
	// and checks for "").
	for _, name := range []string{
		"MEMQL_AI_ANTHROPIC_API_KEY", "MEMQL_ANTHROPIC_API_KEY", "ANTHROPIC_API_KEY",
		"MEMQL_SI_ANTHROPIC_API_KEY",
		"MEMQL_AI_OPENAI_API_KEY", "MEMQL_OPENAI_API_KEY", "OPENAI_API_KEY",
		"MEMQL_SI_OPENAI_API_KEY",
		"MEMQL_AI_ANTHROPIC_FEDERATION_RULE_ID", "MEMQL_AI_ANTHROPIC_ORGANIZATION_ID",
		"MEMQL_AI_ANTHROPIC_SERVICE_ACCOUNT_ID", "MEMQL_AI_ANTHROPIC_WORKSPACE_ID",
		"MEMQL_AI_ANTHROPIC_IDENTITY_TOKEN_FILE",
		"MEMQL_AI_OPENAI_PROJECT_ID", "MEMQL_SI_OPENAI_PROJECT_ID",
	} {
		t.Setenv(name, "")
	}

	h := &allLevelsHandler{}
	registry := newProviderRegistry("")
	total, err := LoadUnifiedProviders(slog.New(h), registry)
	if err != nil {
		t.Fatalf("the provider tree failed to load with no credentials: %v", err)
	}

	// HEALTHY: the tree loaded and every provider is present.
	if total == 0 {
		t.Fatal("no providers registered at all -- this test would then prove nothing about quieting")
	}

	// REGISTERED-UNAVAILABLE: present, callable by nobody, and each carrying
	// why. Bases are Available=false on purpose (they are @extends targets,
	// not callable), so they are excluded by looking only at entries that
	// carry a model.
	var callable, unavailable int
	for _, name := range registry.Names() {
		entry, ok := registry.Entry(name)
		if !ok || entry == nil || entry.Config.Base {
			continue
		}
		if entry.Available {
			callable++
			continue
		}
		unavailable++
		if entry.err == nil {
			t.Errorf("provider %q is unavailable but carries no reason", name)
		}
	}
	if callable != 0 {
		t.Errorf("%d providers are callable with no credential configured anywhere", callable)
	}
	if unavailable == 0 {
		t.Fatal("no unavailable providers -- the instrument could not have moved")
	}

	// NO WARN-LEVEL SPAM. The assertion this epic exists for.
	if warns := h.providerWarns(); len(warns) != 0 {
		t.Errorf("keyless boot emitted %d provider WARNs against the real tree: %+v", len(warns), warns)
	}

	// ONE line, and it counts what it found.
	var summaries []capturedRecord
	for _, r := range h.at(slog.LevelInfo) {
		if r.Message == KeylessBootSummary {
			summaries = append(summaries, r)
		}
	}
	if len(summaries) != 1 {
		t.Fatalf("expected exactly one keyless summary line, got %d", len(summaries))
	}
	if got := fmt.Sprint(summaries[0].Attrs["unavailable"]); got != fmt.Sprint(unavailable) {
		t.Errorf("summary says %s unavailable; the registry holds %d", got, unavailable)
	}
}

// TestPromptDefaultProviderFallsBackQuietly is D2's spot-check: selection
// paths degrade per the existing @disabled/fallback semantics rather than
// erroring, so an automation firing on a keyless cluster fails soft.
//
// It asserts the SHAPE of the degradation, not a log line: asking a registry
// with nothing available for a chat provider returns nil, and nil is what
// every caller already branches on. An error here would be the regression --
// it would turn "no provider configured" into a failure path on a cluster that
// is working exactly as installed.
func TestPromptDefaultProviderFallsBackQuietly(t *testing.T) {
	h := &allLevelsHandler{}
	reg := newProviderRegistry("")
	registerParsedProviders(slog.New(h), reg, []parsedProviderConfig{
		unresolvable("openai", "", true),
		unresolvable("chatA", "openai", false),
	})

	// A name that IS registered but unavailable, and a name that is not
	// registered at all: both must answer nil rather than panic or error.
	if p := reg.ChatProvider("chatA"); p != nil {
		t.Error("an unavailable provider was handed out as callable")
	}
	if p := reg.ChatProvider("providerThatDoesNotExist"); p != nil {
		t.Error("an unregistered name resolved to a provider")
	}
	if p := reg.ChatProvider(""); p != nil {
		t.Error("the default resolved to a provider on a keyless node")
	}
	if d := reg.Default(); d != "" {
		t.Errorf("a keyless registry named %q as its default; nothing is callable", d)
	}
}
