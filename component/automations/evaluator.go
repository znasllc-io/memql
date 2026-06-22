package automations

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// VariableResolver resolves variable names to their values.
// It is called when evaluating $var.VARIABLE_NAME expressions.
type VariableResolver func(ctx context.Context, name string) (string, error)

// CanonicalIdResolver normalizes an id-shaped value to canonical
// form for the named concept (`<partition>:<conceptType>:<bareSlug>`).
// Wired by the engine boot path so automation steps can call
// canonicalId(value, "<conceptType>") and get the same answer as
// mutations / queries. When unset, callers should fall back to the
// raw value (degraded but safe).
type CanonicalIdResolver func(ctx context.Context, value, conceptType string) (string, error)

// Evaluator resolves $ expressions and conditions in automation definitions.
type Evaluator struct {
	// input holds the automation input query result.
	input any

	// steps holds results from completed steps, keyed by step ID.
	steps map[string]*StepResult

	// item holds the current item in a forEach loop.
	item any

	// itemName is the variable name for the current item (default "item").
	itemName string

	// custom holds user-defined variables.
	custom map[string]any

	// variableResolver resolves $var.X references to v1:platform:partitionVariable
	// concept values, falling back to v1:platform:globalVariable.
	variableResolver VariableResolver

	// systemVariableResolver resolves $systemVar.X references to
	// v1:platform:globalVariable (global plaintext). No fallback.
	systemVariableResolver VariableResolver

	// secretResolver resolves $secret.X references to v1:platform:partitionSecret
	// (partition-scoped encrypted), falling back to v1:platform:globalSecret.
	// Returns the decrypted plaintext.
	secretResolver VariableResolver

	// systemSecretResolver resolves $systemSecret.X references to
	// v1:platform:globalSecret (global encrypted). Returns decrypted plaintext.
	systemSecretResolver VariableResolver

	// canonicalIdResolver normalizes an id-shaped value to canonical
	// form for a named concept. Used by canonicalId(value,
	// "<conceptType>") in automation step expressions.
	canonicalIdResolver CanonicalIdResolver

	// logger for logging warnings when expression resolution fails.
	logger *slog.Logger
}

// SetCanonicalIdResolver wires the engine's id-canonicalization helper
// into the evaluator. Required for canonicalId() to work inside
// automation step expressions; without it, callers fall back to the
// raw value (which means automation-derived ids may diverge from
// mutation-derived ids when callers pass bare slugs).
func (e *Evaluator) SetCanonicalIdResolver(r CanonicalIdResolver) {
	if e == nil {
		return
	}
	e.canonicalIdResolver = r
}

// CanonicalIdResolver returns the wired resolver (may be nil).
func (e *Evaluator) CanonicalIdResolver() CanonicalIdResolver {
	if e == nil {
		return nil
	}
	return e.canonicalIdResolver
}

// NewEvaluator creates a new expression evaluator.
func NewEvaluator() *Evaluator {
	return &Evaluator{
		steps:    make(map[string]*StepResult),
		itemName: "item",
		custom:   make(map[string]any),
	}
}

// SetInput sets the automation input data.
func (e *Evaluator) SetInput(input any) {
	e.input = input
}

// SetStepResult records a step's result.
func (e *Evaluator) SetStepResult(stepId string, result *StepResult) {
	e.steps[stepId] = result
}

// SetItem sets the current forEach item.
func (e *Evaluator) SetItem(item any, name string) {
	e.item = item
	if name != "" {
		e.itemName = name
	} else {
		e.itemName = "item"
	}
}

// ClearItem clears the current forEach item.
func (e *Evaluator) ClearItem() {
	e.item = nil
	e.itemName = "item"
}

// SetCustom sets a custom variable.
func (e *Evaluator) SetCustom(name string, value any) {
	e.custom[name] = value
}

// Clone creates a copy of the evaluator for nested contexts.
func (e *Evaluator) Clone() *Evaluator {
	clone := &Evaluator{
		input:                  e.input,
		steps:                  make(map[string]*StepResult),
		item:                   e.item,
		itemName:               e.itemName,
		custom:                 make(map[string]any),
		variableResolver:       e.variableResolver,
		systemVariableResolver: e.systemVariableResolver,
		secretResolver:         e.secretResolver,
		systemSecretResolver:   e.systemSecretResolver,
		logger:                 e.logger,
	}
	for k, v := range e.steps {
		clone.steps[k] = v
	}
	for k, v := range e.custom {
		clone.custom[k] = v
	}
	return clone
}

// SetVariableResolver sets the variable resolver for $var.X expressions.
func (e *Evaluator) SetVariableResolver(resolver VariableResolver) {
	e.variableResolver = resolver
}

// SetSystemVariableResolver sets the resolver for $systemVar.X
// expressions (v1:platform:globalVariable).
func (e *Evaluator) SetSystemVariableResolver(resolver VariableResolver) {
	e.systemVariableResolver = resolver
}

// SetSecretResolver sets the resolver for $secret.X expressions
// (v1:platform:partitionSecret with fallback to v1:platform:globalSecret). Returns
// decrypted plaintext.
func (e *Evaluator) SetSecretResolver(resolver VariableResolver) {
	e.secretResolver = resolver
}

// SetSystemSecretResolver sets the resolver for $systemSecret.X
// expressions (v1:platform:globalSecret). Returns decrypted plaintext.
func (e *Evaluator) SetSystemSecretResolver(resolver VariableResolver) {
	e.systemSecretResolver = resolver
}

// SetLogger sets the logger for warning messages.
func (e *Evaluator) SetLogger(logger *slog.Logger) {
	e.logger = logger
}

// Warnf logs a warning via the configured logger (no-op when unset). Exposed
// so sibling packages (the arg-time builtin evaluator) can surface a
// resolution that silently fell back to a raw literal -- e.g. an unevaluated
// `coalesce(...)` about to reach a mutation arg (memql#574).
func (e *Evaluator) Warnf(msg string, args ...any) {
	if e == nil || e.logger == nil {
		return
	}
	e.logger.Warn(msg, append([]any{"component", ComponentName}, args...)...)
}

// GetStepNodes returns the nodes array from a step result if it exists.
// This is used by the mutation evaluator to resolve bare step names like "getProgress"
// to the step's result nodes (equivalent to $steps.getProgress.result.Bundle.nodes).
func (e *Evaluator) GetStepNodes(stepId string) ([]any, bool) {
	stepResult, ok := e.steps[stepId]
	if !ok || stepResult == nil {
		return nil, false
	}

	result := stepResult.Result
	if result == nil {
		return nil, false
	}

	// Try to extract Bundle.nodes from the result
	// First, try direct map access
	if resultMap, ok := result.(map[string]any); ok {
		if bundle, ok := resultMap["Bundle"].(map[string]any); ok {
			if nodes, ok := bundle["nodes"].([]any); ok {
				return nodes, true
			}
		}
	}

	// Try via JSON marshaling for struct types (like *memql.ExecuteResult)
	jsonBytes, err := json.Marshal(result)
	if err != nil {
		return nil, false
	}
	var resultMap map[string]any
	if err := json.Unmarshal(jsonBytes, &resultMap); err != nil {
		return nil, false
	}
	if bundle, ok := resultMap["Bundle"].(map[string]any); ok {
		if nodes, ok := bundle["nodes"].([]any); ok {
			return nodes, true
		}
	}

	return nil, false
}

// HasStep returns true if the evaluator has a result for the given step ID.
func (e *Evaluator) HasStep(stepId string) bool {
	_, ok := e.steps[stepId]
	return ok
}

// StepResultValue returns a step's raw Result value (and whether the step
// exists). Used by the arg-time builtin evaluator to resolve a BARE step
// identifier (`getActiveGA`, no dot/call) to the step's result -- so
// selector builtins like coalesce(stepA, stepB) can pick the first step
// that produced rows instead of rendering the identifier as its own name.
// See memql#574.
func (e *Evaluator) StepResultValue(stepId string) (any, bool) {
	sr, ok := e.steps[stepId]
	if !ok || sr == nil {
		return nil, false
	}
	return sr.Result, true
}

