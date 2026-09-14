package memql

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/functions"
	"github.com/znasllc-io/memql/component/language/tiers"
	"github.com/znasllc-io/memql/core/num"
)

// expr_eval.go -- EvalExpr, THE in-process evaluator of edition 2026
// (epic memql#5363, memql#5367; D7, D8, D10, D11 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
//
// One parser, one AST, two evaluators (D7): an authored expression is lowered
// to SQL when its position pushes down (the P tier, Lower) and evaluated here
// when it runs in process (the M tier: automation conditions, trigger
// filters, logic bodies, mutation values, step arguments, `refine`). The two
// consume the same tree and are held equal by the differential lane, so every
// rule below is written as the SQL twin's rule stated in Go -- the lowering is
// named next to each one. Evaluation is a walk over the parsed tree: nothing
// here compiles or executes source text.
//
// This file replaces four hand-rolled string scanners (E3 in
// component/automations/evaluator.go, the string half of E4 in
// mutation_templates.go, E5 in automations/steps/function.go, E7 in
// tool_execution.go). It keeps their callers' DATA model and replaces their
// meaning where the record decided a new one:
//
//   - there is no truthiness (D8): a condition is a bool, or absent (false),
//     or it is refused as condition_not_boolean. IsTruthy's reading of "false"
//     and "0" as false is not consulted anywhere here;
//   - equality is typed: `1 == "1"` is false, numbers compare numerically
//     across every Go number kind, strings compare verbatim;
//   - absence has ONE table (D8), written out at exprEqual;
//   - `??` is blank-coalescing through coalesceSelect, the one selection rule
//     (rule 30);
//   - `+` on a string is the old concat(), so the codemod's rewrite of
//     concat(a, b) to a + b is exact;
//   - every function and method is the one entry the catalog
//     (component/language/functions) gives it: its signature is checked from
//     the catalog, and the dispatch tables below are keyed by catalog key, so
//     TestCatalogMatchesTheEvaluators can hold the two to each other.
//
// What it deliberately does NOT do: resolve a name by guessing. A name is a
// lambda parameter, or whatever the caller's ExprScope binds, or `now`;
// anything else is unknown_name. The caller owns the scope (an automation run,
// a logic body, a mutation's args) and this file owns what an expression
// means over it.

// ExprScope resolves the bare names an expression reads. ok=false is an
// unknown name.
type ExprScope interface {
	Lookup(name string) (any, bool)
}

// MapScope is the simple scope: names to values.
type MapScope map[string]any

// Lookup indexes the map.
func (s MapScope) Lookup(name string) (any, bool) {
	v, ok := s[name]
	return v, ok
}

// absentValue is the type of Absent.
type absentValue struct{}

// String prints the sentinel readably in test failures and logs.
func (absentValue) String() string { return "<absent>" }

// MarshalJSON writes JSON null. The sentinel never appears INSIDE a value
// EvalExpr returns -- the container rule drops it -- but a caller that stores
// a top-level Absent without asking IsAbsent would otherwise write `{}`, a
// value nobody authored. null is the honest degradation: absent and null are
// one class in the table.
func (absentValue) MarshalJSON() ([]byte, error) { return []byte("null"), nil }

// Absent is the in-process absence marker: what a member read of a missing
// key yields. JSON null (nil) is the other spelling of absent, and every rule
// of the table treats the two alike; they differ only in containers, where
// Absent contributes nothing and an explicit nil is kept.
var Absent absentValue

// IsAbsent reports whether v is absent: the Absent sentinel, nil, or a typed
// nil (a nil pointer, map or slice) -- the Go shapes of JSON null.
func IsAbsent(v any) bool {
	switch v.(type) {
	case nil, absentValue:
		return true
	case bool, string, int, int64, float64:
		return false
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Interface, reflect.Func, reflect.Chan:
		return rv.IsNil()
	}
	return false
}

// ExprError is an evaluation refusal: a stable Code a caller and a corpus
// cell can match on, a Message for a person, and the Span of the node it is
// about when the node was parsed from source.
type ExprError struct {
	Code    string
	Message string
	Span    ast.Span
}

// Error prints the code, the position when there is one, and the message.
func (e *ExprError) Error() string {
	if e.Span.IsZero() {
		return e.Code + ": " + e.Message
	}
	return fmt.Sprintf("%s at %d:%d: %s", e.Code, e.Span.Line, e.Span.Col, e.Message)
}

// EvalOptions carries what an evaluation may reach beyond its scope. Every
// hook is optional; a nil hook makes the construct it serves a refusal (or,
// for CanonicalID, the identity).
type EvalOptions struct {
	// Now is the clock `now` reads when the scope does not bind it. Zero is
	// time.Now(). It is captured once per call, so one evaluation sees one
	// instant however many times it reads `now`.
	Now time.Time
	// Budget is the number of node evaluations the call may perform; 0 (or
	// less) is tiers.DefaultStepBudget. Exceeding it is
	// expression_budget_exceeded.
	Budget int
	// Predicates resolves a spec or trait by name to its v1 lambda: the
	// parameter name and the body.
	Predicates func(name string) (param string, body ast.ExpressionNode, ok bool)
	// CanonicalID backs canonicalId(value, "concept"). Nil is the identity.
	CanonicalID func(ctx context.Context, value any, concept string) (string, error)
	// Vars backs var / systemVar / secret / systemSecret; kind is the
	// function's name.
	Vars func(ctx context.Context, kind, name string) (string, error)
	// Calls runs a construct call (`query q(a: 1)`). Nil refuses every one
	// with construct_call_not_allowed.
	Calls func(ctx context.Context, call *ast.CallExpr, named map[string]any) (any, error)
}

// EvalExpr evaluates an edition-2026 expression over scope.
//
// The result is a value in the evaluator's domain -- nil, bool, string,
// int64, float64, []any, map[string]any -- or, as the TOP-LEVEL result only,
// the Absent sentinel (test it with IsAbsent). A value read straight out of
// the scope is returned as the scope holds it. Errors are *ExprError, except
// that an error returned by a hook in opts, or by the context, reaches the
// caller unchanged so a typed refusal stays typed.
func EvalExpr(ctx context.Context, n ast.ExpressionNode, scope ExprScope, opts EvalOptions) (any, error) {
	ev := newExprEvaluator(ctx, scope, opts)
	return ev.eval(n, ev.root)
}

// EvalCondition evaluates n and requires a boolean: absent is false, and any
// other value is condition_not_boolean naming its type and its text. There is
// no truthiness (D8) -- the string "yes" is not a condition.
func EvalCondition(ctx context.Context, n ast.ExpressionNode, scope ExprScope, opts EvalOptions) (bool, error) {
	ev := newExprEvaluator(ctx, scope, opts)
	v, err := ev.eval(n, ev.root)
	if err != nil {
		return false, err
	}
	return ev.condition(v, n, "the condition")
}

// exprMaxPredicateDepth bounds how deeply predicate applications may nest. A
// predicate that applies itself, directly or through another, would otherwise
// recurse until the step budget ran out, holding one Go stack frame chain per
// level; the loader refuses such a cycle, and this is the run-time backstop.
// Real predicates nest one or two deep.
const exprMaxPredicateDepth = 64

type exprEvaluator struct {
	ctx       context.Context
	opts      EvalOptions
	root      ExprScope
	now       string
	budget    int
	steps     int
	predDepth int
}

func newExprEvaluator(ctx context.Context, scope ExprScope, opts EvalOptions) *exprEvaluator {
	if ctx == nil {
		ctx = context.Background()
	}
	clock := opts.Now
	if clock.IsZero() {
		clock = time.Now()
	}
	budget := opts.Budget
	if budget <= 0 {
		budget = tiers.DefaultStepBudget
	}
	if scope == nil {
		scope = MapScope(nil)
	}
	return &exprEvaluator{
		ctx:    ctx,
		opts:   opts,
		root:   scope,
		now:    clock.UTC().Format(time.RFC3339Nano),
		budget: budget,
	}
}

// eval is the one dispatch over the parsed tree. Every call is one step of
// the budget, so the budget counts node evaluations exactly: a node skipped by
// a short circuit or an untaken ternary branch costs nothing.
func (ev *exprEvaluator) eval(n ast.ExpressionNode, scope ExprScope) (any, error) {
	ev.steps++
	if ev.steps > ev.budget {
		return nil, exprErr(n, "expression_budget_exceeded",
			"the expression needed more than %d evaluation steps (tiers.DefaultStepBudget, or the caller's budget)", ev.budget)
	}
	if ev.steps&1023 == 0 {
		if err := ev.ctx.Err(); err != nil {
			return nil, err
		}
	}
	switch e := n.(type) {
	case *ast.IdentExpr:
		if e != nil {
			return ev.ident(e, scope)
		}
	case *ast.MemberExpr:
		if e != nil {
			obj, err := ev.eval(e.Object, scope)
			if err != nil {
				return nil, err
			}
			return exprMember(obj, e.Field, e)
		}
	case *ast.CallExpr:
		if e != nil {
			switch {
			case e.Kind != "":
				return ev.constructCall(e, scope)
			case e.Receiver != nil:
				return ev.method(e, scope)
			default:
				return ev.function(e, scope)
			}
		}
	case *ast.UnaryExpr:
		if e != nil {
			return ev.unary(e, scope)
		}
	case *ast.BinaryExpr:
		if e != nil {
			return ev.binary(e, scope)
		}
	case *ast.TernaryExpr:
		if e != nil {
			c, err := ev.eval(e.Condition, scope)
			if err != nil {
				return nil, err
			}
			ok, err := ev.condition(c, e.Condition, "the condition of ?:")
			if err != nil {
				return nil, err
			}
			// Only the chosen branch is evaluated: the other may be an
			// error() or a read that is only meaningful on its own side.
			if ok {
				return ev.eval(e.Then, scope)
			}
			return ev.eval(e.Else, scope)
		}
	case *ast.ListExpr:
		if e != nil {
			return ev.list(e, scope)
		}
	case *ast.MapExpr:
		if e != nil {
			return ev.mapLiteral(e, scope)
		}
	case *ast.ParenExpr:
		if e != nil {
			return ev.eval(e.Inner, scope)
		}
	case *ast.LiteralExpr:
		if e != nil {
			return e.Value, nil
		}
	case *ast.NilExpr:
		return nil, nil
	case *ast.LambdaExpr:
		// A lambda is an argument form, not a value: a collection method
		// (and a relationship traversal, which is SQL) reads it directly and
		// never evaluates the lambda node itself.
		return nil, exprErr(n, "lambda_not_callable",
			"a lambda is only legal as a method argument, as in xs.where(x => ...); `%s` is evaluated on its own here", ast.FormatExpr(n))
	}
	if n == nil {
		return nil, &ExprError{Code: "unsupported_node", Message: "there is no expression to evaluate"}
	}
	return nil, exprErr(n, "unsupported_node", "%T is not an edition-2026 expression node", n)
}

