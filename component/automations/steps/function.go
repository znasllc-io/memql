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
	// Single positional object argument {"0": {...}} is rendered as the
	// named-args invocation body `key: value, ...` (Story 9 / #2335: the
	// kind-prefixed call form `functionName(key: value)` drops the legacy
	// `{...}` wrapper, which the parser now rejects). renderMemQLValue(obj)
	// emits the bare-key object `{key: value}`; strip the outer braces.
	if len(args) == 1 {
		if obj, ok := args["0"].(map[string]any); ok {
			rendered := renderMemQLValue(obj)
			return strings.TrimSuffix(strings.TrimPrefix(rendered, "{"), "}")
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

// isCompiledReference reports whether expr has the exact `$root.path`
// shape the automation compiler emits for runtime references
// (compileStepHelperValue lifts `args.X` / `event.X` / `item.X` source
// expressions to `$`-form; the executor seeds those roots on the
// evaluator). The root must be an evaluator-seeded root and at least
// one dot segment must follow; any whitespace, quote, paren, brace,
// bracket, or comma disqualifies -- those are expression or user-data
// shapes, not bare references. Used to decide whether an unresolvable
// reference drops to null (compiler reference: its source text is
// never valid data, memql#1392) or passes through unchanged
// (everything else, preserving prior behaviour).
func isCompiledReference(expr string) bool {
	if !strings.HasPrefix(expr, "$") {
		return false
	}
	path := expr[1:]
	if path == "" || strings.ContainsAny(path, " \t\r\n\"'(){}[],") {
		return false
	}
	root, rest, found := strings.Cut(path, ".")
	if !found || rest == "" {
		return false
	}
	switch root {
	case "args", "event", "item", "input", "ctx", "steps":
		return true
	}
	return false
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
// so an unresolved builtin call like `coalesce(autoRole.first().payload.value,
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

// isBareStepArgIdentifier reports whether s is a single identifier
// (`[A-Za-z_][A-Za-z0-9_]*`, no dots / parens / operators) -- the shape a bare
// step-variable reference takes when passed straight through as a mutation /
// function arg. The caller gates resolution on the identifier actually matching
// a recorded step, so a plain word literal is never mistaken for a step.
func isBareStepArgIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
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

// resolveArgValueRef resolves a single arg value, recursing into maps and arrays.
func resolveArgValueRef(v any, evaluator *automations.Evaluator) (any, error) {
	switch val := v.(type) {
	case string:
		// Bare step-variable reference: a single identifier (no dot / call /
		// operator) that names a recorded step result. A logic body's later
		// step passes a prior `name := <call>` result straight through as a
		// mutation arg -- e.g. recordTransition's
		// `recordRequestEvent({ requestId: requestId, fromStatus:
		// fromStatus, toStatus: toStatus })`, where requestId / fromStatus /
		// toStatus are all earlier coalesce(...) steps. Without this the bare
		// identifier renders as its own LITERAL NAME ("requestId") into the
		// write, so the audit row carried `requestId:"requestId"` and the
		// transition was unqueryable -- the third facet of #1847. Scoped
		// strictly to identifiers that match a recorded step so genuine word
		// literals ("approved", "submitted") are untouched.
		if evaluator != nil && isBareStepArgIdentifier(val) {
			if resolved, ok := evaluator.StepResultValue(val); ok {
				return resolved, nil
			}
		}
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
				// Fail closed: `val` IS a recognised builtin expression here, so
				// its raw source text is never a valid value. Passing it through
				// verbatim is exactly how an unevaluated `coalesce(...)` literal
				// landed in a mutation arg and became the ghost-AI agentId
				// (memql#574/#580). Return nil so the literal can never reach the
				// graph as data; the caller's guards decide what nil means.
				evaluator.Warnf("builtin arg resolution failed; dropping to nil",
					"expression", val, "error", err.Error())
				return nil, nil
			}
			if resolved == nil {
				evaluator.Warnf("builtin arg resolved to nil",
					"expression", val)
				return nil, nil
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
				// A compiler-emitted reference whose path is missing from
				// the payload must degrade to null -- NEVER to its own
				// source text. Returning the raw string here is exactly how
				// the literal `$args.event.payload.summary` ended up stored
				// as an artifact's summary (memql#1392): renderMemQLValue
				// passes runtime-reference strings through unquoted, so the
				// unresolved reference survives into the mutation row as
				// data. Scoped to the strict `$root.path` shape the
				// compiler produces (see isCompiledReference); anything
				// else -- user data that happens to start with `$`, the
				// bare `event` token pinned by #418 -- keeps the historical
				// pass-through.
				if isCompiledReference(expr) {
					if err != nil {
						evaluator.Warnf("function arg reference failed to resolve; dropping to null",
							"reference", val, "error", err.Error())
					} else {
						evaluator.Warnf("function arg reference resolved to no value; dropping to null",
							"reference", val)
					}
					return nil, nil
				}
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