// ContextFingerprint returns a map of all evaluator state for cache key computation.
// This captures everything a function step could potentially access:
// $input, $steps.*, $item, and custom variables.
func (e *Evaluator) ContextFingerprint() map[string]any {
	ctx := make(map[string]any)

	// Include automation input
	if e.input != nil {
		ctx["input"] = e.input
	}

	// Include all step results (functions can access $steps.X.result)
	if len(e.steps) > 0 {
		stepsCtx := make(map[string]any, len(e.steps))
		for stepId, result := range e.steps {
			if result != nil {
				// Only include fields that affect function output
				stepsCtx[stepId] = map[string]any{
					"status": result.Status,
					"result": result.Result,
				}
			}
		}
		ctx["steps"] = stepsCtx
	}

	// Include current forEach item if set
	if e.item != nil {
		ctx["item"] = e.item
		ctx["itemName"] = e.itemName
	}

	// Include custom variables
	if len(e.custom) > 0 {
		customCtx := make(map[string]any, len(e.custom))
		for k, v := range e.custom {
			customCtx[k] = v
		}
		ctx["custom"] = customCtx
	}

	return ctx
}

// Regular expressions for parsing expressions.
var (
	// Matches ${VAR} for environment variables
	envVarPattern = regexp.MustCompile(`\$\{([A-Z_][A-Z0-9_]*)\}`)

	// Matches $path.to.value for data references, including numeric segments and array indexing.
	// Examples:
	// - $event.payload.id
	// - $steps.extractLead.result.0.extraction.email
	// - $steps.extractLead.result[0].extraction.email
	dataRefPattern = regexp.MustCompile(`\$([a-zA-Z_][a-zA-Z0-9_]*(?:(?:\.(?:[a-zA-Z_][a-zA-Z0-9_]*|\d+))|(?:\[\d+\]))*)`)

	// Matches array access like .result[0]
	arrayIndexPattern = regexp.MustCompile(`\[(\d+)\]`)

	// Matches $pretty(...) for prettified JSON output
	prettyFuncPattern = regexp.MustCompile(`\$pretty\(([^)]+)\)`)

	// Matches $coalesce(...) for picking the first non-empty value.
	// Note: this regex is only used as a quick pre-check; parsing is done manually.
	coalesceFuncPrefix = "$coalesce("
)

// EvaluateString resolves $ expressions in a string value.
// Returns the resolved string with all expressions substituted.
func (e *Evaluator) EvaluateString(expr string) (string, error) {
	return e.evaluateStringWithFormatter(expr, FormatValue)
}

// EvaluateStringForQuery resolves $ expressions in a query string.
// String values containing operator characters (like UUIDs with hyphens) are
// automatically quoted to prevent parsing errors.
func (e *Evaluator) EvaluateStringForQuery(expr string) (string, error) {
	return e.evaluateStringWithFormatter(expr, FormatValueForQuery)
}

// evaluateStringWithFormatter resolves $ expressions using the provided formatter.
func (e *Evaluator) evaluateStringWithFormatter(expr string, formatter func(any) string) (string, error) {
	if expr == "" {
		return "", nil
	}

	// First substitute environment variables ${VAR}
	result := envVarPattern.ReplaceAllStringFunc(expr, func(match string) string {
		varName := match[2 : len(match)-1] // Strip ${ and }
		if val := os.Getenv(varName); val != "" {
			return val
		}
		return match // Keep original if not found
	})

	// Handle $pretty(...) function calls for prettified JSON output
	result = prettyFuncPattern.ReplaceAllStringFunc(result, func(match string) string {
		// Extract the inner expression from $pretty(...)
		inner := prettyFuncPattern.FindStringSubmatch(match)
		if len(inner) < 2 {
			return match
		}
		innerExpr := strings.TrimSpace(inner[1])

		// Resolve the inner expression
		var val any
		var err error
		if strings.HasPrefix(innerExpr, "$") {
			path := innerExpr[1:] // Strip leading $
			val, err = e.resolvePath(path)
		} else {
			val = innerExpr
		}
		if err != nil {
			if e.logger != nil {
				e.logger.Warn("pretty expression resolution failed",
					"component", ComponentName,
					"expression", match,
					"error", err.Error(),
				)
			}
			return match
		}
		return FormatValuePretty(val)
	})

	// Then substitute data references $path.to.value
	result = dataRefPattern.ReplaceAllStringFunc(result, func(match string) string {
		path := match[1:] // Strip leading $
		val, err := e.resolvePath(path)
		if err != nil {
			// Log warning so failures aren't silently swallowed
			if e.logger != nil {
				e.logger.Warn("expression resolution failed",
					"component", ComponentName,
					"expression", match,
					"error", err.Error(),
				)
			}
			return match // Keep original if resolution fails
		}
		return formatter(val)
	})

	// Resolve bare item references (for forEach loops) that appear as comparison values
	// e.g., "payload.agentId==agent.id" where "agent" is the loop variable
	// This pattern matches: ==itemName.path or !=itemName.path (not inside quotes)
	if e.item != nil && e.itemName != "" {
		result = e.resolveItemReferencesInQuery(result, formatter)
	}

	return result, nil
}

// resolveItemReferencesInQuery resolves bare item references in query strings.
// e.g., "payload.field==agent.id" becomes "payload.field==actualValue"
func (e *Evaluator) resolveItemReferencesInQuery(query string, formatter func(any) string) string {
	if e.item == nil || e.itemName == "" {
		return query
	}

	// Build pattern to match itemName.path after comparison operators (==, !=)
	// Pattern: (==|!=)itemName.path where path doesn't contain operators or quotes
	itemPattern := regexp.MustCompile(`(==|!=)` + regexp.QuoteMeta(e.itemName) + `\.([a-zA-Z_][a-zA-Z0-9_.]*[a-zA-Z0-9_])`)

	return itemPattern.ReplaceAllStringFunc(query, func(match string) string {
		// Extract the operator and path
		var operator string
		var path string
		if strings.HasPrefix(match, "==") {
			operator = "=="
			path = match[2:]
		} else if strings.HasPrefix(match, "!=") {
			operator = "!="
			path = match[2:]
		} else {
			return match
		}

		// Resolve the path (e.g., "agent.id" -> actual value)
		val, err := e.resolvePath(path)
		if err != nil {
			if e.logger != nil {
				e.logger.Debug("item reference resolution failed in query",
					"component", ComponentName,
					"path", path,
					"error", err.Error(),
				)
			}
			return match // Keep original if resolution fails
		}

		return operator + formatter(val)
	})
}

// EvaluateValue resolves a single $ expression and returns the raw value.
// Unlike EvaluateString, this preserves the type of the resolved value.
func (e *Evaluator) EvaluateValue(expr string) (any, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, nil
	}

	// Handle function-like $ expressions that are intended to return raw values.
	// These must be handled before the "pure $ reference" fast path.
	if strings.HasPrefix(expr, coalesceFuncPrefix) {
		return e.evaluateCoalesce(expr)
	}
	if strings.HasPrefix(expr, "$pretty(") {
		// $pretty always returns a string.
		return e.EvaluateString(expr)
	}

	// Check if it's a pure $ reference (entire string is one reference)
	if strings.HasPrefix(expr, "$") && !strings.HasPrefix(expr, "${") {
		path := expr[1:]
		// Check for embedded array access or other complex paths
		// Reject function-call-like expressions (e.g. pretty(...), coalesce(...)).
		if !strings.ContainsAny(path, " \t\n\"'(),") {
			return e.resolvePath(path)
		}
	}

	// Bare known-step identifier (no '$', no dot, no call): resolve to the
	// step's result. Mirrors the arg-time evaluator (MutationExecutor.evaluateValue,
	// memql#575) so a selector builtin like coalesce(getActiveGA, getFallbackGA)
	// in a LOGIC body picks the first step that produced rows instead of
	// rendering the identifier as its OWN NAME (a literal string). That literal
	// then defeats the downstream `getGA.First().id` read and feeds an
	// unevaluated coalesce(...) expression into a mutation arg -- the memql#580
	// ghost-AI root cause. The logic-runner path bottoms out here (via
	// EvaluateStepReference / evaluateScalarArg), and #575 only patched the
	// arg-time evaluator, leaving this one literalising bare steps. Guarded on a
	// real step id so non-step bare literals keep prior behaviour (a bare
	// identifier that is NOT a known step still renders as itself below).
	if isBareIdentifier(expr) {
		if result, ok := e.StepResultValue(expr); ok {
			return result, nil
		}
	}

	// Otherwise treat as string with possible embedded expressions
	resolved, err := e.EvaluateString(expr)
	return resolved, err
}

