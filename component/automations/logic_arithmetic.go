package automations

import (
	"fmt"
	"strings"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// logic_arithmetic.go evaluates binary arithmetic (#2316 / #2542 item 1) and
// expression-led binary comparisons (#2542 item 5) at LOGIC time, in-memory,
// against the local Evaluator -- both share the re-parse + operand-walker
// machinery here. A logic terminal return
// like `return revenue / orders` crosses the compile/re-parse boundary as the
// parenthesized operator form the compiler serializer emits (`(revenue /
// orders)`); the engine cannot evaluate it (the operand step bindings live
// only on the logic's local Evaluator), so the LogicRunner short-circuits it
// here. Arithmetic stays in-memory only -- this path never reaches SQL, and
// the arg-time resolver deliberately does NOT parse arithmetic out of strings
// (a `/` in an arg string is data, not division; the explicit add()/sub()
// builtins cover arg-time arithmetic).

// tryEvaluateArithmeticLocally short-circuits a logic-body expression whose
// TOP-LEVEL node is binary arithmetic. Returns (value, true, nil) when the
// expression is arithmetic and evaluated cleanly, (nil, false, nil) when it
// is anything else (the caller falls through to its normal path), and
// (nil, false, err) on a hard evaluation error (division by zero, float
// modulo, an unresolvable operand).
func tryEvaluateArithmeticLocally(expr string, evaluator *Evaluator) (any, bool, error) {
	expr = strings.TrimSpace(expr)
	// Cheap gate: no operator characters, no arithmetic. Correctness does
	// not depend on this -- a false positive just attempts a parse whose
	// top-level node is not an ArithmeticExpr and falls through.
	if !strings.ContainsAny(expr, "+-*/%") {
		return nil, false, nil
	}
	parsed, err := languageParser.ParseExpression(normalizeReparseSource(expr))
	if err != nil {
		return nil, false, nil
	}
	arith, ok := parsed.(*languageParser.ArithmeticExpr)
	if !ok {
		return nil, false, nil
	}
	val, err := evalLogicArithOperand(arith, evaluator)
	if err != nil {
		return nil, false, err
	}
	return val, true, nil
}

// tryEvaluateComparisonLocally short-circuits a logic-body terminal-return
// expression whose TOP-LEVEL node is an expression-led binary comparison
// (`(args.a - args.b) > 0`, `delta - 5 > 0`) -- the serialized shape the
// compiler emits for a multi-step logic whose return is a predicate over
// computed intermediates. Each operand resolves through the same closed operand
// walker the arithmetic path uses (evalLogicArithOperand), then the shared
// memql.EvaluateComparison applies the operator with the engine's ordering
// semantics -- the multi-step LogicRunner counterpart to the engine
// single-return BinaryComparisonExpression plan-root branch (#2542 item 5).
// Only the multi-step shape reaches here; a single-return boolean logic routes
// through the engine directly, never through RunLogic.
//
// Returns (value, true, nil) when the expression is an expression-led
// comparison that evaluated cleanly, (nil, false, nil) when it is anything else
// (the caller falls through to its normal path), and (nil, false, err) on a hard
// evaluation error (an unresolvable operand).
func tryEvaluateComparisonLocally(expr string, evaluator *Evaluator) (any, bool, error) {
	expr = strings.TrimSpace(expr)
	// Cheap gate: no comparison operator characters, no comparison. Correctness
	// does not depend on this -- a false positive just attempts a parse whose
	// top-level node is not a BinaryComparisonExpr and falls through.
	if !strings.ContainsAny(expr, "<>=!") {
		return nil, false, nil
	}
	parsed, err := languageParser.ParseExpression(normalizeReparseSource(expr))
	if err != nil {
		return nil, false, nil
	}
	cmp, ok := parsed.(*languageParser.BinaryComparisonExpr)
	if !ok {
		return nil, false, nil
	}
	lhs, err := evalLogicArithOperand(cmp.Left, evaluator)
	if err != nil {
		return nil, false, err
	}
	rhs, err := evalLogicArithOperand(cmp.Right, evaluator)
	if err != nil {
		return nil, false, err
	}
	val, err := memql.EvaluateComparison(memql.ComparisonOperator(cmp.Operator), lhs, rhs)
	if err != nil {
		return nil, false, err
	}
	return val, true, nil
}

// normalizeReparseSource rewrites the serializer/compiler artifacts that are
// not valid parser source back to their parseable spellings, outside quoted
// strings: the `$args.` / `$event.` runtime-reference prefixes drop their `$`
// (the evaluator resolves both roots), and `timestamp()` -- the serialized
// form of the reserved clock identifier -- becomes `now` (the parser retired
// the call form, #2301).
func normalizeReparseSource(expr string) string {
	var (
		b         strings.Builder
		inString  bool
		quoteChar byte
	)
	b.Grow(len(expr))
	for i := 0; i < len(expr); i++ {
		c := expr[i]
		if inString {
			b.WriteByte(c)
			if c == '\\' && i+1 < len(expr) {
				i++
				b.WriteByte(expr[i])
				continue
			}
			if c == quoteChar {
				inString = false
			}
			continue
		}
		switch {
		case c == '"' || c == '\'':
			inString = true
			quoteChar = c
			b.WriteByte(c)
		case c == '$' && (strings.HasPrefix(expr[i:], "$args.") || strings.HasPrefix(expr[i:], "$event.")):
			// Drop the `$`; the following root spelling parses as a
			// dotted reference the operand resolver understands.
		case c == 't' && strings.HasPrefix(expr[i:], "timestamp()"):
			b.WriteString("now")
			i += len("timestamp()") - 1
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// evalLogicArithOperand evaluates one node of a re-parsed logic arithmetic
// expression. The supported operand subset is deliberately closed: literals,
// nested arithmetic, bare step / dotted references, `args.X` / `event.X`
// references, the reserved clock `now`, the date/duration builtins (#2541),
// and no-arg step-method chains (`rows.count()`). Anything else is a hard,
// named error rather than a silent literal fall-through.
func evalLogicArithOperand(node languageParser.ExpressionNode, evaluator *Evaluator) (any, error) {
	switch n := node.(type) {
	case *languageParser.ArithmeticExpr:
		lhs, err := evalLogicArithOperand(n.Left, evaluator)
		if err != nil {
			return nil, err
		}
		rhs, err := evalLogicArithOperand(n.Right, evaluator)
		if err != nil {
			return nil, err
		}
		return memql.EvaluateArithmetic(n.Op, lhs, rhs)
	case *languageParser.LiteralExpr:
		return n.Value, nil
	case *languageParser.TimestampExprFunc:
		return evaluator.evaluationClock(), nil
	case *languageParser.ArgRefExpr:
		// `args.X` (the parser strips the `args.` root into Path). The
		// `actor.` accessor shares this node type; route it to its own
		// seeded root.
		if strings.HasPrefix(n.Path, "actor.") || n.Path == "actor" {
			return evaluator.EvaluateValue("$" + n.Path)
		}
		return evaluator.EvaluateValue("$args." + n.Path)
	case *languageParser.SpecReferenceExpr:
		return resolveArithLeafPath(n.Name, evaluator)
	case *languageParser.MethodCallExpr:
		src := n.Raw
		if src == "" {
			var ok bool
			src, ok = reconstructMethodCallSource(n)
			if !ok {
				// A collection chain carrying method arguments / a lambda
				// (`args.rows.count(r => r.active)`) as an arithmetic /
				// comparison operand: the closed operand walker resolves only
				// no-arg step-method accessors (`rows.count()`), so a
				// lambda-carrying aggregate operand of a MULTI-STEP logic
				// terminal return (`return chain.count(m => ...) > 0`) lands
				// here. The single-return engine path evaluates this shape
				// (evalCollScalar); in a multi-step body, wrap the comparison in
				// cond -- `return cond(chain.count(m => ...) > 0, thenValue,
				// elseValue)` (the predicate evaluates through the condition
				// evaluator) -- or bind the aggregate to a step first (#2542).
				return nil, fmt.Errorf("a collection-chain aggregate with method arguments (`.count(m => ...)`) is not supported as an arithmetic/comparison operand in a multi-step logic body; wrap the comparison in cond(predicate, thenValue, elseValue), or make this a single-return logic")
			}
		}
		val, handled, err := EvaluateLocalExpr(src, evaluator)
		if err != nil {
			return nil, err
		}
		if !handled {
			return nil, fmt.Errorf("arithmetic operand %q did not resolve locally", src)
		}
		return UnwrapStepResult(val), nil
	case *languageParser.AddDurationExpr:
		return evalArithDateBuiltin("addDuration", evaluator, n.Timestamp, n.Duration)
	case *languageParser.DaysBetweenExpr:
		return evalArithDateBuiltin("daysBetween", evaluator, n.Date1, n.Date2)
	case *languageParser.SubtractTimestampsExpr:
		return evalArithDateBuiltin("subtractTimestamps", evaluator, n.T1, n.T2)
	case *languageParser.YearExpr:
		return evalArithDateBuiltin("year", evaluator, n.Target)
	case *languageParser.QuarterExpr:
		return evalArithDateBuiltin("quarter", evaluator, n.Target)
	case *languageParser.MonthExpr:
		return evalArithDateBuiltin("month", evaluator, n.Target)
	case *languageParser.DayOfMonthExpr:
		return evalArithDateBuiltin("dayOfMonth", evaluator, n.Target)
	case *languageParser.IsAnniversaryExpr:
		return evalArithDateBuiltin("isAnniversary", evaluator, n.StartDate, n.CheckDate)
	case *languageParser.IsFirstDayOfQuarterExpr:
		return evalArithDateBuiltin("isFirstDayOfQuarter", evaluator, n.Target)
	default:
		return nil, fmt.Errorf("expression %T is not supported as an arithmetic operand in a logic body", node)
	}
}

// reconstructMethodCallSource rebuilds source text for a NO-ARG method-call
// chain over a bare reference receiver (`rows.count()`, `rows.first()`) --
// the only method-call shape a serialized arithmetic operand carries without
// a Raw source span. Returns ok=false for anything richer; the caller emits
// a named unsupported-operand error.
func reconstructMethodCallSource(n *languageParser.MethodCallExpr) (string, bool) {
	if len(n.Args) != 0 {
		return "", false
	}
	switch recv := n.Receiver.(type) {
	case *languageParser.SpecReferenceExpr:
		return recv.Name + "." + n.Method + "()", true
	case *languageParser.MethodCallExpr:
		inner, ok := reconstructMethodCallSource(recv)
		if !ok {
			return "", false
		}
		return inner + "." + n.Method + "()", true
	default:
		return "", false
	}
}

// evalArithDateBuiltin evaluates a date/duration builtin appearing as an
// arithmetic operand (`daysBetween(args.a, args.b) / 7`): operands resolve
// through the same arithmetic-operand walker, then the shared name-keyed
// evaluator applies the builtin.
func evalArithDateBuiltin(name string, evaluator *Evaluator, args ...languageParser.ExpressionNode) (any, error) {
	vals := make([]any, len(args))
	for i, a := range args {
		v, err := evalLogicArithOperand(a, evaluator)
		if err != nil {
			return nil, fmt.Errorf("%s() arg %d: %w", name, i, err)
		}
		vals[i] = v
	}
	return memql.EvaluateDateBuiltin(name, vals)
}

// resolveArithLeafPath resolves a bare identifier or dotted reference operand
// against the local Evaluator, hard-failing on a miss (arithmetic has no
// soft-nil semantics -- an unresolved operand must surface, not compute over
// nil). A bare step id resolves to the step's unwrapped result; a custom-var
// root (`args`, `event`, `ctx`, `input`, `item`) resolves via its `$`-path;
// anything else resolves as a step reference (`rows.first.total`, with
// method-call spellings normalised to the navigable dotted form).
func resolveArithLeafPath(name string, evaluator *Evaluator) (any, error) {
	name = strings.TrimSpace(name)
	if isBareIdentifier(name) && evaluator.HasStep(name) {
		if result := evaluator.steps[name]; result != nil {
			return UnwrapStepResult(result.Result), nil
		}
		return nil, nil
	}
	first := name
	if dot := strings.IndexByte(name, '.'); dot > 0 {
		first = name[:dot]
	}
	if isCustomVarRoot(first) {
		return evaluator.EvaluateValue("$" + name)
	}
	val, err := evaluator.EvaluateStepReference(normalizeStepMethodCalls(name))
	if err != nil {
		return nil, fmt.Errorf("arithmetic operand %q: %w", name, err)
	}
	return UnwrapStepResult(val), nil
}