// ident resolves a bare name: the scope first (lambda parameters shadow the
// caller's names), then the clock.
func (ev *exprEvaluator) ident(e *ast.IdentExpr, scope ExprScope) (any, error) {
	if v, ok := scope.Lookup(e.Name); ok {
		return v, nil
	}
	if e.Name == "now" {
		return ev.now, nil
	}
	return nil, exprErr(e, "unknown_name", "%s is not defined here", e.Name)
}

// exprMember reads `.field` (and `.?field`: the two differ only at load,
// where `.?` is required when the object may be absent; at run time both
// propagate absence).
//
//   - absent or nil -> Absent. A read through a missing intermediate is a
//     missing value, never an error (the absence table's last row).
//   - a map -> the key's value, or Absent when the key is missing. A key
//     holding JSON null reads as nil: present-and-null and missing differ
//     only in containers.
//   - a row -> an intrinsic column, or the payload (ExprRow.exprMember).
//   - a list and a numeric field ("0", "-1") -> that element, counting from
//     the end when negative; out of range is Absent.
//   - anything else is normalised first (a typed map or slice, a struct by
//     JSON round trip as the automations resolver read it) and then read;
//     a scalar has no fields, so its member is Absent.
func exprMember(obj any, field string, node ast.ExpressionNode) (any, error) {
	v, err := exprNormalize(obj)
	if err != nil {
		return nil, exprErr(node, "operand_type", "cannot read .%s: %v", field, err)
	}
	switch o := v.(type) {
	case map[string]any:
		if x, ok := o[field]; ok {
			return x, nil
		}
		return Absent, nil
	case []any:
		if i, ok := exprIndexOf(field, len(o)); ok {
			return o[i], nil
		}
		return Absent, nil
	case ExprRow:
		return o.exprMember(field), nil
	}
	return Absent, nil
}

