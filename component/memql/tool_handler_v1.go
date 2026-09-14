package memql

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/language/tiers"
	"github.com/znasllc-io/memql/core/num"
)

// tool_handler_v1.go -- a tool's query handler in edition 2026 (epic
// memql#5363, memql#5367).
//
// A query handler is ONE construct call -- `query todos(done: args.done)`,
// `mutation updateNote(noteId: args.noteId, payload: args.payload)` -- or that
// call inside the one directive a handler carries,
// `paginate(query searchUsers(active: args.active), args.limit)`. It is
// parsed ONCE, when the tool loads (parseToolQueryV1), and checked there: the
// shape, the construct it calls, and every argument expression. At execution
// each named argument evaluates through EvalExpr with `args` bound to the
// tool's arguments -- defaulted, coerced and schema-validated by then -- and
// the call is rendered from the resulting VALUES, each as one MemQL literal
// (memqlCallLiteral).
//
// Nothing is substituted into text. The `$args.` substitution this replaces
// spliced the caller's value into the handler's source, so its correctness
// rested on one careful pass quoting every value exactly right (memql#3609
// was that pass getting it wrong twice). Here a caller's string is a value
// from start to finish, rendered as one quoted literal whatever it contains
// -- quotes, backslashes, newlines, a `$args.` of its own -- so it cannot
// become part of the call.
//
// An argument the caller did not supply is ABSENT and its named argument is
// omitted from the call, so the called construct sees an unset optional arg,
// exactly as if the call had been written without it. The substitution
// wrote the literal `null` there, and `"null"` -- a four-character string --
// for a placeholder written inside quotes.
//
// TRANSITION. A handler that still carries the retired `$args.` placeholder is
// legacy and keeps substituteArgsInMemqlQuery until the tree is migrated; the
// flip deletes that path. Every other query handler is read as v1, and one
// that does not parse as a handler is refused at load.
//
// Webhook handlers are untouched: no tool in the tree declares one, so their
// url and body keep the `$args.` substitution until a v1 spelling for them is
// written against a real caller.

// toolQueryHandlerKinds are the construct kinds a handler may call: the ones
// validateToolHandlerTargets resolves against the function registry, so every
// handler that loads names a target the resolver can check.
var toolQueryHandlerKinds = map[string]bool{
	"query":      true,
	"mutation":   true,
	"logic":      true,
	"builtin":    true,
	"automation": true,
}

// toolHandlerArgCheck is the load-time check of a handler's argument
// expressions: a handler reads the tool's arguments and the clock, and its
// arguments are values -- a construct call inside one would run a second call
// no journal or policy sees.
//
// The tier manifest has no row of its own for a handler's argument. It is
// judged against the mutation-value row because it is the same thing: a value
// computed in process for another construct, which is what that rule set
// (every in-process kind but the construct call) is defined as.
var toolHandlerArgCheck = &inProcessExprCheck{
	where:    "a tool handler's argument",
	position: tiers.PositionMutationValue,
	roots:    map[string]bool{"args": true},
}

// toolQueryV1 is a parsed edition-2026 query handler.
type toolQueryV1 struct {
	// call is the construct call.
	call *ast.CallExpr
	// named is the call's arguments in source order, a punned `x` recorded
	// as `x: x`.
	named []ast.NamedArg
	// count is paginate's second argument, nil for a handler with no
	// paginate.
	count ast.ExpressionNode
}

// toolQueryIsLegacy reports whether a query handler still carries the
// retired `$args.` placeholder, and so keeps the substitution path until the
// tree is migrated.
func toolQueryIsLegacy(query string) bool {
	return strings.Contains(query, "$args.")
}

// prepareToolQueryV1 parses a query handler at load: nil for a legacy
// handler, the parsed handler for a v1 one, or the refusal.
func prepareToolQueryV1(query string) (*toolQueryV1, error) {
	if toolQueryIsLegacy(query) {
		return nil, nil
	}
	return parseToolQueryV1(query)
}

