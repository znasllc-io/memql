package automations

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
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
		// Builtin RHS short-circuit: a step assignment whose body is a
		// positional helper (`coalesce(args.X, "default")`,
		// `concat(...)`, ...) gets evaluated against the local
		// Evaluator. Bypassing engine.Execute is required because
		// these helpers are not registered as user functions in the
		// engine's function registry -- the engine would fail the
		// lookup with `function "coalesce" not found`. See #362.
		if step.Type == StepTypeQuery && step.Query != nil {
			if val, handled, err := r.tryEvaluateBuiltinLocally(step.Query.Query, evaluator); err != nil {
				return nil, fmt.Errorf("logic %q step %q: %w", fnName, step.ID, err)
			} else if handled {
				now := time.Now()
				evaluator.SetStepResult(step.ID, &StepResult{
					StepId:      step.ID,
					Status:      "success",
					StartedAt:   now,
					CompletedAt: now,
					Result:      val,
				})
				continue
			}
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
	// Short-circuit pure step-method-call returns like
	// `expiredDelegations.Len()` / `existing.First()` / `rows.Empty()`.
	// The engine's expression parser doesn't recognise the
	// `stepName.method()` shape and looks the whole dotted name up as
	// a top-level function (-> "function not found"). The local
	// Evaluator's EvaluateStepReference already understands the
	// shape (strips `()`, resolves through `steps.<id>.<method>`
	// shorthand), so route through it before falling back to
	// engine.Execute via the step registry.
	if returnStep.Query != nil {
		if val, handled, err := r.tryEvaluateReturnLocally(returnStep.Query.Query, evaluator); err != nil {
			return nil, fmt.Errorf("logic %q return: %w", fnName, err)
		} else if handled {
			return val, nil
		}
		// Builtin return short-circuit: `return coalesce(stepX, stepY.First())`.
		// Same rationale as the step-RHS short-circuit above -- these
		// helpers aren't engine-registered functions and would fail
		// the lookup. See #362.
		if val, handled, err := r.tryEvaluateBuiltinLocally(returnStep.Query.Query, evaluator); err != nil {
			return nil, fmt.Errorf("logic %q return: %w", fnName, err)
		} else if handled {
			return val, nil
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

// tryEvaluateReturnLocally short-circuits engine.Execute for two
// return-expression shapes that the engine's expression parser
// mis-resolves:
//
//   - Bare step-variable references -- `return nodeRecord` after
//     `nodeRecord := mutationCreateNode(...)`. The engine treats the
//     bare identifier as a spec name and emits `unknown spec
//     "nodeRecord"`.
//   - Step-method calls -- `expiredDelegations.Len()`,
//     `existing.First()`, `rows.Empty()`, etc. The engine looks the
//     dotted name up as a top-level function and emits "function not
//     found".
//
// The local Evaluator already understands both shapes (`steps` map
// for bare lookup, `EvaluateStepReference` for the dotted path), so
// we route through it before the engine sees the expression. Any
// shape outside this set falls back to engine.Execute via the step
// registry.
//
// Returns (value, true, nil) when the expression matched a known
// step-reference shape and resolved cleanly. Returns
// (nil, false, nil) when the expression doesn't match -- the caller
// should fall back to engine.Execute. Returns (nil, false, err) on
// a hard error (e.g. step exists but the dotted suffix didn't
// resolve).
func (r *LogicRunner) tryEvaluateReturnLocally(expr string, evaluator *Evaluator) (any, bool, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, false, nil
	}

	// Bare step-variable reference: a single identifier (no parens,
	// no dot, no whitespace) whose name matches a bound step. Same
	// package, so the unexported steps map is in scope -- mirrors the
	// `_return` lookup at the tail of RunLogic.
	if isBareIdentifier(expr) && evaluator.HasStep(expr) {
		if result := evaluator.steps[expr]; result != nil {
			return result.Result, true, nil
		}
		return nil, true, nil
	}

	// Step-method call: `stepName.method()`.
	inner := strings.TrimSuffix(expr, "()")
	if inner == expr {
		return nil, false, nil
	}
	// The inner part can't carry anything that needs more parsing
	// (no parens, no commas, no quotes). This filters out compound
	// expressions like `coalesce(a.first(), b.first())` whose outer
	// shape happens to end with `()` but isn't a step-method call.
	if strings.ContainsAny(inner, "()[]{}\"', \t\n") {
		return nil, false, nil
	}
	firstDot := strings.Index(inner, ".")
	if firstDot <= 0 {
		return nil, false, nil
	}
	firstSegment := inner[:firstDot]
	if !evaluator.HasStep(firstSegment) {
		return nil, false, nil
	}
	val, err := evaluator.EvaluateStepReference(expr)
	if err != nil {
		return nil, false, err
	}
	return val, true, nil
}

// tryEvaluateBuiltinLocally evaluates positional helper builtins
// (`coalesce(...)`) against the logic's local Evaluator without
// round-tripping through engine.Execute. These helpers are not
// registered in the engine's function registry, so dispatching
// through the query step path fails with `function "<name>" not
// found` (#362). The mutation-template evaluator handles them when
// they appear inside an `insert {...}` arg block, but logic-body
// step RHS values and `return` expressions land here.
//
// Returns (value, true, nil) when expr matches a known builtin and
// every arg resolves cleanly. Returns (nil, false, nil) when expr
// is not a recognised builtin call -- the caller falls back to
// engine.Execute via the step registry. Returns (nil, false, err)
// on a hard resolution error (e.g. arg references a step that
// isn't bound).
func (r *LogicRunner) tryEvaluateBuiltinLocally(expr string, evaluator *Evaluator) (any, bool, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" || !strings.HasSuffix(expr, ")") {
		return nil, false, nil
	}
	open := strings.IndexByte(expr, '(')
	if open <= 0 {
		return nil, false, nil
	}
	name := strings.TrimSpace(expr[:open])
	if !isPositionalBuiltinName(name) {
		return nil, false, nil
	}
	inner := expr[open+1 : len(expr)-1]
	rawArgs, err := splitTopLevelArgs(inner)
	if err != nil {
		return nil, false, fmt.Errorf("parse %s args: %w", name, err)
	}
	switch name {
	case "coalesce":
		val, err := evaluateCoalesceArgs(rawArgs, evaluator)
		if err != nil {
			return nil, false, err
		}
		return val, true, nil
	}
	// Unreachable -- isPositionalBuiltinName admits the same set this
	// switch handles. Surface as a hard error so a future extension to
	// the admit set without a handler doesn't silently fall through.
	return nil, false, fmt.Errorf("positional builtin %q has no local evaluator", name)
}

