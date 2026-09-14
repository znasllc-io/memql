package steps

// v1.go -- the step executors' half of a v1 automation (epic memql#5363,
// memql#5367).
//
// A step of a v1 automation carries Step.Exprs: its expressions, parsed once
// at load (automations.PrepareExpressions). Each executor branches on it at
// the point where it used to hand a string to the legacy string evaluator,
// and evaluates the parsed node over the run instead (Evaluator.EvalV1 /
// ResolveV1Map). Nothing else about the executor changes: the value it gets
// back is the value the string evaluator used to hand it, so the dispatch,
// the result shape and the step record are the same for both grammars.
//
// Two rules make the v1 half safe where the legacy half was not:
//
//   - a value is DATA. An evaluated argument is rendered into the engine
//     call as a MemQL literal by renderMemQLData, which quotes every string.
//     renderMemQLValue passes a string that LOOKS like a reference
//     (`event.x`, `steps.y`, `$z`) through unquoted, so the engine re-reads
//     it as a reference -- right for the legacy compiled form, whose leaves
//     are reference text, and wrong for an evaluated value, where it would
//     let a payload string become an expression.
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

// v1ConstructCallText renders a construct call for engine.Execute:
// `name(k: <literal>, ...)`, its arguments evaluated over the run. An
// argument that evaluates to absent is omitted, so the callee sees the
// argument as not passed (rule 30's container rule applied to a call);
// an explicit nil is passed as null. Positional arguments are refused: a
// construct call names its arguments.
func v1ConstructCallText(ctx context.Context, evaluator *automations.Evaluator, call *ast.CallExpr) (string, error) {
	if len(call.Args) > 0 {
		return "", fmt.Errorf("%s %s: a construct call takes named arguments only, got %d positional", call.Kind, call.Name, len(call.Args))
	}
	args := make(map[string]any, len(call.Named))
	for _, na := range call.Named {
		v, err := evaluator.EvalV1(ctx, na.Value)
		if err != nil {
			return "", fmt.Errorf("%s %s argument %s: %w", call.Kind, call.Name, na.Name, err)
		}
		// The Absent sentinel, not nil: a JSON null is a value the author
		// wrote and is passed.
		if v == memql.Absent {
			continue
		}
		args[na.Name] = v
	}
	return call.Name + "(" + renderV1NamedArgs(args) + ")", nil
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

// renderMemQLData renders an evaluated value as a MemQL literal. It is
// renderMemQLValue with one difference, and that difference is the point:
// every string is QUOTED, including one that looks like a reference, because
// an evaluated value is data (see the file comment). Scalars and the typed
// time values render exactly as renderMemQLValue renders them.
func renderMemQLData(value any) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case string:
		return langparser.QuoteString(v)
	case bool, float64, float32, int, int32, int64, uint, uint32, uint64,
		time.Time, *time.Time, *timestamppb.Timestamp, time.Duration:
		return renderMemQLValue(v)
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
	// Anything else (a struct -- a query result envelope) renders the way
	// renderMemQLValue renders it; it holds no reference text of its own.
	return renderMemQLValue(value)
}
