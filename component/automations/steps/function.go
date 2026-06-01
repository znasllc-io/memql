package steps

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/automations"
)

// FunctionExecutor invokes MemQL functions.
type FunctionExecutor struct{}

// Execute runs a function step.
func (e *FunctionExecutor) Execute(ctx context.Context, step *automations.Step, stepCtx *Context) (*automations.StepResult, error) {
	result := &automations.StepResult{
		StepId:    step.ID,
		StartedAt: time.Now(),
	}

	if step.Function == nil {
		result.Status = "failed"
		result.Error = "function configuration is required"
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("function configuration is required")
	}

	if stepCtx.Engine == nil {
		result.Status = "failed"
		result.Error = "MemQL engine not configured"
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("MemQL engine not configured")
	}

	funcName := strings.TrimSpace(step.Function.Name)
	if funcName == "" {
		result.Status = "failed"
		result.Error = "function name is required"
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("function name is required")
	}

	if stepCtx.Logger != nil {
		stepCtx.Logger.Debug("executing function step",
			"step", step.ID,
			"function", funcName,
		)
	}

	// Execute the function as a query (functions are called with parentheses)
	query := funcName + "()"
	if len(step.Function.Args) > 0 {
		args := step.Function.Args
		if stepCtx.Evaluator != nil {
			if resolved, resolveErr := resolveArgsRefs(args, stepCtx.Evaluator); resolveErr == nil {
				args = resolved
			}
		}
		query = fmt.Sprintf("%s(%s)", funcName, renderFunctionArgs(args))
	}
	execResult, err := stepCtx.Engine.Execute(ctx, query)
	if err != nil {
		result.Status = "failed"
		result.Error = fmt.Sprintf("function %q execution failed: %v", funcName, err)
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("function %q execution failed: %w", funcName, err)
	}

	result.Status = "success"
	result.Result = execResult
	result.CompletedAt = time.Now()
	result.Duration = result.CompletedAt.Sub(result.StartedAt)

	itemCount := extractItemCount(execResult)

	result.Metadata = map[string]any{
		"function":  funcName,
		"itemCount": itemCount,
	}

	// Record step execution in the database
	runId := ""
	if stepCtx.Execution != nil {
		runId = stepCtx.Execution.ID
	}
	stepRecordQuery := RecordStepExecution(ctx, stepCtx.Engine, StepRecordData{
		RunId:        runId,
		StepId:       step.ID,
		StepType:     "function",
		Status:       result.Status,
		Query:        query,
		FunctionName: funcName,
		ItemCount:    itemCount,
		Duration:     float64(result.Duration.Milliseconds()),
	})

	if stepCtx.Logger != nil {
		stepCtx.Logger.Debug("function step completed",
			"step", step.ID,
			"function", funcName,
			"stepRecord", stepRecordQuery,
			"duration", formatDuration(result.Duration),
		)
	}

	return result, nil
}

func renderFunctionArgs(args map[string]any) string {
	// Single positional object argument {"0": {...}} must be rendered as {key: value}
	// because the MemQL query executor expects functionName({key: value}) syntax.
	// Rendering as 0={...} produces an invalid query that the lexer rejects.
	if len(args) == 1 {
		if obj, ok := args["0"].(map[string]any); ok {
			return renderMemQLValue(obj)
		}
	}

	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", key, renderMemQLValue(args[key])))
	}
	return strings.Join(parts, ", ")
}