func (e *Evaluator) evaluateCoalesce(expr string) (any, error) {
	expr = strings.TrimSpace(expr)
	if !strings.HasPrefix(expr, coalesceFuncPrefix) || !strings.HasSuffix(expr, ")") {
		return nil, fmt.Errorf("invalid $coalesce() expression: %q", expr)
	}

	inner := strings.TrimSpace(expr[len(coalesceFuncPrefix) : len(expr)-1])
	args, err := splitCoalesceArgs(inner)
	if err != nil {
		return nil, err
	}
	if len(args) == 0 {
		return nil, nil
	}

	for i, raw := range args {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}

		val, ok := e.evaluateCoalesceArg(raw)
		if !ok {
			continue
		}

		// #1614: the FINAL arg is the ultimate fallback -- return it even
		// when it resolves to "" (so coalesce(absent, "") -> ""). A
		// NON-final arg that resolves to nil or "" is treated as missing
		// and skipped, so coalesce("", fallback) / coalesce(emptyField,
		// fallback) still fall through.
		if i == len(args)-1 {
			return val, nil
		}
		if val == nil {
			continue
		}
		if s, isString := val.(string); isString && strings.TrimSpace(s) == "" {
			continue
		}
		return val, nil
	}

	return nil, nil
}

func (e *Evaluator) evaluateCoalesceArg(raw string) (any, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, false
	}

	// Nested $ expressions.
	if strings.HasPrefix(raw, "$") && !strings.HasPrefix(raw, "${") {
		val, err := e.EvaluateValue(raw)
		if err != nil {
			// Coalesce is intentionally "soft" for missing paths, out-of-bounds indices, etc.
			// This makes it safe to write $coalesce($steps.x.result[0].y, "fallback") without
			// needing separate itemCount guards.
			return nil, false
		}
		if val == nil {
			return nil, false
		}
		return val, true
	}

	// Quoted string literal.
	if (strings.HasPrefix(raw, "\"") && strings.HasSuffix(raw, "\"")) || (strings.HasPrefix(raw, "'") && strings.HasSuffix(raw, "'")) {
		// strconv.Unquote expects double quotes, but also supports single quotes.
		v, err := strconv.Unquote(raw)
		if err != nil {
			return nil, false
		}
		if strings.TrimSpace(v) == "" {
			return nil, false
		}
		return v, true
	}

	switch raw {
	case "null":
		return nil, false
	case "true":
		return true, true
	case "false":
		return false, true
	}

	// Numeric literal (best effort).
	if n, err := strconv.ParseFloat(raw, 64); err == nil {
		return n, true
	}

	// Treat as an unquoted string literal.
	if strings.TrimSpace(raw) == "" {
		return nil, false
	}
	return raw, true
}

func splitCoalesceArgs(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}

	var args []string
	var current strings.Builder

	inQuote := false
	quoteChar := byte(0)
	depth := 0

	for i := 0; i < len(s); i++ {
		ch := s[i]

		if inQuote {
			current.WriteByte(ch)
			// Basic handling: ignore escaped quote chars.
			if ch == quoteChar && (i == 0 || s[i-1] != '\\') {
				inQuote = false
				quoteChar = 0
			}
			continue
		}

		switch ch {
		case '"', '\'':
			inQuote = true
			quoteChar = ch
			current.WriteByte(ch)
		case '(':
			depth++
			current.WriteByte(ch)
		case ')':
			if depth == 0 {
				return nil, fmt.Errorf("unbalanced parentheses in $coalesce() args: %q", s)
			}
			depth--
			current.WriteByte(ch)
		case ',':
			if depth == 0 {
				args = append(args, current.String())
				current.Reset()
				continue
			}
			current.WriteByte(ch)
		default:
			current.WriteByte(ch)
		}
	}

	if inQuote {
		return nil, fmt.Errorf("unterminated quote in $coalesce() args: %q", s)
	}
	if depth != 0 {
		return nil, fmt.Errorf("unbalanced parentheses in $coalesce() args: %q", s)
	}

	args = append(args, current.String())
	return args, nil
}

// conditionRootSegment reports the explicit reference root of a bare dotted
// path when that root must resolve against itself rather than the implicit
// `event.` prefix EvaluateFilterValue applies to unprefixed paths. It returns
// the root segment string (truthy) for a known explicit root, or "" when the
// path should keep the legacy `event.`-prefixed resolution.
//
// Explicit roots are the values the logic/automation evaluator seeds or the
// resolvePath root switch understands: the caller-arg + context roots a logic
// body reads (`args`, `ctx`, `input`), the loop item (`item` or the configured
// item name), recorded step results (`steps`), and the resolver-backed roots
// (`var` / `systemVar` / `secret` / `systemSecret` / `automation`). `event` is
// intentionally NOT listed: a bare `event.X` already short-circuits the prefix
// in EvaluateFilterValue and keeps its existing pass-through, so behaviour for
// the common `payload.X` / `event.X` filter shapes is unchanged. A first
// segment that merely matches a seeded custom key also qualifies so a logic
// that seeds an extra root resolves it explicitly.
func (e *Evaluator) conditionRootSegment(expr string) string {
	dot := strings.Index(expr, ".")
	if dot <= 0 {
		return ""
	}
	root := expr[:dot]
	switch root {
	case "args", "ctx", "input", "item", "steps",
		"var", "systemVar", "secret", "systemSecret", "automation":
		return root
	}
	if root == e.itemName && e.item != nil {
		return root
	}
	if _, ok := e.custom[root]; ok && root != "event" {
		return root
	}
	return ""
}

