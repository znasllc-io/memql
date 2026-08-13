package memql

// Load-time contract tests for memql#3616: a prompt's declared inputs must
// cover what its template reads, and its @defaultProvider must name a
// provider that exists.

import (
	"strings"
	"testing"
	"text/template"
)

// loadCorpusPrompts loads every prompt in the DSL tree, failing the test on
// any load problem -- which, post-fix, includes both new checks.
func loadCorpusPrompts(t *testing.T) (*PromptRegistry, *LoadReport) {
	t.Helper()
	registry := newPromptRegistry()
	rep := newLoadReport()
	partials, err := loadPartials()
	if err != nil {
		t.Fatalf("loadPartials: %v", err)
	}
	if _, err := LoadUnifiedPrompts(discardLogger(), registry, partials, rep); err != nil {
		t.Fatalf("LoadUnifiedPrompts: %v", err)
	}
	return registry, rep
}

func loadCorpusProviders(t *testing.T) *ProviderRegistry {
	t.Helper()
	registry := newProviderRegistry("")
	if _, err := LoadUnifiedProviders(discardLogger(), registry, newLoadReport()); err != nil {
		t.Fatalf("LoadUnifiedProviders: %v", err)
	}
	return registry
}

// TestCorpusPromptDefaultProvidersResolve is the sweep: every prompt in the
// tree names a provider that exists. Before the fix, agentFactoryAnalyze
// named the `strongReasoning` POLICY and this failed.
func TestCorpusPromptDefaultProvidersResolve(t *testing.T) {
	prompts, _ := loadCorpusPrompts(t)
	providers := loadCorpusProviders(t)

	if len(prompts.List()) == 0 {
		t.Fatal("no prompts loaded -- the sweep would pass vacuously")
	}
	if providers.Count() == 0 {
		t.Fatal("no providers loaded -- the sweep would fail spuriously")
	}

	if dangling := findDanglingPromptProviders(prompts, providers); len(dangling) > 0 {
		for _, d := range dangling {
			t.Errorf("prompt %q: @defaultProvider(%q) is not a declared provider", d.Prompt, d.Provider)
		}
	}
	if err := ValidatePromptDefaultProviders(prompts, providers); err != nil {
		t.Errorf("ValidatePromptDefaultProviders: %v", err)
	}
}

// TestDanglingDefaultProviderIsRefused pins the refusal itself: a prompt
// pointing at a name no provider declares must fail the load-time check
// rather than silently resolving to the default provider at call time.
func TestDanglingDefaultProviderIsRefused(t *testing.T) {
	providers := newProviderRegistry("")
	registerParsedProviders(discardLogger(), providers, []parsedProviderConfig{
		mkProviderCfg("openai", "OpenAI", "", "", true, false),
		mkProviderCfg("realProvider", "", "gpt-real", "openai", false, false),
	})

	prompts := newPromptRegistry()
	prompts.set(&PromptTemplate{Name: "goodPrompt", DefaultProvider: "realProvider"})
	prompts.set(&PromptTemplate{Name: "badPrompt", DefaultProvider: "noSuchProvider"})

	err := ValidatePromptDefaultProviders(prompts, providers)
	if err == nil {
		t.Fatal("a prompt with @defaultProvider(\"noSuchProvider\") must be refused at load")
	}
	if !strings.Contains(err.Error(), "badPrompt") || !strings.Contains(err.Error(), "noSuchProvider") {
		t.Errorf("error must name the offending prompt + provider, got: %v", err)
	}
	if strings.Contains(err.Error(), "goodPrompt") {
		t.Errorf("a prompt whose provider resolves must not be flagged, got: %v", err)
	}
}

// TestPolicySlugInProviderSlotIsRefused is the exact memql#3616 shape: the
// name is a real construct in the tree, just the wrong KIND. A policy is not
// a provider and must not pass the provider slot.
func TestPolicySlugInProviderSlotIsRefused(t *testing.T) {
	providers := loadCorpusProviders(t)
	prompts := newPromptRegistry()
	prompts.set(&PromptTemplate{Name: "somePrompt", DefaultProvider: "strongReasoning"})

	if err := ValidatePromptDefaultProviders(prompts, providers); err == nil {
		t.Fatal("@defaultProvider naming the `strongReasoning` POLICY must be refused; " +
			"policies are resolved by the AI router, never by prompt provider resolution")
	}
}

// TestDisabledDefaultProviderStillLoads guards the other side of the line.
// @disabled means "off right now", and the documented contract (#1081) is
// that dependents degrade to the default. That must stay a load-time PASS --
// otherwise turning a keyless vendor lane off would brick every node.
func TestDisabledDefaultProviderStillLoads(t *testing.T) {
	providers := newProviderRegistry("")
	registerParsedProviders(discardLogger(), providers, []parsedProviderConfig{
		mkProviderCfg("openai", "OpenAI", "", "", true, false),
		mkProviderCfg("acme", "Acme", "", "", true, true), // @disabled base
		mkProviderCfg("acmeMini", "", "acme-mini", "acme", false, false),
	})

	if _, registered := providers.Entry("acmeMini"); registered {
		t.Fatal("precondition: a child of a @disabled base must not be registered")
	}
	if !providers.Declared("acmeMini") {
		t.Error("a @disabled provider must still count as DECLARED -- that is what separates " +
			"a deliberately-off lane from a typo")
	}

	prompts := newPromptRegistry()
	prompts.set(&PromptTemplate{Name: "usesDisabled", DefaultProvider: "acmeMini"})
	if err := ValidatePromptDefaultProviders(prompts, providers); err != nil {
		t.Errorf("a prompt pointing at a @disabled provider must still load (it falls back): %v", err)
	}
}

