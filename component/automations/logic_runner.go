package automations

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/compiler"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// LogicRunner implements memql.LogicRunner. It dispatches a parsed
// multi-step Logic body (an *AutomationDef whose steps are the
// intermediate `name := <call>` assignments plus a synthetic
// `_return` step) through the existing step registry and
// evaluator, returning the `_return` step's evaluated value as the
// Logic function's return.
//
// The runner bypasses the automation Executor's heavy machinery on
// purpose: a Logic call should not fire automation lifecycle events,
// burn a concurrency slot, persist an execution row, or participate
// in storm detection / dedup. Those are properties of automations,
// not of the function-call dispatch path. The per-step registry
// (steps.Registry.Execute) and the expression evaluator carry the
// work that matters -- step-result binding for later step
// references + caller arg substitution.
//
// Wire at app bootstrap via `engine.SetLogicRunner(automations.NewLogicRunner(...))`
// once the step registry is built. When no runner is wired the
// engine's executeLogicFunctionCall surfaces an actionable error;
// stripped-down binaries that omit the automations package keep the
// single-step Logic dispatch path unchanged.
type LogicRunner struct {
	engine       *memql.MemQLEngine
	stepRegistry StepExecutorRegistry
	logger       *slog.Logger
	compiler     *compiler.Compiler
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
		compiler:     compiler.NewDefault(),
		loader:       NewLoader(LoaderOptions{Logger: logger}),
	}
}

// RunLogic walks the body's steps in order. Each step's result is
// registered on the Evaluator so later steps + the `_return`
// expression can reference it by name (`stepName.first()`,
// `steps.stepName.result.x`).
//
// Side-effect isolation: Logic invocations don't trigger automation
// lifecycle events, don't persist an execution row, don't compete
// for a concurrency slot, and don't participate in dedup / storm
// detection. They run as plain function calls with the step
// runner doing the orchestration.
func (r *LogicRunner) RunLogic(ctx context.Context, fnName string, body *languageParser.AutomationDef, args map[string]any) (any, error) {
	if body == nil {
		return nil, fmt.Errorf("logic body is nil")
	}
	if r.engine == nil {
		return nil, fmt.Errorf("logic runner has no engine wired")
	}
	if r.stepRegistry == nil {
		return nil, fmt.Errorf("logic runner has no step registry wired")
	}

	automation, err := r.compileBodyToAutomation(fnName, body)
	if err != nil {
		return nil, err
	}

	evaluator := r.newEvaluatorForLogic(ctx, args)
	stepCtx := &StepContext{
		Logger:    r.logger,
		Engine:    r.engine,
		Evaluator: evaluator,
		// Wire the engine's event bus so `emit` / publishEvent steps INSIDE a
		// logic body can publish. Without this the StepContext.EventBus is nil
		// and any logic that emits (e.g. logicAutoJoinAI's emitAutoJoinComplete,
		// bootstrapSession's session.created) fails its emit step with
		// "event bus not configured" -- and because the compiler topologically
		// orders steps with no inter-dependency arbitrarily, that abort can land
		// BEFORE the load-bearing mutation step (the AI join / session insert),
		// so the side effect never runs at all. The engine's bus is wired at app
		// bootstrap (SetEventBus) before this runner is constructed, so it's
		// non-nil at runtime; stripped binaries with no bus keep the prior
		// graceful "event bus not configured" error on the emit step. memql#572.
		EventBus: r.engine.EventBus(),
		Execution: &AutomationExecution{
			ID:             fmt.Sprintf("logic-%s-%d", fnName, time.Now().UnixNano()),
			AutomationName: "logic:" + fnName,
		},
	}

	// Walk intermediate steps in order. The compiler already
	// topologically sorts them by dependency, so a forward pass
	// guarantees a step's references are bound before we hit it. Every
	// step carries its expressions parsed at load, and every step kind
	// evaluates them through EvalExpr over the run: the query executor
	// evaluates an in-process expression (`total := a + b`, `active :=
	// rows.where(r => r.active)`) itself, and a construct call goes to the
	// engine with its arguments evaluated.
	var returnStep *Step
	for _, step := range automation.Steps {
		if step == nil {
			continue
		}
		if step.ID == "_return" {
			returnStep = step
			continue
		}
		if err := r.runOneStep(ctx, step, stepCtx, evaluator); err != nil {
			return nil, fmt.Errorf("logic %q step %q: %w", fnName, step.ID, err)
		}
	}

	if returnStep == nil {
		// No explicit `return <expr>` in the body. Logic bodies are
		// supposed to terminate with a return; if the parser produced
		// no `_return` step the body is malformed. Surface as an error
		// instead of returning nil silently.
		return nil, fmt.Errorf("logic %q has no `_return` step (body must end with `return <expr>`)", fnName)
	}
	// `return <stepName>` hands back the step's RECORDED result -- a query
	// step's execute result, not the node list an expression reads it as --
	// so a logic's output keeps its shape. Every other return runs as the
	// `_return` step: an expression evaluates in process, a construct call
	// runs on the engine.
	if x := returnStep.Exprs; x != nil {
		if id, isIdent := ast.Unparen(x.Query).(*ast.IdentExpr); isIdent && evaluator.HasStep(id.Name) {
			if recorded := evaluator.steps[id.Name]; recorded != nil {
				return recorded.Result, nil
			}
			return nil, nil
		}
	}
	if err := r.runOneStep(ctx, returnStep, stepCtx, evaluator); err != nil {
		return nil, fmt.Errorf("logic %q return: %w", fnName, err)
	}
	if returnResult, ok := evaluator.steps["_return"]; ok && returnResult != nil {
		return returnResult.Result, nil
	}
	return nil, nil
}