func renderMemQLValue(value any) string {
	switch v := value.(type) {
	case string:
		if isRuntimeReference(v) {
			return v
		}
		return fmt.Sprintf("%q", v)
	case bool:
		if v {
			return "true"
		}
		return "false"
	case float64, float32, int, int32, int64, uint, uint32, uint64:
		return fmt.Sprintf("%v", v)
	case []string:
		// Common typed-slice case the type-switch above doesn't catch.
		// Falling through to default produces Go's `[a b c]` format
		// (unquoted, space-separated) which the langparser rejects
		// with `expected ']', got "b"`. Render as a proper MemQL
		// array literal of quoted strings instead. memql#344.
		items := make([]string, 0, len(v))
		for _, item := range v {
			items = append(items, renderMemQLValue(item))
		}
		return fmt.Sprintf("[%s]", strings.Join(items, ", "))
	case []any:
		items := make([]string, 0, len(v))
		for _, item := range v {
			items = append(items, renderMemQLValue(item))
		}
		return fmt.Sprintf("[%s]", strings.Join(items, ", "))
	case map[string]string:
		// Common typed-map case the type-switch above doesn't catch
		// (event payloads carry `nodeIdentity.Labels` as
		// map[string]string; identity-provider info carries similar
		// shapes). Falling through to default produces Go's
		// `map[k:v k2:v2]` format which the langparser rejects with
		// `expected '}', got "["` -- the exact failure from
		// memql#344. Render as a proper MemQL object literal of
		// quoted key/value pairs.
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, fmt.Sprintf("%q: %s", key, renderMemQLValue(v[key])))
		}
		return fmt.Sprintf("{%s}", strings.Join(parts, ", "))
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, fmt.Sprintf("%q: %s", key, renderMemQLValue(v[key])))
		}
		return fmt.Sprintf("{%s}", strings.Join(parts, ", "))
	default:
		// Reflection fallback: catch any typed slice or map the
		// explicit cases above don't enumerate (e.g.
		// []map[string]any, map[string]int, custom alias types
		// from upstream packages). Without this, the value gets
		// fmt.Sprintf("%v", ...) which produces Go-format text the
		// langparser rejects. Falling back to reflection-based
		// rendering preserves the contract: "every value reaches the
		// runtime parser as valid MemQL text." memql#344.
		return renderMemQLValueReflect(value)
	}
}

// renderMemQLValueReflect handles slice/map values whose element
// type isn't enumerated in renderMemQLValue's type switch. Walks
// the value via the reflect package so any typed slice (e.g.
// []int, []bool, []customString) and any typed map (e.g.
// map[string]int) renders as a valid MemQL array / object literal.
//
// Non-slice / non-map values fall back to fmt.Sprintf("%v", ...)
// which matches the historical default-case behaviour for genuine
// unknown scalars (uncommon -- the explicit cases cover every
// runtime-arg value type seen on the production paths).
func renderMemQLValueReflect(value any) string {
	if value == nil {
		return "null"
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		items := make([]string, 0, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			items = append(items, renderMemQLValue(rv.Index(i).Interface()))
		}
		return fmt.Sprintf("[%s]", strings.Join(items, ", "))
	case reflect.Map:
		// String-keyed maps render as MemQL object literals.
		// Non-string-keyed maps (rare; arrives only when an
		// upstream API misuses the args bag) fall back to %v --
		// we have no MemQL surface to express them and producing
		// invalid syntax is more honest than silently mis-rendering.
		if rv.Type().Key().Kind() != reflect.String {
			return fmt.Sprintf("%v", value)
		}
		keys := make([]string, 0, rv.Len())
		iter := rv.MapRange()
		valuesByKey := make(map[string]any, rv.Len())
		for iter.Next() {
			k := iter.Key().String()
			keys = append(keys, k)
			valuesByKey[k] = iter.Value().Interface()
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, fmt.Sprintf("%q: %s", key, renderMemQLValue(valuesByKey[key])))
		}
		return fmt.Sprintf("{%s}", strings.Join(parts, ", "))
	default:
		return fmt.Sprintf("%v", value)
	}
}

func isRuntimeReference(value string) bool {
	return strings.HasPrefix(value, "$") ||
		value == "event" ||
		strings.HasPrefix(value, "event.") ||
		value == "item" ||
		strings.HasPrefix(value, "item.") ||
		value == "index" ||
		strings.HasPrefix(value, "index.") ||
		strings.Contains(value, ".result.") ||
		strings.Contains(value, ".metadata.")
}