// parseToolQueryV1 parses and checks a v1 query handler.
func parseToolQueryV1(src string) (*toolQueryV1, error) {
	n, err := languageParser.ParseV1Expression(src)
	if err != nil {
		return nil, fmt.Errorf("query handler `%s` does not parse: %w", strings.TrimSpace(src), err)
	}
	call, ok := ast.Unparen(n).(*ast.CallExpr)
	if !ok || call == nil {
		return nil, toolQueryShapeError(src)
	}
	q := &toolQueryV1{}
	if call.Kind == "" && call.Receiver == nil && call.Name == "paginate" {
		if len(call.Named) > 0 || len(call.Args) != 2 {
			return nil, fmt.Errorf("query handler `%s`: paginate takes the call and a count, as in paginate(query q(...), args.limit)", strings.TrimSpace(src))
		}
		inner, ok := ast.Unparen(call.Args[0]).(*ast.CallExpr)
		if !ok || inner == nil || inner.Kind == "" {
			return nil, fmt.Errorf("query handler `%s`: paginate's first argument is the construct call it pages, as in paginate(query q(...), args.limit)", strings.TrimSpace(src))
		}
		if err := toolHandlerArgCheck.check(call.Args[1]); err != nil {
			return nil, fmt.Errorf("query handler `%s`: paginate's count: %w", strings.TrimSpace(src), err)
		}
		q.count = call.Args[1]
		call = inner
	}
	if call.Kind == "" || call.Receiver != nil {
		return nil, toolQueryShapeError(src)
	}
	if !toolQueryHandlerKinds[call.Kind] {
		return nil, fmt.Errorf("query handler `%s`: a handler calls a query, mutation, logic, builtin or automation, not %s %s(...)", strings.TrimSpace(src), call.Kind, call.Name)
	}
	q.call = call
	for _, a := range call.Args {
		// A construct call's only positional argument is a pun, `f(x)` for
		// `f(x: x)`; the parser refuses every other. It is read as the
		// named argument it abbreviates, so the root check below judges
		// the name it reads.
		id, isPun := a.(*ast.IdentExpr)
		if !isPun || id == nil {
			return nil, fmt.Errorf("query handler `%s`: name every argument, as in %s %s(k: args.k)", strings.TrimSpace(src), call.Kind, call.Name)
		}
		q.named = append(q.named, ast.NamedArg{Name: id.Name, Value: id})
	}
	q.named = append(q.named, call.Named...)
	for _, a := range q.named {
		if err := toolHandlerArgCheck.check(a.Value); err != nil {
			return nil, fmt.Errorf("query handler `%s`: argument %s: %w", strings.TrimSpace(src), a.Name, err)
		}
	}
	return q, nil
}

func toolQueryShapeError(src string) error {
	return fmt.Errorf("query handler `%s` is not a handler: write one construct call, as in query q(k: args.k), or that call paged, as in paginate(query q(k: args.k), args.limit)", strings.TrimSpace(src))
}

// target is the registry name the handler calls, for
// validateToolHandlerTargets.
func (q *toolQueryV1) target() string { return q.call.Name }

// render evaluates the handler's arguments over args and renders the call the
// engine executes.
func (q *toolQueryV1) render(ctx context.Context, args map[string]any) (string, error) {
	scope := MapScope{"args": args}
	opts := EvalOptions{Now: time.Now().UTC()}

	var b strings.Builder
	b.WriteString(q.call.Kind)
	b.WriteByte(' ')
	b.WriteString(q.call.Name)
	b.WriteByte('(')
	first := true
	for _, a := range q.named {
		v, err := EvalExpr(ctx, a.Value, scope, opts)
		if err != nil {
			return "", fmt.Errorf("evaluate argument %s: %w", a.Name, err)
		}
		if _, absent := v.(absentValue); absent {
			// The caller supplied nothing: the argument is omitted, and
			// the construct sees an unset optional arg.
			continue
		}
		lit, err := memqlCallLiteral(v)
		if err != nil {
			return "", fmt.Errorf("argument %s: %w", a.Name, err)
		}
		if !first {
			b.WriteString(", ")
		}
		first = false
		b.WriteString(a.Name)
		b.WriteString(": ")
		b.WriteString(lit)
	}
	b.WriteByte(')')
	if q.count == nil {
		return b.String(), nil
	}

	v, err := EvalExpr(ctx, q.count, scope, opts)
	if err != nil {
		return "", fmt.Errorf("evaluate paginate's count: %w", err)
	}
	count, err := toolPaginateCount(q.count, v)
	if err != nil {
		return "", err
	}
	return "paginate(" + b.String() + ", " + strconv.FormatInt(count, 10) + ")", nil
}

