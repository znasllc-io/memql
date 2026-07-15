package automations

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/znasllc-io/memql/component/auth"
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
			if val, handled, err := tryEvaluateBuiltinLocally(step.Query.Query, evaluator); err != nil {
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
		// Positional-builtin step ASSIGNMENTS (`getGA := coalesce(a, b)`)
		// are compiled as StepTypeFunction, not StepTypeQuery, so the
		// short-circuit above misses them and they fall through to
		// FunctionExecutor -- which serializes positional args in named
		// form (`coalesce(0="a", 1="b")`), a string the MemQL parser
		// rejects with `parse error ... expected ')', got "="`. Route
		// them through the same local Evaluator by reconstructing the
		// positional call string. See #362 (autoJoinAI getGA / worker /
		// workbench all use standalone `name := coalesce(...)` steps).
		if step.Type == StepTypeFunction && step.Function != nil {
			if callStr, ok := reconstructPositionalBuiltinCall(step.Function); ok {
				if val, handled, err := tryEvaluateBuiltinLocally(callStr, evaluator); err != nil {
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
		}
		// Collection-method / lambda chain RHS (#2317): an intermediate
		// `active := rows.where(r => r.active)` assignment evaluates the chain
		// in-memory against the local Evaluator -- the base receiver (a prior
		// `:=` step result or `args.X`) resolves through it, normalised to its
		// node collection -- and binds the resulting collection as the step
		// result, so a later `return active.count()` reads it. Only a genuine
		// chain is short-circuited; a plain query / function / builtin step
		// falls through to the step registry. (The struct-form parser support
		// for a method-chain step RHS lands in #2316; this is the runtime
		// dispatch that backs it.)
		if step.Type == StepTypeQuery && step.Query != nil {
			if val, handled, err := tryEvaluateCollectionChainLocally(step.Query.Query, evaluator); err != nil {
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
		// Binary arithmetic RHS (#2542 GAP 2): an intermediate `delta := a * 2`
		// step serialized to the parenthesized operator form. Evaluate it
		// against the local Evaluator -- the same route the terminal return
		// takes (#2542 item 1) -- and bind the numeric result so later steps and
		// the `_return` read it. Routed AFTER the collection-chain attempt so a
		// chain whose value feeds arithmetic keeps its own path.
		if step.Type == StepTypeQuery && step.Query != nil {
			if val, handled, err := tryEvaluateArithmeticLocally(step.Query.Query, evaluator); err != nil {
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
	// `expiredDelegations.count()` / `existing.first()` / `rows.empty()`.
	// The engine's expression parser doesn't recognise the
	// `stepName.method()` shape and looks the whole dotted name up as
	// a top-level function (-> "function not found"). The local
	// Evaluator's EvaluateStepReference already understands the
	// shape (strips `()`, resolves through `steps.<id>.<method>`
	// shorthand), so route through it before falling back to
	// engine.Execute via the step registry.
	if returnStep.Query != nil {
		// One logic-time leaf entry (memql#593): EvaluateLocalExpr short-circuits
		// engine.Execute for the shapes the engine's expression parser doesn't
		// recognise -- a bare step ref, a `stepName.method()` call, or a local
		// positional builtin (`coalesce(...)`, #362) -- evaluating them against
		// the local Evaluator. It returns handled=false for a real engine query,
		// which then runs through the step registry below.
		if val, handled, err := EvaluateLocalExpr(returnStep.Query.Query, evaluator); err != nil {
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
//     `nodeRecord := createNode(...)`. The engine treats the
//     bare identifier as a spec name and emits `unknown spec
//     "nodeRecord"`.
//   - Step-method calls -- `expiredDelegations.count()`,
//     `existing.first()`, `rows.empty()`, etc. The engine looks the
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
// EvaluateLocalExpr is the single logic-time entry for resolving a leaf value
// expression against the local Evaluator WITHOUT round-tripping through the
// engine (memql#593). It is the logic-time counterpart to the arg-time
// MutationExecutor resolver, and the converge-onto-one entry point both the
// LogicRunner return path and the conformance matrix drive.
//
// It handles exactly the shapes the engine's expression parser does NOT
// recognise as a function call:
//   - a scalar literal (`1`, `"x"`, `true`, `null`) -> its typed value,
//   - a bare step reference (`getActiveGA`) -> the step's result,
//   - a `stepName.method()` call (`rows.first()` / `rows.count()` / ...),
//   - a local positional builtin (`coalesce(...)`, #362).
//
// Returns (value, true, nil) when it resolved the expression locally,
// (nil, false, nil) when the expression is a real engine query/function that
// the caller must run through the step registry, and (nil, false, err) on a
// hard resolution error.
func EvaluateLocalExpr(expr string, evaluator *Evaluator) (any, bool, error) {
	if val, handled := tryEvaluateLiteralLocally(expr); handled {
		return val, true, nil
	}
	if val, handled, err := tryEvaluateObjectLiteralLocally(expr, evaluator); err != nil || handled {
		return val, handled, err
	}
	if val, handled, err := tryEvaluateReturnLocally(expr, evaluator); err != nil || handled {
		return val, handled, err
	}
	// Collection-method / lambda chain (#2317): `rows.where(r => r.active).count()`
	// over a prior step result or `args.X`. Routed AFTER the single
	// step-method short-circuit so a bare `step.count()` keeps its legacy
	// step-result-accessor semantics; only a genuine chain (carrying a lambda
	// or a non-accessor collection operator) lands here.
	if val, handled, err := tryEvaluateCollectionChainLocally(expr, evaluator); err != nil || handled {
		return val, handled, err
	}
	// Binary arithmetic (#2542 item 1): a terminal `return a / b` serialized
	// to the parenthesized operator form. Routed AFTER the collection-chain
	// attempt so a pure chain keeps its path (an arithmetic expression whose
	// operand is a chain re-parses to a top-level ArithmeticExpr, which the
	// chain evaluator already declines).
	if val, handled, err := tryEvaluateArithmeticLocally(expr, evaluator); err != nil || handled {
		return val, handled, err
	}
	// Expression-led comparison (#2542 item 5): a terminal `return (a - b) > 0`
	// serialized to the parenthesized operator form re-parses to a top-level
	// BinaryComparisonExpr. Routed AFTER the arithmetic attempt (which declines
	// a comparison node) so the two are mutually exclusive: arithmetic matches
	// only *ArithmeticExpr, this matches only *BinaryComparisonExpr. Without it
	// the leading `(` defeats the builtin path and the return falls through to
	// the step registry, where the engine rejects BinaryComparisonExpression
	// with the `only available in logic` scope error.
	if val, handled, err := tryEvaluateComparisonLocally(expr, evaluator); err != nil || handled {
		return val, handled, err
	}
	return tryEvaluateBuiltinLocally(expr, evaluator)
}

// tryEvaluateObjectLiteralLocally resolves an object-literal return /
// step-RHS -- `{ key: <expr>, ... }` -- by evaluating each value against the
// local Evaluator and building a Go map, instead of stringifying it back into a
// query for engine.Execute.
//
// The engine round-trip is the #2274 blocker: a value that references a prior
// step result (e.g. `rows: found`, a node list) renders back into the query text
// as a JSON array / envelope and the engine parser rejects it ("unexpected token
// after expression"). Resolving locally keeps each value as its Go value, so the
// decide->persist pattern can return a flat object whose fields a downstream step
// reads via `field(decide.result, "x")` / `decide.result.x` (the #2271 read-side
// unwrap exposes them flat).
//
// Values resolve through the same leaf path the rest of a logic body uses
// (literals, bare step refs, builtins, then step/path references); nested object
// literals recurse. Returns (map, true, nil) for an object literal, and
// (nil, false, nil) for anything else (so the caller falls through).
func tryEvaluateObjectLiteralLocally(expr string, evaluator *Evaluator) (any, bool, error) {
	expr = strings.TrimSpace(expr)
	if len(expr) < 2 || expr[0] != '{' || expr[len(expr)-1] != '}' {
		return nil, false, nil
	}
	inner := strings.TrimSpace(expr[1 : len(expr)-1])
	out := map[string]any{}
	if inner == "" {
		return out, true, nil
	}
	pairs, err := splitTopLevelArgs(inner)
	if err != nil {
		return nil, false, nil // not a clean object literal -- let the normal path try
	}
	for _, pair := range pairs {
		key, rawVal, ok := splitFirstTopLevelColon(pair)
		if !ok {
			return nil, false, nil // not key:value shape -- not an object literal we own
		}
		key = strings.TrimSpace(key)
		if len(key) >= 2 && (key[0] == '"' || key[0] == '\'') && key[len(key)-1] == key[0] {
			key = key[1 : len(key)-1]
		}
		val, verr := resolveObjectLiteralValue(strings.TrimSpace(rawVal), evaluator)
		if verr != nil {
			return nil, false, fmt.Errorf("object literal field %q: %w", key, verr)
		}
		out[key] = val
	}
	return out, true, nil
}

// resolveObjectLiteralValue resolves one object-literal value expression against
// the evaluator: nested object literals recurse, then the shared leaf path
// (literals / bare step refs / builtins), then step / path references
// (`args.event.payload.x`, `someStep.first()`, `someStep`, `$steps.x...`). The
// result is unwrapped so a Bundle-backed step ref yields its flat value (#2271).
func resolveObjectLiteralValue(raw string, evaluator *Evaluator) (any, error) {
	if len(raw) >= 2 && raw[0] == '{' && raw[len(raw)-1] == '}' {
		if v, handled, err := tryEvaluateObjectLiteralLocally(raw, evaluator); err != nil {
			return nil, err
		} else if handled {
			return v, nil
		}
	}
	if v, handled, err := EvaluateLocalExpr(raw, evaluator); err != nil {
		return nil, err
	} else if handled {
		return UnwrapStepResult(v), nil
	}
	v, err := evaluator.EvaluateStepReference(raw)
	if err != nil {
		return nil, err
	}
	return UnwrapStepResult(v), nil
}

// splitFirstTopLevelColon splits "key: value" on the first colon not nested in
// brackets or a quoted string. ok=false when there is no top-level colon.
func splitFirstTopLevelColon(s string) (key, val string, ok bool) {
	var (
		depth     int
		inString  bool
		quoteChar byte
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			if c == '\\' && i+1 < len(s) {
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
		case ':':
			if depth == 0 {
				return s[:i], s[i+1:], true
			}
		}
	}
	return "", "", false
}

// tryEvaluateLiteralLocally resolves a return / step-RHS expression that is a
// bare scalar literal -- a number (`1`, `3.14`), a quoted string (`"x"`,
// `'x'`), or one of the keyword literals `true` / `false` / `null`.
//
// The engine's query path can't carry these: a Logic body terminating in
// `return 1` (the common no-op-return shape, e.g. `seed := <builtin>; return
// 1`) lands at engine.Execute as the raw string "1". Parse converts it to a
// LiteralValueNode, but the plan-function-resolution pass clones the root
// through cloneExpressionNode, which has no LiteralValueNode case and returns
// nil -- so plan.Root goes nil and the unbounded-query guard rejects the call
// with "query must include at least one filter or relationship expression".
// memql#1090. Resolving the literal to its value here short-circuits that
// round-trip entirely, the same way coalesce(...) and bare step refs already
// bypass engine.Execute.
//
// Returns (value, true) when expr is a recognised scalar literal, and
// (nil, false) for anything else -- a real query / function / step reference
// the caller must resolve through its normal path. Intentionally strict: only
// the four literal shapes match, so a genuine concept query is never swallowed.
func tryEvaluateLiteralLocally(expr string) (any, bool) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, false
	}
	// Double-quoted string literal -- strconv.Unquote handles escapes.
	if len(expr) >= 2 && strings.HasPrefix(expr, "\"") && strings.HasSuffix(expr, "\"") {
		if unquoted, err := strconv.Unquote(expr); err == nil {
			return unquoted, true
		}
		return nil, false
	}
	// Single-quoted string literal -- the DSL's string form. strconv.Unquote
	// only accepts single quotes around a single rune (Go char-literal
	// semantics), so peel the quotes and take the inner text verbatim. A
	// stray inner single-quote (`'a'b'`) is not a clean literal -> fall
	// through.
	if len(expr) >= 2 && strings.HasPrefix(expr, "'") && strings.HasSuffix(expr, "'") {
		inner := expr[1 : len(expr)-1]
		if !strings.Contains(inner, "'") {
			return inner, true
		}
		return nil, false
	}
	// Keyword literals.
	switch expr {
	case "null":
		return nil, true
	case "true":
		return true, true
	case "false":
		return false, true
	}
	// Integer literal.
	if n, err := strconv.ParseInt(expr, 10, 64); err == nil {
		return n, true
	}
	// Float literal.
	if n, err := strconv.ParseFloat(expr, 64); err == nil {
		return n, true
	}
	return nil, false
}

func tryEvaluateReturnLocally(expr string, evaluator *Evaluator) (any, bool, error) {
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

	// Step-accessor call followed by a field path (#2542 item 4):
	// `rows.first().createdAt`. The engine's expression parser would look
	// the dotted name up as a function, so normalise the accessor call to
	// the dotted form (`rows.first.createdAt`) resolvePath already
	// navigates -- the same rewrite evaluateScalarArg applies to coalesce
	// args. Gated on a bound step so genuine chains (`.where(...)`) and
	// non-step paths keep their existing routes; resolvePath propagates a
	// nil .first() (empty result) through the field walk as a clean nil.
	if normalized := normalizeStepMethodCalls(expr); normalized != expr &&
		!strings.ContainsAny(normalized, "()[]{}\"', \t\n") {
		if firstDot := strings.Index(normalized, "."); firstDot > 0 {
			if evaluator.HasStep(normalized[:firstDot]) {
				val, err := evaluator.EvaluateStepReference(normalized)
				if err != nil {
					return nil, false, err
				}
				return val, true, nil
			}
			// Custom-var root (`args.rows.first().createdAt`; the compiler
			// emits the `$`-prefixed spelling for an args-rooted return, so
			// accept both). resolvePath has no accessor cases outside the
			// `steps.` root -- `.first` on a raw array is not navigable --
			// so route the ORIGINAL call form through the shared chain
			// evaluator, which resolves the base via resolveCollectionBase
			// (the `$`-path for custom roots) and applies the accessor plus
			// the trailing field walk in-memory. Steps stay first in
			// precedence (checked above), mirroring resolveCollectionBaseRaw.
			if root := strings.TrimPrefix(normalized[:firstDot], "$"); isCustomVarRoot(root) && !evaluator.HasStep(root) {
				val, isChain, err := memql.EvaluateCollectionChainString(
					strings.TrimPrefix(expr, "$"),
					evaluator.resolveCollectionBase,
					evaluator.collectionLambdaArgs(),
				)
				if isChain {
					if err != nil {
						return nil, false, err
					}
					return val, true, nil
				}
			}
		}
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
	// Resolve the parens-stripped dotted form (`rows.first` not `rows.first()`).
	// Story 5 (ADR §2.2 / #2303) unified step accessors on the lowercase
	// spelling, which lexically overlaps the Story 4 collection-method set
	// (`first` / `last` / `count` / `empty`). EvaluateStepReference checks for a
	// collection chain BEFORE it strips a trailing `()`, so passing the raw
	// `step.first()` would misroute a step-result accessor into the collection
	// evaluator. The dotted form (no `(`) is unambiguous and resolves through
	// resolvePath's step-accessor cases.
	val, err := evaluator.EvaluateStepReference(inner)
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
func tryEvaluateBuiltinLocally(expr string, evaluator *Evaluator) (any, bool, error) {
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
	switch {
	case name == "coalesce":
		rawArgs, err := splitTopLevelArgs(inner)
		if err != nil {
			return nil, false, fmt.Errorf("parse %s args: %w", name, err)
		}
		val, err := evaluateCoalesceArgs(rawArgs, evaluator)
		if err != nil {
			return nil, false, err
		}
		return val, true, nil
	case name == "cond":
		// cond(predicate, thenValue, elseValue) over a collection chain
		// (#2542 item 2). The predicate and either branch may be a collection
		// chain / comparison / step reference, so evaluate them through the
		// local surface rather than round-tripping through engine.Execute
		// (which fails: "cond" is not a registered function, and the
		// FunctionExecutor serializes its positional args in an unparseable
		// named form). Reached both as a terminal `return cond(...)` and as a
		// `x := cond(...)` step value (reconstructPositionalBuiltinCall).
		rawArgs, err := splitTopLevelArgs(inner)
		if err != nil {
			return nil, false, fmt.Errorf("parse %s args: %w", name, err)
		}
		if len(rawArgs) != 3 {
			return nil, false, fmt.Errorf("cond() requires three arguments: cond(predicate, thenValue, elseValue)")
		}
		val, err := evaluateCondLocally(rawArgs[0], rawArgs[1], rawArgs[2], evaluator)
		if err != nil {
			return nil, false, err
		}
		return val, true, nil
	case memql.IsDateBuiltin(name):
		// Date/duration builtins (#2541): operands resolve through the
		// evaluator's operand path (quoted literals, $-paths, step refs,
		// timestamp()/now, nested builtins), then the shared name-keyed
		// evaluator applies the builtin. An evaluation failure is a hard
		// logic error, not a fall-through -- the engine has no fallback
		// for these calls.
		val, err := evaluator.evaluateDateBuiltinCall(name, inner)
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
// the logic runner evaluates locally: `coalesce` (#362) plus the
// date/duration builtin set (#2541). Extending the set is a matter
// of adding a name here and a case in tryEvaluateBuiltinLocally's
// switch.
func isPositionalBuiltinName(name string) bool {
	if memql.IsDateBuiltin(name) {
		return true
	}
	switch name {
	case "coalesce", "cond":
		return true
	}
	return false
}

// evaluateCondLocally evaluates a cond(predicate, then, else) builtin in a
// logic body (#2542 item 2). The predicate resolves to a boolean; the chosen
// branch value then resolves through the shared logic-time leaf path, so a
// branch that is itself a collection chain / arithmetic / object literal /
// step reference / literal evaluates in-memory.
func evaluateCondLocally(condRaw, thenRaw, elseRaw string, evaluator *Evaluator) (any, error) {
	condVal, err := evaluateCondPredicate(condRaw, evaluator)
	if err != nil {
		return nil, err
	}
	chosen := elseRaw
	if condVal {
		chosen = thenRaw
	}
	// resolveObjectLiteralValue is the shared value resolver: it recurses into
	// a nested object literal, then routes through EvaluateLocalExpr (literals,
	// collection chains, arithmetic, builtins incl. a nested cond) and finally
	// a step / path reference. A quoted-string branch (`"has"`) resolves to its
	// unquoted value via the literal leg.
	return resolveObjectLiteralValue(strings.TrimSpace(chosen), evaluator)
}

// evaluateCondPredicate resolves a cond predicate to a boolean. A genuine
// collection chain (`active.any()`, `rows.where(r => r.vip).any()`) evaluates
// through the in-memory collection surface and its truthiness is the gate; a
// comparison whose operand is a chain (`active.count() > 0`) or a step
// reference is NOT a genuine chain (the accessor overlaps a step method) and
// routes to the condition evaluator, which resolves both operands. Bare
// boolean literals short-circuit.
func evaluateCondPredicate(raw string, evaluator *Evaluator) (bool, error) {
	raw = strings.TrimSpace(raw)
	switch raw {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	}
	if isGenuineCollectionChain(raw) {
		if val, handled, err := tryEvaluateCollectionChainLocally(raw, evaluator); err != nil {
			return false, err
		} else if handled {
			return isTruthy(val), nil
		}
	}
	return evaluator.EvaluateCondition(raw)
}

// reconstructPositionalBuiltinCall rebuilds the source call string for a
// StepTypeFunction step whose target is a positional builtin (e.g.
// `coalesce(a, b)`), from the compiler's FunctionStepConfig (Name +
// positional Args keyed "0","1",...). Returns ("", false) when Name is
// not a locally-evaluable positional builtin. The reconstructed string
// is fed back through tryEvaluateBuiltinLocally so the same arg parsing
// + evaluation path the StepTypeQuery short-circuit uses applies --
// bypassing FunctionExecutor's invalid named-arg serialization for
// positional builtins.
func reconstructPositionalBuiltinCall(fn *FunctionStepConfig) (string, bool) {
	if fn == nil || !isPositionalBuiltinName(fn.Name) {
		return "", false
	}
	var args []string
	for i := 0; ; i++ {
		v, ok := fn.Args[strconv.Itoa(i)]
		if !ok {
			break
		}
		args = append(args, fmt.Sprintf("%v", v))
	}
	return fn.Name + "(" + strings.Join(args, ", ") + ")", true
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
	for i, raw := range rawArgs {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		val, err := evaluateScalarArg(raw, evaluator)
		if err != nil {
			return nil, err
		}
		// #1614: the FINAL arg is the ultimate fallback -- return it even
		// when it resolves to "" (so coalesce(absent, "") -> "" reaches a
		// string field; the #1605 artifact-index logic relies on this).
		// A NON-final arg that resolves to nil or "" is treated as missing
		// and skipped, so coalesce("", fallback) and coalesce(emptyField,
		// fallback) still fall through.
		if i == len(rawArgs)-1 {
			return val, nil
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
	// Normalize step-method CALLS (`rows.first().id`) to the navigable dotted
	// form (`rows.First.id`) the resolver already handles -- so a method-then-
	// field path inside a coalesce arg resolves to its value instead of its raw
	// text (memql#593; matches the arg-time normalizer).
	val, err := evaluator.EvaluateStepReference(normalizeStepMethodCalls(raw))
	if err != nil {
		return nil, nil //nolint:nilerr // soft-fail for coalesce
	}
	return val, nil
}

// stepMethodAccessors are the step-method names resolvePath navigates in dotted
// form (`rows.first` / `rows.count` / `rows.nodes` / ...). Story 5 (ADR §2.2 /
// #2303) retired the capitalized aliases; `Ran` is the lone capitalized
// survivor (no lowercase pair).
var stepMethodAccessors = []string{"first", "last", "empty", "count", "nodes", "Ran"}

// normalizeStepMethodCalls strips the `()` from a step-method CALL so a
// method-THEN-field path (`rows.first().id`) collapses to the navigable dotted
// form (`rows.First.id`) the resolver already understands. The logic-time
// counterpart to the arg-time method-accessor normalizer (memql#593).
func normalizeStepMethodCalls(expr string) string {
	for _, m := range stepMethodAccessors {
		expr = strings.ReplaceAll(expr, "."+m+"()", "."+m)
	}
	return expr
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
// `event` (the first-class triggering-event binding). $input mirrors
// args so the existing $input.X expression resolver works too.
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
	// contract); the runner's step evaluator must bind it from the caller's
	// auth context or a logic step like `role := coalesce(actor.role, "")`
	// silently resolves to the literal path text (#2380, the deploy role
	// gates). Absent auth leaves actor unbound -- reads resolve nil.
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
		evaluator.SetCustom("actor", map[string]any{
			"userId":         ac.UserId,
			"role":           string(ac.Role),
			"identityId":     ac.IdentityId,
			"isClusterOwner": ac.Role == auth.RoleOwner,
		})
	}
	if r.logger != nil {
		evaluator.SetLogger(r.logger)
	}
	return evaluator
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