// isPositionalBuiltinName reports whether name is a helper builtin
// the logic runner evaluates locally. Today only `coalesce` is on
// this list (the only helper that appears outside mutation arg
// blocks across the live DSL tree); extending the set is a matter
// of adding a name here and a case in tryEvaluateBuiltinLocally's
// switch.
func isPositionalBuiltinName(name string) bool {
	switch name {
	case "coalesce":
		return true
	}
	return false
}

// splitTopLevelArgs splits the inside of a function call (`a, b,
// c.method()`) on top-level commas, respecting nested parens,
// brackets, braces, and quoted strings. Returns the slice of arg
// expressions in order; empty inner returns a nil slice.
func splitTopLevelArgs(inner string) ([]string, error) {
	inner = strings.TrimSpace(inner)
	if inner == "" {
		return nil, nil
	}
	var (
		args      []string
		depth     int
		inString  bool
		quoteChar byte
		start     = 0
	)
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		if inString {
			if c == '\\' && i+1 < len(inner) {
				i++
				continue
			}
			if c == quoteChar {
				inString = false
			}
			continue
		}
		switch c {
		case '"', '\'':
			inString = true
			quoteChar = c
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
			if depth < 0 {
				return nil, fmt.Errorf("unbalanced brackets")
			}
		case ',':
			if depth == 0 {
				args = append(args, strings.TrimSpace(inner[start:i]))
				start = i + 1
			}
		}
	}
	if depth != 0 || inString {
		return nil, fmt.Errorf("unbalanced brackets or quotes")
	}
	args = append(args, strings.TrimSpace(inner[start:]))
	return args, nil
}