// exprIndexOf reads a member name as a list index: an optional minus sign
// and decimal digits, negative counting from the end.
func exprIndexOf(field string, n int) (int, bool) {
	digits := strings.TrimPrefix(field, "-")
	if digits == "" {
		return 0, false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	i, err := strconv.Atoi(field)
	if err != nil {
		return 0, false
	}
	if i < 0 {
		i += n
	}
	if i < 0 || i >= n {
		return 0, false
	}
	return i, true
}

// ---------------------------------------------------------------------------
// operators
// ---------------------------------------------------------------------------

func (ev *exprEvaluator) unary(e *ast.UnaryExpr, scope ExprScope) (any, error) {
	v, err := ev.eval(e.Operand, scope)
	if err != nil {
		return nil, err
	}
	switch e.Op {
	case "!":
		// `!x`: x must be a bool or absent. Absent is false, so `!absent`
		// is TRUE -- the SQL twin is NOT COALESCE((x), FALSE), which makes
		// every lowered predicate two-valued and `!(x == v)` exactly
		// `x != v` (D8).
		ok, err := ev.condition(v, e.Operand, "the operand of !")
		if err != nil {
			return nil, err
		}
		return !ok, nil
	case "-":
		nv, err := exprNormalize(v)
		if err != nil {
			return nil, exprErr(e, "operand_type", "unary - needs a number: %v", err)
		}
		n, ok := exprNumberOf(nv)
		if !ok {
			return nil, exprErr(e, "operand_type", "unary - needs a number; `%s` is %s", ast.FormatExpr(e.Operand), exprTypeName(nv))
		}
		if n.isInt {
			if n.i == math.MinInt64 {
				return nil, exprErr(e, "arithmetic_overflow", "-(%d) does not fit a 64-bit integer", n.i)
			}
			return -n.i, nil
		}
		return -n.f, nil
	}
	return nil, exprErr(e, "unsupported_node", "unknown unary operator %q", e.Op)
}

func (ev *exprEvaluator) binary(e *ast.BinaryExpr, scope ExprScope) (any, error) {
	switch e.Op {
	case "&&", "||":
		// Short-circuit, and booleans only: each operand is a bool or
		// absent (false); anything else is refused rather than read for
		// truthiness (D8). The right operand is never evaluated when the
		// left decides.
		l, err := ev.eval(e.Left, scope)
		if err != nil {
			return nil, err
		}
		lb, err := ev.condition(l, e.Left, "the left operand of "+e.Op)
		if err != nil {
			return nil, err
		}
		if (e.Op == "&&" && !lb) || (e.Op == "||" && lb) {
			return lb, nil
		}
		r, err := ev.eval(e.Right, scope)
		if err != nil {
			return nil, err
		}
		return ev.condition(r, e.Right, "the right operand of "+e.Op)
	case "??":
		// Rule 30: `a ?? b` is b when a is absent, nil, or a string that is
		// empty or whitespace-only; false, 0, [] and {} are values and are
		// kept. It is driven through coalesceSelect -- THE selection rule
		// the mutation templates' two spellings already share (memql#3627)
		// -- so there is one implementation, not a third. Lazy: b is not
		// evaluated when a wins. The final arm is returned even when blank,
		// and a missing final arm comes back nil, never the sentinel.
		return coalesceSelect(2, func(i int) (any, error) {
			arm := e.Left
			if i == 1 {
				arm = e.Right
			}
			v, err := ev.eval(arm, scope)
			if err != nil {
				return nil, err
			}
			return exprCoalesceArm(v), nil
		})
	}

	lraw, err := ev.eval(e.Left, scope)
	if err != nil {
		return nil, err
	}
	rraw, err := ev.eval(e.Right, scope)
	if err != nil {
		return nil, err
	}
	l, err := exprNormalize(lraw)
	if err != nil {
		return nil, exprErr(e.Left, "operand_type", "the left operand of %s: %v", e.Op, err)
	}
	r, err := exprNormalize(rraw)
	if err != nil {
		return nil, exprErr(e.Right, "operand_type", "the right operand of %s: %v", e.Op, err)
	}

	switch e.Op {
	case "==":
		return exprEqual(e.Left, e.Right, l, r), nil
	case "!=":
		// The exact negation of `==`, for every right-hand side. That is
		// what makes `x != v` null-safe (rule 27, #1685: IS DISTINCT FROM,
		// TRUE on absent) and `x != ""` the "is set" idiom (#1708 / #1714:
		// COALESCE(x, '') <> '', FALSE on absent) at once -- both fall out
		// of the one `==` table below rather than being two carve-outs.
		return !exprEqual(e.Left, e.Right, l, r), nil
	case "<", "<=", ">", ">=":
		return exprOrdered(e.Op, l, r), nil
	case "in":
		return exprIn(e, l, r)
	case "startsWith":
		return exprStartsWith(e, l, r)
	case "+":
		return exprPlus(e, l, r)
	case "-", "*", "/", "%":
		ln, lok := exprNumberOf(l)
		rn, rok := exprNumberOf(r)
		if !lok || !rok {
			return nil, exprErr(e, "operand_type", "%s needs two numbers; `%s` is %s and `%s` is %s",
				e.Op, ast.FormatExpr(e.Left), exprTypeName(l), ast.FormatExpr(e.Right), exprTypeName(r))
		}
		return exprArithmetic(e, ln, rn)
	}
	return nil, exprErr(e, "unsupported_node", "unknown operator %q", e.Op)
}

// exprEqual is `==`, and THE absence table (D8, completing authoring rule
// 27). An absent field, a JSON null, the `nil` literal and the empty string
// are one value when compared: UNSET. Written out, for x absent or null:
//
//	x == nil   true    x != nil   false   unset is unset
//	x == ""    true    x != ""    false   absent equals blank (rule 27)
//	x == "a"   false   x != "a"   true    null-safe inequality (IS DISTINCT FROM)
//	x == 0     false   x != 0     true
//	x == false false   x != false true
//
// and for two set values, typed equality: numbers numerically across int and
// float, strings verbatim (" " is a value, not unset), bools as bools, lists
// and maps deeply, and different types never equal (`1 == "1"` is false).
//
// One notion of unset rather than two is the point. Rule 27 already treats an
// absent string as blank ("both mean not set"), and the retired automation
// evaluator collapsed nil and blank everywhere; keeping the `nil` literal a
// separate presence test would have made `x == nil` and `x == args.missing`
// answer differently for a blank x. With one notion, `==` is symmetric, `!=`
// is its exact negation for every pair, `!(x == v)` is exactly `x != v`
// (TestEvalExprEqualityIsNegationAndSymmetric), and the SQL twin is one
// expression: `COALESCE(x, ”) = ”` for any comparison against unset.
func exprEqual(_, _ ast.ExpressionNode, l, r any) bool {
	lu, ru := exprIsUnset(l), exprIsUnset(r)
	if lu || ru {
		return lu && ru
	}
	return exprStrictEqual(l, r)
}

// exprIsUnset is the one unset test the comparisons share: absent, JSON null
// or the empty string. Whitespace is a value here -- only `??` (rule 30)
// reads a whitespace-only string as blank.
func exprIsUnset(v any) bool {
	if IsAbsent(v) {
		return true
	}
	s, ok := v.(string)
	return ok && s == ""
}

// exprStrictEqual is typed equality with no absence rules: nil equals only
// nil. It is what `==` reaches once neither side is absent, what `in` tests
// membership by (SQL IN never matches NULL, so a blank is not a member of
// [nil]), and what lists and maps compare their elements by.
func exprStrictEqual(a, b any) bool {
	a, errA := exprNormalize(a)
	b, errB := exprNormalize(b)
	if errA != nil || errB != nil {
		return false
	}
	switch x := a.(type) {
	case nil, absentValue:
		return IsAbsent(b)
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case string:
		y, ok := b.(string)
		return ok && x == y
	case int64, float64:
		xn, _ := exprNumberOf(x)
		yn, ok := exprNumberOf(b)
		if !ok {
			return false
		}
		c, ok := exprCompareNumbers(xn, yn)
		return ok && c == 0
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !exprStrictEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, xv := range x {
			yv, ok := y[k]
			if !ok || !exprStrictEqual(xv, yv) {
				return false
			}
		}
		return true
	case ExprRow:
		y, ok := b.(ExprRow)
		return ok && reflect.DeepEqual(x, y)
	}
	return false
}

// exprOrdered is `< <= > >=`: two numbers compare numerically, two strings by
// BYTE (the SQL side compares text COLLATE "C", so RFC3339 UTC timestamps
// order as instants and no locale folds "é" into "e"). Anything else --
// absent on either side, a type mismatch, two bools -- is false, on both
// paths (rule 27: the ordered comparisons are not null-safe).
func exprOrdered(op string, l, r any) bool {
	var c int
	ln, lok := exprNumberOf(l)
	rn, rok := exprNumberOf(r)
	switch {
	case lok && rok:
		cc, ok := exprCompareNumbers(ln, rn)
		if !ok {
			return false
		}
		c = cc
	default:
		ls, lok := l.(string)
		rs, rok := r.(string)
		if !lok || !rok {
			return false
		}
		c = strings.Compare(ls, rs)
	}
	switch op {
	case "<":
		return c < 0
	case "<=":
		return c <= 0
	case ">":
		return c > 0
	case ">=":
		return c >= 0
	}
	return false
}

// exprIn is `v in list`. The right-hand side must be a list, or absent (which
// contains nothing); anything else is in_requires_list -- checked BEFORE the
// left side, so the refusal does not depend on the data. Membership is `==`.
func exprIn(e *ast.BinaryExpr, l, r any) (any, error) {
	var list []any
	switch x := r.(type) {
	case nil, absentValue:
		return false, nil
	case []any:
		list = x
	default:
		return nil, exprErr(e.Right, "in_requires_list", "the right side of `in` must be a list; `%s` is %s",
			ast.FormatExpr(e.Right), exprTypeName(r))
	}
	// `v in [a, b]` is exactly `v == a || v == b`, so membership uses the
	// same equality: an unset v is a member only of a list holding an unset
	// element ("" or nil), and never of ["a"] (rule 27: `in` is not
	// null-safe for a set value).
	for _, el := range list {
		if exprEqual(nil, nil, l, el) {
			return true, nil
		}
	}
	return false, nil
}

// exprStartsWith is `s startsWith p` (rule 32): p is a string or a list of
// strings, blank (empty or whitespace-only) prefixes are dropped by
// normalizePrefixValues -- the SQL twin's own normaliser -- and an empty set
// matches nothing, so a blank input can never widen a selection. The subject
// must be a string; anything else, absent included, is false. A right side
// that is not a string or a list of strings is refused whatever the subject.
func exprStartsWith(e *ast.BinaryExpr, l, r any) (any, error) {
	var raw []string
	switch p := r.(type) {
	case nil, absentValue:
		// No prefix at all: an absent prefix is a blank one.
		return false, nil
	case string:
		raw = []string{p}
	case []any:
		raw = make([]string, 0, len(p))
		for _, item := range p {
			iv, err := exprNormalize(item)
			if err != nil || !exprIsStringOrAbsent(iv) {
				return nil, exprErr(e.Right, "operand_type", "startsWith takes a string or a list of strings; `%s` holds %s",
					ast.FormatExpr(e.Right), exprTypeName(iv))
			}
			if s, ok := iv.(string); ok {
				raw = append(raw, s)
			}
		}
	default:
		return nil, exprErr(e.Right, "operand_type", "startsWith takes a string or a list of strings; `%s` is %s",
			ast.FormatExpr(e.Right), exprTypeName(r))
	}
	prefixes, err := normalizePrefixValues(raw)
	if err != nil {
		return nil, exprErr(e.Right, "operand_type", "%v", err)
	}
	s, ok := l.(string)
	if !ok {
		return false, nil
	}
	return startsWithAny(s, prefixes), nil
}

func exprIsStringOrAbsent(v any) bool {
	switch v.(type) {
	case string, nil, absentValue:
		return true
	}
	return false
}

// exprPlus is `+`, which means three things by operand:
//
//   - two numbers: addition (int64 when both are integers, else float64);
//   - a string on EITHER side: concatenation of both sides' canonical text,
//     absent and nil contributing "" -- exactly what concat() meant, so the
//     codemod's concat(a, b) -> a + b changes no output;
//   - two lists: a list of both.
//
// Anything else -- a number and an absent value, two maps, a bool and a
// number -- is operand_type: `1 + absent` is not 1, and not "1".
func exprPlus(e *ast.BinaryExpr, l, r any) (any, error) {
	ln, lok := exprNumberOf(l)
	rn, rok := exprNumberOf(r)
	if lok && rok {
		return exprArithmetic(e, ln, rn)
	}
	_, ls := l.(string)
	_, rs := r.(string)
	if ls || rs {
		return exprText(l) + exprText(r), nil
	}
	ll, llist := l.([]any)
	rl, rlist := r.([]any)
	if llist && rlist {
		out := make([]any, 0, len(ll)+len(rl))
		out = append(out, ll...)
		return append(out, rl...), nil
	}
	return nil, exprErr(e, "operand_type",
		"+ adds two numbers, joins two lists, or concatenates when either side is a string; `%s` is %s and `%s` is %s",
		ast.FormatExpr(e.Left), exprTypeName(l), ast.FormatExpr(e.Right), exprTypeName(r))
}

// exprArithmetic applies `+ - * / %` to two numbers.
//
// Integer arithmetic runs when both operands are integer-typed, and it never
// wraps: an overflowing result is arithmetic_overflow rather than a number
// with the wrong sign. Otherwise the arithmetic is float64, and a result that
// leaves the finite range (which JSON cannot carry) is refused the same way.
// Division and modulo by zero are division_by_zero. `%` is defined on whole
// numbers, and a float holding one counts -- a decoded payload number is
// always a float64, and `row.n % 2` must work on it -- while `7.5 % 2` is
// operand_type.
//
// Not evalArithmetic (arithmetic.go): its errors carry no code, it wraps on
// overflow, and it refuses `%` on the float64 every decoded payload number is.
func exprArithmetic(e *ast.BinaryExpr, a, b exprNumber) (any, error) {
	if a.isInt && b.isInt {
		x, y := a.i, b.i
		switch e.Op {
		case "+":
			if (y > 0 && x > math.MaxInt64-y) || (y < 0 && x < math.MinInt64-y) {
				return nil, exprOverflow(e, x, y)
			}
			return x + y, nil
		case "-":
			if (y < 0 && x > math.MaxInt64+y) || (y > 0 && x < math.MinInt64+y) {
				return nil, exprOverflow(e, x, y)
			}
			return x - y, nil
		case "*":
			if x != 0 && y != 0 {
				p := x * y
				if p/y != x || (x == -1 && y == math.MinInt64) || (y == -1 && x == math.MinInt64) {
					return nil, exprOverflow(e, x, y)
				}
			}
			return x * y, nil
		case "/":
			if y == 0 {
				return nil, exprErr(e, "division_by_zero", "`%s` divides by zero", ast.FormatExpr(e))
			}
			if x == math.MinInt64 && y == -1 {
				return nil, exprOverflow(e, x, y)
			}
			return x / y, nil
		case "%":
			if y == 0 {
				return nil, exprErr(e, "division_by_zero", "`%s` divides by zero", ast.FormatExpr(e))
			}
			if y == -1 {
				return int64(0), nil
			}
			return x % y, nil
		}
	}
	if e.Op == "%" {
		x, xok := a.whole()
		y, yok := b.whole()
		if !xok || !yok {
			return nil, exprErr(e, "operand_type", "%% needs whole numbers; `%s` is not one", ast.FormatExpr(e))
		}
		if y == 0 {
			return nil, exprErr(e, "division_by_zero", "`%s` divides by zero", ast.FormatExpr(e))
		}
		if y == -1 {
			return int64(0), nil
		}
		return x % y, nil
	}
	x, y := a.float(), b.float()
	var r float64
	switch e.Op {
	case "+":
		r = x + y
	case "-":
		r = x - y
	case "*":
		r = x * y
	case "/":
		if y == 0 {
			return nil, exprErr(e, "division_by_zero", "`%s` divides by zero", ast.FormatExpr(e))
		}
		r = x / y
	default:
		return nil, exprErr(e, "unsupported_node", "unknown arithmetic operator %q", e.Op)
	}
	if math.IsInf(r, 0) || math.IsNaN(r) {
		return nil, exprErr(e, "arithmetic_overflow", "`%s` leaves the range of a finite number", ast.FormatExpr(e))
	}
	return r, nil
}

func exprOverflow(e *ast.BinaryExpr, x, y int64) *ExprError {
	return exprErr(e, "arithmetic_overflow", "%d %s %d does not fit a 64-bit integer", x, e.Op, y)
}

// condition reads v as a boolean: a bool is itself, absent (missing or nil)
// is false, and anything else is condition_not_boolean naming the value's
// type and the source text that produced it. There is no truthiness (D8):
// neither "false" nor "" nor 0 nor [] is a condition.
func (ev *exprEvaluator) condition(v any, node ast.ExpressionNode, role string) (bool, error) {
	nv, err := exprNormalize(v)
	if err == nil {
		switch b := nv.(type) {
		case bool:
			return b, nil
		case nil, absentValue:
			return false, nil
		}
	} else {
		nv = v
	}
	return false, exprErr(node, "condition_not_boolean", "%s `%s` is %s; a condition must be boolean",
		role, ast.FormatExpr(node), exprTypeName(nv))
}

// exprCoalesceArm prepares one `??` arm for coalesceSelect: the Absent
// sentinel becomes the mutation templates' missingValue (so the one selection
// rule treats both notions of missing alike), a typed nil becomes nil, and a
// named string type becomes a string so its blankness is visible.
func exprCoalesceArm(v any) any {
	switch v.(type) {
	case absentValue:
		return missingValue{}
	case nil, string:
		return v
	}
	if IsAbsent(v) {
		return nil
	}
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.String {
		return rv.String()
	}
	return v
}

// ---------------------------------------------------------------------------
// containers
// ---------------------------------------------------------------------------

// list builds a list literal. CONTAINER RULE (rule 30's last paragraph,
// memql#3627): an element that evaluates to the Absent sentinel contributes
// nothing, exactly as a missing arg contributed nothing to a mutation
// template's array; an explicit nil the author wrote is kept.
func (ev *exprEvaluator) list(e *ast.ListExpr, scope ExprScope) (any, error) {
	out := make([]any, 0, len(e.Elems))
	for _, el := range e.Elems {
		v, err := ev.eval(el, scope)
		if err != nil {
			return nil, err
		}
		if _, absent := v.(absentValue); absent {
			continue
		}
		out = append(out, v)
	}
	return out, nil
}

// mapLiteral builds an object literal under the same container rule: an
// Absent value omits its key, an explicit nil keeps it as null.
func (ev *exprEvaluator) mapLiteral(e *ast.MapExpr, scope ExprScope) (any, error) {
	out := make(map[string]any, len(e.Entries))
	for _, en := range e.Entries {
		v, err := ev.eval(en.Value, scope)
		if err != nil {
			return nil, err
		}
		if _, absent := v.(absentValue); absent {
			continue
		}
		out[en.Key] = v
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// calls: the catalog, predicates, construct calls
// ---------------------------------------------------------------------------

// exprFunctionImpl implements one catalog function over its evaluated,
// normalised arguments.
type exprFunctionImpl func(ev *exprEvaluator, e *ast.CallExpr, args []any) (any, error)

// exprMethodImpl implements one catalog method. recv is the normalised
// receiver: the string for a string method, the collection ([]any) for a list
// method -- except a method only a string has (includes), which reads
// whatever the receiver is and decides itself.
type exprMethodImpl func(ev *exprEvaluator, e *ast.CallExpr, recv any, scope ExprScope) (any, error)

var (
	// exprCatalog is component/language/functions' catalog by Function.Key,
	// copied once so a call does not copy an entry per evaluation.
	exprCatalog map[string]functions.Function
	// exprFunctionImpls and exprMethodImpls are the dispatch, keyed by
	// catalog key ("lower", "list.where", "string.includes"). A key here is
	// what EvalExpr implements: TestCatalogMatchesTheEvaluators holds these
	// two maps and the catalog to each other in both directions. The
	// relationship traversals are the catalog's only entries with no key
	// here -- they select rows in SQL and have no in-process meaning.
	exprFunctionImpls map[string]exprFunctionImpl
	exprMethodImpls   map[string]exprMethodImpl
)

// The tables are built in init because the method bodies evaluate lambdas,
// which reaches back into the dispatch: a package-level initialiser would be
// an initialisation cycle.
func init() {
	exprCatalog = map[string]functions.Function{}
	for _, f := range functions.Catalog() {
		exprCatalog[f.Key()] = f
	}
	variable := func(ev *exprEvaluator, e *ast.CallExpr, a []any) (any, error) { return ev.variable(e, a[0]) }
	exprFunctionImpls = map[string]exprFunctionImpl{
		"lower": func(_ *exprEvaluator, _ *ast.CallExpr, a []any) (any, error) {
			return strings.ToLower(exprText(a[0])), nil
		},
		"upper": func(_ *exprEvaluator, _ *ast.CallExpr, a []any) (any, error) {
			return strings.ToUpper(exprText(a[0])), nil
		},
		"trim": func(_ *exprEvaluator, _ *ast.CallExpr, a []any) (any, error) {
			return strings.TrimSpace(exprText(a[0])), nil
		},
		// A digest of the canonical text. Absent hashes as "" rather than
		// yielding "", so hash() is fixed-width on every input (memql#3009:
		// a zero-width part would let two different composite ids
		// concatenate to one).
		"hash": func(_ *exprEvaluator, _ *ast.CallExpr, a []any) (any, error) {
			sum := sha256.Sum256([]byte(exprText(a[0])))
			return hex.EncodeToString(sum[:]), nil
		},
		// The RuntimeEvaluator's builtin: strips ONE canonical prefix
		// (memql#2981); absent or blank in, "" out.
		"shortId": func(_ *exprEvaluator, _ *ast.CallExpr, a []any) (any, error) {
			return NewRuntimeEvaluator(nil).EvaluateShortId(exprText(a[0])), nil
		},
		"toString": func(_ *exprEvaluator, _ *ast.CallExpr, a []any) (any, error) {
			return exprText(a[0]), nil
		},
		"canonicalId": func(ev *exprEvaluator, e *ast.CallExpr, a []any) (any, error) {
			return ev.canonicalID(e, a[0], a[1])
		},
		"addDuration":  exprAddDuration,
		"daysBetween":  exprDaysBetween,
		"error":        exprRaise,
		"var":          variable,
		"systemVar":    variable,
		"secret":       variable,
		"systemSecret": variable,
	}
	exprMethodImpls = map[string]exprMethodImpl{
		"string.includes":  (*exprEvaluator).includes,
		"string.count":     exprStringCount,
		"list.count":       exprListMethod(exprListCount),
		"list.any":         exprListMethod((*exprEvaluator).listAny),
		"list.all":         exprListMethod((*exprEvaluator).listAll),
		"list.where":       exprListMethod((*exprEvaluator).listWhere),
		"list.select":      exprListMethod((*exprEvaluator).listSelect),
		"list.first":       exprListMethod(exprListFirst),
		"list.last":        exprListMethod(exprListLast),
		"list.single":      exprListMethod(exprListSingle),
		"list.empty":       exprListMethod(exprListEmpty),
		"list.sum":         exprListMethod((*exprEvaluator).listAggregate),
		"list.min":         exprListMethod((*exprEvaluator).listAggregate),
		"list.max":         exprListMethod((*exprEvaluator).listAggregate),
		"list.avg":         exprListMethod((*exprEvaluator).listAggregate),
		"list.orderBy":     exprListMethod((*exprEvaluator).listOrderBy),
		"list.orderByDesc": exprListMethod((*exprEvaluator).listOrderBy),
		"list.groupBy":     exprListMethod((*exprEvaluator).listGroupBy),
		"list.distinct":    exprListMethod((*exprEvaluator).listDistinct),
		"list.take":        exprListMethod((*exprEvaluator).listTakeSkip),
		"list.skip":        exprListMethod((*exprEvaluator).listTakeSkip),
		"list.reduce":      exprListMethod((*exprEvaluator).listReduce),
		"list.nodes":       exprListMethod(exprListNodes),
	}
}

// exprLambdaParams is how many parameters a catalog entry's lambda takes: one
// (the element) for every entry but reduce, whose lambda also takes the
// accumulator.
var exprLambdaParams = map[string]int{"list.reduce": 2}

// exprCheckShape checks a call's argument list against its catalog
// signature: the count (optional parameters may be left out), and the KIND of
// each argument -- a lambda where the signature has a lambda, a value where
// it has anything else, and a lambda of the right parameter count. A call of
// the wrong shape is argument_count naming the signature, and it is refused
// before anything is evaluated, so the refusal does not depend on the data.
// The TYPE of a value argument is checked when the value exists, by the
// function that reads it (invalid_argument).
func exprCheckShape(e *ast.CallExpr, fn functions.Function) error {
	if len(e.Named) > 0 {
		return exprErr(e, "argument_count", "%s takes positional arguments, not named ones", fn.Signature())
	}
	required := 0
	for _, p := range fn.Params {
		if !p.Optional {
			required++
		}
	}
	n := len(e.Args)
	if n < required || n > len(fn.Params) {
		want := strconv.Itoa(required)
		if required != len(fn.Params) {
			want = strconv.Itoa(required) + " to " + strconv.Itoa(len(fn.Params))
		}
		return exprErr(e, "argument_count", "%s takes %s argument(s), got %d", fn.Signature(), want, n)
	}
	for i, a := range e.Args {
		p := fn.Params[i]
		lam, isLambda := ast.Unparen(a).(*ast.LambdaExpr)
		isLambda = isLambda && lam != nil
		switch {
		case p.Type == functions.TypeLambda && !isLambda:
			return exprErr(e, "argument_count", "argument %d of %s is a lambda, as in x => ...; got `%s`", i+1, fn.Signature(), ast.FormatExpr(a))
		case p.Type != functions.TypeLambda && isLambda:
			return exprErr(e, "argument_count", "argument %d of %s is a value, not a lambda", i+1, fn.Signature())
		case isLambda:
			want := 1
			if k, ok := exprLambdaParams[fn.Key()]; ok {
				want = k
			}
			if len(lam.Params) != want {
				return exprErr(e, "argument_count", "the lambda of %s takes %d parameter(s); `%s` has %d", fn.Signature(), want, ast.FormatExpr(lam), len(lam.Params))
			}
		}
	}
	return nil
}

// function evaluates a call with no receiver and no construct kind: a catalog
// function, else a predicate application, else unknown_function. The catalog
// is consulted first, so a spec or trait can never shadow a builtin by taking
// its name.
func (ev *exprEvaluator) function(e *ast.CallExpr, scope ExprScope) (any, error) {
	if fn, inCatalog := exprCatalog[e.Name]; inCatalog && fn.Receiver == "" {
		impl, implemented := exprFunctionImpls[e.Name]
		if !implemented {
			return nil, exprErr(e, "unknown_function",
				"%s is a relationship traversal: it selects rows where a query filter runs, and has no in-process value", fn.Signature())
		}
		if err := exprCheckShape(e, fn); err != nil {
			return nil, err
		}
		args := make([]any, len(e.Args))
		for i, a := range e.Args {
			v, err := ev.eval(a, scope)
			if err != nil {
				return nil, err
			}
			nv, err := exprNormalize(v)
			if err != nil {
				return nil, exprErr(a, "invalid_argument", "argument %d of %s: %v", i+1, fn.Signature(), err)
			}
			args[i] = nv
		}
		return impl(ev, e, args)
	}
	if ev.opts.Predicates != nil {
		if param, body, ok := ev.opts.Predicates(e.Name); ok {
			return ev.applyPredicate(e, param, body, scope)
		}
	}
	if replacement, retired := functions.RetiredFunctions()[e.Name]; retired {
		return nil, exprErr(e, "unknown_function", "%s() is retired in edition 2026: write %s", e.Name, replacement)
	}
	return nil, exprErr(e, "unknown_function", "%s() is not a function or a predicate known here", e.Name)
}

// canonicalID is canonicalId(value, "concept"): empty in, empty out (an
// absent or blank id is an absent optional id, as every earlier spelling of
// the builtin answered), else the hook's answer for the trimmed text, else --
// with no hook -- the trimmed text itself.
func (ev *exprEvaluator) canonicalID(e *ast.CallExpr, value, concept any) (any, error) {
	c, ok := concept.(string)
	if !ok || strings.TrimSpace(c) == "" {
		return nil, exprErr(e, "invalid_argument", "canonicalId() takes the concept as its second argument, a non-blank string")
	}
	text := strings.TrimSpace(exprText(value))
	if text == "" {
		return "", nil
	}
	if ev.opts.CanonicalID == nil {
		return text, nil
	}
	return ev.opts.CanonicalID(ev.ctx, text, strings.TrimSpace(c))
}

// variable is var / systemVar / secret / systemSecret: the caller's resolver,
// asked by the function's own name. A secret's value is returned and never
// logged here.
func (ev *exprEvaluator) variable(e *ast.CallExpr, arg any) (any, error) {
	name, ok := arg.(string)
	if !ok || strings.TrimSpace(name) == "" {
		return nil, exprErr(e, "invalid_argument", "%s() takes the variable's name as a string", e.Name)
	}
	if ev.opts.Vars == nil {
		return nil, exprErr(e, "var_not_available", "%s(%q) cannot be resolved here: no variable resolver is configured", e.Name, name)
	}
	return ev.opts.Vars(ev.ctx, e.Name, strings.TrimSpace(name))
}

// exprAddDuration is addDuration(ts, dur). parseDate is the RuntimeEvaluator's
// flexible reader (RFC3339, with fractional seconds, or a bare date) and
// parseISO8601Duration its duration reader, a leading minus moving back; the
// answer is always RFC3339Nano in UTC, the one timestamp spelling the
// language compares by byte.
func exprAddDuration(_ *exprEvaluator, e *ast.CallExpr, a []any) (any, error) {
	ts, err := exprStringArg(e, 0, a[0], "a timestamp")
	if err != nil {
		return nil, err
	}
	dur, err := exprStringArg(e, 1, a[1], "an ISO 8601 duration")
	if err != nil {
		return nil, err
	}
	t, err := parseDate(ts)
	if err != nil {
		return nil, exprErr(e, "invalid_argument", "addDuration(): %q is not a timestamp: %v", ts, err)
	}
	d, err := parseISO8601Duration(dur)
	if err != nil {
		return nil, exprErr(e, "invalid_argument", "addDuration(): %q is not an ISO 8601 duration: %v", dur, err)
	}
	return t.Add(d).UTC().Format(time.RFC3339Nano), nil
}

// exprDaysBetween is daysBetween(a, b): whole days from a to b, truncated
// toward zero, through the RuntimeEvaluator's own implementation.
func exprDaysBetween(_ *exprEvaluator, e *ast.CallExpr, a []any) (any, error) {
	from, err := exprStringArg(e, 0, a[0], "a date")
	if err != nil {
		return nil, err
	}
	to, err := exprStringArg(e, 1, a[1], "a date")
	if err != nil {
		return nil, err
	}
	days, err := NewRuntimeEvaluator(nil).EvaluateDaysBetween(from, to)
	if err != nil {
		return nil, exprErr(e, "invalid_argument", "daysBetween(): %v", err)
	}
	return int64(days), nil
}

// exprRaise is error(message): the author raising, with their message
// verbatim.
func exprRaise(_ *exprEvaluator, e *ast.CallExpr, a []any) (any, error) {
	return nil, &ExprError{Code: "raised", Message: exprText(a[0]), Span: e.Span}
}

// exprStringArg requires a normalised argument to be a string; an absent one
// is refused -- the date functions have no answer for a missing input.
func exprStringArg(e *ast.CallExpr, i int, v any, what string) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", exprErr(e, "invalid_argument", "%s() argument %d must be %s (a string); it is %s", e.Name, i+1, what, exprTypeName(v))
	}
	return s, nil
}

// applyPredicate evaluates a spec or trait applied to its receiver,
// `isActiveRecord(row)`: the predicate's body with its parameter bound to the
// argument. The body's scope is the parameter over the caller's ROOT scope,
// not the scope at the call site: a predicate is declared at top level, so a
// lambda parameter of the caller (`x` in `xs.where(x => isP(x))`) must not
// leak into it -- a body that read one would mean different things at
// different call sites.
func (ev *exprEvaluator) applyPredicate(e *ast.CallExpr, param string, body ast.ExpressionNode, scope ExprScope) (any, error) {
	if len(e.Args) != 1 || len(e.Named) > 0 {
		return nil, exprErr(e, "argument_count", "a predicate is applied to exactly one argument, as in %s(row); got %d", e.Name, len(e.Args)+len(e.Named))
	}
	if exprIsLambda(e.Args[0]) {
		return nil, exprErr(e, "argument_count", "%s() is applied to a value, not a lambda", e.Name)
	}
	arg, err := ev.eval(e.Args[0], scope)
	if err != nil {
		return nil, err
	}
	if ev.predDepth >= exprMaxPredicateDepth {
		return nil, exprErr(e, "predicate_depth_exceeded", "predicate applications nest more than %d deep at %s(); a predicate applies itself", exprMaxPredicateDepth, e.Name)
	}
	ev.predDepth++
	defer func() { ev.predDepth-- }()
	v, err := ev.eval(body, &exprLambdaScope{parent: ev.root, names: []string{param}, values: []any{arg}})
	if err != nil {
		return nil, err
	}
	return ev.condition(v, body, "the body of predicate "+e.Name)
}

// constructCall is `query q(a: 1)` and its siblings: the caller's hook runs
// it. Refused when there is no hook -- a position that may not call
// constructs hands none -- and checked before any argument is evaluated. A
// named argument whose value is Absent is not passed, under the container
// rule.
func (ev *exprEvaluator) constructCall(e *ast.CallExpr, scope ExprScope) (any, error) {
	if ev.opts.Calls == nil {
		return nil, exprErr(e, "construct_call_not_allowed", "%s %s(...) cannot be called from this position", e.Kind, e.Name)
	}
	if len(e.Args) > 0 {
		return nil, exprErr(e, "argument_count", "%s %s() takes named arguments, as in %s %s(name: value)", e.Kind, e.Name, e.Kind, e.Name)
	}
	named := make(map[string]any, len(e.Named))
	for _, a := range e.Named {
		v, err := ev.eval(a.Value, scope)
		if err != nil {
			return nil, err
		}
		if _, absent := v.(absentValue); absent {
			continue
		}
		named[a.Name] = v
	}
	return ev.opts.Calls(ev.ctx, e, named)
}

// ---------------------------------------------------------------------------
// methods
// ---------------------------------------------------------------------------

// exprMethodEntry finds a method's catalog entry for one receiver type,
// falling back, as functions.Method does, to a method every receiver has.
func exprMethodEntry(receiver, name string) (functions.Function, bool) {
	if f, ok := exprCatalog[receiver+"."+name]; ok {
		return f, true
	}
	f, ok := exprCatalog[functions.TypeAny+"."+name]
	return f, ok
}

// method evaluates `recv.name(args)`. The catalog decides everything about
// the call but the receiver's runtime type: whether the name is a method at
// all, its signature (checked before the receiver is evaluated), and -- once
// the receiver's type is known -- which entry runs.
func (ev *exprEvaluator) method(e *ast.CallExpr, scope ExprScope) (any, error) {
	listFn, onList := exprMethodEntry(functions.TypeList, e.Name)
	strFn, onString := exprMethodEntry(functions.TypeString, e.Name)
	if !onList && !onString {
		if replacement, retired := functions.RetiredMethods()[functions.TypeList+"."+e.Name]; retired {
			return nil, exprErr(e, "unknown_method", ".%s() is retired in edition 2026: write %s", e.Name, replacement)
		}
		return nil, exprErr(e, "unknown_method", ".%s() is not a method", e.Name)
	}
	// The shape is checked against the entry the name has; a name on both
	// receivers (count) must fit one of them here and is checked against the
	// receiver's own entry below.
	shapeFn := listFn
	if !onList {
		shapeFn = strFn
	}
	if err := exprCheckShape(e, shapeFn); err != nil {
		if !onList || !onString || exprCheckShape(e, strFn) != nil {
			return nil, err
		}
	}

	raw, err := ev.eval(e.Receiver, scope)
	if err != nil {
		return nil, err
	}
	recv, err := exprNormalize(raw)
	if err != nil {
		return nil, exprErr(e.Receiver, "operand_type", ".%s(): %v", e.Name, err)
	}

	if !onList {
		// A method only a string has (includes) decides about every
		// receiver itself.
		return ev.dispatchMethod(e, strFn, recv, scope)
	}
	if s, isString := recv.(string); isString {
		if !onString {
			return nil, exprErr(e, "operand_type", ".%s() is a list method; `%s` is a string", e.Name, ast.FormatExpr(e.Receiver))
		}
		if err := exprCheckShape(e, strFn); err != nil {
			return nil, err
		}
		return ev.dispatchMethod(e, strFn, s, scope)
	}
	if err := exprCheckShape(e, listFn); err != nil {
		return nil, err
	}
	coll, present, err := exprCollection(recv)
	if err != nil {
		return nil, exprErr(e.Receiver, "operand_type", ".%s() is a list method; `%s` is %s", e.Name, ast.FormatExpr(e.Receiver), exprTypeName(recv))
	}
	if !present {
		return ev.absentReceiverMethod(e, scope)
	}
	return ev.dispatchMethod(e, listFn, coll, scope)
}

func (ev *exprEvaluator) dispatchMethod(e *ast.CallExpr, fn functions.Function, recv any, scope ExprScope) (any, error) {
	impl, ok := exprMethodImpls[fn.Key()]
	if !ok {
		return nil, exprErr(e, "unknown_method", "%s has no in-process implementation", fn.Signature())
	}
	return impl(ev, e, recv, scope)
}

// exprCollection reads a normalised receiver as a list, as
// collection_method.go's toCollection does: a list is itself, a bundle
// envelope (a map with `nodes`) is its node list, and any other map or a row
// is a one-element list -- a step or query that returned a single object.
// Absent and nil are no collection at all (present=false), which the caller
// answers per method. A number or bool is not a collection (the error) rather
// than a one-element list: `5.any(x => ...)` is a mistake, not a question.
func exprCollection(v any) ([]any, bool, error) {
	switch x := v.(type) {
	case nil, absentValue:
		return nil, false, nil
	case []any:
		return x, true, nil
	case map[string]any:
		if nodes, ok := x["nodes"]; ok {
			nv, err := exprNormalize(nodes)
			if err != nil {
				return nil, false, err
			}
			coll, present, err := exprCollection(nv)
			if err != nil {
				return nil, false, err
			}
			if !present {
				return []any{}, true, nil
			}
			return coll, true, nil
		}
		return []any{x}, true, nil
	case ExprRow:
		return []any{x}, true, nil
	}
	return nil, false, fmt.Errorf("not a collection")
}

// absentReceiverMethod answers a list method whose receiver is absent, as the
// catalog documents each one: an absent list is an empty list -- count 0,
// any false, all true, empty true, sum 0, min / max / avg absent, the
// list-producing methods an empty list, reduce its seed -- except that
// first() and last() are the Absent sentinel (there was never an element to
// be missing) where an empty list's are nil, and single() is refused, as it
// is on an empty list.
func (ev *exprEvaluator) absentReceiverMethod(e *ast.CallExpr, scope ExprScope) (any, error) {
	switch e.Name {
	case "count":
		return int64(0), nil
	case "any":
		return false, nil
	case "all", "empty":
		return true, nil
	case "first", "last":
		return Absent, nil
	case "sum":
		return float64(0), nil
	case "min", "max", "avg":
		return nil, nil
	case "single":
		return nil, exprErr(e, "single_requires_one", "single() needs exactly one element; `%s` is absent", ast.FormatExpr(e.Receiver))
	case "reduce":
		return ev.eval(e.Args[0], scope)
	}
	return []any{}, nil
}

// exprListMethod adapts a method over the collection to the dispatch
// signature; method() only dispatches a list entry with a []any.
func exprListMethod(f func(ev *exprEvaluator, e *ast.CallExpr, coll []any, scope ExprScope) (any, error)) exprMethodImpl {
	return func(ev *exprEvaluator, e *ast.CallExpr, recv any, scope ExprScope) (any, error) {
		coll, _ := recv.([]any)
		return f(ev, e, coll, scope)
	}
}

// includes is the string method `s.includes(sub)`, the substring test that
// was contains(s, sub): whether sub occurs in s.
//
//   - A BLANK sub (empty or whitespace-only) matches NOTHING, exactly as a
//     blank startsWith prefix does (rule 32). "" occurs in every string, so
//     the literal reading would let a caller who sends an empty search widen
//     a selection to every row -- the fail-open shape rule 32 exists to
//     refuse. A selection is never a pass-through.
//   - A LIST receiver is refused: `.includes(v)` reads as list membership to
//     anyone who writes JavaScript, and answering false would hide that the
//     language spells membership `v in list`.
//   - Any other non-string receiver, absent included, is false -- the subject
//     must be a string, as for startsWith.
//   - An absent sub is false; a sub that is not a string is refused.
func (ev *exprEvaluator) includes(e *ast.CallExpr, recv any, scope ExprScope) (any, error) {
	if _, isList := recv.([]any); isList {
		return nil, exprErr(e, "operand_type", "includes() tests a substring and `%s` is a list; for list membership write `v in list`", ast.FormatExpr(e.Receiver))
	}
	raw, err := ev.eval(e.Args[0], scope)
	if err != nil {
		return nil, err
	}
	needle, err := exprNormalize(raw)
	if err != nil {
		return nil, exprErr(e.Args[0], "invalid_argument", "includes(): %v", err)
	}
	var sub string
	switch n := needle.(type) {
	case nil, absentValue:
		return false, nil
	case string:
		if strings.TrimSpace(n) == "" {
			return false, nil
		}
		sub = n
	default:
		return nil, exprErr(e.Args[0], "invalid_argument", "includes() takes a string; `%s` is %s", ast.FormatExpr(e.Args[0]), exprTypeName(needle))
	}
	s, ok := recv.(string)
	if !ok {
		return false, nil
	}
	return strings.Contains(s, sub), nil
}

// exprStringCount is `s.count()`: characters, not bytes -- "é" is one.
func exprStringCount(_ *exprEvaluator, _ *ast.CallExpr, recv any, _ ExprScope) (any, error) {
	s, _ := recv.(string)
	return int64(utf8.RuneCountInString(s)), nil
}

// The list methods keep collection_method.go's behaviour method for method,
// with the changes the record and the catalog decided: a lambda that decides
// (where, any, all) answers a bool or is refused (D8, no truthiness); count,
// first, last and single take no argument, the predicate forms retired in
// favour of filtering first (`xs.where(x => p).first()`); single on anything
// but exactly one element is single_requires_one; and distinct and groupBy
// key by TYPED value (the old %v key made 1 and "1" one group).

func exprListCount(_ *exprEvaluator, _ *ast.CallExpr, coll []any, _ ExprScope) (any, error) {
	return int64(len(coll)), nil
}

func exprListFirst(_ *exprEvaluator, _ *ast.CallExpr, coll []any, _ ExprScope) (any, error) {
	if len(coll) == 0 {
		return nil, nil
	}
	return coll[0], nil
}

func exprListLast(_ *exprEvaluator, _ *ast.CallExpr, coll []any, _ ExprScope) (any, error) {
	if len(coll) == 0 {
		return nil, nil
	}
	return coll[len(coll)-1], nil
}

func exprListSingle(_ *exprEvaluator, e *ast.CallExpr, coll []any, _ ExprScope) (any, error) {
	if len(coll) != 1 {
		return nil, exprErr(e, "single_requires_one", "single() needs exactly one element; `%s` has %d", ast.FormatExpr(e.Receiver), len(coll))
	}
	return coll[0], nil
}

func exprListEmpty(_ *exprEvaluator, _ *ast.CallExpr, coll []any, _ ExprScope) (any, error) {
	return len(coll) == 0, nil
}

func exprListNodes(_ *exprEvaluator, _ *ast.CallExpr, coll []any, _ ExprScope) (any, error) {
	return coll, nil
}

func (ev *exprEvaluator) listAny(e *ast.CallExpr, coll []any, scope ExprScope) (any, error) {
	lam := ev.bind(e, 0, scope)
	for _, el := range coll {
		ok, err := lam.test(el)
		if err != nil || ok {
			return ok, err
		}
	}
	return false, nil
}

func (ev *exprEvaluator) listAll(e *ast.CallExpr, coll []any, scope ExprScope) (any, error) {
	lam := ev.bind(e, 0, scope)
	for _, el := range coll {
		ok, err := lam.test(el)
		if err != nil || !ok {
			return ok, err
		}
	}
	return true, nil
}

func (ev *exprEvaluator) listWhere(e *ast.CallExpr, coll []any, scope ExprScope) (any, error) {
	lam := ev.bind(e, 0, scope)
	out := make([]any, 0, len(coll))
	for _, el := range coll {
		ok, err := lam.test(el)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, el)
		}
	}
	return out, nil
}