// resolveArgsRefs recursively resolves runtime reference strings in a function
// args map using the evaluator. Strings like "event.payload.subject" are resolved
// to their actual values before the query string is built.
func resolveArgsRefs(args map[string]any, evaluator *automations.Evaluator) (map[string]any, error) {
	result := make(map[string]any, len(args))
	for k, v := range args {
		resolved, err := resolveArgValueRef(v, evaluator)
		if err != nil {
			return nil, err
		}
		result[k] = resolved
	}
	return result, nil
}

// sharedArgEvaluator is the runtime-evaluator used for expression builtins
// that need to be resolved at arg-resolution time rather than evaluated
// downstream by the MemQL engine.
//
// Why resolve builtins here at all? The function-arg renderer
// (renderMemQLValue) quotes every string that isn't a runtime reference,
// so an unresolved builtin call like `coalesce(autoRole.First().payload.value,
// "writer")` reaches the engine as a STRING LITERAL, not as an expression.
// The receiving function's validator then sees the raw expression text as
// the argument value -- failing enum validation ("coalesce(...)" is not in
// [owner, admin, writer, reader]) or landing the literal expression in the
// database as a user's name. Every builtin with a renderable return type
// must therefore be resolved here before the query is built.
//
// MutationExecutor's evaluateValue has the full builtin dispatch table
// (cond, coalesce, hash, first, last, field, add, sub, len, ...). We
// delegate any string that looks like a builtin call to it.
var sharedArgEvaluator = &MutationExecutor{}

// resolveArgValueRef resolves a single arg value, recursing into maps and arrays.
func resolveArgValueRef(v any, evaluator *automations.Evaluator) (any, error) {
	switch val := v.(type) {
	case string:
		// concat() has its own arg-time evaluator that recurses into
		// nested builtins. Kept separate because evaluateArgConcat was
		// written to handle concat's specific splitting rules.
		if strings.HasPrefix(val, "concat(") && strings.HasSuffix(val, ")") {
			resolved, err := evaluateArgConcat(evaluator, val)
			if err != nil {
				return v, nil
			}
			return resolved, nil
		}
		// Any other expression builtin (cond, coalesce, hash, first, last,
		// lower, upper, trim, var, field, lt, gt, add, sub, index, append,
		// len, mean, pluck, whereEq) -- delegate to the shared evaluator
		// so the resolved value flows through renderMemQLValue as a proper
		// literal (quoted string, number, bool) instead of as raw
		// expression text that downstream validators will reject.
		if looksLikeNestedBuiltin(val) {
			resolved, err := sharedArgEvaluator.evaluateValue(evaluator, val)
			if err != nil {
				// Surface the silent fallback: a builtin literal that fails to
				// resolve is passed through verbatim and would land in the
				// outgoing mutation arg as raw expression text (the memql#574
				// ghost-SI failure mode). Log so it is never silent again.
				evaluator.Warnf("builtin arg resolution failed; passing literal through",
					"expression", val, "error", err.Error())
				return v, nil
			}
			if resolved == nil {
				evaluator.Warnf("builtin arg resolved to nil; passing literal through",
					"expression", val)
				return v, nil
			}
			return resolved, nil
		}
		// Handle timestamp()/now() expressions
		if val == "timestamp()" || val == "now()" {
			return time.Now().UTC().Format(time.RFC3339), nil
		}
		// Handle runtime references ($event.payload.*, $steps.*, etc.)
		if isRuntimeReference(val) {
			expr := val
			if !strings.HasPrefix(expr, "$") {
				expr = "$" + expr
			}
			resolved, err := evaluator.EvaluateValue(expr)
			if err != nil || resolved == nil {
				return v, nil // keep original if resolution fails
			}
			return resolved, nil
		}
		return v, nil
	case map[string]any:
		return resolveArgsRefs(val, evaluator)
	case []any:
		result := make([]any, len(val))
		for i, item := range val {
			r, err := resolveArgValueRef(item, evaluator)
			if err != nil {
				return v, nil
			}
			result[i] = r
		}
		return result, nil
	default:
		return v, nil
	}
}