// TestCorpusPromptsLoadWithoutProblems asserts the whole tree is clean under
// both new checks -- no prompt was skipped for reading an undeclared input.
func TestCorpusPromptsLoadWithoutProblems(t *testing.T) {
	_, rep := loadCorpusPrompts(t)
	if rep.HasProblems() {
		t.Errorf("prompt corpus load reported problems:\n%s", rep.Detail())
	}
}

// TestTemplateReadingUndeclaredInputIsRefused is the cognitionPrediction
// shape in miniature: the template renders a field the body never declares,
// so no caller can supply it. The prompt must be refused at load.
func TestTemplateReadingUndeclaredInputIsRefused(t *testing.T) {
	decl := &promptDecl{
		name: "staleDecl",
		fields: []toolField{
			{name: "transcript", typeName: "array", required: true},
		},
	}
	tmpl := template.Must(template.New(decl.name).Parse(
		"{{range .transcript}}{{.text}}{{end}}{{if .phase}}phase: {{.phase}}{{end}}",
	))

	err := validatePromptTemplateFields(decl, tmpl)
	if err == nil {
		t.Fatal("a template reading an undeclared root field must be refused at load")
	}
	if !strings.Contains(err.Error(), "phase") {
		t.Errorf("error must name the undeclared field, got: %v", err)
	}
	// `.text` is read inside {{range}} -- it is an ELEMENT field, not a
	// prompt input, and must not be reported.
	if strings.Contains(err.Error(), "text") {
		t.Errorf("a field read inside {{range}} is element scope, not a prompt input; got: %v", err)
	}
}

// TestPromptTemplateRootFields pins the scope tracking the check depends on.
// Getting this wrong in either direction is worse than having no check:
// under-reporting misses the bug, over-reporting refuses valid prompts.
func TestPromptTemplateRootFields(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{"plain action", `{{ .goal }} and {{.now}}`, []string{"goal", "now"}},
		{"nested path counts only the head", `{{ .space.goal.statement }}`, []string{"space"}},
		{"range rebinds dot", `{{range .transcript}}{{.speakerName}}: {{.text}}{{end}}`, []string{"transcript"}},
		{"range else keeps outer dot", `{{range .rows}}{{.x}}{{else}}{{.fallback}}{{end}}`, []string{"fallback", "rows"}},
		{"with rebinds dot", `{{with .directive}}{{.mode}}{{end}}`, []string{"directive"}},
		{"with else keeps outer dot", `{{with .a}}{{.b}}{{else}}{{.c}}{{end}}`, []string{"a", "c"}},
		{"if keeps dot", `{{if .flag}}{{.other}}{{end}}`, []string{"flag", "other"}},
		{"$ is always root", `{{range .rows}}{{$.owner}}{{end}}`, []string{"owner", "rows"}},
		{"range vars are not root reads", `{{range $i, $d := .domains}}{{$i}}{{$d}}{{end}}`, []string{"domains"}},
		{"function args are walked", `{{if not (and .directive .directive.instruction)}}x{{end}}`, []string{"directive"}},
		{"pipelines are walked", `{{ .count | printf "%d" }}`, []string{"count"}},
		{"comments and text read nothing", `hello {{/* .ghost */}} world`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := template.Must(template.New("t").Parse(tc.src))
			got := promptTemplateRootFields(tmpl)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("promptTemplateRootFields(%q) = %v, want %v", tc.src, got, tc.want)
			}
		})
	}
}

// TestPromptTemplateRootFieldsFollowsPartials covers the one partial-using
// template in the tree. A partial invoked WITHOUT an argument gets a nil dot
// and can read nothing; one invoked with `.` at root scope is reading the
// prompt's own inputs and must be followed.
func TestPromptTemplateRootFieldsFollowsPartials(t *testing.T) {
	set := template.Must(template.New("__base__").Parse(`{{define "part"}}{{ .fromPartial }}{{end}}`))

	noArg := template.Must(template.Must(set.Clone()).New("noArg").Parse(`{{template "part"}}{{ .own }}`))
	if got := promptTemplateRootFields(noArg); strings.Join(got, ",") != "own" {
		t.Errorf("{{template \"part\"}} with no argument passes a nil dot; want [own], got %v", got)
	}

	withDot := template.Must(template.Must(set.Clone()).New("withDot").Parse(`{{template "part" .}}{{ .own }}`))
	if got := promptTemplateRootFields(withDot); strings.Join(got, ",") != "fromPartial,own" {
		t.Errorf("{{template \"part\" .}} forwards the root dot; want [fromPartial own], got %v", got)
	}
}

// TestZeroFieldPromptSkipsTemplateCheck: a prompt with no declared inputs
// compiles to a nil schema, so ValidateData accepts anything and there is no
// rejection to guard against. Holding it to the rule would refuse prompts
// that work fine.
func TestZeroFieldPromptSkipsTemplateCheck(t *testing.T) {
	decl := &promptDecl{name: "staticPrompt"}
	tmpl := template.Must(template.New(decl.name).Parse(`{{ .anything }}`))
	if err := validatePromptTemplateFields(decl, tmpl); err != nil {
		t.Errorf("a prompt with no declared fields has no schema to violate: %v", err)
	}
}