// listSelect projects each element. The list keeps its length: a missing
// field projects as nil, never as a hole that shifts every later element.
func (ev *exprEvaluator) listSelect(e *ast.CallExpr, coll []any, scope ExprScope) (any, error) {
	lam := ev.bind(e, 0, scope)
	out := make([]any, 0, len(coll))
	for _, el := range coll {
		v, err := lam.apply(el)
		if err != nil {
			return nil, err
		}
		out = append(out, exprUnsentinel(v))
	}
	return out, nil
}

// listAggregate is sum / min / max / avg through collection_method.go's
// aggregateNumeric: sum of nothing is 0, min / max / avg of nothing is nil,
// and every lambda result must be a number.
func (ev *exprEvaluator) listAggregate(e *ast.CallExpr, coll []any, scope ExprScope) (any, error) {
	lam := ev.bind(e, 0, scope)
	var lambdaErr error
	apply := func(_ *LambdaExpression, el any) (any, error) {
		v, err := lam.apply(el)
		if err != nil {
			lambdaErr = err
			return nil, err
		}
		nv, err := exprNormalize(v)
		if err != nil {
			return nil, err
		}
		return exprUnsentinel(nv), nil
	}
	v, err := aggregateNumeric(e.Name, coll, apply, nil)
	if err != nil {
		if lambdaErr != nil {
			return nil, lambdaErr
		}
		return nil, exprErr(e, "operand_type", "%v", err)
	}
	return v, nil
}