// isBareStepIdentifier reports whether expr is a single identifier
// (`[A-Za-z_][A-Za-z0-9_]*`, no dots / parens / operators) -- the shape a bare
// step-variable reference (`toStatus`) takes as a comparison operand. The
// caller still gates resolution on the identifier actually matching a recorded
// step, so a plain word literal that isn't a step name is unaffected.
func isBareStepIdentifier(expr string) bool {
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

// EvaluateFilterValue resolves a value in a filter context.
// Bare paths like "payload.X" are auto-resolved to "$event.payload.X".
// This allows cleaner filter syntax without requiring explicit $event. prefix.
func (e *Evaluator) EvaluateFilterValue(expr string) (any, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, nil
	}

	// If already a $ expression, use standard evaluation
	if strings.HasPrefix(expr, "$") {
		return e.EvaluateValue(expr)
	}

	// Handle len() function - returns array length
	if strings.HasPrefix(expr, "len(") && strings.HasSuffix(expr, ")") {
		innerExpr := strings.TrimSpace(expr[4 : len(expr)-1])
		innerVal, err := e.EvaluateFilterValue(innerExpr)
		if err != nil {
			return nil, fmt.Errorf("len() argument evaluation failed: %w", err)
		}
		switch arr := innerVal.(type) {
		case []any:
			return len(arr), nil
		case []map[string]any:
			return len(arr), nil
		case nil:
			return 0, nil
		default:
			return nil, fmt.Errorf("len() requires an array, got %T", innerVal)
		}
	}

	// Handle mean() function - returns arithmetic mean
	if strings.HasPrefix(expr, "mean(") && strings.HasSuffix(expr, ")") {
		innerExpr := strings.TrimSpace(expr[5 : len(expr)-1])
		innerVal, err := e.EvaluateFilterValue(innerExpr)
		if err != nil {
			return nil, fmt.Errorf("mean() argument evaluation failed: %w", err)
		}
		arr, ok := innerVal.([]any)
		if !ok {
			if innerVal == nil {
				return 0.0, nil
			}
			return nil, fmt.Errorf("mean() requires an array, got %T", innerVal)
		}
		if len(arr) == 0 {
			return 0.0, nil
		}
		var sum float64
		for _, v := range arr {
			switch n := v.(type) {
			case int:
				sum += float64(n)
			case int64:
				sum += float64(n)
			case float64:
				sum += n
			case string:
				if f, err := strconv.ParseFloat(n, 64); err == nil {
					sum += f
				}
			}
		}
		return sum / float64(len(arr)), nil
	}

	// Bare step-variable reference: a single identifier (no dot) that names a
	// recorded step result. A logic body's guard like `if toStatus == "queued"`
	// (logicRecordTransition, where `toStatus := coalesce(...)` is a prior step)
	// reaches here as the bare operand `toStatus`. Without this it fails
	// looksLikePath (no dot), falls through to the literal-string fallback, and
	// the comparison tests the path TEXT ("toStatus" != "queued") -- so every
	// `toStatus`-guarded mutationRecordRequestEvent step was skipped (the other
	// half of #1847). Scoped strictly to recorded step ids so a genuine string
	// literal (`"owner"`, `submitted`) is never swallowed.
	if isBareStepIdentifier(expr) {
		if _, known := e.steps[expr]; known {
			if val, err := e.resolvePath("steps." + expr + ".result"); err == nil {
				return val, nil
			}
		}
	}

	// Check if this looks like a path (contains dots, alphanumeric/underscore only)
	if looksLikePath(expr) {
		// Step-result references (memql#1366): a `steps.<id>.<field>` path in
		// a condition / filter context resolves against the recorded step
		// results, exactly like its `$steps.` form. Before this, the
		// `event.`-prefix attempt below could never resolve it
		// (`event.steps.*` does not exist) and the path fell through to a
		// LITERAL string -- so `steps.x.status == "success"` compared the
		// path text itself and was constant. Only attempted when the
		// referenced step id is actually recorded, so a literal that merely
		// starts with "steps." keeps the prior fall-through behaviour.
		if strings.HasPrefix(expr, "steps.") {
			seg := strings.SplitN(expr, ".", 3)
			if len(seg) >= 2 {
				if _, known := e.steps[seg[1]]; known {
					if val, err := e.resolvePath(expr); err == nil {
						return val, nil
					}
				}
			}
		}
		// Explicit-root references resolve against their OWN root, never the
		// implicit `event.` prefix. A logic body guard like
		// `args.event.payload.submitterRole == "owner"` (or `ctx.X`, `input.X`,
		// `item.X`, `steps.X`) names its root explicitly; forcing the `event.`
		// prefix turned it into `event.args.event....` which never resolves, so
		// the guard fell through to a LITERAL string and evaluated false -- the
		// #1847 outage: every guarded `name := if <args-cond> { mutation }` step
		// in a logic body invoked as an automation step (logicRouteRequest's
		// fast-track / approval / validation transitions, logicRecordTransition's
		// whole body) was silently skipped while the UNGUARDED steps ran. Resolve
		// these against resolvePath's root switch directly (the same path
		// `$<root>.X` takes). `event.X` keeps its existing pass-through below.
		if e.conditionRootSegment(expr) != "" {
			if val, err := e.resolvePath(expr); err == nil {
				return val, nil
			}
			// Fall through to literal if the explicit-root path doesn't resolve.
			return expr, nil
		}

		// Try resolving as $event.{path} first
		// If it already starts with "event.", don't add the prefix again
		path := expr
		if !strings.HasPrefix(expr, "event.") {
			path = "event." + expr
		}
		val, err := e.resolvePath(path)
		if err == nil {
			return val, nil
		}
		// Fall back to literal if resolution fails
	}

	// Treat as literal value
	return expr, nil
}

// EvaluateStepReference resolves a value that may reference a step result or loop item.
// Bare paths like "stepName.result.X" are auto-resolved to "$steps.stepName.result.X".
// Bare paths like "itemName.field" are auto-resolved to the current forEach item.
// This allows cleaner .memql syntax without requiring explicit $ prefix.
//
// Resolution order:
// 1. If starts with $, use standard evaluation
// 2. If first segment matches the current item name (forEach variable), resolve from item
// 3. If first segment matches a known step ID, resolve as $steps.{path}
// 4. Fall back to standard evaluation
func (e *Evaluator) EvaluateStepReference(expr string) (any, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, nil
	}

	// If already a $ expression, use standard evaluation
	if strings.HasPrefix(expr, "$") {
		return e.EvaluateValue(expr)
	}

	// Normalise method-call syntax for step-result shorthand accessors
	// (`.Nodes()`, `.First()`, `.Len()`, `.Empty()`, `.Last()`, `.Ran()`).
	// These can legally appear with or without parentheses (see the
	// comment near `case "nodes", "Nodes":` in resolvePath). We strip a
	// single trailing `()` here so the path matcher, which only accepts
	// `a-zA-Z0-9._-`, still recognises the expression as a step
	// reference.
	if strings.HasSuffix(expr, "()") {
		expr = strings.TrimSuffix(expr, "()")
	}

	// Check if this looks like a path
	if looksLikePath(expr) {
		// Extract the first segment
		firstDot := strings.Index(expr, ".")
		if firstDot > 0 {
			firstSegment := expr[:firstDot]

			// Check if this matches the current forEach item name (in .memql we enforce "item")
			if firstSegment == e.itemName && e.item != nil {
				// Resolve using the item as root
				val, err := e.resolvePath(expr)
				if err == nil {
					return val, nil
				}
				// Return the error - item exists but path resolution failed
				return nil, fmt.Errorf("item %q exists but path resolution failed: %w", firstSegment, err)
			}

			// Check if this is a known step ID
			if _, exists := e.steps[firstSegment]; exists {
				// Resolve as $steps.{path}
				stepsPath := "steps." + expr
				val, err := e.resolvePath(stepsPath)
				if err == nil {
					return val, nil
				}
				// Return the error - step exists but path resolution failed
				return nil, fmt.Errorf("step %q exists but path resolution failed: %w", firstSegment, err)
			}
		}
	}

	// Fall back to standard evaluation
	return e.EvaluateValue(expr)
}

// ResolveStrictBarePath resolves a bare dotted path ONLY when its first segment is:
// - a known step ID (resolved as $steps.<stepId>...), or
// - the reserved "item" root (resolved as the current forEach item).
//
// This is intentionally stricter than EvaluateStepReference, and is used to avoid
// accidentally treating arbitrary dotted strings as references.
//
// Returns:
// - (value, true, nil) when the path is eligible and resolves successfully
// - (nil, false, nil) when the path is not eligible (caller should treat as literal)
// - (nil, true, err) when the path is eligible but resolution fails (caller should fail fast)
func (e *Evaluator) ResolveStrictBarePath(expr string) (any, bool, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, false, nil
	}
	if !LooksLikePath(expr) {
		return nil, false, nil
	}

	firstDot := strings.Index(expr, ".")
	if firstDot <= 0 {
		return nil, false, nil
	}
	first := expr[:firstDot]

	// Reserved root: item.*
	if first == "item" {
		if e.item == nil {
			return nil, true, fmt.Errorf("invalid reference %q: no current item is set", expr)
		}
		val, err := e.resolvePath(expr)
		if err != nil {
			return nil, true, err
		}
		return val, true, nil
	}

	// Known step ID: stepId.*
	if _, ok := e.steps[first]; ok {
		val, err := e.resolvePath("steps." + expr)
		if err != nil {
			return nil, true, err
		}
		return val, true, nil
	}

	return nil, false, nil
}

// LooksLikePath checks if an expression appears to be a field path (e.g., "payload.field").
// Exported for use in other packages like steps.
func LooksLikePath(expr string) bool {
	if expr == "" || !strings.Contains(expr, ".") {
		return false
	}
	// Must not contain spaces, quotes, or special characters (except dots and underscores)
	for _, r := range expr {
		if !isPathChar(r) {
			return false
		}
	}
	return true
}

// looksLikePath is an alias for internal use.
func looksLikePath(expr string) bool {
	return LooksLikePath(expr)
}

