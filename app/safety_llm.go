package app

import (
	"os"
	"strings"

	"github.com/znasllc-io/memql/component/safety"
	"github.com/znasllc-io/memql/component/safety/llm"
)

// EnvSafetyLLMProvider is the env var that opts a node into the LLM
// semantic classifier (memql#230). When set + matched by a
// registered SI provider, app boot upgrades the process-wide
// safety.DefaultGate to include the LLM layer in the chain. Unset
// (or unmatched) leaves the default rules-only gate -- which is the
// safe default: the system runs in shadow mode end-to-end, the LLM
// classifier is opt-in per node.
const EnvSafetyLLMProvider = "MEMQL_SAFETY_LLM_PROVIDER"

// wireSafetyClassifier upgrades the safety.DefaultGate to include
// the LLM classifier when MEMQL_SAFETY_LLM_PROVIDER names a
// registered provider. Chain shape:
//
//	ChainClassifier(
//	    RuleClassifier(DefaultRules()...),  // microsecond fast-path
//	    LLMClassifier{provider},            // ambiguous middle
//	)
//
// Idempotent (safe to call from multiple transport entry points);
// silent + non-fatal on any "can't wire" condition so the surface
// gates keep working with the rules-only default. Logs the outcome
// either way -- ops can grep for "safety classifier:" to see which
// nodes have the LLM layer.
func (a *App) wireSafetyClassifier() {
	providerName := strings.TrimSpace(os.Getenv(EnvSafetyLLMProvider))
	if providerName == "" {
		a.Logger.Info("safety classifier: LLM layer disabled (MEMQL_SAFETY_LLM_PROVIDER unset)",
			"component", "safety")
		return
	}
	if a.engine == nil {
		a.Logger.Warn("safety classifier: engine not ready; LLM layer disabled",
			"component", "safety")
		return
	}
	provider := a.engine.StructuredChatProviderByName(providerName)
	if provider == nil {
		a.Logger.Warn("safety classifier: provider not registered; LLM layer disabled",
			"component", "safety",
			"provider", providerName)
		return
	}
	llmClassifier, err := llm.NewClassifier(llm.Options{Provider: provider})
	if err != nil {
		a.Logger.Warn("safety classifier: NewClassifier failed; LLM layer disabled",
			"component", "safety",
			"error", err)
		return
	}
	chain := safety.NewChainClassifier(
		safety.NewRuleClassifier(safety.DefaultRules()...),
		llmClassifier,
	)
	safety.SetDefaultGate(safety.NewGate(safety.GateOptions{
		Classifier: chain,
		Logger:     a.Logger,
	}))
	a.Logger.Info("safety classifier: LLM layer enabled",
		"component", "safety",
		"provider", providerName,
		"mode", safety.ModeFromEnv())
}