// listOrderBy is orderBy / orderByDesc through collection_method.go's
// orderByLambda: a stable sort by runtimeCompareValues over the normalised
// keys (nil first, numbers numerically, everything else by its text).
func (ev *exprEvaluator) listOrderBy(e *ast.CallExpr, coll []any, scope ExprScope) (any, error) {
	lam := ev.bind(e, 0, scope)
	apply := func(_ *LambdaExpression, el any) (any, error) {
		v, err := lam.apply(el)
		if err != nil {
			return nil, err
		}
		nv, err := exprNormalize(v)
		if err != nil {
			return nil, exprErr(e, "operand_type", ".%s() key: %v", e.Name, err)
		}
		return exprUnsentinel(nv), nil
	}
	return orderByLambda(coll, apply, nil, e.Name == "orderByDesc")
}

// listGroupBy groups by typed key, in first-seen order, each group a map of
// key and items.
func (ev *exprEvaluator) listGroupBy(e *ast.CallExpr, coll []any, scope ExprScope) (any, error) {
	lam := ev.bind(e, 0, scope)
	var order []string
	groups := map[string][]any{}
	keys := map[string]any{}
	for _, el := range coll {
		k, err := lam.apply(el)
		if err != nil {
			return nil, err
		}
		k = exprUnsentinel(k)
		ks := exprKey(k)
		if _, seen := groups[ks]; !seen {
			order = append(order, ks)
			keys[ks] = k
		}
		groups[ks] = append(groups[ks], el)
	}
	out := make([]any, 0, len(order))
	for _, ks := range order {
		out = append(out, map[string]any{"key": keys[ks], "items": groups[ks]})
	}
	return out, nil
}