// isPathChar returns true if the rune is valid in a path expression.
func isPathChar(r rune) bool {
	return (r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') ||
		r == '.' || r == '_' || r == '-'
}

// EvaluateMap resolves $ expressions in all string values within a map.
func (e *Evaluator) EvaluateMap(data map[string]any) (map[string]any, error) {
	if data == nil {
		return nil, nil
	}

	result := make(map[string]any)
	for key, value := range data {
		resolved, err := e.evaluateAny(value)
		if err != nil {
			return nil, fmt.Errorf("evaluating %q: %w", key, err)
		}
		result[key] = resolved
	}
	return result, nil
}

// evaluateAny recursively resolves $ expressions in any value.
func (e *Evaluator) evaluateAny(value any) (any, error) {
	switch v := value.(type) {
	case string:
		return e.EvaluateValue(v)
	case map[string]any:
		return e.EvaluateMap(v)
	case []any:
		result := make([]any, len(v))
		for i, item := range v {
			resolved, err := e.evaluateAny(item)
			if err != nil {
				return nil, err
			}
			result[i] = resolved
		}
		return result, nil
	default:
		return value, nil
	}
}

// EvaluateCondition evaluates a condition expression.
// Supports compound conditions with:
//   - ";" (semicolon) for AND - all parts must be true
//   - "," (comma) for OR - any part must be true
//
// Example: "payload.type==\"human\";payload.status==\"active\""
// Returns true if the condition is satisfied.
func (e *Evaluator) EvaluateCondition(condition string) (bool, error) {
	condition = strings.TrimSpace(condition)
	if condition == "" {
		return true, nil // Empty condition is always true
	}

	// Canonical boolean operators (#973): `&&` is AND, `||` is OR. They
	// normalise to the legacy `;` / `,` separators below. The infix English
	// words `and` / `or` are NOT supported -- they never were (the normaliser
	// only ever rewrote `&&` / `||`), so a condition written with the words
	// fell through to a malformed single comparison. The DSL uses `&&` / `||`
	// everywhere; a conformance test rejects the word forms.
	condition = normalizeConditionOperators(condition)

	// Strip a redundant fully-wrapping paren pair so a parenthesised group
	// like `(a || b || c)` recurses into its OR-split instead of reaching the
	// atomic evaluator as one un-splittable comparison.
	condition = stripRedundantOuterParens(condition)

	// Handle OR conditions first (lower precedence)
	// Split on comma, but be careful not to split inside quoted strings
	orParts := splitConditionParts(condition, ',')
	if len(orParts) > 1 {
		// OR: any part must be true
		for _, part := range orParts {
			result, err := e.EvaluateCondition(part)
			if err != nil {
				continue // Skip erroring parts in OR
			}
			if result {
				return true, nil
			}
		}
		return false, nil
	}

	// Handle AND conditions (higher precedence)
	// Split on semicolon, but be careful not to split inside quoted strings
	andParts := splitConditionParts(condition, ';')
	if len(andParts) > 1 {
		// AND: all parts must be true
		for _, part := range andParts {
			result, err := e.EvaluateCondition(part)
			if err != nil {
				return false, err
			}
			if !result {
				return false, nil
			}
		}
		return true, nil
	}

	// Single atomic condition - evaluate it
	return e.evaluateAtomicCondition(condition)
}

// evaluateAtomicCondition evaluates a single comparison condition.
func (e *Evaluator) evaluateAtomicCondition(condition string) (bool, error) {
	condition = strings.TrimSpace(condition)

	// Unary `!` (NOT) -- a leading `!` negates the boolean value of the
	// operand/group that follows (#1096). It only applies when the `!` is a
	// PREFIX of an operand/group, never when it is the `!=` comparison
	// operator: `!=` always has a left operand, so a depth-0 leading `!`
	// (whitespace after it allowed, e.g. the compiler's `! $steps.x.Empty`)
	// can never be a `!=`. Recurse through EvaluateCondition so the remainder
	// re-enters the OR/AND/paren machinery -- this composes with `&&`/`||`
	// (which already split above this point, so `!a && b` arrives here as the
	// bare atom `!a`), handles parenthesised groups (`!(a == b)`, `!(a || b)`),
	// and collapses double negation (`!!x` -> negate(negate(x)) -> x).
	if rest, ok := stripLeadingBang(condition); ok {
		result, err := e.EvaluateCondition(rest)
		if err != nil {
			return false, err
		}
		return !result, nil
	}

	// exists(<path>) builtin -- true when the referenced field is present
	// AND non-empty. Without this, an `exists(payload.X)` atom has no
	// comparison operator, falls through to the truthy fallthrough below,
	// renders as the LITERAL string "exists(payload.X)" (a non-empty
	// string is always truthy), and the filter ALWAYS passes regardless of
	// the field. That silently disabled `@filter(exists(payload.X))` gates
	// (memql#1396: reRouteNeedsAgentOnAgentCreate fired on every signup's
	// plan-less agent create and then called mutationUpdatePlanStatus with
	// an empty planId; same latent bug on cascadeSupersession). Empty
	// string is treated as "not exists" to match the coalesce
	// empty-string-is-missing semantics used elsewhere in this evaluator.
	if inner, ok := matchSingleArgCall(condition, "exists"); ok {
		return e.evaluateExists(inner)
	}

	// Handle comparison operators
	if strings.Contains(condition, "==") {
		return e.evaluateComparison(condition, "==")
	}
	if strings.Contains(condition, "!=") {
		return e.evaluateComparison(condition, "!=")
	}
	if strings.Contains(condition, ">=") {
		return e.evaluateNumericComparison(condition, ">=")
	}
	if strings.Contains(condition, "<=") {
		return e.evaluateNumericComparison(condition, "<=")
	}
	if strings.Contains(condition, ">") {
		return e.evaluateNumericComparison(condition, ">")
	}
	if strings.Contains(condition, "<") {
		return e.evaluateNumericComparison(condition, "<")
	}

	// Try to evaluate as a truthy value
	val, err := e.EvaluateValue(condition)
	if err != nil {
		return false, err
	}
	return isTruthy(val), nil
}

// matchSingleArgCall reports whether expr is a single-argument call of the
// named function (`name(<arg>)`) at the top level and, if so, returns the
// trimmed inner argument text. It is whitespace-tolerant around the call and
// requires the closing paren to be the final character, so it does not match
// a larger expression that merely contains the call (those are split into
// atoms before reaching here).
func matchSingleArgCall(expr, name string) (string, bool) {
	expr = strings.TrimSpace(expr)
	prefix := name + "("
	if !strings.HasPrefix(expr, prefix) || !strings.HasSuffix(expr, ")") {
		return "", false
	}
	return strings.TrimSpace(expr[len(prefix) : len(expr)-1]), true
}

// evaluateExists resolves the inner field path of an `exists(<path>)` filter
// builtin and reports whether the field is present AND non-empty. Path
// resolution reuses EvaluateFilterValue so bare paths like
// `payload.originatingPlanId` auto-resolve to `$event.payload.<...>` exactly
// like comparison operands do. A nil value, an empty/whitespace-only string,
// an empty array, or an empty object all count as "not exists" -- matching the
// coalesce empty-string-is-missing semantics used elsewhere in this evaluator,
// so a present-but-blank field does not spuriously pass the gate.
func (e *Evaluator) evaluateExists(inner string) (bool, error) {
	inner = strings.TrimSpace(inner)
	if inner == "" {
		return false, nil
	}
	val, err := e.EvaluateFilterValue(inner)
	if err != nil {
		// A path that fails to resolve does not exist.
		return false, nil
	}
	switch v := val.(type) {
	case nil:
		return false, nil
	case string:
		return strings.TrimSpace(v) != "", nil
	case []any:
		return len(v) > 0, nil
	case []map[string]any:
		return len(v) > 0, nil
	case map[string]any:
		return len(v) > 0, nil
	default:
		return true, nil
	}
}

// splitConditionParts splits a condition on a delimiter, respecting quoted
// strings and parenthesisation -- a delimiter only splits at paren depth 0, so
// `(a , b , c) ; d` splits on `;` into `(a , b , c)` and `d` (the inner commas
// stay grouped).
func splitConditionParts(condition string, delimiter rune) []string {
	var parts []string
	var current strings.Builder
	inDoubleQuote := false
	inSingleQuote := false
	parenDepth := 0

	for _, r := range condition {
		switch r {
		case '"':
			if !inSingleQuote {
				inDoubleQuote = !inDoubleQuote
			}
			current.WriteRune(r)
		case '\'':
			if !inDoubleQuote {
				inSingleQuote = !inSingleQuote
			}
			current.WriteRune(r)
		case '(':
			if !inDoubleQuote && !inSingleQuote {
				parenDepth++
			}
			current.WriteRune(r)
		case ')':
			if !inDoubleQuote && !inSingleQuote && parenDepth > 0 {
				parenDepth--
			}
			current.WriteRune(r)
		case delimiter:
			if !inDoubleQuote && !inSingleQuote && parenDepth == 0 {
				part := strings.TrimSpace(current.String())
				if part != "" {
					parts = append(parts, part)
				}
				current.Reset()
			} else {
				current.WriteRune(r)
			}
		default:
			current.WriteRune(r)
		}
	}

	// Don't forget the last part
	part := strings.TrimSpace(current.String())
	if part != "" {
		parts = append(parts, part)
	}

	return parts
}

// stripRedundantOuterParens removes a single fully-wrapping parenthesis pair
// (e.g. `(a || b)` -> `a || b`). It only strips when the leading `(` matches
// the trailing `)` -- `(a) && (b)` is left untouched because its outer parens
// don't wrap the whole expression. Quotes are respected.
func stripRedundantOuterParens(condition string) string {
	for {
		condition = strings.TrimSpace(condition)
		if len(condition) < 2 || condition[0] != '(' || condition[len(condition)-1] != ')' {
			return condition
		}
		depth := 0
		inDoubleQuote := false
		inSingleQuote := false
		wraps := true
		for i, r := range condition {
			switch r {
			case '"':
				if !inSingleQuote {
					inDoubleQuote = !inDoubleQuote
				}
			case '\'':
				if !inDoubleQuote {
					inSingleQuote = !inSingleQuote
				}
			case '(':
				if !inDoubleQuote && !inSingleQuote {
					depth++
				}
			case ')':
				if !inDoubleQuote && !inSingleQuote {
					depth--
					// If we return to depth 0 before the final char, the
					// leading `(` does not wrap the whole expression.
					if depth == 0 && i != len(condition)-1 {
						wraps = false
					}
				}
			}
		}
		if !wraps || depth != 0 {
			return condition
		}
		condition = condition[1 : len(condition)-1]
	}
}

// stripLeadingBang reports whether condition begins with a unary `!` (NOT)
// prefix and, if so, returns the trimmed remainder after it. A depth-0
// leading `!` is always the NOT operator and never the `!=` comparison: `!=`
// requires a left operand, so it can never begin a (trimmed) condition. The
// one ambiguity is `!=` written with no left operand, which is malformed
// anyway; we still avoid mis-stripping it by refusing when the very next
// non-space character is `=`. Whitespace after the `!` is allowed (the
// compiler emits the method-truthiness form as `! $steps.x.Empty`).
func stripLeadingBang(condition string) (string, bool) {
	condition = strings.TrimSpace(condition)
	if len(condition) == 0 || condition[0] != '!' {
		return "", false
	}
	rest := strings.TrimSpace(condition[1:])
	// `!=` (or a stray `! =`) is a comparison, not a unary NOT.
	if strings.HasPrefix(rest, "=") {
		return "", false
	}
	if rest == "" {
		return "", false
	}
	return rest, true
}

func normalizeConditionOperators(condition string) string {
	if !strings.Contains(condition, "&&") && !strings.Contains(condition, "||") {
		return condition
	}

	var out strings.Builder
	out.Grow(len(condition))

	inDoubleQuote := false
	inSingleQuote := false

	for i := 0; i < len(condition); {
		ch := condition[i]

		switch ch {
		case '"':
			if !inSingleQuote {
				inDoubleQuote = !inDoubleQuote
			}
			out.WriteByte(ch)
			i++
			continue
		case '\'':
			if !inDoubleQuote {
				inSingleQuote = !inSingleQuote
			}
			out.WriteByte(ch)
			i++
			continue
		}

		if !inDoubleQuote && !inSingleQuote && i+1 < len(condition) {
			next := condition[i : i+2]
			if next == "&&" {
				out.WriteByte(';')
				i += 2
				continue
			}
			if next == "||" {
				out.WriteByte(',')
				i += 2
				continue
			}
		}

		out.WriteByte(ch)
		i++
	}

	return out.String()
}

func parseQuotedStringLiteral(s string) (string, bool, error) {
	s = strings.TrimSpace(s)
	if len(s) < 2 {
		return "", false, nil
	}

	if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
		v, err := strconv.Unquote(s)
		if err != nil {
			return "", true, err
		}
		return v, true, nil
	}
	return "", false, nil
}

