package app

import (
	"context"
	"os"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/safety"
	"github.com/znasllc-io/memql/component/safety/llm"
	"github.com/znasllc-io/memql/component/safety/recorder"
	workspine "github.com/znasllc-io/memql/integrations/work"
)

// EnvSafetyLLMProvider opts a node into the LLM semantic classifier
// (memql#230). When set + matched by a registered structured-chat AI
// provider, app boot includes the LLM layer in the chain. Unset (or
// unmatched) leaves the rules-only default -- the system runs in
// shadow mode end-to-end, the LLM classifier is opt-in per node.
const EnvSafetyLLMProvider = "MEMQL_SAFETY_LLM_PROVIDER"

// EnvPersistClassifications opts a node OUT of persisting
// v1:safety:classification rows via insertSafetyClassification
// (memql#234). Default is on (persist whenever the engine is ready).
// Recognised off values: "off" / "false" / "0" (case-insensitive,
// trimmed).
const EnvPersistClassifications = "MEMQL_SAFETY_PERSIST_CLASSIFICATIONS"

// EnvApprovalSink opts a node OUT of the approval sink (memql#232).
// Default is on (consult v1:work:approval rows whenever the engine is
// ready -- epic memql#4966 moved the sink off v1:safety:approvalRequest so
// every human gate shares one inbox). Recognised off values: "off" /
// "false" / "0" (case-insensitive, trimmed). When opted out, Ask
// verdicts in enforce mode fall back to the pre-approval "requires
// user approval" refusal -- shadow mode never reaches the Ask branch
// regardless.
const EnvApprovalSink = "MEMQL_SAFETY_APPROVAL_SINK"

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
//   - ApprovalSink (Noop OR work-backed v1:work:approval)
//
// Single SetDefaultGate call -- all three halves arrive together so
// a half-wired gate never goes live.
//
// Idempotent + non-fatal on every "can't wire" condition; surface
// gates keep working with the safest default (rules-only / slog-only
// / noop-approval) if any half declines. Logs the outcome of each
// half -- ops can grep `safety classifier:` to see which nodes have
// which layers.
//
// Called from transportBase so the gate is set before any handler
// can dispatch.
func (a *App) wireSafetyGate() {
	classifier := a.buildSafetyClassifier()
	rec := a.buildSafetyRecorder()
	sink := a.buildSafetyApprovalSink()
	safety.SetDefaultGate(safety.NewGate(safety.GateOptions{
		Classifier:   classifier,
		Recorder:     rec,
		ApprovalSink: sink,
		Logger:       a.Logger,
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

// buildSafetyApprovalSink returns the approval sink for this node.
//
// It writes v1:work:approval rows (epic memql#4966), which REPLACES the
// v1:safety:approvalRequest sink this used to build. The work spine's design
// records the reason: one concept for every human gate, and therefore ONE
// INBOX. The safety gate's Ask, the loop's feedback question, a budget
// breach and a plan review were four surfaces, and in an engine-only cluster
// the canvas half was not registered at all -- so a person could be asked
// something they had no way to see.
//
// Falls back to NoopApprovalSink when the engine isn't ready, when the work
// plug-in isn't materialized on this node type, or when the env var opts out
// -- the same safe default the recorder keeps. Every one of those leaves the
// Gate's pre-approval "requires user approval" refusal exactly as it was.
func (a *App) buildSafetyApprovalSink() safety.ApprovalSink {
	off := strings.ToLower(strings.TrimSpace(os.Getenv(EnvApprovalSink)))
	if off == "off" || off == "false" || off == "0" {
		a.Logger.Info("safety classifier: approval sink disabled (MEMQL_SAFETY_APPROVAL_SINK)",
			"component", "safety")
		return safety.NoopApprovalSink{}
	}
	if a.engine == nil {
		a.Logger.Warn("safety classifier: engine not ready; approval sink disabled",
			"component", "safety")
		return safety.NoopApprovalSink{}
	}
	integ := a.lookupWorkIntegration()
	if integ == nil {
		a.Logger.Warn("safety classifier: the work plug-in is not materialized on this node, so an Ask verdict keeps the gate's own refusal",
			"component", "safety")
		return safety.NoopApprovalSink{}
	}
	a.Logger.Info("safety classifier: approval sink enabled (v1:work:approval rows)",
		"component", "safety")
	return integ.NewSink(workspine.SinkOptions{Logger: a.Logger})
}

// lookupWorkIntegration recovers the materialized work plug-in, or nil.
func (a *App) lookupWorkIntegration() *workspine.Integration {
	if a.engine == nil {
		return nil
	}
	provider := a.engine.IntegrationByName("work")
	if provider == nil {
		return nil
	}
	integ, _ := provider.(*workspine.Integration)
	return integ
}

// wireOutputGate builds the per-node output-screening gate
// (memql#233). Mirrors wireSafetyGate but with the OutputGate
// shape: a Screener chain (rule-only today; LLM screener deferred)
// + a Recorder fanout (slog + persisting v1:safety:outputScreening).
//
// Idempotent + non-fatal: if the engine isn't ready (very early
// boot) or persistence is opted out, the gate stays slog-only.
// Logs the outcome of each half -- ops can grep `safety output
// screener:` to see which nodes have which layers.
//
// Called from transportBase alongside wireSafetyGate so both halves
// are configured before any handler can dispatch.
func (a *App) wireOutputGate() {
	// Rule screener with the canonical default pattern set. App
	// boot doesn't append deployment-specific rules today; the
	// extension point is here when it does.
	screener := safety.NewRuleScreener(safety.DefaultScreenRules()...)

	// Recorder fanout: slog (always) + persisting
	// (engine-required; opt-out via env).
	var rec safety.ScreeningRecorder = safety.SlogScreeningRecorder{Logger: a.Logger}
	off := strings.ToLower(strings.TrimSpace(os.Getenv(EnvPersistClassifications)))
	if off == "off" || off == "false" || off == "0" {
		a.Logger.Info("safety output screener: persistence disabled (MEMQL_SAFETY_PERSIST_CLASSIFICATIONS)",
			"component", "safety.output")
	} else if a.engine != nil {
		rec = safety.NewFanoutScreeningRecorder(
			rec,
			recorder.NewOutputPersistingRecorder(engineMutationRunner{engine: a.engine}, a.Logger),
		)
		a.Logger.Info("safety output screener: persistence enabled (v1:safety:outputScreening)",
			"component", "safety.output")
	} else {
		a.Logger.Warn("safety output screener: engine not ready; persistence disabled",
			"component", "safety.output")
	}

	mode := safety.OutputModeFromEnv()
	a.Logger.Info("safety output screener: gate wired",
		"component", "safety.output",
		"mode", string(mode))
	safety.SetDefaultOutputGate(safety.NewOutputGate(safety.OutputGateOptions{
		Screener: screener,
		Recorder: rec,
		Mode:     mode,
		Logger:   a.Logger,
	}))
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