// listDistinct keeps the first element of each typed value (or of each key,
// with a key lambda).
func (ev *exprEvaluator) listDistinct(e *ast.CallExpr, coll []any, scope ExprScope) (any, error) {
	var lam *exprBoundLambda
	if len(e.Args) == 1 {
		lam = ev.bind(e, 0, scope)
	}
	seen := map[string]bool{}
	out := make([]any, 0, len(coll))
	for _, el := range coll {
		key := el
		if lam != nil {
			k, err := lam.apply(el)
			if err != nil {
				return nil, err
			}
			key = exprUnsentinel(k)
		}
		ks := exprKey(key)
		if seen[ks] {
			continue
		}
		seen[ks] = true
		out = append(out, el)
	}
	return out, nil
}

// listTakeSkip is take(n) / skip(n): n truncated toward zero and clamped to
// the list, so a negative n takes none and skips none.
func (ev *exprEvaluator) listTakeSkip(e *ast.CallExpr, coll []any, scope ExprScope) (any, error) {
	raw, err := ev.eval(e.Args[0], scope)
	if err != nil {
		return nil, err
	}
	nv, err := exprNormalize(raw)
	if err != nil {
		return nil, exprErr(e.Args[0], "invalid_argument", ".%s(): %v", e.Name, err)
	}
	n, ok := exprNumberOf(nv)
	if !ok {
		return nil, exprErr(e.Args[0], "invalid_argument", ".%s() takes a number; `%s` is %s", e.Name, ast.FormatExpr(e.Args[0]), exprTypeName(nv))
	}
	k := exprClampCount(n, len(coll))
	if e.Name == "take" {
		return append([]any(nil), coll[:k]...), nil
	}
	return append([]any(nil), coll[k:]...), nil
}