func parseUnquotedLiteral(s string) (any, bool) {
	s = strings.TrimSpace(s)
	switch s {
	case "null":
		return nil, true
	case "true":
		return true, true
	case "false":
		return false, true
	}

	// Numeric literal (best effort).
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return n, true
	}

	return nil, false
}

// evaluateComparison handles == and != comparisons.
// Uses EvaluateFilterValue to auto-resolve bare paths like "payload.X" to "$event.payload.X".
func (e *Evaluator) evaluateComparison(condition, op string) (bool, error) {
	parts := strings.SplitN(condition, op, 2)
	if len(parts) != 2 {
		return false, fmt.Errorf("invalid comparison: %s", condition)
	}

	// Use EvaluateFilterValue for left side (the field path)
	left, err := e.EvaluateFilterValue(strings.TrimSpace(parts[0]))
	if err != nil {
		return false, err
	}

	rightRaw := strings.TrimSpace(parts[1])

	// Quoted string literal.
	if s, ok, err := parseQuotedStringLiteral(rightRaw); err != nil {
		return false, err
	} else if ok {
		rightRaw = s
	}

	var right any
	// Typed literals: null/true/false/numbers.
	if lit, ok := parseUnquotedLiteral(rightRaw); ok {
		right = lit
	} else {
		// Use EvaluateFilterValue for right side too (could be a path reference)
		// Note: EvaluateFilterValue returns strings for unknown tokens; we only treat
		// "null"/"true"/"false"/numbers as typed literals via parseUnquotedLiteral above.
		val, err := e.EvaluateFilterValue(rightRaw)
		if err != nil {
			// Treat as literal string
			right = rightRaw
		} else {
			right = val
		}
	}

	equal := compareValues(left, right)
	if op == "==" {
		return equal, nil
	}
	return !equal, nil
}

// evaluateNumericComparison handles >, <, >=, <= comparisons.
// Uses EvaluateFilterValue to auto-resolve bare paths like "payload.X" to "$event.payload.X".
func (e *Evaluator) evaluateNumericComparison(condition, op string) (bool, error) {
	parts := strings.SplitN(condition, op, 2)
	if len(parts) != 2 {
		return false, fmt.Errorf("invalid comparison: %s", condition)
	}

	left, err := e.EvaluateFilterValue(strings.TrimSpace(parts[0]))
	if err != nil {
		return false, err
	}

	right, err := e.EvaluateFilterValue(strings.TrimSpace(parts[1]))
	if err != nil {
		return false, err
	}

	leftNum := toNumber(left)
	rightNum := toNumber(right)

	switch op {
	case ">":
		return leftNum > rightNum, nil
	case "<":
		return leftNum < rightNum, nil
	case ">=":
		return leftNum >= rightNum, nil
	case "<=":
		return leftNum <= rightNum, nil
	default:
		return false, fmt.Errorf("unknown operator: %s", op)
	}
}

