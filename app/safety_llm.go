package app

import (
	"context"
	"os"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/safety"
	"github.com/znasllc-io/memql/component/safety/llm"
	"github.com/znasllc-io/memql/component/safety/recorder"
)

// EnvSafetyLLMProvider opts a node into the LLM semantic classifier
// (memql#230). When set + matched by a registered structured-chat SI
// provider, app boot includes the LLM layer in the chain. Unset (or
// unmatched) leaves the rules-only default -- the system runs in
// shadow mode end-to-end, the LLM classifier is opt-in per node.
const EnvSafetyLLMProvider = "MEMQL_SAFETY_LLM_PROVIDER"

// EnvPersistClassifications opts a node OUT of persisting
// v1:safety:classification rows via mutationInsertSafetyClassification
// (memql#234). Default is on (persist whenever the engine is ready).
// Recognised off values: "off" / "false" / "0" (case-insensitive,
// trimmed).
const EnvPersistClassifications = "MEMQL_SAFETY_PERSIST_CLASSIFICATIONS"

// engineMutationRunner adapts memql.MemQLEngine.Execute into the
// safety/recorder.MutationRunner interface. Lives here (in app/)
// because the recorder package stays engine-independent to avoid the
// memql<->safety cycle.
type engineMutationRunner struct{ engine *memql.MemQLEngine }

// RunMutation runs the mutation call string against the engine and
// drops the (any) result -- the recorder only cares about success.
func (e engineMutationRunner) RunMutation(ctx context.Context, query string) error {
	_, err := e.engine.Execute(ctx, query)
	return err
}

// wireSafetyGate composes the per-node command-classifier gate from
// the engine + env knobs:
//
//   - Classifier chain (rules-only OR rules + LLM)
//   - Recorder (slog-only OR slog + persisting v1:safety:classification)
//
// Single SetDefaultGate call -- both halves arrive together so a
// half-wired gate never goes live.
//
// Idempotent + non-fatal on every "can't wire" condition; surface
// gates keep working with the rules-only / slog-only default if
// any half declines. Logs the outcome of each half -- ops can grep
// `safety classifier:` to see which nodes have which layers.
//
// Called from transportBase so the gate is set before any handler
// can dispatch.
func (a *App) wireSafetyGate() {
	classifier := a.buildSafetyClassifier()
	rec := a.buildSafetyRecorder()
	safety.SetDefaultGate(safety.NewGate(safety.GateOptions{
		Classifier: classifier,
		Recorder:   rec,
		Logger:     a.Logger,
	}))
}

// buildSafetyClassifier returns the classifier chain for this node.
// Always includes the rule engine; adds the LLM layer when the env
// var names a registered provider.
func (a *App) buildSafetyClassifier() safety.Classifier {
	rules := safety.NewRuleClassifier(safety.DefaultRules()...)

	providerName := strings.TrimSpace(os.Getenv(EnvSafetyLLMProvider))
	if providerName == "" {
		a.Logger.Info("safety classifier: LLM layer disabled (MEMQL_SAFETY_LLM_PROVIDER unset)",
			"component", "safety")
		return safety.NewChainClassifier(rules, safety.NoopClassifier{})
	}
	if a.engine == nil {
		a.Logger.Warn("safety classifier: engine not ready; LLM layer disabled",
			"component", "safety")
		return safety.NewChainClassifier(rules, safety.NoopClassifier{})
	}
	provider := a.engine.StructuredChatProviderByName(providerName)
	if provider == nil {
		a.Logger.Warn("safety classifier: provider not registered; LLM layer disabled",
			"component", "safety",
			"provider", providerName)
		return safety.NewChainClassifier(rules, safety.NoopClassifier{})
	}
	llmClassifier, err := llm.NewClassifier(llm.Options{Provider: provider})
	if err != nil {
		a.Logger.Warn("safety classifier: NewClassifier failed; LLM layer disabled",
			"component", "safety",
			"error", err)
		return safety.NewChainClassifier(rules, safety.NoopClassifier{})
	}
	a.Logger.Info("safety classifier: LLM layer enabled",
		"component", "safety",
		"provider", providerName,
		"mode", safety.ModeFromEnv())
	return safety.NewChainClassifier(rules, llmClassifier)
}

// buildSafetyRecorder returns the recorder fanout for this node.
// Always includes the slog sink; adds the persisting sink when the
// engine is ready + the env var isn't opted out. The fanout itself
// is panic-safe per sink, so a misconfigured backing store cannot
// crash the dispatch path.
func (a *App) buildSafetyRecorder() safety.DecisionRecorder {
	slogSink := safety.SlogRecorder{Logger: a.Logger}

	off := strings.ToLower(strings.TrimSpace(os.Getenv(EnvPersistClassifications)))
	if off == "off" || off == "false" || off == "0" {
		a.Logger.Info("safety classifier: persistence disabled (MEMQL_SAFETY_PERSIST_CLASSIFICATIONS)",
			"component", "safety")
		return slogSink
	}
	if a.engine == nil {
		a.Logger.Warn("safety classifier: engine not ready; persistence disabled",
			"component", "safety")
		return slogSink
	}
	a.Logger.Info("safety classifier: persistence enabled (v1:safety:classification rows)",
		"component", "safety")
	return safety.NewFanoutRecorder(
		slogSink,
		recorder.New(engineMutationRunner{engine: a.engine}, a.Logger),
	)
}