// runOneStep evaluates an optional condition, dispatches the step,
// and records the result on the evaluator so later steps can read
// it. Mirrors the executor's main loop but without the lifecycle
// event publishing / step record persistence.
func (r *LogicRunner) runOneStep(ctx context.Context, step *Step, stepCtx *StepContext, evaluator *Evaluator) error {
	if step.Condition != "" {
		// StepCondition: the condition parsed at load, through
		// EvalCondition (memql#5367).
		shouldRun, err := evaluator.StepCondition(ctx, step)
		if err != nil {
			// Match executor behaviour: condition errors skip the step
			// rather than failing the whole Logic. The evaluator logs
			// the underlying error.
			shouldRun = false
		}
		if !shouldRun {
			skipResult := &StepResult{
				StepId:      step.ID,
				Status:      "skipped",
				StartedAt:   time.Now(),
				CompletedAt: time.Now(),
			}
			evaluator.SetStepResult(step.ID, skipResult)
			return nil
		}
	}
	result, err := r.stepRegistry.Execute(ctx, step, stepCtx)
	if err != nil {
		return err
	}
	if result != nil {
		evaluator.SetStepResult(step.ID, result)
	}
	return nil
}

// compileBodyToAutomation runs the parsed Logic body through the
// existing compiler + JSON loader so the resulting *Automation
// has the same shape the automation executor consumes. Reusing
// the compiler keeps the AST→runtime translation (step references,
// `event.X` rewrites, helper builtin recognition, topological sort)
// in exactly one place.
//
// The compiler peels the parser's synthetic `_return` step out of the
// step list and emits it as a top-level `_return` JSON field
// (automation_generator.go's "If there's a return statement..."
// branch). The automations Loader struct has no field for `_return`,
// so a vanilla unmarshal drops it. The runtime LogicRunner looks for
// a step with ID `_return` in `automation.Steps` and refuses to run
// when it isn't there ("logic %q has no `_return` step"), which is
// why every multi-step Logic that ends with `return <expr>` failed
// at runtime even though the parser captured the return correctly.
// We undo the round-trip loss here by re-reading the `_return` field
// off the compiled JSON and stitching a synthetic Step{ID: "_return",
// Type: query} back onto the end of the slice.
func (r *LogicRunner) compileBodyToAutomation(fnName string, body *languageParser.AutomationDef) (*Automation, error) {
	fakeFunc := &languageParser.FunctionDef{
		Name: fnName,
		Type: languageParser.FunctionTypeAutomation,
		Body: body,
	}
	fakeFile := &languageParser.File{
		Definitions: []languageParser.Node{fakeFunc},
	}
	result, err := r.compiler.CompileFile(fakeFile)
	if err != nil {
		return nil, fmt.Errorf("compile logic body: %w", err)
	}
	if len(result.Automations) == 0 {
		return nil, fmt.Errorf("compiler emitted no automation for logic %q", fnName)
	}

	compiled := result.Automations[0].JSON
	// A body that is one `return` compiles to no steps, which an automation
	// may not have. Its return is then its one step, prepared by parseJSON
	// like any other.
	if steps, _ := compiled["steps"].([]map[string]any); len(steps) == 0 {
		if returnExpr, ok := compiled["_return"].(string); ok && strings.TrimSpace(returnExpr) != "" {
			compiled["steps"] = []map[string]any{{
				"id":    "_return",
				"type":  string(StepTypeQuery),
				"query": map[string]any{"query": returnExpr},
			}}
			delete(compiled, "_return")
		}
	}
	jsonBytes, err := json.Marshal(compiled)
	if err != nil {
		return nil, fmt.Errorf("marshal compiled logic %q: %w", fnName, err)
	}
	automation, err := r.loader.parseJSON(jsonBytes, "logic:"+fnName)
	if err != nil {
		return nil, fmt.Errorf("parse compiled logic %q: %w", fnName, err)
	}

	if returnExpr, ok := compiled["_return"].(string); ok && strings.TrimSpace(returnExpr) != "" {
		ret := &Step{
			ID:   "_return",
			Type: StepTypeQuery,
			Query: &QueryStepConfig{
				Query: returnExpr,
			},
		}
		// The return is edition-2026 source like every other expression of
		// the body; parse it once now, as parseJSON parsed the rest.
		if err := (&exprPreparer{automation: automation.Name}).step(ret); err != nil {
			return nil, fmt.Errorf("parse compiled logic %q: %w", fnName, err)
		}
		automation.Steps = append(automation.Steps, ret)
	}

	return automation, nil
}

