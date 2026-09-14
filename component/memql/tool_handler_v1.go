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

// tool_handler_v1.go -- a tool's handler in edition 2026 (epic memql#5363,
// memql#5367).
//
// A query handler is ONE construct call -- `query todos(done: args.done)`,
// `mutation updateNote(noteId: args.noteId, payload: args.payload)` -- or that
// call inside the one directive a handler carries,
// `paginate(query searchUsers(active: args.active), args.limit)`. It is
// parsed ONCE, when the tool loads (prepareToolQueryV1), and checked there: the
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
// The retired `$args.` placeholder is refused wherever it appears in a
// handler, a quoted `"$args.x"` included: read as v1, the quoted form is a
// string literal, so a handler that still carried it would load and hand the
// construct the eleven characters "$args.slug" instead of the caller's value.
//
// A webhook handler's url and body are expressions too (webhookURL,
// webhookBody below): the url one expression that renders the address, the
// body a map whose string leaves are expressions. Nothing is substituted into
// either.

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

// toolDollarArgsRetired is the refusal for the retired placeholder, naming
// the spelling that replaces it and the migration that writes it.
const toolDollarArgsRetired = "$args.x is retired in edition 2026: write args.x -- a quoted \"$args.x\" is args.x too, without the quotes (memqlmigrate --rewrite=expressions rewrites both)"

// refuseDollarArgs refuses a handler source that still carries the retired
// placeholder anywhere, inside a string literal included.
func refuseDollarArgs(what, src string) error {
	if strings.Contains(src, "$args.") {
		return fmt.Errorf("%s `%s`: %s", what, strings.TrimSpace(src), toolDollarArgsRetired)
	}
	return nil
}

// prepareToolQueryV1 parses and checks a query handler: at load for a tool
// declared in `.memql` (toolDeclToTool), and on each call for a tool built in
// Go, which was never loaded (renderToolQuery).
func prepareToolQueryV1(src string) (*toolQueryV1, error) {
	if err := refuseDollarArgs("query handler", src); err != nil {
		return nil, err
	}
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
// parsed handler rendered from the values of its arguments. A handler loaded
// from `.memql` was parsed when it loaded; one built in Go is parsed here, on
// each call, and refused if it is not a handler.
func (h *ToolHandler) renderToolQuery(ctx context.Context, args map[string]any) (string, error) {
	plan := h.queryV1
	if plan == nil {
		var err error
		if plan, err = prepareToolQueryV1(strings.TrimSpace(h.Query)); err != nil {
			return "", err
		}
	}
	return plan.render(ctx, args)
}

// ---------------------------------------------------------------------------
// webhook url and body
// ---------------------------------------------------------------------------

// webhookValueCheck is the load-time check of a webhook's url and body
// expressions: the same rule set as a query handler's arguments -- the tool's
// arguments and the clock, no construct call -- for the same reason.
var webhookValueCheck = &inProcessExprCheck{
	where:    "a webhook handler's url or body",
	position: tiers.PositionMutationValue,
	roots:    map[string]bool{"args": true},
}

// prepareWebhookURL parses and checks a webhook url expression: at load for a
// tool declared in `.memql`, on each call for one built in Go. The url is ONE
// expression rendering the address -- a fixed address is a quoted string, and
// a caller's value is joined with +, as in "https://api.example.com/items/" +
// args.id -- so it is refused, with that spelling, when it is written as a
// bare address.
func prepareWebhookURL(src string) (ast.ExpressionNode, error) {
	if err := refuseDollarArgs("webhook url", src); err != nil {
		return nil, err
	}
	n, err := languageParser.ParseV1Expression(src)
	if err != nil {
		if strings.Contains(src, "://") && !strings.Contains(src, `"`) {
			return nil, fmt.Errorf("webhook url `%s` is an expression in edition 2026: quote a fixed address, as in %s, and join a caller's value with +, as in %s + args.id",
				strings.TrimSpace(src), languageParser.QuoteString(strings.TrimSpace(src)), languageParser.QuoteString("https://api.example.com/items/"))
		}
		return nil, fmt.Errorf("webhook url `%s` does not parse: %w", strings.TrimSpace(src), err)
	}
	if err := webhookValueCheck.check(n); err != nil {
		return nil, fmt.Errorf("webhook url `%s`: %w", strings.TrimSpace(src), err)
	}
	return n, nil
}

// webhookURL renders the handler's url for args: the expression parsed at
// load, or -- for a tool built in Go -- the url parsed here. It must render
// as text.
func (h *ToolHandler) webhookURL(ctx context.Context, args map[string]any) (string, error) {
	n := h.urlV1
	if n == nil {
		var err error
		if n, err = prepareWebhookURL(h.URL); err != nil {
			return "", err
		}
	}
	v, err := EvalExpr(ctx, n, MapScope{"args": args}, EvalOptions{Now: time.Now().UTC()})
	if err != nil {
		return "", fmt.Errorf("webhook url: %w", err)
	}
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("webhook url `%s` is %s, not an address", ast.FormatExpr(n), exprValueForMessage(v))
	}
	return s, nil
}

// webhookBody renders a webhook's body template for args. The template is a
// Go map (a `.memql` handler declares no body): each string leaf is an
// expression over args, a nested map or list is rendered leaf by leaf, and
// any other leaf is the value it is. A leaf whose argument the caller did not
// supply is absent and omits its key or element, the container rule a
// mutation's values follow. Nothing is substituted into text, so a caller's
// value -- a `$args.` of its own included -- is data.
func webhookBody(ctx context.Context, body map[string]any, args map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(body))
	for _, k := range sortedAnyKeys(body) {
		v, absent, err := webhookBodyValue(ctx, body[k], args)
		if err != nil {
			return nil, fmt.Errorf("webhook body %q: %w", k, err)
		}
		if !absent {
			out[k] = v
		}
	}
	return out, nil
}

func webhookBodyValue(ctx context.Context, v any, args map[string]any) (any, bool, error) {
	switch t := v.(type) {
	case string:
		if err := refuseDollarArgs("expression", t); err != nil {
			return nil, false, err
		}
		n, err := languageParser.ParseV1Expression(t)
		if err != nil {
			return nil, false, fmt.Errorf("`%s` does not parse -- a body leaf is an expression, and fixed text is quoted, as in %s: %w", t, languageParser.QuoteString(t), err)
		}
		if err := webhookValueCheck.check(n); err != nil {
			return nil, false, fmt.Errorf("`%s`: %w", t, err)
		}
		ev, err := EvalExpr(ctx, n, MapScope{"args": args}, EvalOptions{Now: time.Now().UTC()})
		if err != nil {
			return nil, false, err
		}
		if _, absent := ev.(absentValue); absent {
			return nil, true, nil
		}
		nv, err := exprNormalize(ev)
		return nv, false, err
	case map[string]any:
		m, err := webhookBody(ctx, t, args)
		return m, false, err
	case []any:
		out := make([]any, 0, len(t))
		for i, el := range t {
			ev, absent, err := webhookBodyValue(ctx, el, args)
			if err != nil {
				return nil, false, fmt.Errorf("[%d]: %w", i, err)
			}
			if !absent {
				out = append(out, ev)
			}
		}
		return out, false, nil
	}
	return v, false, nil
}