// evaluateCoalesceArgs returns the first non-nil / non-empty value
// from rawArgs, evaluating each through the local Evaluator. Matches
// the "first non-missing value" semantics the mutation-template
// evaluator implements for the same builtin (mutation_templates.go's
// evalCoalesce). Empty strings are treated as missing so callers can
// rely on `coalesce(args.X, "fallback")` to land the fallback when
// args.X is absent.
func evaluateCoalesceArgs(rawArgs []string, evaluator *Evaluator) (any, error) {
	for _, raw := range rawArgs {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		val, err := evaluateScalarArg(raw, evaluator)
		if err != nil {
			return nil, err
		}
		if val == nil {
			continue
		}
		if s, ok := val.(string); ok && strings.TrimSpace(s) == "" {
			continue
		}
		return val, nil
	}
	return nil, nil
}

// evaluateScalarArg resolves a single positional-builtin arg
// expression to its runtime value against the local Evaluator:
//
//   - Quoted string literal (`"x"`, `'x'`) -> string value
//   - Numeric / bool / null literal              -> typed value
//   - `args.X.Y`, `event.X`, `steps.X`           -> resolved via $-path
//   - `stepName` / `stepName.method()`           -> resolved via step ref
//
// Soft-fails (returns nil) when a $-path can't be resolved, mirroring
// the soft-resolution semantics the automations evaluator uses for
// `$coalesce(...)` -- coalesce's whole job is to tolerate missing
// values and pick the next fallback.
func evaluateScalarArg(raw string, evaluator *Evaluator) (any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	// Quoted string.
	if (strings.HasPrefix(raw, "\"") && strings.HasSuffix(raw, "\"")) ||
		(strings.HasPrefix(raw, "'") && strings.HasSuffix(raw, "'")) {
		unquoted, err := strconv.Unquote(raw)
		if err != nil {
			return nil, fmt.Errorf("unquote %q: %w", raw, err)
		}
		return unquoted, nil
	}
	// Literals.
	switch raw {
	case "null":
		return nil, nil
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	// Numeric literal.
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return n, nil
	}
	if n, err := strconv.ParseFloat(raw, 64); err == nil {
		return n, nil
	}
	// $-prefixed expression -- delegate to the evaluator.
	if strings.HasPrefix(raw, "$") {
		val, err := evaluator.EvaluateValue(raw)
		if err != nil {
			return nil, nil //nolint:nilerr // soft-fail for coalesce
		}
		return val, nil
	}
	// Step reference or known custom-var path (`args.X.Y`, `event.X`,
	// `item.X`). EvaluateStepReference handles bare step names and
	// step.method() shapes; for paths whose first segment is one of
	// the well-known custom-var roots (`args`, `event`, `ctx`) we
	// upgrade to $-form so resolvePath drives the lookup.
	firstSegment := raw
	if dot := strings.IndexAny(raw, "."); dot > 0 {
		firstSegment = raw[:dot]
	}
	if isCustomVarRoot(firstSegment) {
		val, err := evaluator.EvaluateValue("$" + raw)
		if err != nil {
			return nil, nil //nolint:nilerr // soft-fail for coalesce
		}
		return val, nil
	}
	val, err := evaluator.EvaluateStepReference(raw)
	if err != nil {
		return nil, nil //nolint:nilerr // soft-fail for coalesce
	}
	return val, nil
}

// isCustomVarRoot reports whether segment is a well-known root the
// evaluator seeds for logic bodies (see newEvaluatorForLogic and
// resolvePath). Anything else falls through to step-reference
// resolution.
func isCustomVarRoot(segment string) bool {
	switch segment {
	case "args", "event", "ctx", "input", "item":
		return true
	}
	return false
}

// isBareIdentifier returns true when expr is a single identifier --
// letters, digits, and underscores only, starting with a letter or
// underscore. Used to gate the bare-step-variable fast path in
// tryEvaluateReturnLocally.
func isBareIdentifier(expr string) bool {
	if expr == "" {
		return false
	}
	for i, r := range expr {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r == '_':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
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
	jsonBytes, err := json.Marshal(compiled)
	if err != nil {
		return nil, fmt.Errorf("marshal compiled logic %q: %w", fnName, err)
	}
	automation, err := r.loader.parseJSON(jsonBytes, "logic:"+fnName)
	if err != nil {
		return nil, fmt.Errorf("parse compiled logic %q: %w", fnName, err)
	}

	if returnExpr, ok := compiled["_return"].(string); ok && strings.TrimSpace(returnExpr) != "" {
		automation.Steps = append(automation.Steps, &Step{
			ID:   "_return",
			Type: StepTypeQuery,
			Query: &QueryStepConfig{
				Query: returnExpr,
			},
		})
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