// toolPaginateCount reads paginate's count: the directive takes a positive
// whole number, so anything else is refused here, naming the value, rather
// than rendered for the parser to refuse with a message about the text.
func toolPaginateCount(n ast.ExpressionNode, v any) (int64, error) {
	nv, err := exprNormalize(v)
	if err == nil {
		if x, ok := exprNumberOf(nv); ok {
			if w, whole := x.whole(); whole && w > 0 {
				return w, nil
			}
		}
	} else {
		nv = v
	}
	return 0, fmt.Errorf("paginate takes a positive whole number of rows; `%s` is %s", ast.FormatExpr(n), exprValueForMessage(nv))
}

// exprValueForMessage describes a value for a refusal: its text when it is a
// scalar, its type otherwise.
func exprValueForMessage(v any) string {
	switch x := v.(type) {
	case nil, absentValue:
		return "absent"
	case bool, int64, float64:
		return exprText(x)
	case string:
		return languageParser.QuoteString(x)
	}
	return exprTypeName(v)
}

// memqlCallLiteral renders one evaluated value as the MemQL literal the
// engine's internal query form reads back to the same value.
//
//   - A string is languageParser.QuoteString -- the one definition of a MemQL
//     string literal (memql#3192) -- for a value and for a map key alike, so
//     no value can end its literal early, whatever it contains.
//   - A number is decimal: an integer, or a float with no integer value in
//     its shortest form; a NaN or an infinity has no literal and is refused.
//   - nil is `null`. Not `nil`: inside an object or a list the internal
//     query form reads `nil` as a nil NODE rather than a nil value, and a map
//     carrying that node marshals as `{}`; `null` is nil in every value
//     position of that grammar.
//   - A list and a map are written compactly, a map's keys sorted -- the
//     layout json.Marshal gave the substitution this replaces, so the call a
//     tool renders is the one it rendered before, byte for byte, for the
//     values the tree sends (TestToolHandlerV1RendersTheLegacyCall).
func memqlCallLiteral(v any) (string, error) {
	nv, err := exprNormalize(v)
	if err != nil {
		return "", err
	}
	switch x := nv.(type) {
	case nil, absentValue:
		return "null", nil
	case bool:
		return strconv.FormatBool(x), nil
	case string:
		return languageParser.QuoteString(x), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return "", fmt.Errorf("%v has no MemQL literal", x)
		}
		// narrowing: GUARDED -- num.WholeInt64 is the guard (memql#4779); a
		// whole float outside int64 keeps its float form below.
		if whole, ok := num.WholeInt64(x); ok {
			return strconv.FormatInt(whole, 10), nil
		}
		return strconv.FormatFloat(x, 'g', -1, 64), nil
	case []any:
		var b strings.Builder
		b.WriteByte('[')
		for i, el := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			lit, err := memqlCallLiteral(el)
			if err != nil {
				return "", err
			}
			b.WriteString(lit)
		}
		b.WriteByte(']')
		return b.String(), nil
	case map[string]any:
		var b strings.Builder
		b.WriteByte('{')
		for i, k := range sortedAnyKeys(x) {
			if i > 0 {
				b.WriteByte(',')
			}
			lit, err := memqlCallLiteral(x[k])
			if err != nil {
				return "", err
			}
			b.WriteString(languageParser.QuoteString(k))
			b.WriteByte(':')
			b.WriteString(lit)
		}
		b.WriteByte('}')
		return b.String(), nil
	}
	return "", fmt.Errorf("%s has no MemQL literal", exprTypeName(nv))
}

// renderToolQuery returns the query a query handler executes for args: the
// legacy substitution for a handler that still carries `$args.`, otherwise
// the parsed handler rendered from the values of its arguments. A handler
// loaded from `.memql` was parsed when it loaded; one built in Go is parsed
// here, on each call, and refused if it is not a handler.
func (h *ToolHandler) renderToolQuery(ctx context.Context, args map[string]any) (string, error) {
	query := strings.TrimSpace(h.Query)
	if toolQueryIsLegacy(query) {
		if args != nil {
			query = substituteArgsInMemqlQuery(query, args)
		}
		return query, nil
	}
	plan := h.queryV1
	if plan == nil {
		var err error
		if plan, err = parseToolQueryV1(query); err != nil {
			return "", err
		}
	}
	return plan.render(ctx, args)
}
