package memql

// prompt_default_provider.go holds the load-time check that a prompt's
// `@defaultProvider("...")` names a provider that actually exists.
//
// Why this needs a check at all (memql#3616): `@defaultProvider`, a policy
// slug, and a provider name are all bare identifiers, and nothing at load
// time knew the difference. `agentFactoryAnalyze` carried
// @defaultProvider("strongReasoning") -- a POLICY -- and registered with
// zero reported problems. At runtime resolveProviderName handed that name
// straight through, ChatStructuredProviderByName missed, and the call fell
// through to the hardcoded preferred list: the prompt silently ran on a
// cheap chat model instead of the reasoning chain its author chose. The
// only trace was one INFO line on the structured path and nothing at all
// on the plain chat path.
//
// A downgrade that quiet is worse than a refusal, so this refuses.
//
// The check is deliberately about DECLARATION, not availability:
//
//   - name absent from the tree      -> load error (typo, or a policy slug
//     in a provider slot -- neither can ever resolve).
//   - name declared but @disabled    -> fine. Turning a lane off and having
//     dependents degrade to the default is the documented lifecycle
//     contract (#1081), not a mistake.
//   - name declared, auth unresolved -> fine. A keyless provider registers
//     Available=false with its own WARN; that is an environment fact, not
//     an authoring one, and it must not brick boot.

import (
	"fmt"
	"sort"
	"strings"
)

// danglingPromptProvider names one prompt whose @defaultProvider does not
// resolve to any declared provider.
type danglingPromptProvider struct {
	Prompt   string
	Provider string
}

// findDanglingPromptProviders returns every prompt whose @defaultProvider
// names something the provider tree never declares, sorted by prompt name.
// Split from the error-returning wrapper so tests can assert on the set
// rather than parse a message.
func findDanglingPromptProviders(prompts *PromptRegistry, providers *ProviderRegistry) []danglingPromptProvider {
	if prompts == nil {
		return nil
	}
	var out []danglingPromptProvider
	for _, p := range prompts.List() {
		if p == nil {
			continue
		}
		name := strings.TrimSpace(p.DefaultProvider)
		if name == "" {
			// No @defaultProvider: the runtime default applies. Fine.
			continue
		}
		if providers.Declared(name) || isFleetDefaultProvider(name) {
			continue
		}
		out = append(out, danglingPromptProvider{Prompt: p.Name, Provider: name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Prompt < out[j].Prompt })
	return out
}

// ValidatePromptDefaultProviders refuses the load when any prompt's
// @defaultProvider names a provider the DSL tree does not declare.
//
// Called from engine bootstrap AFTER both registries are populated --
// prompts load before providers, so this cannot live inside the prompt
// loader.
// A `fleet:<modelId>` default provider is exempt from the declared-name check
// (epic memql#4676): a fleet model is not declared anywhere, because it exists
// only while a machine hosting it is awake. Refusing one at load would make an
// asleep fleet refuse BOOT, which is the failure the whole selection-time
// resolution exists to avoid. The name still cannot be a typo in the way this
// gate protects against -- an unrecognised model resolves to an UNAVAILABLE
// provider and produces the typed no_local_model_available refusal naming it,
// rather than silently falling through to the default.
func isFleetDefaultProvider(name string) bool {
	_, ok := IsFleetReference(name)
	return ok
}

func ValidatePromptDefaultProviders(prompts *PromptRegistry, providers *ProviderRegistry) error {
	dangling := findDanglingPromptProviders(prompts, providers)
	if len(dangling) == 0 {
		return nil
	}
	lines := make([]string, 0, len(dangling))
	for _, d := range dangling {
		lines = append(lines, fmt.Sprintf("  prompt %q: @defaultProvider(%q) is not a declared provider", d.Prompt, d.Provider))
	}
	return fmt.Errorf("prompt @defaultProvider validation failed: %d prompt(s) name a provider that does not exist.\n%s\n"+
		"@defaultProvider must name a `provider` declared in dsl/providers/providers.memql -- NOT a `policy` slug "+
		"(policies are consumed by the AI router, not by prompt resolution) and not a free-form label. "+
		"An unresolvable name does not error at call time: it falls through to the default provider, so the prompt "+
		"quietly runs on a model its author did not choose. Fix the name, or drop the annotation to accept the default.",
		len(dangling), strings.Join(lines, "\n"))
}