// evaluateArgConcat evaluates a concat() expression by resolving each argument.
func evaluateArgConcat(evaluator *automations.Evaluator, expr string) (string, error) {
	argsStr := expr[7 : len(expr)-1] // Remove "concat(" and ")"
	args := splitConcatArgs(argsStr)

	var result strings.Builder
	for _, arg := range args {
		arg = strings.TrimSpace(arg)
		if arg == "" {
			continue
		}
		// String literal
		if strings.HasPrefix(arg, "\"") && strings.HasSuffix(arg, "\"") {
			result.WriteString(arg[1 : len(arg)-1])
			continue
		}
		// Nested concat
		if strings.HasPrefix(arg, "concat(") && strings.HasSuffix(arg, ")") {
			nested, err := evaluateArgConcat(evaluator, arg)
			if err != nil {
				return "", err
			}
			result.WriteString(nested)
			continue
		}
		// Nested builtin (hash, cond, coalesce, first, ...) -- delegate to
		// the shared MutationExecutor evaluator which has the full
		// dispatch table. Without this, `hash($event.payload.email)`
		// falls through to runtime-reference handling and gets looked
		// up as `$hash(...)`, which fails with "unknown reference".
		if looksLikeNestedBuiltin(arg) {
			resolved, err := sharedArgEvaluator.evaluateValue(evaluator, arg)
			if err != nil {
				return "", fmt.Errorf("evaluating concat arg %q: %w", arg, err)
			}
			result.WriteString(automations.FormatValue(resolved))
			continue
		}
		// Runtime reference
		ref := arg
		if !strings.HasPrefix(ref, "$") {
			ref = "$" + ref
		}
		resolved, err := evaluator.EvaluateValue(ref)
		if err != nil {
			return "", fmt.Errorf("evaluating concat arg %q: %w", arg, err)
		}
		result.WriteString(automations.FormatValue(resolved))
	}
	return result.String(), nil
}

// looksLikeNestedBuiltin reports whether a concat arg looks like a
// nested call to an expression builtin (hash, cond, coalesce, first,
// last, lower, upper, trim, ...). Used to decide whether to delegate
// to the shared MutationExecutor evaluator vs treat the arg as a
// bare runtime reference.
func looksLikeNestedBuiltin(s string) bool {
	if !strings.HasSuffix(s, ")") {
		return false
	}
	for _, prefix := range nestedBuiltinPrefixes {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// nestedBuiltinPrefixes lists the builtins we hoist into the shared
// evaluator when they appear nested inside a concat() arg, OR when
// the automation compiler reconstructs an expression back to source
// text and feeds it through resolveArgValueRef as a string. Mirrors
// MutationExecutor.evaluateValue's dispatch table.
//
// IMPORTANT: any new builtin added to MutationExecutor.evaluateValue
// MUST be added here too, otherwise the reconstructed source text
// reaches the engine as a literal string -- which then gets stored
// verbatim in payload fields (the `*ast.CanonicalIdExpr` /
// `canonicalId(concat(...))` orphan-participant bug landed exactly
// because canonicalId( was missing from this list).
var nestedBuiltinPrefixes = []string{
	"hash(", "cond(", "coalesce(", "first(", "last(",
	"lower(", "upper(", "trim(", "var(", "field(",
	"lt(", "gt(", "add(", "sub(", "index(", "append(",
	"len(", "mean(", "pluck(", "whereEq(",
	"canonicalId(",
}

// splitConcatArgs splits concat arguments handling nested parentheses and quoted strings.
func splitConcatArgs(s string) []string {
	var args []string
	var current strings.Builder
	depth := 0
	inString := false
	escaped := false

	for _, ch := range s {
		if escaped {
			current.WriteRune(ch)
			escaped = false
			continue
		}
		if ch == '\\' && inString {
			escaped = true
			current.WriteRune(ch)
			continue
		}
		if ch == '"' {
			inString = !inString
			current.WriteRune(ch)
			continue
		}
		if !inString {
			if ch == '(' {
				depth++
			} else if ch == ')' {
				depth--
			} else if ch == ',' && depth == 0 {
				args = append(args, current.String())
				current.Reset()
				continue
			}
		}
		current.WriteRune(ch)
	}
	if current.Len() > 0 {
		args = append(args, current.String())
	}
	return args
}
