package automations

import (
	"context"
	"log/slog"

	"github.com/znasllc-io/memql/component/memql"
)

// LogicRunner implements memql.LogicRunner: it runs a logic's statement
// body, compiled at load, on the sequence runner an automation's statements
// run on (logic_statements.go).
//
// A logic call does not fire automation lifecycle events, burn a concurrency
// slot, or take part in storm detection or dedup: those are properties of
// automations, not of a function call. Its statements journal as the file
// comment of logic_statements.go describes.
//
// Wire at app bootstrap via `engine.SetLogicRunner(automations.NewLogicRunner(...))`
// once the step registry is built. When no runner is wired the engine's
// executeLogicFunctionCall surfaces an actionable error.
type LogicRunner struct {
	engine       *memql.MemQLEngine
	stepRegistry StepExecutorRegistry
	logger       *slog.Logger
	loader       *Loader

	// journalExec writes a statement-body logic's journal instead of the
	// engine: a test's recorder (logic_statements.go). Nil in production.
	journalExec journalExecutor
	// noJournal is WithoutJournal's: the logic journals nothing.
	noJournal bool
}

// NewLogicRunner constructs a LogicRunner. The step registry and engine
// must be the same instances the automation scheduler / executor use --
// per-step caching, integration plug-ins, and AI providers all hang off
// the engine, and the step executors look them up via stepCtx.Engine.
func NewLogicRunner(engine *memql.MemQLEngine, registry StepExecutorRegistry, logger *slog.Logger) *LogicRunner {
	if logger == nil {
		logger = NewLogger()
	}
	return &LogicRunner{
		engine:       engine,
		stepRegistry: registry,
		logger:       logger,
		loader:       NewLoader(LoaderOptions{Logger: logger}),
	}
}

// newEvaluatorForLogic builds the evaluator a logic's statements read: the
// caller's args under `args`, and the ambient roots. A logic reads what its
// caller passes, so an event it acts on arrives as an argument and is read
// args.event (validateLogicEventBinding holds an event-reading logic to
// declaring it); a bare `event` is not a root of a logic (CheckBody).
func (r *LogicRunner) newEvaluatorForLogic(ctx context.Context, args map[string]any) *Evaluator {
	evaluator := NewEvaluator()
	if args == nil {
		args = map[string]any{}
	}
	evaluator.SetCustom("args", args)
	// actor.* is an ambient every body may read (the argument-resolution
	// contract), so the runner's step evaluator binds it from the caller's
	// auth context (#2380) -- UNCONDITIONALLY (#2801): with no auth context
	// ActorEnvelopeMap denies (owner bits false, identity empty), so
	// `actor.isClusterOwner != false` is false rather than reading an
	// unbound actor. One canonical envelope (#2623), via the one shared
	// binder so the evaluator sites cannot drift apart again.
	bindActorEnvelope(ctx, evaluator)
	// The REST of the ambient envelope -- `config` / `partition` / `now`
	// (memql#3024). `actor` is bound above by the shared binder; these three
	// come from the engine's one canonical envelope so this surface cannot
	// grow a second builder (#2623). Unbound, a read of one resolves against
	// nothing and is a silent constant (memql#2962).
	bindEngineAmbientEnvelope(ctx, r.engine, evaluator)
	if r.logger != nil {
		evaluator.SetLogger(r.logger)
	}
	return evaluator
}

// bindEngineAmbientEnvelope binds the non-actor half of the ambient envelope
// (`config` / `partition` / `now`) onto evaluator, sourced from the engine's
// single canonical builder.
//
// `actor` is deliberately NOT bound here: bindActorEnvelope owns it, and
// actor_envelope_invariant_test.go enforces that every evaluator passes
// through that binder. Splitting the two keeps that invariant checkable by the
// test that already exists rather than making it depend on this function too.
//
// A nil engine still binds, and that is deliberate rather than defensive.
// AmbientEnvelope tolerates a nil receiver and yields every key with the same
// empty/denying values a configured engine yields for an unset snapshot, so
// binding unconditionally keeps the #2801 rule intact on this path: EVERY KEY
// PRESENT, never absent keys.
//
// Skipping the bind for a nil engine looks safer and is not. Absent keys are
// the third nil-representation #2801 exists to eliminate -- a negated
// predicate reads an absent key as TRUE, so `config.demoMode != true ?
// <allow> : <deny>` would take the ALLOW branch under a nil engine and the
// DENY branch under a real one. That is the same predicate answering
// differently depending on how the evaluator was constructed, which is exactly
// what #2623 forbids and what authoring-rules.md promises does not happen.
func bindEngineAmbientEnvelope(ctx context.Context, engine *memql.MemQLEngine, evaluator *Evaluator) {
	if evaluator == nil {
		return
	}
	envelope := engine.AmbientEnvelope(ctx)
	for _, key := range []string{"config", "partition", "now"} {
		if value, ok := envelope[key]; ok {
			evaluator.SetCustom(key, value)
		}
	}
}

// Compile-time check that LogicRunner satisfies the engine's
// LogicRunner contract. If the interface signature drifts this fails
// to build, surfacing the mismatch at the consumer site.
var _ memql.LogicRunner = (*LogicRunner)(nil)