// resolvePath resolves a dot-separated path to a value.
func (e *Evaluator) resolvePath(path string) (any, error) {
	// Handle array indices in path (convert path.result[0] to path segments)
	path = arrayIndexPattern.ReplaceAllString(path, ".$1")
	parts := strings.Split(path, ".")

	if len(parts) == 0 {
		return nil, fmt.Errorf("empty path")
	}

	// Determine the root value based on first segment
	var current any
	switch parts[0] {
	case "input":
		current = e.input
		parts = parts[1:]
	case "steps":
		if len(parts) < 2 {
			return nil, fmt.Errorf("incomplete steps reference: %s", path)
		}
		stepId := parts[1]
		stepResult, ok := e.steps[stepId]
		if !ok {
			return nil, fmt.Errorf("step %q not found", stepId)
		}
		// Result shorthand accessors. Both field-style (.count, .empty,
		// .first, .nodes) and Go-style method names (.Len, .Empty,
		// .First, .Last, .Nodes, .Ran) are recognised. Phase 6 of
		// the language-improvements plan migrates existing callsites
		// to the method-style; Phase 4 lands the recognition here so
		// both forms work during the grace period.
		//
		// .Ran() is new: it disambiguates "step skipped (condition
		// evaluated to false, step never executed)" from "step ran
		// but produced zero results". With only .empty, those two
		// states collapsed, which was a real bug-farm.
		if len(parts) > 2 {
			switch parts[2] {
			case "count", "Len":
				nodes, _ := e.GetStepNodes(stepId)
				current = len(nodes)
				parts = parts[3:]
				goto navigate
			case "nodes", "Nodes":
				nodes, _ := e.GetStepNodes(stepId)
				current = toAnySlice(nodes)
				parts = parts[3:]
				goto navigate
			case "empty", "Empty":
				nodes, _ := e.GetStepNodes(stepId)
				current = len(nodes) == 0
				parts = parts[3:]
				goto navigate
			case "first", "First":
				nodes, _ := e.GetStepNodes(stepId)
				if len(nodes) > 0 {
					current = nodes[0]
				} else {
					current = nil
				}
				parts = parts[3:]
				goto navigate
			case "last", "Last":
				nodes, _ := e.GetStepNodes(stepId)
				if len(nodes) > 0 {
					current = nodes[len(nodes)-1]
				} else {
					current = nil
				}
				parts = parts[3:]
				goto navigate
			case "Ran":
				// True when the step executed at all (any status).
				// Distinct from .Empty which flags zero-results.
				current = stepResult.Status != ""
				parts = parts[3:]
				goto navigate
			case "result":
				current = stepResult.Result
				parts = parts[3:]
				goto navigate
			}
		}
		current = stepResult
		parts = parts[2:]
	case "item":
		current = e.item
		parts = parts[1:]
	case "automation":
		// Reserved for automation-level metadata
		return e.resolveAutomationMeta(parts[1:])
	case "var":
		// Resolve partition-scoped plaintext variable from
		// v1:platform:partitionVariable, falling back to v1:platform:globalVariable.
		if len(parts) < 2 {
			return nil, fmt.Errorf("incomplete var reference: %s", path)
		}
		varName := parts[1]
		if e.variableResolver == nil {
			return nil, fmt.Errorf("variable resolver not configured")
		}
		value, err := e.variableResolver(context.Background(), varName)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve variable %q: %w", varName, err)
		}
		return value, nil
	case "systemVar":
		// Global plaintext variable from v1:platform:globalVariable.
		if len(parts) < 2 {
			return nil, fmt.Errorf("incomplete systemVar reference: %s", path)
		}
		varName := parts[1]
		if e.systemVariableResolver == nil {
			return nil, fmt.Errorf("system variable resolver not configured")
		}
		value, err := e.systemVariableResolver(context.Background(), varName)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve system variable %q: %w", varName, err)
		}
		return value, nil
	case "secret":
		// Partition-scoped encrypted secret, decrypted under
		// MEMQL_MASTER_KEY. Falls back to v1:platform:globalSecret on miss.
		if len(parts) < 2 {
			return nil, fmt.Errorf("incomplete secret reference: %s", path)
		}
		secretName := parts[1]
		if e.secretResolver == nil {
			return nil, fmt.Errorf("secret resolver not configured")
		}
		value, err := e.secretResolver(context.Background(), secretName)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve secret %q: %w", secretName, err)
		}
		return value, nil
	case "systemSecret":
		// Global encrypted secret from v1:platform:globalSecret.
		if len(parts) < 2 {
			return nil, fmt.Errorf("incomplete systemSecret reference: %s", path)
		}
		secretName := parts[1]
		if e.systemSecretResolver == nil {
			return nil, fmt.Errorf("system secret resolver not configured")
		}
		value, err := e.systemSecretResolver(context.Background(), secretName)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve system secret %q: %w", secretName, err)
		}
		return value, nil
	default:
		// Check if it matches the current item name
		if parts[0] == e.itemName {
			current = e.item
			parts = parts[1:]
		} else if val, ok := e.custom[parts[0]]; ok {
			current = val
			parts = parts[1:]
		} else {
			return nil, fmt.Errorf("unknown reference: %s", parts[0])
		}
	}

navigate:
	// Navigate remaining path segments
	for _, segment := range parts {
		if current == nil {
			return nil, nil
		}

		// Try numeric index (supports negative indices for last element access)
		if idx, err := strconv.Atoi(segment); err == nil {
			switch v := current.(type) {
			case []any:
				// Handle negative indices (e.g., -1 for last element)
				if idx < 0 {
					idx = len(v) + idx
				}
				if idx >= 0 && idx < len(v) {
					current = v[idx]
					continue
				}
				return nil, fmt.Errorf("index %d out of bounds (len=%d)", idx, len(v))
			case []map[string]any:
				// Handle negative indices
				if idx < 0 {
					idx = len(v) + idx
				}
				if idx >= 0 && idx < len(v) {
					current = v[idx]
					continue
				}
				return nil, fmt.Errorf("index %d out of bounds (len=%d)", idx, len(v))
			}
		}

		// Try map access
		switch v := current.(type) {
		case map[string]any:
			current = v[segment]
		case *StepResult:
			switch segment {
			case "result":
				current = v.Result
			case "status":
				current = v.Status
			case "error":
				current = v.Error
			case "metadata":
				current = v.Metadata
			default:
				return nil, fmt.Errorf("unknown StepResult field: %s", segment)
			}
		default:
			// Try JSON marshaling for struct access
			data, err := json.Marshal(current)
			if err != nil {
				return nil, fmt.Errorf("cannot access %q on %T", segment, current)
			}
			var m map[string]any
			if err := json.Unmarshal(data, &m); err != nil {
				return nil, fmt.Errorf("cannot access %q on %T", segment, current)
			}
			// Debug: log available keys when accessing via JSON marshal path
			if e.logger != nil {
				keys := make([]string, 0, len(m))
				for k := range m {
					keys = append(keys, k)
				}
				e.logger.Debug("resolvePath: JSON marshal access",
					"segment", segment,
					"type", fmt.Sprintf("%T", current),
					"availableKeys", keys,
					"foundValue", m[segment] != nil,
				)
			}
			current = m[segment]
		}
	}

	return current, nil
}

// resolveAutomationMeta handles $automation.* references.
func (e *Evaluator) resolveAutomationMeta(parts []string) (any, error) {
	if len(parts) == 0 {
		return nil, fmt.Errorf("incomplete automation reference")
	}

	switch parts[0] {
	case "errors":
		// Collect all errors from steps
		var errors []string
		for _, step := range e.steps {
			if step.Error != "" {
				errors = append(errors, fmt.Sprintf("%s: %s", step.StepId, step.Error))
			}
		}
		return errors, nil
	default:
		return nil, fmt.Errorf("unknown automation property: %s", parts[0])
	}
}

// Helper functions

// FormatValue converts any value to a string representation.
func FormatValue(v any) string {
	if v == nil {
		return ""
	}
	switch val := v.(type) {
	case string:
		return val
	case bool:
		return strconv.FormatBool(val)
	case int:
		return strconv.Itoa(val)
	case int64:
		return strconv.FormatInt(val, 10)
	case float64:
		return strconv.FormatFloat(val, 'f', -1, 64)
	case json.Number:
		return val.String()
	default:
		data, err := json.Marshal(val)
		if err != nil {
			return fmt.Sprintf("%v", val)
		}
		return string(data)
	}
}

