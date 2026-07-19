package memql

import (
	"io"
	"log/slog"
	"testing"
	"testing/fstest"
	"text/template"

	memqldsl "github.com/znasllc-io/memql/dsl"
)

// #2606: @disabled must actually disable tools, seeds, and prompts. All three
// kinds tolerated the annotation as a documented no-op (tool_decl.go,
// seed_decl.go, prompt_converter.go), which is the worst state: authors pay
// the tokens and get nothing. These tests mirror the provider-lifecycle
// precedent (TestRegisterParsedProviders_DisabledSkipAndPropagation): a
// @disabled decl is skipped at load, an enabled peer registers, and the
// skipped decl does not count toward the registered total.

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestLoadUnifiedTools_DisabledSkipped(t *testing.T) {
	overlay := fstest.MapFS{"tools.memql": {Data: []byte(`@disabled
@handler(type="query", query="builtin help(name: \"$args.name\")")
@description("retired probe tool")
tool retiredProbeTool {
  name string @required @description("function name")
}

@enabled
@handler(type="query", query="builtin help(name: \"$args.name\")")
@description("live probe tool")
tool liveProbeTool {
  name string @required @description("function name")
}
`)}}
	const domain = "lifecycledisabledtools"
	memqldsl.RegisterTree(domain, overlay)
	t.Cleanup(func() { memqldsl.UnregisterTree(domain) })

	registry := newToolRegistry()
	if _, err := LoadUnifiedTools(discardLogger(), registry); err != nil {
		t.Fatalf("LoadUnifiedTools: %v", err)
	}
	if registry.Has("retiredProbeTool") {
		t.Error("@disabled tool registered; @disabled must skip tool registration")
	}
	if !registry.Has("liveProbeTool") {
		t.Error("enabled peer tool did not register")
	}
}

func TestLoadUnifiedPrompts_DisabledSkipped(t *testing.T) {
	overlay := fstest.MapFS{
		"prompts.memql": {Data: []byte(`@disabled
@templateFile("retired.tmpl")
@description("retired probe prompt")
prompt retiredProbePrompt {
  subject string @required @description("subject")
}

@enabled
@templateFile("live.tmpl")
@description("live probe prompt")
prompt liveProbePrompt {
  subject string @required @description("subject")
}
`)},
		"retired.tmpl": {Data: []byte("retired {{.subject}}")},
		"live.tmpl":    {Data: []byte("live {{.subject}}")},
	}
	const domain = "lifecycledisabledprompts"
	memqldsl.RegisterTree(domain, overlay)
	t.Cleanup(func() { memqldsl.UnregisterTree(domain) })

	registry := newPromptRegistry()
	if _, err := LoadUnifiedPrompts(discardLogger(), registry, template.New("partials")); err != nil {
		t.Fatalf("LoadUnifiedPrompts: %v", err)
	}
	if _, ok := registry.Get("retiredProbePrompt"); ok {
		t.Error("@disabled prompt registered; @disabled must skip prompt registration")
	}
	if _, ok := registry.Get("liveProbePrompt"); !ok {
		t.Error("enabled peer prompt did not register")
	}
}

func TestLoadUnifiedSeeds_DisabledSkipped(t *testing.T) {
	overlay := fstest.MapFS{"seeds.memql": {Data: []byte(`@disabled
seed role retiredProbeRole {
  slug: "retired-probe-role"
  name: "Retired Probe Role"
}

@enabled
seed role liveProbeRole {
  slug: "live-probe-role"
  name: "Live Probe Role"
}
`)}}
	const domain = "lifecycledisabledseeds"
	memqldsl.RegisterTree(domain, overlay)
	t.Cleanup(func() { memqldsl.UnregisterTree(domain) })

	registry := NewSeedRegistry()
	if _, err := LoadUnifiedSeeds(discardLogger(), registry); err != nil {
		t.Fatalf("LoadUnifiedSeeds: %v", err)
	}
	if _, ok := registry.Get("retiredProbeRole"); ok {
		t.Error("@disabled seed registered; @disabled must skip seed registration")
	}
	if _, ok := registry.Get("liveProbeRole"); !ok {
		t.Error("enabled peer seed did not register")
	}
}