// listReduce folds the list through a two-parameter lambda, the accumulator
// first.
func (ev *exprEvaluator) listReduce(e *ast.CallExpr, coll []any, scope ExprScope) (any, error) {
	acc, err := ev.eval(e.Args[0], scope)
	if err != nil {
		return nil, err
	}
	lam := ast.Unparen(e.Args[1]).(*ast.LambdaExpr)
	pair := &exprLambdaScope{parent: scope, names: lam.Params[:2], values: make([]any, 2)}
	for _, el := range coll {
		pair.values[0], pair.values[1] = acc, el
		if acc, err = ev.eval(lam.Body, pair); err != nil {
			return nil, err
		}
	}
	return acc, nil
}

// exprBoundLambda is a one-parameter lambda ready to apply per element. Its
// scope is reused across elements: nothing retains a lambda scope past the
// evaluation of its body (a lambda is not a value), so rebinding the
// parameter for the next element is safe and saves an allocation per
// element.
type exprBoundLambda struct {
	ev     *exprEvaluator
	method string
	body   ast.ExpressionNode
	scope  exprLambdaScope
}

// bind readies argument i of e, which exprCheckShape has already confirmed is
// a one-parameter lambda.
func (ev *exprEvaluator) bind(e *ast.CallExpr, i int, parent ExprScope) *exprBoundLambda {
	lam := ast.Unparen(e.Args[i]).(*ast.LambdaExpr)
	return &exprBoundLambda{
		ev:     ev,
		method: e.Name,
		body:   lam.Body,
		scope:  exprLambdaScope{parent: parent, names: lam.Params[:1], values: make([]any, 1)},
	}
}

func (b *exprBoundLambda) apply(el any) (any, error) {
	b.scope.values[0] = el
	return b.ev.eval(b.body, &b.scope)
}

// test applies a predicate lambda: its body must answer a bool (absent is
// false), never a value read for truthiness.
func (b *exprBoundLambda) test(el any) (bool, error) {
	v, err := b.apply(el)
	if err != nil {
		return false, err
	}
	return b.ev.condition(v, b.body, "the lambda body of ."+b.method+"()")
}

// exprLambdaScope binds lambda (or predicate) parameters over a parent scope.
type exprLambdaScope struct {
	parent ExprScope
	names  []string
	values []any
}

// Lookup finds a parameter, else asks the parent.
func (s *exprLambdaScope) Lookup(name string) (any, bool) {
	for i, n := range s.names {
		if n == name {
			return s.values[i], true
		}
	}
	if s.parent == nil {
		return nil, false
	}
	return s.parent.Lookup(name)
}

func exprIsLambda(n ast.ExpressionNode) bool {
	lam, ok := ast.Unparen(n).(*ast.LambdaExpr)
	return ok && lam != nil
}

// exprClampCount truncates a take/skip count toward zero and clamps it to
// [0, n].
//
// narrowing: SATURATE -- a count past either end of the list means "all" or
// "none", which is exactly what saturating and then clamping preserves.
func exprClampCount(c exprNumber, n int) int {
	i := c.i
	if !c.isInt {
		i = num.ClampFloat64ToInt64(c.f)
	}
	if i < 0 {
		return 0
	}
	if i > int64(n) {
		return n
	}
	return num.ClampInt64(i)
}

// exprUnsentinel turns the Absent sentinel into nil where a value is about to
// be stored inside a list or map a method builds: the sentinel never escapes
// into a container.
func exprUnsentinel(v any) any {
	if _, absent := v.(absentValue); absent {
		return nil
	}
	return v
}

// exprKey is a TYPED identity for distinct and groupBy: two values share a
// key exactly when exprStrictEqual would call them equal -- 1 and 1.0 do,
// 1 and "1" do not.
func exprKey(v any) string {
	nv, err := exprNormalize(v)
	if err != nil {
		return "?:" + fmt.Sprintf("%v", v)
	}
	switch x := nv.(type) {
	case nil, absentValue:
		return "z:"
	case bool:
		return "b:" + strconv.FormatBool(x)
	case string:
		return "s:" + x
	case int64:
		return "n:" + strconv.FormatInt(x, 10)
	case float64:
		if w, ok := num.WholeInt64(x); ok {
			return "n:" + strconv.FormatInt(w, 10)
		}
		return "n:" + strconv.FormatFloat(x, 'g', -1, 64)
	}
	if s, ok := exprJSONText(nv); ok {
		return "j:" + s
	}
	return "?:" + fmt.Sprintf("%v", nv)
}

