package steps

// v1.go -- what the step executors share for reading a step's expressions
// (epic memql#5363, memql#5367).
//
// Every step carries Step.Exprs: its expressions, parsed once at load
// (automations.PrepareExpressions). An executor evaluates the parsed node over
// the run (Evaluator.EvalV1 / ResolveV1Map) and gets back a value.
//
// Two rules make that safe:
//
//   - a value is DATA. An evaluated argument is rendered into the engine
//     call as a MemQL literal by renderMemQLData, which quotes every string,
//     including one that looks like a reference (`event.x`, `steps.y`, `$z`):
//     an evaluated value is never re-read as an expression, so a payload
//     string cannot become one.
//   - a construct call's arguments are evaluated, never substituted: the
//     call text the engine receives is built from the call's name and its
//     evaluated arguments, not by replacing references inside source text.

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/language/ast"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// preparedExprs returns a step's expressions, parsed at load. A step that was
// never prepared is refused: its expression positions are text nothing reads,
// and running it would publish to no topic, request no URL or write no id
// rather than fail (automations.PrepareExpressions prepares every loaded
// automation, and the executor prepares one built in Go before its first
// run).
func preparedExprs(step *automations.Step) (*automations.StepExprs, error) {
	if step.Exprs == nil {
		return nil, fmt.Errorf("step %q was never prepared (automations.PrepareExpressions)", step.ID)
	}
	return step.Exprs, nil
}

// v1Value evaluates a v1 position that yields a value (a forEach or shape
// source, a switch subject). The Absent sentinel comes back as nil -- one
// notion of unset -- so the executor's existing nil handling applies.
func v1Value(ctx context.Context, evaluator *automations.Evaluator, n ast.ExpressionNode) (any, error) {
	if n == nil {
		return nil, nil
	}
	v, err := evaluator.EvalV1(ctx, n)
	if err != nil {
		return nil, err
	}
	if v == memql.Absent {
		return nil, nil
	}
	return v, nil
}

// v1Text evaluates a string-typed v1 position (a topic, a URL, an id): nil is
// "" (the position was not written), and a value is its text
// (automations.V1Text -- absent is "").
func v1Text(ctx context.Context, evaluator *automations.Evaluator, n ast.ExpressionNode) (string, error) {
	if n == nil {
		return "", nil
	}
	v, err := evaluator.EvalV1(ctx, n)
	if err != nil {
		return "", err
	}
	return automations.V1Text(v), nil
}

// v1RequiredText is v1Text for a position that must name something: an
// expression that evaluates to nothing is refused rather than publishing to
// the empty topic or requesting the empty URL.
func v1RequiredText(ctx context.Context, evaluator *automations.Evaluator, n ast.ExpressionNode, what string) (string, error) {
	s, err := v1Text(ctx, evaluator, n)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("%s `%s` evaluated to nothing", what, ast.FormatExpr(n))
	}
	return s, nil
}

// renderV1CallArgs renders a v1 function step's evaluated arguments: named
// (`k: <literal>`, sorted), or -- when every key is an index "0".."n-1", the
// shape a positional call is stored in -- positionally, in index order.
func renderV1CallArgs(args map[string]any) string {
	if len(args) > 0 {
		positional := make([]string, len(args))
		for i := range positional {
			v, ok := args[strconv.Itoa(i)]
			if !ok {
				positional = nil
				break
			}
			positional[i] = renderMemQLData(v)
		}
		if positional != nil {
			return strings.Join(positional, ", ")
		}
	}
	return renderV1NamedArgs(args)
}

// renderV1NamedArgs renders evaluated named arguments as `k: <literal>`,
// sorted by name so the call text is deterministic.
func renderV1NamedArgs(args map[string]any) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+": "+renderMemQLData(args[k]))
	}
	return strings.Join(parts, ", ")
}

// renderMemQLData renders an evaluated value as a MemQL literal. Every string
// is QUOTED (see the file comment), with langparser.QuoteString rather than
// %q: Go's %q and the lexer that re-parses this text disagree on the escape
// set, and a control byte in a value would make the whole call unparseable
// (memql#3192). A typed time renders as a quoted RFC3339 string in UTC --
// Go's default format and proto text are both refused by the parser
// (memql#2543) -- and a duration as its quoted Go spelling, there being no
// duration literal. A typed slice or string-keyed map renders as a list or
// object literal (memql#344).
func renderMemQLData(value any) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case string:
		return langparser.QuoteString(v)
	case bool:
		if v {
			return "true"
		}
		return "false"
	case float64, float32, int, int32, int64, uint, uint32, uint64:
		return fmt.Sprintf("%v", v)
	case time.Time:
		return langparser.QuoteString(v.UTC().Format(time.RFC3339))
	case *time.Time:
		if v == nil {
			return "null"
		}
		return langparser.QuoteString(v.UTC().Format(time.RFC3339))
	case *timestamppb.Timestamp:
		if v == nil {
			return "null"
		}
		return langparser.QuoteString(v.AsTime().UTC().Format(time.RFC3339))
	case time.Duration:
		return langparser.QuoteString(v.String())
	case []any:
		items := make([]string, 0, len(v))
		for _, item := range v {
			items = append(items, renderMemQLData(item))
		}
		return "[" + strings.Join(items, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, langparser.QuoteString(key)+": "+renderMemQLData(v[key]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Pointer:
		if rv.IsNil() {
			return "null"
		}
	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			return "null"
		}
		items := make([]string, 0, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			items = append(items, renderMemQLData(rv.Index(i).Interface()))
		}
		return "[" + strings.Join(items, ", ") + "]"
	case reflect.Map:
		if rv.Type().Key().Kind() == reflect.String {
			if rv.IsNil() {
				return "null"
			}
			vals := make(map[string]any, rv.Len())
			iter := rv.MapRange()
			for iter.Next() {
				vals[iter.Key().String()] = iter.Value().Interface()
			}
			return renderMemQLData(vals)
		}
	case reflect.String:
		return langparser.QuoteString(rv.String())
	}
	// Anything else (a struct -- a query result envelope -- or a map with
	// non-string keys) has no MemQL literal; its Go spelling fails the parse
	// rather than being mis-rendered.
	return fmt.Sprintf("%v", value)
}