// FormatValueForQuery converts a value to a string suitable for MemQL queries.
// Strings containing operator characters (like hyphens in UUIDs) are quoted.
func FormatValueForQuery(v any) string {
	if v == nil {
		return ""
	}
	switch val := v.(type) {
	case string:
		// Check if string needs quoting (contains operator characters)
		if needsQuoting(val) {
			// Escape any embedded quotes and wrap in double quotes
			escaped := strings.ReplaceAll(val, `\`, `\\`)
			escaped = strings.ReplaceAll(escaped, `"`, `\"`)
			return `"` + escaped + `"`
		}
		return val
	case bool:
		return strconv.FormatBool(val)
	case int:
		return strconv.Itoa(val)
	case int64:
		return strconv.FormatInt(val, 10)
	case float64:
		return strconv.FormatFloat(val, 'f', -1, 64)
	case json.Number:
		return val.String()
	default:
		data, err := json.Marshal(val)
		if err != nil {
			return fmt.Sprintf("%v", val)
		}
		return string(data)
	}
}

// needsQuoting returns true if a string contains characters that could be
// misinterpreted as operators in MemQL queries.
// However, if the string looks like a MemQL fragment (contains query structure
// like semicolons or comparison operators), it should NOT be quoted as it's
// intended to be interpreted as MemQL syntax.
func needsQuoting(s string) bool {
	// If string looks like a MemQL fragment, don't quote it
	// MemQL fragments typically contain semicolons (query separators) or
	// comparison operators like ==, !=, in, etc.
	if looksLikeMemQLFragment(s) {
		return false
	}

	// Characters that could be interpreted as operators or delimiters
	// These are problematic in scalar values (like UUIDs with hyphens)
	// NOTE: ':' is used heavily in IDs (e.g., v1:cognition:space:<id>) and must be quoted,
	// otherwise values that contain numeric segments can be mis-tokenized by the MemQL parser.
	operatorChars := "-+*/<>=!&|:"
	for _, r := range s {
		if strings.ContainsRune(operatorChars, r) {
			return true
		}
	}

	// Check for strings that start with a digit but aren't valid numbers.
	// These can be misinterpreted as numeric literals by the parser.
	// Example: "69701e53728ad014454d19f7" looks like scientific notation
	// (69701 × 10^53728) followed by invalid characters.
	if len(s) > 0 && s[0] >= '0' && s[0] <= '9' {
		if _, err := strconv.ParseFloat(s, 64); err != nil {
			return true
		}
	}

	return false
}

// looksLikeMemQLFragment returns true if the string appears to be a MemQL
// query fragment rather than a scalar value. Fragments should pass through
// unquoted so they're interpreted as query syntax.
//
// A MemQL fragment has structural characteristics:
// - Contains semicolon (;) which is the query AND operator
// - Contains a field path followed by a comparison operator (e.g., "payload.status==active")
// - Starts with "concept==" which is the concept filter pattern
//
// Simple strings containing operators (like "abc==def" or checksums) should NOT
// be treated as fragments - they're scalar values that happen to contain operator characters.
func looksLikeMemQLFragment(s string) bool {
	// Contains semicolon (query separator) - definitely a fragment
	// Example: "concept==v1:user;payload.active==true"
	if strings.Contains(s, ";") {
		return true
	}

	// Starts with "concept==" - this is the canonical fragment pattern
	// Example: "concept==v1:crm:lead"
	if strings.HasPrefix(s, "concept==") {
		return true
	}

	// Check for field.path patterns followed by operators
	// MemQL queries have structure like "payload.field==value" or "field in (...)"
	// A literal value like "abc==def" lacks this field.path structure
	memqlOperators := []string{"==", "!=", ">", ">=", "<", "<=", " in ", " not in ", " has "}
	for _, op := range memqlOperators {
		idx := strings.Index(s, op)
		if idx > 0 {
			// Check if there's a field path pattern before the operator
			// Field paths contain dots (payload.field) or are known prefixes
			prefix := s[:idx]
			if looksLikeFieldPath(prefix) {
				return true
			}
		}
	}

	return false
}

// looksLikeFieldPath returns true if the string looks like a MemQL field path.
// Field paths are dot-separated identifiers like "payload.status" or "metadata.count".
func looksLikeFieldPath(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}

	// Contains a dot - likely a field path like "payload.field"
	if strings.Contains(s, ".") {
		return true
	}

	// Known top-level MemQL fields that appear without dots
	knownFields := []string{"id", "concept", "payload", "metadata", "createdAt", "createdBy"}
	return slices.Contains(knownFields, s)
}

// FormatValuePretty converts any value to a prettified JSON string with indentation.
// Used by the $pretty() function for human-readable JSON output.
func FormatValuePretty(v any) string {
	if v == nil {
		return ""
	}
	switch val := v.(type) {
	case string:
		return val
	case bool:
		return strconv.FormatBool(val)
	case int:
		return strconv.Itoa(val)
	case int64:
		return strconv.FormatInt(val, 10)
	case float64:
		return strconv.FormatFloat(val, 'f', -1, 64)
	case json.Number:
		return val.String()
	default:
		data, err := json.MarshalIndent(val, "", "  ")
		if err != nil {
			return fmt.Sprintf("%v", val)
		}
		return string(data)
	}
}

// compareValues checks if two values are equal.
func compareValues(a, b any) bool {
	// Handle nil
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}

	// Convert to comparable types
	aStr := FormatValue(a)
	bStr := FormatValue(b)
	return aStr == bStr
}

// isTruthy determines if a value is truthy.
func isTruthy(v any) bool {
	if v == nil {
		return false
	}
	switch val := v.(type) {
	case bool:
		return val
	case string:
		return val != "" && val != "false" && val != "0"
	case int:
		return val != 0
	case int64:
		return val != 0
	case float64:
		return val != 0
	case []any:
		return len(val) > 0
	case map[string]any:
		return len(val) > 0
	default:
		return true
	}
}

// toNumber converts a value to a float64.
func toNumber(v any) float64 {
	if v == nil {
		return 0
	}
	switch val := v.(type) {
	case int:
		return float64(val)
	case int64:
		return float64(val)
	case float64:
		return val
	case string:
		if f, err := strconv.ParseFloat(val, 64); err == nil {
			return f
		}
		return 0
	case json.Number:
		if f, err := val.Float64(); err == nil {
			return f
		}
		return 0
	default:
		return 0
	}
}

// GetLength returns the length of a collection value.
func GetLength(v any) int {
	if v == nil {
		return 0
	}
	switch val := v.(type) {
	case []any:
		return len(val)
	case []map[string]any:
		return len(val)
	case map[string]any:
		return len(val)
	case string:
		return len(val)
	default:
		// Try JSON conversion
		data, err := json.Marshal(v)
		if err != nil {
			return 0
		}
		var arr []any
		if json.Unmarshal(data, &arr) == nil {
			return len(arr)
		}
		return 0
	}
}

// ToSlice converts a value to a slice of any.
func ToSlice(v any) ([]any, error) {
	if v == nil {
		return nil, nil
	}
	switch val := v.(type) {
	case []any:
		return val, nil
	case []map[string]any:
		result := make([]any, len(val))
		for i, item := range val {
			result[i] = item
		}
		return result, nil
	default:
		// Try JSON conversion
		data, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("cannot convert %T to slice", v)
		}

		// First try direct array unmarshal
		var arr []any
		if err := json.Unmarshal(data, &arr); err == nil {
			return arr, nil
		}

		// If that fails, check for Bundle.nodes structure (MemQL ExecuteResult)
		// This handles the case where the input is a query result with structure:
		// {"Bundle":{"nodes":[...],"edges":[],"root_ids":[...]}}
		var obj map[string]any
		if err := json.Unmarshal(data, &obj); err == nil {
			if bundle, ok := obj["Bundle"].(map[string]any); ok {
				if nodes, ok := bundle["nodes"].([]any); ok {
					return nodes, nil
				}
			}
		}

		return nil, fmt.Errorf("cannot convert %T to slice", v)
	}
}

// toAnySlice converts a []any to []any (identity), needed for type assertion.
func toAnySlice(nodes []any) any {
	if nodes == nil {
		return []any{}
	}
	return nodes
}
