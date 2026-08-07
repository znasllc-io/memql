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
	"time"

	"github.com/znasllc-io/memql/component/memql"
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

// hasCustomRoot reports whether name is a root this evaluator has been seeded
// with, and can therefore resolve.
//
// Exists so a caller asking "can this evaluator resolve <root>.X?" asks the
// evaluator rather than keeping a parallel list (memql#2818). One such list in
// the logic runner had drifted -- it omitted `actor`, which newEvaluatorForLogic
// seeds -- which silently turned both shipped deploy role gates into
// deny-everyone.
func (e *Evaluator) hasCustomRoot(name string) bool {
	if e == nil || name == "" {
		return false
	}
	_, ok := e.custom[name]
	return ok
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

	// A collection-valued step result (Story 4 collection chain bound by a
	// `:=` step, #2317) is already the node list -- return it directly so the
	// step-result accessors (.count() / .first() / .empty() / .last() / .nodes)
	// work over a `active := rows.where(...)` result, not just a Bundle-wrapped
	// query result. A Bundle / *ExecuteResult keeps the envelope-aware path below.
	switch v := result.(type) {
	case []any:
		return v, true
	case []map[string]any:
		out := make([]any, len(v))
		for i := range v {
			out[i] = v[i]
		}
		return out, true
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
	// then defeats the downstream `getGA.first().id` read and feeds an
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
		// G2 (memql#2364): in an args-block automation a bare identifier
		// resolves loop-var -> step -> args field (declared-but-absent ->
		// nil) instead of falling through to the literal-string fallback.
		// Gated on the args binding G1 seeds, so args-less automations and
		// logic bodies keep prior behavior.
		if v, ok := e.resolveBareForArgsAutomation(expr); ok {
			return v, nil
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
	return e.coalesceFromArgs(args, e.evaluateCoalesceArg), nil
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

	// A dotted path rooted at a seeded/ambient root (`actor.*`, `args.*`,
	// `steps.*`, ...) is a REFERENCE, not a literal. Without this it fell
	// through to the unquoted-string case below and coalesce returned the
	// path's own SOURCE TEXT (memql#2848).
	//
	// The text is the dangerous part, not merely the wrongness: it is a
	// non-empty and therefore TRUTHY string, so `$coalesce(actor.isClusterOwner,
	// false)` read as an authorization gate is fail-OPEN -- it reports admin
	// for a caller with no AccessContext at all. The same class was fixed one
	// position at a time in #2841 and #2818; routing through
	// conditionRootSegment is what makes this position agree with the bare
	// form rather than being a fourth parallel implementation of "what is a
	// reference".
	if e.conditionRootSegment(raw) != "" {
		v, err := e.EvaluateFilterValue(raw)
		if err != nil || v == nil {
			return nil, false
		}
		return v, true
	}
	// Softness here is INHERITED from EvaluateFilterValue rather than
	// established, and it is uneven: a missing key under a map-backed root
	// (`actor.*`, `ctx.*`, `input.*`, `item.*`) resolves to nil and falls
	// through to the next argument, while an unresolved `args.` / `steps.` /
	// `var.` / `secret.` / `automation.` path returns its own text and so
	// reads as PRESENT. Measured on both sides of this change and identical,
	// so it is pre-existing and not widened here -- but it means
	// `$coalesce(var.killSwitch, false)` still yields a truthy string.
	// Filed as memql#2851; narrowing it would change what every existing
	// coalesce returns.

	// Treat as an unquoted string literal.
	if strings.TrimSpace(raw) == "" {
		return nil, false
	}
	return raw, true
}

// splitCoalesceArgs splits `??` / $coalesce() arguments on top-level commas.
//
// Escape state is TRACKED, never inferred from the preceding byte
// (memql#3046). The previous `(i == 0 || s[i-1] != '\\')` lookback could not
// tell an escaped quote from a quote following a COMPLETED `\\` escape, so a
// literal ending in a backslash pair never left quote state and consumed the
// rest of the argument list -- surfacing here as "unterminated quote" at
// automation-condition EVALUATION time, i.e. during a live run rather than at
// boot. Same defect as splitTopLevelArgs and as the one memql#2949 fixed in
// args_resolution.go.
//
// This one does NOT reuse the parser package's blankCommentsAndStrings, and
// neither does splitTopLevelArgs -- a draft of this change did, and the reason
// it was reverted is recorded on that function. There are two independent
// reasons not to, one per site:
//
//   - Here: the blanker handles `"` only, and this splitter accepts BOTH `"`
//     and `'`. Routing single-quoted literals through it would silently stop
//     treating them as strings.
//   - There: the blanker ended a string at a NEWLINE, which the MemQL lexer
//     does not do -- it accepts a literal spanning lines. Delegating swallowed
//     every comma after such a literal, which is memql#3046 in the other
//     direction. Fixed since, in memql#3116; memql#3190 then answered the
//     delegation question for that site -- still no, on the two grounds
//     recorded there (it slices the original, and delegating would newly blank
//     comments inside argument lists).
//
// The reason HERE is unaffected by that fix and still stands on its own: the
// shared blanker knows one quote character and this splitter accepts two.
// Widening it was the other option and is deliberately not taken -- it backs a
// shipped load rule whose behaviour is pinned by its own tests, and changing
// what it considers a string is a bigger change than this bug fix.
func splitCoalesceArgs(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}

	var args []string
	var current strings.Builder

	inQuote := false
	quoteChar := byte(0)
	escaped := false
	depth := 0

	for i := 0; i < len(s); i++ {
		ch := s[i]

		if inQuote {
			current.WriteByte(ch)
			switch {
			case escaped:
				// This byte was escaped by the backslash before it; the
				// escape is now spent. A `\\` pair therefore leaves escaped
				// false, so the NEXT quote closes the literal.
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == quoteChar:
				inQuote = false
				quoteChar = 0
			}
			continue
		}

		switch ch {
		case '"', '\'':
			inQuote = true
			quoteChar = ch
			escaped = false
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

// explicitPathRoots is THE list of path roots the evaluator resolves through
// resolvePath's root switch, shared so it cannot be reimplemented per call site
// (memql#2851).
//
// It was already duplicated: conditionRootSegment listed all ten, while
// logic_runner's isCustomVarRoot listed only args/event/ctx/input/item. That
// divergence is exactly why `$coalesce(var.missing, "FB")` and the logic-body
// `var.missing ?? "FB"` behaved DIFFERENTLY -- the second fell through to a
// step-reference lookup, which hands back the raw path text, so coalesce read
// it as present and skipped the fallback.
//
// `event` is deliberately NOT here: conditionRootSegment excludes it (the
// `event.`-prefix retry below owns that root) while isCustomVarRoot includes
// it, so each keeps that rule locally rather than forcing one list to carry a
// caveat.
var explicitPathRoots = map[string]bool{
	"args": true, "ctx": true, "input": true, "item": true, "steps": true,
	"var": true, "systemVar": true, "secret": true, "systemSecret": true,
	"automation": true,
}

func (e *Evaluator) conditionRootSegment(expr string) string {
	dot := strings.Index(expr, ".")
	if dot <= 0 {
		return ""
	}
	root := expr[:dot]
	if explicitPathRoots[root] {
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

	// timestamp() -- the current evaluation time as an RFC3339 string. The
	// executor / logic-runner seeds a stable "timestamp" custom value per run;
	// prefer it so every comparison in one pass reads a single clock, and fall
	// back to now when unseeded. Without this a date-window gate like
	// `addDuration(...) < timestamp()` could not resolve its right operand
	// (#2256).
	if expr == "timestamp()" {
		return e.evaluationClock(), nil
	}

	// Builtin calls usable as comparison operands (#2256): concat / coalesce
	// plus the date/duration builtin set (#2541 -- addDuration and
	// daysBetween since the #2707 retirement of the seven zero-use calendar
	// builtins). Before this they fell through to the literal-string
	// fallback at the bottom of this function, and a date-arithmetic
	// expression then toNumber()'d to 0 -- making every retention/deletion
	// date-window gate constant-false (#2254). An unrecognised call name
	// falls through unchanged.
	if name, builtinArgs, ok := matchBuiltinCall(expr); ok {
		switch {
		case name == "concat":
			return e.evaluateConcatArgs(builtinArgs)
		case name == "coalesce":
			return e.evaluateBareCoalesce(builtinArgs)
		case memql.IsDateBuiltin(name):
			return e.evaluateDateBuiltinCall(name, builtinArgs)
		case IsStringBuiltin(name):
			// #2656: lower/upper/trim/hash/shortId reached the
			// literal-string fallback below, so `lower(args.x) == "y"`
			// compared the TEXT `lower(args.x)` against "y" and was
			// always-false -- load-green, evaluate-wrong, exactly the
			// class the date-builtin case above was added to close.
			// (concat was already handled; the rest were not.)
			return e.evaluateStringBuiltinCall(name, builtinArgs)
		}
	}

	// Bare step-variable reference: a single identifier (no dot) that names a
	// recorded step result. A logic body's guard like `if toStatus == "queued"`
	// (recordTransition, where `toStatus := coalesce(...)` is a prior step)
	// reaches here as the bare operand `toStatus`. Without this it fails
	// looksLikePath (no dot), falls through to the literal-string fallback, and
	// the comparison tests the path TEXT ("toStatus" != "queued") -- so every
	// `toStatus`-guarded recordRequestEvent step was skipped (the other
	// half of #1847). Scoped strictly to recorded step ids so a genuine string
	// literal (`"owner"`, `submitted`) is never swallowed.
	if isBareStepIdentifier(expr) {
		if _, known := e.steps[expr]; known {
			if val, err := e.resolvePath("steps." + expr + ".result"); err == nil {
				return val, nil
			}
		}
		// G2 (memql#2364): bare args-field / loop-var operand in an
		// args-block automation's condition or filter. Gated on the G1 args
		// binding; args-less behavior (literal fall-through) is unchanged.
		if v, ok := e.resolveBareForArgsAutomation(expr); ok {
			return v, nil
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
		// in a logic body invoked as an automation step (routeRequest's
		// fast-track / approval / validation transitions, recordTransition's
		// whole body) was silently skipped while the UNGUARDED steps ran. Resolve
		// these against resolvePath's root switch directly (the same path
		// `$<root>.X` takes). `event.X` keeps its existing pass-through below.
		if e.conditionRootSegment(expr) != "" {
			if val, err := e.resolvePath(expr); err == nil {
				return val, nil
			}
			// UNRESOLVED means ABSENT, not "the literal text of the path"
			// (memql#2851).
			//
			// This used to `return expr, nil`, and the returned text is
			// non-empty and therefore TRUTHY -- the #2380 hazard. Two
			// consequences, both silent:
			//
			//   - coalesce read the text as a PRESENT value, so its fallback
			//     was never reached. `enabled := var.killSwitch ?? false`
			//     yielded "var.killSwitch": a kill switch written to be safe
			//     when its variable is missing was ON instead. Fail-OPEN.
			//   - in a predicate the same text admits, for the same reason.
			//
			// The softness was ROOT-DEPENDENT, which is what made it hard to
			// see: resolvePath returns (nil, nil) for a missing key in a
			// map-backed root, so `actor.*` / `ctx.*` / `item.*` and a seeded
			// `args.*` were already soft and behaved correctly. Only the
			// resolver-backed roots (var / systemVar / secret / systemSecret /
			// automation) and an unresolvable `steps.` sub-path took this
			// branch. One spelling of coalesce therefore worked and another
			// did not, depending on the root -- see
			// coalesce_root_softness_test.go for the enumerated matrix.
			//
			// Scope of the change: this branch is reached ONLY for a path
			// whose first segment conditionRootSegment recognises as an
			// explicit root. A dotted token that is not one of those --
			// "example.com", "v1.2.3", a concept id -- never enters here and
			// still passes through as a literal, which
			// TestNonPathLiteralsStillPassThrough pins. So nothing that was
			// legitimately a literal becomes nil; only paths that name a real
			// root and fail to resolve do.
			return nil, nil
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

// matchBuiltinCall reports whether expr is a single top-level function call
// `<ident>(<args>)` -- the opening paren follows a bare identifier, the closing
// paren is the final character, and the parens inside the args are balanced.
// Used to dispatch builtin calls that appear as a comparison operand in a
// condition / filter context (#2256). Returns the function name and the raw
// (unsplit) argument text.
func matchBuiltinCall(expr string) (name, args string, ok bool) {
	expr = strings.TrimSpace(expr)
	open := strings.IndexByte(expr, '(')
	if open <= 0 || !strings.HasSuffix(expr, ")") {
		return "", "", false
	}
	name = expr[:open]
	for i, r := range name {
		switch {
		case r == '_':
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return "", "", false
		}
	}
	args = expr[open+1 : len(expr)-1]
	depth := 0
	for _, r := range args {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return "", "", false
			}
		}
	}
	if depth != 0 {
		return "", "", false
	}
	return name, strings.TrimSpace(args), true
}

// evaluateOperand resolves a builtin-call argument: a quoted string literal is
// unquoted to its value, everything else routes through EvaluateFilterValue.
// EvaluateFilterValue does not strip surrounding quotes (it treats an unknown
// token as a literal verbatim), so addDuration("...", "P30D") needs this to
// hand the duration parser `P30D` rather than `"P30D"` (#2256).
func (e *Evaluator) evaluateOperand(raw string) (any, error) {
	raw = strings.TrimSpace(raw)
	if v, ok, err := parseQuotedStringLiteral(raw); err == nil && ok {
		return v, nil
	}
	return e.EvaluateFilterValue(raw)
}

// evaluateConcatArgs evaluates a concat(a, b, ...) call: each argument is
// resolved through evaluateOperand, string-formatted, and joined (#2256).
func (e *Evaluator) evaluateConcatArgs(argsRaw string) (any, error) {
	parts, err := splitCoalesceArgs(argsRaw)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	for _, raw := range parts {
		val, err := e.evaluateOperand(raw)
		if err != nil {
			return nil, fmt.Errorf("concat() argument %q: %w", raw, err)
		}
		if val == nil {
			continue
		}
		b.WriteString(FormatValue(val))
	}
	return b.String(), nil
}

// evaluateBareCoalesce evaluates a bare coalesce(a, b, ...) call (the
// no-`$`-prefix form that appears as a condition operand), reusing the same
// first-non-empty + final-fallback semantics as $coalesce() (#1614, #2256).
func (e *Evaluator) evaluateBareCoalesce(argsRaw string) (any, error) {
	args, err := splitCoalesceArgs(argsRaw)
	if err != nil {
		return nil, err
	}
	return e.coalesceFromArgs(args, e.coalesceBareArg), nil
}

// coalesceBareArg resolves a bare-form coalesce argument: a quoted literal,
// numeric, or a bare field path (resolved through EvaluateFilterValue). A nil
// resolution (absent field) counts as "missing" so coalesce falls through --
// the bare condition form (`coalesce(item.payload.x, "30")`) needs path
// resolution that evaluateCoalesceArg (the $-form helper) does not do (#2256).
func (e *Evaluator) coalesceBareArg(raw string) (any, bool) {
	val, err := e.evaluateOperand(raw)
	if err != nil || val == nil {
		return nil, false
	}
	return val, true
}

// evaluateDateBuiltinCall evaluates one date/duration builtin call (#2256
// addDuration, generalised to the full set by #2541): each operand is
// resolved through evaluateDateOperand, then the shared name-keyed evaluator
// (memql.EvaluateDateBuiltin) applies the builtin -- the same implementations
// the mutation-template path runs, so a `daysBetween(...)` computes
// identically in a condition operand, a logic step value, and an insert arg.
func (e *Evaluator) evaluateDateBuiltinCall(name, argsRaw string) (any, error) {
	args, err := splitCoalesceArgs(argsRaw)
	if err != nil {
		return nil, err
	}
	vals := make([]any, len(args))
	for i, raw := range args {
		v, err := e.evaluateDateOperand(raw)
		if err != nil {
			return nil, fmt.Errorf("%s() arg %d: %w", name, i, err)
		}
		vals[i] = v
	}
	return memql.EvaluateDateBuiltin(name, vals)
}

// stringBuiltinArity is the set of pure string/value builtins the logic
// runner evaluates locally, with their argument counts (#2656). Before
// this set existed, `lower(args.x) == "y"` in a cond predicate loaded
// GREEN and evaluated always-false: isPositionalBuiltinName admitted
// only coalesce/cond/date builtins, so the local path never resolved
// the call and the comparison could never match -- the same
// load-green/runtime-wrong class #2612 closed for equality shapes, one
// builtin-set over. -1 means variadic.
var stringBuiltinArity = map[string]int{
	"lower":   1,
	"upper":   1,
	"trim":    1,
	"hash":    1,
	"shortId": 1,
	// concat is dispatched by its own case in evaluateOperand (it predates
	// this set); listed here so the arity check and the local-evaluator
	// switch stay complete for callers that route through this path.
	"concat": -1,
}

// IsStringBuiltin reports whether name is a locally-evaluable string
// builtin (#2656).
func IsStringBuiltin(name string) bool {
	_, ok := stringBuiltinArity[name]
	return ok
}

// evaluateStringBuiltinCall resolves a string builtin over locally
// resolved operands, mirroring evaluateDateBuiltinCall. The semantics
// come from memql.RuntimeEvaluator so the logic-body path and the
// mutation-template path cannot diverge on what lower()/concat()/hash()
// mean.
func (e *Evaluator) evaluateStringBuiltinCall(name, argsRaw string) (any, error) {
	arity, ok := stringBuiltinArity[name]
	if !ok {
		return nil, fmt.Errorf("%s() is not a string builtin", name)
	}
	args, err := splitCoalesceArgs(argsRaw)
	if err != nil {
		return nil, err
	}
	if arity >= 0 && len(args) != arity {
		return nil, fmt.Errorf("%s() requires %d argument(s), got %d", name, arity, len(args))
	}
	vals := make([]any, len(args))
	for i, raw := range args {
		v, verr := e.evaluateOperand(raw)
		if verr != nil {
			return nil, fmt.Errorf("%s() arg %d: %w", name, i, verr)
		}
		vals[i] = v
	}
	re := &memql.RuntimeEvaluator{}
	switch name {
	case "lower":
		return re.EvaluateLower(vals[0]), nil
	case "upper":
		return re.EvaluateUpper(vals[0]), nil
	case "trim":
		return re.EvaluateTrim(vals[0]), nil
	case "hash":
		return re.EvaluateHash(vals[0]), nil
	case "shortId":
		return re.EvaluateShortId(vals[0]), nil
	case "concat":
		return re.EvaluateConcat(vals...), nil
	}
	return nil, fmt.Errorf("string builtin %q has no local evaluator", name)
}

// evaluateDateOperand resolves a single date-builtin argument. The bare
// reserved identifier `now` resolves to the evaluation clock -- the compiler
// leaves `now` verbatim inside condition strings and raw arg text, so without
// this it would reach the date parsers as the literal text "now". Everything
// else routes through evaluateOperand (quoted literals, paths, timestamp(),
// nested builtin calls).
func (e *Evaluator) evaluateDateOperand(raw string) (any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "now" {
		return e.evaluationClock(), nil
	}
	return e.evaluateOperand(raw)
}

// evaluationClock returns the per-run clock: the executor / logic-runner
// seeds a stable "timestamp" custom value per pass so every comparison reads
// a single clock; unseeded evaluators fall back to the current UTC time.
func (e *Evaluator) evaluationClock() any {
	if v, ok := e.custom["timestamp"]; ok {
		return v
	}
	return time.Now().UTC().Format(time.RFC3339)
}

// TryEvaluateDateBuiltin evaluates expr when it is a single top-level
// date/duration builtin call (#2541). Returns (value, true, nil) on success,
// (nil, false, nil) when expr is not a date-builtin call (the caller keeps
// its normal resolution), and (nil, false, err) on an evaluation error.
// Exported for the arg-time resolver (steps.MutationExecutor.evaluateValue)
// so mutation/function step args evaluate the same builtin set the
// logic-time path does.
func (e *Evaluator) TryEvaluateDateBuiltin(expr string) (any, bool, error) {
	name, argsRaw, ok := matchBuiltinCall(expr)
	if !ok || !memql.IsDateBuiltin(name) {
		return nil, false, nil
	}
	val, err := e.evaluateDateBuiltinCall(name, argsRaw)
	if err != nil {
		return nil, false, err
	}
	return val, true, nil
}

// coalesceFromArgs runs the shared coalesce selection over already-split args:
// the first non-nil / non-empty argument wins, EXCEPT the final argument is the
// ultimate fallback and is returned even when it resolves to "" (#1614) -- so
// coalesce(absent, "") -> "" while coalesce("", fallback) falls through.
func (e *Evaluator) coalesceFromArgs(args []string, evalArg func(string) (any, bool)) any {
	if len(args) == 0 {
		return nil
	}
	for i, raw := range args {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		val, ok := evalArg(raw)
		if !ok {
			continue
		}
		if i == len(args)-1 {
			return val
		}
		if val == nil {
			continue
		}
		if s, isString := val.(string); isString && strings.TrimSpace(s) == "" {
			continue
		}
		return val
	}
	return nil
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

	// Story 4 (#2302 / ADR §2.2): a forEach source may be a collection-method
	// chain (`args.members.where(m => m.active)`). Detect it cheaply (legacy
	// step/path sources never carry `=>` or a `.<collectionMethod>(` call),
	// then resolve the chain's base receiver through the standard evaluator
	// and run the in-memory method chain over it.
	if memql.LooksLikeCollectionChain(expr) {
		val, isChain, err := memql.EvaluateCollectionChainString(
			expr,
			e.resolveCollectionBase,
			e.collectionLambdaArgs(),
		)
		if isChain {
			return val, err
		}
	}

	// If already a $ expression, use standard evaluation
	if strings.HasPrefix(expr, "$") {
		return e.EvaluateValue(expr)
	}

	// Normalise method-call syntax for step-result shorthand accessors
	// (`.nodes()`, `.first()`, `.count()`, `.empty()`, `.last()`, `.Ran()`).
	// These can legally appear with or without parentheses (see the
	// comment near `case "nodes":` in resolvePath; Story 5 / #2303
	// retired the capitalized aliases except `.Ran()`). We strip a
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
	// plan-less agent create and then called updatePlanStatus with
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

	// A bare dotted path that does NOT resolve renders as its own path text,
	// and a non-empty string is truthy -- so the condition is unconditionally
	// TRUE (memql#2819). `@filter(actor.isClusterOwner)` matched every event
	// even with the denying envelope bound, while both comparison spellings
	// correctly read false:
	//
	//	@filter(actor.isClusterOwner)          -> was TRUE  (fail-open)
	//	@filter(actor.isClusterOwner == true)  -> false
	//	@filter(actor.isClusterOwner != false) -> false
	//
	// #2801 could not reach this: it makes an absent actor DENY, but the
	// truthiness is decided on the rendered string before any resolved value
	// participates, so binding an envelope changes nothing. Same class as the
	// exists() fall-through above (memql#1396), and not specific to actor --
	// ANY unresolved bare path was unconditionally true.
	//
	// Erroring rather than returning false is deliberate: a condition on a
	// path that does not resolve is an authoring mistake, and a silent false
	// would trade a filter that always fires for one that never fires --
	// equally wrong and harder to notice. Matches the strict-boot posture.
	// Two distinct defects met here, and both had to be fixed to close it.
	// EvaluateValue below does not resolve the ambient roots -- that is
	// EvaluateFilterValue's job, which is why the comparison spellings worked
	// and this one did not. So a bare path is resolved through the SAME
	// resolver the comparison operands use, and its resolved value decides
	// the truthiness; only a path that genuinely does not resolve errors.
	if isBareDottedPath(condition) {
		resolved, ok, rerr := e.resolveBarePathOnce(condition)
		if rerr != nil || !ok {
			return false, fmt.Errorf("condition %q is a bare path that does not resolve: it renders as its own text, and a non-empty string is truthy, so this filter would match EVERYTHING. Write the comparison explicitly (%s == true) or use exists(%s)",
				condition, condition, condition)
		}
		return memql.IsTruthy(resolved), nil
	}

	// Try to evaluate as a truthy value
	val, err := e.EvaluateValue(condition)
	if err != nil {
		return false, err
	}
	return memql.IsTruthy(val), nil
}

// bareDottedPathRe matches a dotted reference and nothing else -- no
// operators, calls or quotes. Single identifiers are deliberately excluded: a
// bare `toStatus` is the legitimate bare-step-variable shape (see
// isBareStepIdentifier), and treating it as an unresolved path would reject
// working automations.
//
// Whitespace around the dots is tolerated because a condition does not always
// reach here as the author typed it: the `forEach ... where` parser
// (component/language/parser/parser.go) rebuilds the clause with
// strings.Join(parts, " "), so `where a.b` arrives as `a . b`. An anchored
// no-space pattern silently failed to match that spelling, leaving the exact
// fail-open this guard exists to close alive on that one path.
var bareDottedPathRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:[ \t]*\.[ \t]*[A-Za-z_][A-Za-z0-9_]*)+$`)

func isBareDottedPath(condition string) bool {
	return bareDottedPathRe.MatchString(strings.TrimSpace(condition))
}

// renderedAsOwnText reports whether resolution handed back the expression
// itself -- the signature of a path that did not resolve, for the roots that
// still fall back to returning their input rather than nil.
//
// It is no longer sufficient on its own: an EXPLICIT-root path now returns nil
// when it does not resolve (memql#2851), and nil is not a string, so this
// sniff can never fire for those. explicitRootDidNotResolve is the companion
// that keeps memql#2819's fail-loud posture intact for them.
func renderedAsOwnText(condition string, resolved any) bool {
	s, ok := resolved.(string)
	return ok && strings.TrimSpace(s) == strings.TrimSpace(condition)
}

// explicitRootDidNotResolve reports whether condition is an explicit-root path
// whose resolution FAILED, as opposed to one that resolved to a nil/absent
// value.
//
// This exists because #2851 and #2819 pull in opposite directions and both are
// right. #2851 wants an unresolved path to be ABSENT so a coalesce fallback is
// reached. #2819 wants an unresolved BARE-PATH CONDITION to be a LOUD ERROR --
// its test says so explicitly: "a silent false would trade a filter that always
// fires for one that never fires -- equally wrong and harder to notice."
//
// Returning nil satisfied the first and silently broke the second, because
// #2819's guard detects non-resolution by sniffing for the path's own text.
// Measured: `@filter(var.killSwitch)` went from a boot-time error naming the
// trap to a silent false, so a typo'd variable produced a never-firing gate.
//
// The distinction the two need is resolvability, not the value, so the guard
// asks resolvePath directly. Note this deliberately does NOT fire for a
// map-backed root: resolvePath returns (nil, nil) for a missing key, so
// `actor.nonexistent` and `args.missing` stay silent -- exactly as they were
// before #2851, since #2819's text sniff never caught them either. Widening to
// those is a real question, but it is #2819's to answer, not a side effect of
// this change.
// resolveBarePathOnce resolves a bare-path condition and reports whether it
// RESOLVED, in a single pass.
//
// Single pass is the point. The first cut asked EvaluateFilterValue for the
// value and then asked resolvePath again for the resolvability, which was
// wrong twice over (memql#2851 review):
//
//   - COST. resolvePath invokes the variable/secret resolvers with a real
//     context and no caching at that site, so every `@filter(secret.X)` did
//     TWO secret-store reads -- double the audit trail and double the KMS/DB
//     hit. `||` short-circuits after success, so the happy path paid it too.
//   - CORRECTNESS. The two reads are independent and can disagree. With a
//     transient store error between them, "call 1 OK, call 2 fails" hard-errors
//     a condition that actually resolved, and "call 1 fails, call 2 OK" returns
//     a silent false -- which is #2819's fail-quiet reintroduced by the very
//     guard added to prevent it. Neither is reachable when you resolve once.
//
// For an EXPLICIT root, resolvability is exactly "resolvePath did not error";
// that is the signal #2819 needs and #2851 took away by returning nil. For any
// other shape the older text sniff still applies, since those resolvers do
// still hand back their input.
func (e *Evaluator) resolveBarePathOnce(condition string) (any, bool, error) {
	condition = strings.TrimSpace(condition)
	// looksLikePath as well as the root check. isBareDottedPath tolerates
	// whitespace around the dots (the `forEach ... where` rebuilder emits
	// `a . b`), but EvaluateFilterValue gates its explicit-root branch on
	// looksLikePath, whose isPathChar excludes space. Calling resolvePath
	// without that gate let a space-bearing spelling through that
	// EvaluateFilterValue would have treated as a literal -- and for a
	// map-backed root the space-bearing key is a quiet MISS, so
	// `@filter(args. typo)` went from a loud error to a silently never-firing
	// filter. Same fail-quiet class this whole change exists to close, so it is
	// closed here rather than filed. (Asymmetric spacing only; the symmetric
	// `a . b` the rebuilder actually emits was never affected, and the spelling
	// has zero hits in dsl/.)
	if e.conditionRootSegment(condition) != "" && looksLikePath(condition) {
		val, err := e.resolvePath(condition)
		if err != nil {
			// Unresolved. Deliberately NOT an error return: the caller owns
			// the diagnostic, and a map-backed root reaching here with
			// (nil, nil) is a resolved ABSENCE, which stays quiet.
			return nil, false, nil
		}
		return val, true, nil
	}
	val, err := e.EvaluateFilterValue(condition)
	return val, !renderedAsOwnText(condition, val), err
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

	var right any
	// Quoted string literal -- captured here and used verbatim, INCLUDING the
	// empty string. Letting `""` fall through to parseUnquotedLiteral /
	// EvaluateFilterValue collapsed it to nil, and compareValues(value, nil) is
	// never equal, so `X != ""` spuriously fired on empty-string rows -- the
	// #2257 kill-switch over-suspend. Keeping "" a real string is the fix.
	if s, ok, err := parseQuotedStringLiteral(rightRaw); err != nil {
		return false, err
	} else if ok {
		right = s
	} else if lit, ok := parseUnquotedLiteral(rightRaw); ok {
		// Typed literals: null/true/false/numbers.
		right = lit
	} else {
		// Use EvaluateFilterValue for right side too (could be a path reference).
		// EvaluateFilterValue returns strings for unknown tokens; only
		// null/true/false/numbers are treated as typed literals above.
		val, err := e.EvaluateFilterValue(rightRaw)
		if err != nil {
			// Treat as literal string
			right = rightRaw
		} else {
			right = val
		}
	}

	// Empty / missing equivalence (#2257): a missing field resolves to nil and
	// an empty field to ""; compared against an empty-string literal these must
	// be equal, else `X != ""` over-fires on empty/absent rows (and `X == ""`
	// under-fires). Only the nil<->"" gap is collapsed; non-empty operands are
	// unaffected.
	equal := compareValues(left, right) || (isEmptyish(left) && isEmptyish(right))
	if op == "==" {
		return equal, nil
	}
	return !equal, nil
}

// isEmptyish reports whether v is the "empty" sentinel for equality purposes:
// nil, or a string that is empty / whitespace-only. Lets a missing field (nil)
// and an empty-string field both compare equal to an empty-string literal
// (#2257).
func isEmptyish(v any) bool {
	if v == nil {
		return true
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s) == ""
	}
	return false
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

	return compareOrdered(left, right, op)
}

// compareOrdered evaluates an ordering comparison (> < >= <=). It tries, in
// order: a numeric comparison (both operands numeric), an RFC3339 / date
// timestamp comparison (both operands non-numeric date strings), and finally a
// lexicographic string comparison. Before this, ordering coerced every operand
// through toNumber(), which returns 0 for any non-numeric string (RFC3339 dates
// included), so a window gate like `addDuration(...) < timestamp()` was
// `0 < 0` -> constant-false and three retention/deletion crons silently never
// fired (#2254).
func compareOrdered(left, right any, op string) (bool, error) {
	if lNum, lOK := asNumber(left); lOK {
		if rNum, rOK := asNumber(right); rOK {
			return applyOrder(lNum, rNum, op)
		}
	}

	leftStr, leftIsStr := left.(string)
	rightStr, rightIsStr := right.(string)
	if leftIsStr && rightIsStr {
		if lt, lerr := memql.ParseFlexibleTimestamp(leftStr); lerr == nil {
			if rt, rerr := memql.ParseFlexibleTimestamp(rightStr); rerr == nil {
				switch op {
				case ">":
					return lt.After(rt), nil
				case "<":
					return lt.Before(rt), nil
				case ">=":
					return !lt.Before(rt), nil
				case "<=":
					return !lt.After(rt), nil
				}
			}
		}
		return applyOrder2(leftStr, rightStr, op)
	}

	// Mixed / non-comparable operands: keep the legacy numeric coercion so a
	// numeric-vs-stringified-number comparison still behaves as before.
	return applyOrder(toNumber(left), toNumber(right), op)
}

// asNumber reports whether v is (or parses as) a number, returning its float64
// value. A non-numeric string (e.g. an RFC3339 date) returns ok=false so the
// caller can fall back to a timestamp / lexicographic comparison (#2254).
func asNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case json.Number:
		if f, err := n.Float64(); err == nil {
			return f, true
		}
		return 0, false
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil {
			return f, true
		}
		return 0, false
	default:
		return 0, false
	}
}

func applyOrder(l, r float64, op string) (bool, error) {
	switch op {
	case ">":
		return l > r, nil
	case "<":
		return l < r, nil
	case ">=":
		return l >= r, nil
	case "<=":
		return l <= r, nil
	default:
		return false, fmt.Errorf("unknown operator: %s", op)
	}
}

func applyOrder2(l, r, op string) (bool, error) {
	switch op {
	case ">":
		return l > r, nil
	case "<":
		return l < r, nil
	case ">=":
		return l >= r, nil
	case "<=":
		return l <= r, nil
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
		// Result shorthand accessors. The canonical surface is the
		// lowercase field-style spelling (.count, .empty, .first,
		// .last, .nodes). Story 5 (ADR §2.2 / #2303) retired the
		// legacy capitalized Go-style aliases (.Len, .Empty, .First,
		// .Last, .Nodes) that were recognised here during the
		// language-improvements grace period; only the lowercase
		// forms resolve now.
		//
		// .Ran() is the one capitalized accessor that survives: it has
		// no lowercase pair and is a distinct accessor (not part of the
		// retired result-set surface). It disambiguates "step skipped
		// (condition evaluated to false, step never executed)" from
		// "step ran but produced zero results". With only .empty, those
		// two states collapsed, which was a real bug-farm.
		if len(parts) > 2 {
			switch parts[2] {
			case "count":
				nodes, _ := e.GetStepNodes(stepId)
				current = len(nodes)
				parts = parts[3:]
				goto navigate
			case "nodes":
				nodes, _ := e.GetStepNodes(stepId)
				current = toAnySlice(nodes)
				parts = parts[3:]
				goto navigate
			case "empty":
				nodes, _ := e.GetStepNodes(stepId)
				current = len(nodes) == 0
				parts = parts[3:]
				goto navigate
			case "first":
				nodes, _ := e.GetStepNodes(stepId)
				if len(nodes) > 0 {
					current = nodes[0]
				} else {
					current = nil
				}
				parts = parts[3:]
				goto navigate
			case "last":
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
				// Peel the Bundle-wrapped engine envelope so a returned
				// object-literal field (decide.result.x) reads the flat value
				// the construct returned, not the opaque envelope. #2271.
				current = UnwrapStepResult(stepResult.Result)
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