// newEvaluatorForLogic builds an evaluator seeded with the caller's
// args under every spelling Logic step bodies might use: `args` (the
// author-facing form), `ctx` (the older runtime form), and
// `event` (the first-class triggering-event binding). `input` mirrors
// args.
//
// First-class event-context binding (memql#1706): the triggering event
// is bound as a top-level, in-scope value resolvable from EVERY step's
// argument expressions -- both the author-facing `args.event.payload.X`
// (through the `args` map) and the bare `event.payload.X` form (through
// this dedicated `event` root). The SAME object backs both spellings.
// Seeding is UNCONDITIONAL with a well-formed empty envelope fallback so
// a bare `event.payload.X` reference resolves to empty (never to an
// unbound-root resolution error) even on a misconfigured/direct call
// that forgot to pass `event` -- mirroring the synthetic-envelope
// philosophy the automation executor uses for schedule-triggered runs
// (buildEventEnvelope, #418). Compile-time validation
// (validateLogicEventBinding) guarantees every event-reading logic
// DECLARES `event` as a required input, so the real path always carries
// a concrete event; the fallback is purely defensive.
func (r *LogicRunner) newEvaluatorForLogic(ctx context.Context, args map[string]any) *Evaluator {
	evaluator := NewEvaluator()
	if args == nil {
		args = map[string]any{}
	}
	evaluator.SetInput(args)
	evaluator.SetCustom("args", args)
	evaluator.SetCustom("ctx", map[string]any{
		"input":  args,
		"output": nil,
		"error":  "",
	})
	evaluator.SetCustom("event", logicEventBinding(args))
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
	// grow a second builder (#2623).
	//
	// Why this is a fix and not an addition: the validator memql#3024 deletes
	// refused an ambient cond predicate in EVERY step, multi-step bodies
	// included. Deleting that refusal makes `cond(config.demoMode == true,
	// ...)` loadable here -- and with nothing bound it resolves against
	// nothing and is a silent constant, which is the memql#2962 defect the
	// deletion was meant to be retiring, not relocating. Binding is what makes
	// the deletion honest on this path.
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
// predicate reads an absent key as TRUE, so `cond(config.demoMode != true,
// <allow>, <deny>)` would take the ALLOW branch under a nil engine and the
// DENY branch under a real one. That is the same predicate answering
// differently depending on how the evaluator was constructed, which is exactly
// what #2623 forbids and what authoring-rules.md now promises does not happen.
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

// logicEventBinding resolves the first-class `event` value bound into a
// logic's step scope. It returns the caller-passed `event` arg verbatim
// when present, otherwise a well-formed empty envelope
// (`{topic, kind, payload:{}}`) so `event.payload.X` / `args.event.payload.X`
// references resolve to empty rather than failing on an unbound `event`
// root. Keeping the SAME object under both the `args.event` path and the
// bare `event` root means the two spellings can never drift.
func logicEventBinding(args map[string]any) any {
	if eventVal, ok := args["event"]; ok && eventVal != nil {
		return eventVal
	}
	return map[string]any{
		"topic":   "",
		"kind":    "",
		"payload": map[string]any{},
	}
}

// Compile-time check that LogicRunner satisfies the engine's
// LogicRunner contract. If the interface signature drifts this fails
// to build, surfacing the mismatch at the consumer site.
var _ memql.LogicRunner = (*LogicRunner)(nil)