// ---------------------------------------------------------------------------
// values
// ---------------------------------------------------------------------------

// exprNormalize maps a Go value onto the evaluator's domain: nil, Absent,
// bool, string, int64, float64, []any, map[string]any and ExprRow. It is
// SHALLOW -- a list's elements are normalised when they are read -- and it is
// applied where an operator needs to know what a value is, never to a value
// merely passed through, so a scope value comes back from a bare name as the
// scope held it.
//
// What it absorbs is the set of shapes a scope carries that are not the
// decoded-JSON ones: every Go int, uint and float kind and json.Number (the
// numbers), a named string or bool type, a time.Time (as its RFC3339Nano UTC
// string -- the language has no datetime type, and that spelling orders
// correctly by byte), json.RawMessage (decoded), typed slices and
// string-keyed maps (the []map[string]any trap MaterializeRows documents),
// pointers, and structs by JSON round trip, as the automations resolver read
// them. A typed nil is nil. A value with no JSON form (a func, a channel) is
// an error.
func exprNormalize(v any) (any, error) {
	switch x := v.(type) {
	case nil, absentValue, bool, string, int64, float64, ExprRow:
		return v, nil
	case []any:
		if x == nil {
			return nil, nil
		}
		return x, nil
	case map[string]any:
		if x == nil {
			return nil, nil
		}
		return x, nil
	case *ExprRow:
		if x == nil {
			return nil, nil
		}
		return *x, nil
	case int:
		return int64(x), nil
	case int8:
		return int64(x), nil
	case int16:
		return int64(x), nil
	case int32:
		return int64(x), nil
	case uint8:
		return int64(x), nil
	case uint16:
		return int64(x), nil
	case uint32:
		return int64(x), nil
	case uint:
		if uint64(x) > math.MaxInt64 {
			return float64(x), nil
		}
		return int64(x), nil
	case uint64:
		if x > math.MaxInt64 {
			return float64(x), nil
		}
		return int64(x), nil
	case float32:
		return float64(x), nil
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i, nil
		}
		if f, err := x.Float64(); err == nil {
			return f, nil
		}
		return x.String(), nil
	case json.RawMessage:
		if len(x) == 0 {
			return nil, nil
		}
		var decoded any
		if err := json.Unmarshal(x, &decoded); err != nil {
			return nil, err
		}
		return decoded, nil
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano), nil
	case []map[string]any:
		if x == nil {
			return nil, nil
		}
		out := make([]any, len(x))
		for i := range x {
			out[i] = x[i]
		}
		return out, nil
	case []string:
		if x == nil {
			return nil, nil
		}
		out := make([]any, len(x))
		for i := range x {
			out[i] = x[i]
		}
		return out, nil
	case map[string]string:
		if x == nil {
			return nil, nil
		}
		out := make(map[string]any, len(x))
		for k, s := range x {
			out[k] = s
		}
		return out, nil
	}
	return exprNormalizeReflect(v)
}

func exprNormalizeReflect(v any) (any, error) {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return nil, nil
		}
		return exprNormalize(rv.Elem().Interface())
	case reflect.Bool:
		return rv.Bool(), nil
	case reflect.String:
		return rv.String(), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int(), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		u := rv.Uint()
		if u > math.MaxInt64 {
			return float64(u), nil
		}
		return int64(u), nil
	case reflect.Float32, reflect.Float64:
		return rv.Float(), nil
	case reflect.Slice:
		if rv.IsNil() {
			return nil, nil
		}
		fallthrough
	case reflect.Array:
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = rv.Index(i).Interface()
		}
		return out, nil
	case reflect.Map:
		if rv.IsNil() {
			return nil, nil
		}
		if rv.Type().Key().Kind() == reflect.String {
			out := make(map[string]any, rv.Len())
			iter := rv.MapRange()
			for iter.Next() {
				out[iter.Key().String()] = iter.Value().Interface()
			}
			return out, nil
		}
		return exprJSONRoundTrip(v)
	case reflect.Struct:
		return exprJSONRoundTrip(v)
	}
	return nil, fmt.Errorf("a %T cannot be used in an expression", v)
}

// exprJSONRoundTrip reads a struct (or a map with non-string keys) as its
// JSON form, with numbers decoded as float64 like every payload.
func exprJSONRoundTrip(v any) (any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("a %T has no JSON form: %w", v, err)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

// exprText is a value's CANONICAL TEXT -- what `+` concatenates, what hash()
// digests, what toString() and the string functions read: absent and nil are
// "", a string is itself, a bool is true/false, an integer is decimal, and a
// float is decimal with no exponent (strconv 'f', -1: 1.5e-7 is "0.00000015"),
// matching payloadText and so what the SQL side's text extraction renders. A
// list or map is its compact JSON, HTML characters left unescaped.
func exprText(v any) string {
	nv, err := exprNormalize(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	switch x := nv.(type) {
	case nil, absentValue:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	if s, ok := exprJSONText(nv); ok {
		return s
	}
	return fmt.Sprint(nv)
}

func exprJSONText(v any) (string, bool) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", false
	}
	return strings.TrimSuffix(buf.String(), "\n"), true
}

// exprTypeName names a normalised value's type for a refusal.
func exprTypeName(v any) string {
	switch v.(type) {
	case absentValue:
		return "absent"
	case nil:
		return "nil"
	case bool:
		return "a bool"
	case string:
		return "a string"
	case int64, float64:
		return "a number"
	case []any:
		return "a list"
	case map[string]any:
		return "a map"
	case ExprRow:
		return "a row"
	}
	return fmt.Sprintf("a %T", v)
}

// exprNumber is a number of the language: an integer when the value is
// integer-typed, a float64 otherwise.
type exprNumber struct {
	isInt bool
	i     int64
	f     float64
}

// exprNumberOf reads a normalised value as a number.
func exprNumberOf(v any) (exprNumber, bool) {
	switch x := v.(type) {
	case int64:
		return exprNumber{isInt: true, i: x}, true
	case float64:
		return exprNumber{f: x}, true
	}
	return exprNumber{}, false
}

func (n exprNumber) float() float64 {
	if n.isInt {
		return float64(n.i)
	}
	return n.f
}

// whole reports the number as an int64 when it is a whole number an int64
// holds.
func (n exprNumber) whole() (int64, bool) {
	if n.isInt {
		return n.i, true
	}
	return num.WholeInt64(n.f)
}

// exprCompareNumbers compares two numbers EXACTLY, across int and float: an
// int64 is never rounded to a float64 to be compared (2^53 + 1 is not the
// float 2^53), which is what the SQL side's ::numeric comparison gives. ok is
// false when either side is NaN, which orders against nothing and equals
// nothing.
func exprCompareNumbers(a, b exprNumber) (int, bool) {
	switch {
	case a.isInt && b.isInt:
		switch {
		case a.i < b.i:
			return -1, true
		case a.i > b.i:
			return 1, true
		}
		return 0, true
	case !a.isInt && !b.isInt:
		if math.IsNaN(a.f) || math.IsNaN(b.f) {
			return 0, false
		}
		switch {
		case a.f < b.f:
			return -1, true
		case a.f > b.f:
			return 1, true
		}
		return 0, true
	case a.isInt:
		return exprCompareIntFloat(a.i, b.f)
	}
	c, ok := exprCompareIntFloat(b.i, a.f)
	return -c, ok
}

// exprCompareIntFloat compares an int64 with a float64 without converting
// the int: the float's integer part is compared as an int64 (exact, because
// it is whole and inside the int64 range by then), and only a tie looks at the
// fraction.
func exprCompareIntFloat(i int64, f float64) (int, bool) {
	if math.IsNaN(f) {
		return 0, false
	}
	// float64(math.MaxInt64) is 2^63, above every int64; -2^63 is exactly
	// math.MinInt64, the smallest.
	if f >= 9.223372036854775807e18 {
		return -1, true
	}
	if f < -9.223372036854775808e18 {
		return 1, true
	}
	t := math.Trunc(f)
	ti, ok := num.WholeInt64(t)
	if !ok {
		// Unreachable: t is whole and inside the int64 range.
		return 0, false
	}
	switch {
	case i < ti:
		return -1, true
	case i > ti:
		return 1, true
	case f > t:
		return -1, true
	case f < t:
		return 1, true
	}
	return 0, true
}

// ---------------------------------------------------------------------------
// errors
// ---------------------------------------------------------------------------

func exprErr(n ast.ExpressionNode, code, format string, a ...any) *ExprError {
	return &ExprError{Code: code, Message: fmt.Sprintf(format, a...), Span: exprSpan(n)}
}

// exprSpan is the source span of a node that carries one. The four nodes the
// v1 set reuses from the older AST (LambdaExpr, TernaryExpr, LiteralExpr,
// NilExpr) carry none, so a refusal about one of them has no position.
func exprSpan(n ast.ExpressionNode) ast.Span {
	switch e := n.(type) {
	case *ast.IdentExpr:
		if e != nil {
			return e.Span
		}
	case *ast.MemberExpr:
		if e != nil {
			return e.Span
		}
	case *ast.CallExpr:
		if e != nil {
			return e.Span
		}
	case *ast.UnaryExpr:
		if e != nil {
			return e.Span
		}
	case *ast.BinaryExpr:
		if e != nil {
			return e.Span
		}
	case *ast.ListExpr:
		if e != nil {
			return e.Span
		}
	case *ast.MapExpr:
		if e != nil {
			return e.Span
		}
	case *ast.ParenExpr:
		if e != nil {
			return e.Span
		}
	}
	return ast.Span{}
}
