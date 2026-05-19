package automations

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

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
}

// NewLogicRunner constructs a LogicRunner. The step registry and engine
// must be the same instances the automation scheduler / executor use --
// per-step caching, integration plug-ins, and SI providers all hang off
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
// expression can reference it via `stepName.method()` or
// `$steps.stepName.result.X`.
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

	evaluator := r.newEvaluatorForLogic(args)
	stepCtx := &StepContext{
		Logger:    r.logger,
		Engine:    r.engine,
		Evaluator: evaluator,
		Execution: &AutomationExecution{
			ID:             fmt.Sprintf("logic-%s-%d", fnName, time.Now().UnixNano()),
			AutomationName: "logic:" + fnName,
		},
	}

	// Walk intermediate steps in order. The compiler already
	// topologically sorts them by dependency, so a forward pass
	// guarantees a step's references are bound before we hit it.
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
		shouldRun, err := evaluator.EvaluateCondition(step.Condition)
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

	jsonBytes, err := json.Marshal(result.Automations[0].JSON)
	if err != nil {
		return nil, fmt.Errorf("marshal compiled logic %q: %w", fnName, err)
	}
	automation, err := r.loader.parseJSON(jsonBytes, "logic:"+fnName)
	if err != nil {
		return nil, fmt.Errorf("parse compiled logic %q: %w", fnName, err)
	}
	return automation, nil
}

// newEvaluatorForLogic builds an evaluator seeded with the caller's
// args under every spelling Logic step bodies might use: `args` (the
// author-facing form), `ctx` (the legacy runtime form), and
// `event` (when the caller passes the event payload through,
// matching how automation steps see it). $input mirrors args so
// the existing $input.X expression resolver works too.
func (r *LogicRunner) newEvaluatorForLogic(args map[string]any) *Evaluator {
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
	// The `event` arg is the conventional plumb-through for cron /
	// graph-event-triggered logics. When present, expose it as a
	// top-level variable so step expressions matching the
	// automation flow (`event.payload.X`) resolve.
	if eventVal, ok := args["event"]; ok {
		evaluator.SetCustom("event", eventVal)
	}
	if r.logger != nil {
		evaluator.SetLogger(r.logger)
	}
	return evaluator
}

// Compile-time check that LogicRunner satisfies the engine's
// LogicRunner contract. If the interface signature drifts this fails
// to build, surfacing the mismatch at the consumer site.
var _ memql.LogicRunner = (*LogicRunner)(nil)
