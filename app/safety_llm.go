package app

import (
	"context"
	"os"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/safety"
	"github.com/znasllc-io/memql/component/safety/approval"
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

// EnvApprovalSink opts a node OUT of the DSL-backed approval sink
// (memql#232). Default is on (consult v1:safety:approvalRequest rows
// whenever the engine is ready). Recognised off values: "off" /
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
//   - ApprovalSink (Noop OR DSL-backed v1:safety:approvalRequest)
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
// DSL-backed by default (consults v1:safety:approvalRequest rows);
// falls back to NoopApprovalSink if the engine isn't ready or the
// env var opts out. Mirrors the recorder's safe-default posture.
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
	a.Logger.Info("safety classifier: approval sink enabled (v1:safety:approvalRequest rows)",
		"component", "safety")
	return approval.New(approval.Options{
		Engine: engineApprovalAdapter{engine: a.engine},
		Logger: a.Logger,
	})
}

// engineApprovalAdapter wraps memql.MemQLEngine to satisfy
// approval.Engine -- one method, returns (any, error). Keeps the
// approval package engine-independent (mirrors engineMutationRunner).
//
// The conversion via ConvertExecuteResultToMap is load-bearing:
// memql.MemQLEngine.Execute returns *ExecuteResult (a typed struct
// holding a GraphBundle + shaped output), NOT a raw map. Without the
// conversion the approval.Sink's rowsFromResult / idFromResult see
// an opaque pointer they can't decode, every query looks empty +
// every mutation looks empty-id'd -- idempotency breaks + the
// approved-bypass flow never fires. Same pattern as CognitionEngineAdapter
// in adapters.go.
type engineApprovalAdapter struct{ engine *memql.MemQLEngine }

func (e engineApprovalAdapter) Execute(ctx context.Context, query string) (any, error) {
	result, err := e.engine.Execute(ctx, query)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, nil
	}
	return ConvertExecuteResultToMap(result), nil
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
