package memql

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/baseparser"
)

// FunctionMutationTemplate is a compiled representation of a mutation function body.
// It is evaluated at execution time using function call args and engine variable lookup.
type FunctionMutationTemplate struct {
	// Kind = "insert" (full payload) or "update" (read-merge-validate-write).
	// Empty defaults to insert for backwards compatibility with templates
	// compiled before update() landed.
	Kind ast.MutationKind

	Concept string

	// IDTemplate is an optional template for the explicit ID.
	// If omitted or evaluates to empty, the engine will generate an ID (content-addressed).
	IDTemplate any

	// CreatedAtTemplate is an optional timestamp override for node creation time.
	// If provided, it must evaluate to an RFC3339/RFC3339Nano timestamp string.
	CreatedAtTemplate any

	// PayloadTemplate is the payload template (required).
	// It must evaluate to an object (map[string]any) at execution time.
	PayloadTemplate any

	// ParentTemplate is an optional parent relationship hint.
	ParentTemplate any

	// AliasOfTemplate is an optional aliasOf relationship hint.
	AliasOfTemplate any
}

// renderMutationTemplate evaluates a mutation template into a concrete MutationNode.
func (e *MemQLEngine) renderMutationTemplate(ctx context.Context, tmpl *FunctionMutationTemplate, args map[string]any) (MutationNode, error) {
	if e == nil {
		return MutationNode{}, fmt.Errorf("engine is nil")
	}
	if tmpl == nil {
		return MutationNode{}, fmt.Errorf("mutation template is nil")
	}
	concept := strings.TrimSpace(tmpl.Concept)
	if concept == "" {
		return MutationNode{}, fmt.Errorf("mutation template concept is required")
	}

	eval := &mutationTemplateEvaluator{
		engine: e,
		args:   args,
		now:    time.Now().UTC(),
	}

	// Evaluate ID (optional)
	id, err := eval.evalStringMaybe(ctx, tmpl.IDTemplate)
	if err != nil {
		return MutationNode{}, fmt.Errorf("evaluate id: %w", err)
	}

	// Evaluate createdAt (optional)
	createdAtRaw, err := eval.evalStringMaybe(ctx, tmpl.CreatedAtTemplate)
	if err != nil {
		return MutationNode{}, fmt.Errorf("evaluate createdAt: %w", err)
	}
	var createdAtRef *time.Time
	if strings.TrimSpace(createdAtRaw) != "" {
		ts, err := parseRFC3339Timestamp(createdAtRaw)
		if err != nil {
			return MutationNode{}, fmt.Errorf("createdAt must be RFC3339/RFC3339Nano: %w", err)
		}
		createdAtRef = &ts
	}

	// Evaluate payload (required)
	payloadAny, err := eval.evalValue(ctx, tmpl.PayloadTemplate)
	if err != nil {
		return MutationNode{}, fmt.Errorf("evaluate payload: %w", err)
	}
	payloadMap, ok := payloadAny.(map[string]any)
	if !ok || payloadMap == nil {
		return MutationNode{}, fmt.Errorf("payload must evaluate to an object")
	}

	// Auto-canonicalize @relationship payload fields. Every concept's
	// outgoing relationships (foreign-key fields like
	// participant.userId -> v1:identity:user) get rewritten to
	// canonical `<partition>:<targetConcept>:<bareSlug>` form before
	// the payload hits the database.
	//
	// Why insert-time, not on read: `payload.userId == arg(...)` and
	// `id == arg(...)` lookups operate on the stored bytes. Two callers
	// inserting the same logical reference under different shapes
	// ("user-abc" vs canonical) would otherwise produce two distinct
	// stored values that don't match each other under `==`. Collapsing
	// to canonical at insert eliminates the class entirely without
	// touching every query site.
	if err := e.canonicalizeRelationshipFields(ctx, concept, payloadMap); err != nil {
		return MutationNode{}, fmt.Errorf("canonicalize relationship fields: %w", err)
	}

	payloadJSON, err := json.Marshal(payloadMap)
	if err != nil {
		return MutationNode{}, fmt.Errorf("marshal payload: %w", err)
	}

	// Evaluate relationship hints (optional)
	parent, err := eval.evalStringMaybe(ctx, tmpl.ParentTemplate)
	if err != nil {
		return MutationNode{}, fmt.Errorf("evaluate parent: %w", err)
	}
	aliasOf, err := eval.evalStringMaybe(ctx, tmpl.AliasOfTemplate)
	if err != nil {
		return MutationNode{}, fmt.Errorf("evaluate aliasOf: %w", err)
	}

	var parentRef *string
	if strings.TrimSpace(parent) != "" {
		copy := parent
		parentRef = &copy
	}

	var aliasRef *string
	if strings.TrimSpace(aliasOf) != "" {
		copy := aliasOf
		aliasRef = &copy
	}

	kind := tmpl.Kind
	if kind == "" {
		kind = ast.MutationKindInsert
	}

	return MutationNode{
		Kind:       kind,
		Concept:    concept,
		ID:         strings.TrimSpace(id),
		PayloadRaw: string(payloadJSON),
		CreatedAt:  createdAtRef,
		ParentRef:  parentRef,
		AliasOfRef: aliasRef,
	}, nil
}

func parseRFC3339Timestamp(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, fmt.Errorf("timestamp is empty")
	}
	if ts, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return ts.UTC(), nil
	}
	ts, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, err
	}
	return ts.UTC(), nil
}

type mutationTemplateEvaluator struct {
	engine *MemQLEngine
	args   map[string]any
	now    time.Time
}

// missingValue is a sentinel used to distinguish a missing optional argument
// from an explicit null value. Missing args are omitted from payload objects.
type missingValue struct{}

func isMissing(v any) bool {
	_, ok := v.(missingValue)
	return ok
}

func (e *mutationTemplateEvaluator) evalStringMaybe(ctx context.Context, v any) (string, error) {
	if v == nil {
		return "", nil
	}
	value, err := e.evalValue(ctx, v)
	if err != nil {
		return "", err
	}
	if value == nil {
		return "", nil
	}
	if isMissing(value) {
		return "", nil
	}
	switch t := value.(type) {
	case string:
		return t, nil
	default:
		return fmt.Sprintf("%v", t), nil
	}
}

func (e *mutationTemplateEvaluator) evalValue(ctx context.Context, v any) (any, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case bool, int, int64, float64:
		return t, nil
	case json.Number:
		// Normalize json.Number to int64/float64 when possible.
		if i, err := t.Int64(); err == nil {
			return i, nil
		}
		if f, err := t.Float64(); err == nil {
			return f, nil
		}
		return t.String(), nil
	case string:
		return e.evalString(ctx, t)
	case languageParser.ExpressionNode:
		return e.evalParserExpression(ctx, t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, v2 := range t {
			ev, err := e.evalValue(ctx, v2)
			if err != nil {
				return nil, fmt.Errorf("evaluate %q: %w", k, err)
			}
			// Omit missing optional args (sentinel). Preserve explicit nulls.
			if isMissing(ev) {
				continue
			}
			out[k] = ev
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i := range t {
			ev, err := e.evalValue(ctx, t[i])
			if err != nil {
				return nil, fmt.Errorf("evaluate [%d]: %w", i, err)
			}
			if isMissing(ev) {
				out[i] = nil
			} else {
				out[i] = ev
			}
		}
		return out, nil
	default:
		// Best-effort: stringify and then evaluate as expression-ish string.
		return e.evalString(ctx, fmt.Sprintf("%v", t))
	}
}

func (e *mutationTemplateEvaluator) evalString(ctx context.Context, s string) (any, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return "", nil
	}

	// Scalar literals
	switch trimmed {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "null":
		return nil, nil
	}

	// Numeric literals (e.g. 8, 3.14) - must be checked before function calls
	// to correctly type coalesce fallback values like coalesce(ctx.x, 8).
	if i, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
		return i, nil
	}
	if f, err := strconv.ParseFloat(trimmed, 64); err == nil {
		return f, nil
	}

	// ctx.X / ctx.X.Y -- caller-argument reference. Same resolution
	// semantics: look up in e.args, returning missingValue{} for
	// absent paths so optional arguments still drop their object field.
	if strings.HasPrefix(trimmed, "ctx.") {
		path := strings.TrimSpace(strings.TrimPrefix(trimmed, "ctx."))
		if path == "" {
			return nil, fmt.Errorf("ctx: argument path is required")
		}
		// Disallow embedded whitespace / call-shaped suffixes: only a
		// dotted identifier path is valid here. Anything else is a
		// composite expression that should be tokenised by the caller.
		for i := 0; i < len(path); i++ {
			c := path[i]
			if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '.' {
				continue
			}
			return nil, fmt.Errorf("ctx.<path>: invalid character %q in %q", c, trimmed)
		}
		val, ok := getNestedValue(e.args, path)
		if !ok {
			return missingValue{}, nil
		}
		return val, nil
	}

	// timestamp()/now()
	if trimmed == "timestamp()" || trimmed == "now()" {
		return e.now.Format(time.RFC3339Nano), nil
	}

	// var("NAME") -- partition-scoped plaintext variable, falls back to
	// v1:platform:globalVariable (global) when the partition lookup misses.
	if strings.HasPrefix(trimmed, "var(") && strings.HasSuffix(trimmed, ")") {
		name, ok, err := parseSingleStringArg(trimmed, "var")
		if err != nil {
			return nil, err
		}
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("var(): variable name is required")
		}
		value, err := e.engine.ResolveVariable(ctx, name)
		if err != nil {
			return nil, err
		}
		return value, nil
	}

	// systemVar("NAME") -- global plaintext variable on v1:platform:globalVariable.
	if strings.HasPrefix(trimmed, "systemVar(") && strings.HasSuffix(trimmed, ")") {
		name, ok, err := parseSingleStringArg(trimmed, "systemVar")
		if err != nil {
			return nil, err
		}
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("systemVar(): variable name is required")
		}
		value, err := e.engine.ResolveSystemVariable(ctx, name)
		if err != nil {
			return nil, err
		}
		return value, nil
	}

	// secret("NAME") -- partition-scoped encrypted secret, decrypted under
	// MEMQL_MASTER_KEY, falls back to v1:platform:globalSecret when missing.
	if strings.HasPrefix(trimmed, "secret(") && strings.HasSuffix(trimmed, ")") {
		name, ok, err := parseSingleStringArg(trimmed, "secret")
		if err != nil {
			return nil, err
		}
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("secret(): secret name is required")
		}
		value, err := e.engine.ResolveSecret(ctx, name)
		if err != nil {
			return nil, err
		}
		return value, nil
	}

	// systemSecret("NAME") -- global encrypted secret on v1:platform:globalSecret.
	if strings.HasPrefix(trimmed, "systemSecret(") && strings.HasSuffix(trimmed, ")") {
		name, ok, err := parseSingleStringArg(trimmed, "systemSecret")
		if err != nil {
			return nil, err
		}
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("systemSecret(): secret name is required")
		}
		value, err := e.engine.ResolveSystemSecret(ctx, name)
		if err != nil {
			return nil, err
		}
		return value, nil
	}

	// concat(...)
	if strings.HasPrefix(trimmed, "concat(") && strings.HasSuffix(trimmed, ")") {
		return e.evalConcat(ctx, trimmed)
	}

	// hash(...)
	if strings.HasPrefix(trimmed, "hash(") && strings.HasSuffix(trimmed, ")") {
		return e.evalHash(ctx, trimmed)
	}

	// canonicalId(value, "<concept>") -- normalize an id-shaped value
	// to canonical form (`<partition>:<concept>:<bareSlug>`) regardless
	// of input shape (bare slug or already-canonical). The engine
	// looks up the concept's @scope to pick the right partition
	// prefix (`_system` for global concepts, the request envelope's
	// partition otherwise).
	//
	// Use this in mutation id derivations that hash foreign-key args:
	//
	//   id = concat("participant-", hash(concat(
	//     canonicalId(ctx.spaceId, "v1:cognition:space"), ":",
	//     canonicalId(ctx.userId,  "v1:identity:user")
	//   )))
	//
	// The id stays stable whether the caller passes "user-abc" or
	// "_system:v1:identity:user:user-abc". Without it, the two forms
	// hash to different strings and produce duplicate participant rows.
	if strings.HasPrefix(trimmed, "canonicalId(") && strings.HasSuffix(trimmed, ")") {
		return e.evalCanonicalId(ctx, trimmed)
	}

	// coalesce(a,b,...) - minimal helper for optional IDs and fields.
	if strings.HasPrefix(trimmed, "coalesce(") && strings.HasSuffix(trimmed, ")") {
		return e.evalCoalesce(ctx, trimmed)
	}

	// cond(predicate, then, else) -- the canonical conditional-value
	// builtin. Evaluates the predicate and returns the matching branch.
	if strings.HasPrefix(trimmed, "cond(") && strings.HasSuffix(trimmed, ")") {
		return e.evalCond(ctx, trimmed)
	}

	// Array literals (e.g. [], [1, 2, "a"]) - must be checked before treating as string
	// to correctly type coalesce fallback values like coalesce(ctx.x, []).
	if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
		if arr := parseArrayLiteral(trimmed); arr != nil {
			return arr, nil
		}
	}

	// Object literals (e.g. {}, {key: "value"}) - must be checked before treating as string
	// to correctly type coalesce fallback values like coalesce(ctx.x, {}).
	if strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}") {
		if obj := parseObjectLiteral(trimmed); obj != nil {
			return obj, nil
		}
	}

	// Treat as literal string.
	return s, nil
}

func (e *mutationTemplateEvaluator) evalConcat(ctx context.Context, expr string) (string, error) {
	args, err := parseArgList(expr, "concat")
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, raw := range args {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		// String literal
		if isQuotedString(raw) {
			b.WriteString(unquoteSafe(raw))
			continue
		}
		// Evaluate as expression string (supports arg(), var(), timestamp(), concat(), hash())
		ev, err := e.evalString(ctx, raw)
		if err != nil {
			return "", err
		}
		if ev == nil || isMissing(ev) {
			continue
		}
		b.WriteString(fmt.Sprintf("%v", ev))
	}
	return b.String(), nil
}

func (e *mutationTemplateEvaluator) evalParserExpression(ctx context.Context, expr languageParser.ExpressionNode) (any, error) {
	switch t := expr.(type) {
	case nil:
		return nil, nil
	case *languageParser.LiteralExpr:
		return t.Value, nil
	case *languageParser.ArgRefExpr:
		val, ok := getNestedValue(e.args, t.Path)
		if !ok {
			return missingValue{}, nil
		}
		return val, nil
	case *languageParser.VarRefExpr:
		name := strings.TrimSpace(t.Name)
		if name == "" {
			return nil, fmt.Errorf("var(): variable name is required")
		}
		return e.engine.ResolveVariable(ctx, name)
	case *languageParser.TimestampExprFunc:
		return e.now.Format(time.RFC3339Nano), nil
	case *languageParser.ConcatExpr:
		var b strings.Builder
		for _, a := range t.Args {
			ev, err := e.evalParserExpression(ctx, a)
			if err != nil {
				return nil, err
			}
			if ev == nil || isMissing(ev) {
				continue
			}
			b.WriteString(fmt.Sprintf("%v", ev))
		}
		return b.String(), nil
	case *languageParser.HashExpr:
		ev, err := e.evalParserExpression(ctx, t.Target)
		if err != nil {
			return nil, err
		}
		if ev == nil || isMissing(ev) {
			return "", nil
		}
		sum := sha256.Sum256([]byte(fmt.Sprintf("%v", ev)))
		return hex.EncodeToString(sum[:]), nil
	case *languageParser.CanonicalIdExpr:
		ev, err := e.evalParserExpression(ctx, t.Value)
		if err != nil {
			return nil, err
		}
		if ev == nil || isMissing(ev) {
			return "", nil
		}
		return e.engine.canonicalizeIdValue(ctx, fmt.Sprintf("%v", ev), t.Concept)
	case *languageParser.CoalesceExpr:
		for _, a := range t.Args {
			ev, err := e.evalParserExpression(ctx, a)
			if err != nil {
				return nil, err
			}
			if ev == nil || isMissing(ev) {
				continue
			}
			return ev, nil
		}
		return nil, nil
	case *languageParser.CondExpr:
		cond, err := e.evalParserExpression(ctx, t.Condition)
		if err != nil {
			return nil, err
		}
		if cond != nil && !isMissing(cond) && isTruthy(cond) {
			return e.evalParserExpression(ctx, t.Then)
		}
		return e.evalParserExpression(ctx, t.Else)
	case *languageParser.AndExpr:
		for _, a := range t.Args {
			ev, err := e.evalParserExpression(ctx, a)
			if err != nil {
				return nil, err
			}
			if !isTruthy(ev) {
				return false, nil
			}
		}
		return true, nil
	case *languageParser.OrExpr:
		for _, a := range t.Args {
			ev, err := e.evalParserExpression(ctx, a)
			if err != nil {
				return nil, err
			}
			if isTruthy(ev) {
				return true, nil
			}
		}
		return false, nil
	case *languageParser.NotExpr:
		ev, err := e.evalParserExpression(ctx, t.Target)
		if err != nil {
			return nil, err
		}
		return !isTruthy(ev), nil
	case *languageParser.EqExpr:
		left, err := e.evalParserExpression(ctx, t.Left)
		if err != nil {
			return nil, err
		}
		right, err := e.evalParserExpression(ctx, t.Right)
		if err != nil {
			return nil, err
		}
		return runtimeCompareValues(left, right) == 0, nil
	case *languageParser.LtExpr:
		left, err := e.evalParserExpression(ctx, t.Left)
		if err != nil {
			return nil, err
		}
		right, err := e.evalParserExpression(ctx, t.Right)
		if err != nil {
			return nil, err
		}
		return runtimeCompareValues(left, right) < 0, nil
	case *languageParser.GtExpr:
		left, err := e.evalParserExpression(ctx, t.Left)
		if err != nil {
			return nil, err
		}
		right, err := e.evalParserExpression(ctx, t.Right)
		if err != nil {
			return nil, err
		}
		return runtimeCompareValues(left, right) > 0, nil
	case *languageParser.LteExpr:
		left, err := e.evalParserExpression(ctx, t.Left)
		if err != nil {
			return nil, err
		}
		right, err := e.evalParserExpression(ctx, t.Right)
		if err != nil {
			return nil, err
		}
		return runtimeCompareValues(left, right) <= 0, nil
	case *languageParser.GteExpr:
		left, err := e.evalParserExpression(ctx, t.Left)
		if err != nil {
			return nil, err
		}
		right, err := e.evalParserExpression(ctx, t.Right)
		if err != nil {
			return nil, err
		}
		return runtimeCompareValues(left, right) >= 0, nil
	case *languageParser.ToStringExpr:
		ev, err := e.evalParserExpression(ctx, t.Target)
		if err != nil {
			return nil, err
		}
		if ev == nil {
			return "", nil
		}
		return fmt.Sprintf("%v", ev), nil
	case *languageParser.FirstExpr:
		ev, err := e.evalParserExpression(ctx, t.Target)
		if err != nil {
			return nil, err
		}
		return evalFirst(ev), nil
	case *languageParser.LastExpr:
		ev, err := e.evalParserExpression(ctx, t.Target)
		if err != nil {
			return nil, err
		}
		return evalLast(ev), nil
	case *languageParser.LowerExpr:
		ev, err := e.evalParserExpression(ctx, t.Target)
		if err != nil {
			return nil, err
		}
		if ev == nil {
			return "", nil
		}
		return strings.ToLower(fmt.Sprintf("%v", ev)), nil
	case *languageParser.UpperExpr:
		ev, err := e.evalParserExpression(ctx, t.Target)
		if err != nil {
			return nil, err
		}
		if ev == nil {
			return "", nil
		}
		return strings.ToUpper(fmt.Sprintf("%v", ev)), nil
	case *languageParser.TrimExpr:
		ev, err := e.evalParserExpression(ctx, t.Target)
		if err != nil {
			return nil, err
		}
		if ev == nil {
			return "", nil
		}
		return strings.TrimSpace(fmt.Sprintf("%v", ev)), nil
	case *languageParser.ContainsExpr:
		target, err := e.evalParserExpression(ctx, t.Target)
		if err != nil {
			return nil, err
		}
		substr, err := e.evalParserExpression(ctx, t.Substring)
		if err != nil {
			return nil, err
		}
		if target == nil || substr == nil {
			return false, nil
		}
		return strings.Contains(fmt.Sprintf("%v", target), fmt.Sprintf("%v", substr)), nil
	case *languageParser.AddDurationExpr:
		ts, err := e.evalParserExpression(ctx, t.Timestamp)
		if err != nil {
			return nil, err
		}
		dur, err := e.evalParserExpression(ctx, t.Duration)
		if err != nil {
			return nil, err
		}
		tsStr := fmt.Sprintf("%v", ts)
		durStr := fmt.Sprintf("%v", dur)
		rt := NewRuntimeEvaluator(nil)
		return rt.EvaluateAddDuration(tsStr, durStr)
	case *languageParser.DaysBetweenExpr:
		d1, err := e.evalParserExpression(ctx, t.Date1)
		if err != nil {
			return nil, err
		}
		d2, err := e.evalParserExpression(ctx, t.Date2)
		if err != nil {
			return nil, err
		}
		rt := NewRuntimeEvaluator(nil)
		return rt.EvaluateDaysBetween(fmt.Sprintf("%v", d1), fmt.Sprintf("%v", d2))
	case *languageParser.SubtractTimestampsExpr:
		t1, err := e.evalParserExpression(ctx, t.T1)
		if err != nil {
			return nil, err
		}
		t2, err := e.evalParserExpression(ctx, t.T2)
		if err != nil {
			return nil, err
		}
		rt := NewRuntimeEvaluator(nil)
		return rt.EvaluateSubtractTimestamps(fmt.Sprintf("%v", t1), fmt.Sprintf("%v", t2))
	case *languageParser.YearExpr:
		ts, err := e.evalParserExpression(ctx, t.Target)
		if err != nil {
			return nil, err
		}
		rt := NewRuntimeEvaluator(nil)
		return rt.EvaluateYear(fmt.Sprintf("%v", ts))
	case *languageParser.QuarterExpr:
		ts, err := e.evalParserExpression(ctx, t.Target)
		if err != nil {
			return nil, err
		}
		rt := NewRuntimeEvaluator(nil)
		return rt.EvaluateQuarter(fmt.Sprintf("%v", ts))
	case *languageParser.MonthExpr:
		ts, err := e.evalParserExpression(ctx, t.Target)
		if err != nil {
			return nil, err
		}
		rt := NewRuntimeEvaluator(nil)
		return rt.EvaluateMonth(fmt.Sprintf("%v", ts))
	case *languageParser.DayOfMonthExpr:
		ts, err := e.evalParserExpression(ctx, t.Target)
		if err != nil {
			return nil, err
		}
		rt := NewRuntimeEvaluator(nil)
		return rt.EvaluateDayOfMonth(fmt.Sprintf("%v", ts))
	case *languageParser.IsAnniversaryExpr:
		start, err := e.evalParserExpression(ctx, t.StartDate)
		if err != nil {
			return nil, err
		}
		check, err := e.evalParserExpression(ctx, t.CheckDate)
		if err != nil {
			return nil, err
		}
		rt := NewRuntimeEvaluator(nil)
		return rt.EvaluateIsAnniversary(fmt.Sprintf("%v", start), fmt.Sprintf("%v", check))
	case *languageParser.IsFirstDayOfQuarterExpr:
		ts, err := e.evalParserExpression(ctx, t.Target)
		if err != nil {
			return nil, err
		}
		rt := NewRuntimeEvaluator(nil)
		return rt.EvaluateIsFirstDayOfQuarter(fmt.Sprintf("%v", ts))
	default:
		return nil, fmt.Errorf("unsupported expression in mutation template: %T", expr)
	}
}

// evalFirst returns the first element of a collection.
func evalFirst(v any) any {
	if v == nil {
		return nil
	}
	switch c := v.(type) {
	case []any:
		if len(c) > 0 {
			return c[0]
		}
	case []map[string]any:
		if len(c) > 0 {
			return c[0]
		}
	}
	return nil
}

// evalLast returns the last element of a collection.
func evalLast(v any) any {
	if v == nil {
		return nil
	}
	switch c := v.(type) {
	case []any:
		if len(c) > 0 {
			return c[len(c)-1]
		}
	case []map[string]any:
		if len(c) > 0 {
			return c[len(c)-1]
		}
	}
	return nil
}

func (e *mutationTemplateEvaluator) evalHash(ctx context.Context, expr string) (string, error) {
	args, ok, err := parseSingleArg(expr, "hash")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("hash() requires one argument")
	}
	raw := strings.TrimSpace(args)
	var input string
	if isQuotedString(raw) {
		input = unquoteSafe(raw)
	} else {
		ev, err := e.evalString(ctx, raw)
		if err != nil {
			return "", err
		}
		if ev == nil || isMissing(ev) {
			input = ""
		} else {
			input = fmt.Sprintf("%v", ev)
		}
	}
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:]), nil
}

// evalCanonicalId resolves canonicalId(value, "<conceptType>") into
// the canonical id form for the named concept. The engine reads the
// concept's @scope to pick the partition prefix (`_system` for global
// concepts, otherwise the request envelope's partition).
//
// Behavior matrix:
//
//	value=""                                   -> "" (missing/optional id)
//	value="user-abc",            concept=user  -> "_system:v1:identity:user:user-abc" (global)
//	value="abc-123",             concept=space -> "<envelope>:v1:cognition:space:abc-123"
//	value="default:v1:cognition:space:abc",
//	      concept=space                        -> as-is (already canonical for this concept)
//	value="weird:v1:cognition:space:abc",
//	      concept=space                        -> "<envelope>:v1:cognition:space:abc"
//	      (re-partitioned: tolerates legacy callers that hand in a
//	      canonical id under a stale partition)
//
// Errors when the concept isn't registered (catches typos at runtime;
// the assert layer catches them at parse-time, this is the safety
// net for dynamic callers).
func (e *mutationTemplateEvaluator) evalCanonicalId(ctx context.Context, expr string) (string, error) {
	args, err := parseArgList(expr, "canonicalId")
	if err != nil {
		return "", err
	}
	if len(args) != 2 {
		return "", fmt.Errorf("canonicalId() requires exactly two arguments: (value, \"<conceptType>\")")
	}

	// First arg: the value (any expression).
	rawValue := strings.TrimSpace(args[0])
	var value string
	if isQuotedString(rawValue) {
		value = unquoteSafe(rawValue)
	} else {
		ev, err := e.evalString(ctx, rawValue)
		if err != nil {
			return "", err
		}
		if ev == nil || isMissing(ev) {
			return "", nil
		}
		value = fmt.Sprintf("%v", ev)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}

	// Second arg: the concept type, must be a literal quoted string
	// (we need it at parse time to resolve the concept's scope; passing
	// it as a dynamic expression would be a footgun).
	rawConcept := strings.TrimSpace(args[1])
	if !isQuotedString(rawConcept) {
		return "", fmt.Errorf("canonicalId(): second argument must be a literal quoted concept name (e.g. \"v1:identity:user\")")
	}
	conceptType := strings.TrimSpace(unquoteSafe(rawConcept))
	if conceptType == "" {
		return "", fmt.Errorf("canonicalId(): concept name is empty")
	}

	return e.engine.canonicalizeIdValue(ctx, value, conceptType)
}

func (e *mutationTemplateEvaluator) evalCoalesce(ctx context.Context, expr string) (any, error) {
	args, err := parseArgList(expr, "coalesce")
	if err != nil {
		return nil, err
	}
	// Track the last non-missing fallback we evaluated so callers can
	// rely on `coalesce(ctx.X, "")` to land an empty-string default
	// when the optional arg is missing. The previous implementation
	// treated empty-string as "still missing" and returned nil, which
	// produced JSON-schema validation failures on non-required string
	// fields downstream (the mutation payload had `null` instead of
	// `""`).
	var fallback any
	haveFallback := false

	for i, raw := range args {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		var ev any
		var err error
		if isQuotedString(raw) {
			ev = unquoteSafe(raw)
		} else {
			ev, err = e.evalString(ctx, raw)
			if err != nil {
				return nil, err
			}
		}
		if ev == nil {
			continue
		}
		if isMissing(ev) {
			continue
		}
		// First non-empty value wins, matching the documented
		// "first non-nil value" semantics.
		if s, ok := ev.(string); !ok || strings.TrimSpace(s) != "" {
			return ev, nil
		}
		// Empty-string value: remember it as a possible fallback.
		// We only adopt it if no later arg produces a non-empty
		// value. The last arg in the list is the canonical default,
		// so we always update fallback to the latest empty value
		// we've seen.
		if i == len(args)-1 || !haveFallback {
			fallback = ev
			haveFallback = true
		}
	}

	if haveFallback {
		return fallback, nil
	}
	return nil, nil
}

func (e *mutationTemplateEvaluator) evalCond(ctx context.Context, expr string) (any, error) {
	args, err := parseArgList(expr, "cond")
	if err != nil {
		return nil, err
	}
	if len(args) != 3 {
		return nil, fmt.Errorf("cond() requires three arguments: cond(predicate, thenValue, elseValue)")
	}
	condRaw := strings.TrimSpace(args[0])
	thenRaw := strings.TrimSpace(args[1])
	elseRaw := strings.TrimSpace(args[2])

	condVal, err := e.evalCondition(ctx, condRaw)
	if err != nil {
		return nil, err
	}
	chosen := elseRaw
	if condVal {
		chosen = thenRaw
	}
	if isQuotedString(chosen) {
		return unquoteSafe(chosen), nil
	}
	return e.evalString(ctx, chosen)
}

func (e *mutationTemplateEvaluator) evalCondition(ctx context.Context, raw string) (bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false, nil
	}
	// Support simple boolean literals.
	switch raw {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	// Support == and != comparisons for common use cases.
	if parts := strings.Split(raw, "=="); len(parts) == 2 {
		left, err := e.evalString(ctx, strings.TrimSpace(parts[0]))
		if err != nil {
			return false, err
		}
		right, err := e.evalString(ctx, strings.TrimSpace(parts[1]))
		if err != nil {
			return false, err
		}
		if isMissing(left) {
			left = ""
		}
		if isMissing(right) {
			right = ""
		}
		return fmt.Sprintf("%v", left) == fmt.Sprintf("%v", right), nil
	}
	if parts := strings.Split(raw, "!="); len(parts) == 2 {
		left, err := e.evalString(ctx, strings.TrimSpace(parts[0]))
		if err != nil {
			return false, err
		}
		right, err := e.evalString(ctx, strings.TrimSpace(parts[1]))
		if err != nil {
			return false, err
		}
		if isMissing(left) {
			left = ""
		}
		if isMissing(right) {
			right = ""
		}
		return fmt.Sprintf("%v", left) != fmt.Sprintf("%v", right), nil
	}
	// Fallback: evaluate expression and treat truthy strings/bools.
	ev, err := e.evalString(ctx, raw)
	if err != nil {
		return false, err
	}
	if isMissing(ev) {
		return false, nil
	}
	switch v := ev.(type) {
	case bool:
		return v, nil
	case string:
		return strings.TrimSpace(v) != "" && v != "0" && v != "false", nil
	default:
		return ev != nil, nil
	}
}

// parsePayloadRawToTemplate parses a MemQL object literal (payload={...} or {id:...,payload:{...}})
// into a mutation template. It uses a simplified parser compatible with the compiler's automation parsing.
func parsePayloadRawToTemplate(raw string) (map[string]any, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	obj := parseObjectLiteral(trimmed)
	if obj == nil {
		return nil, fmt.Errorf("invalid object literal payload")
	}
	return obj, nil
}

// parseObjectLiteral parses a MemQL object literal like {key: value, nested: {x: 1}} into map[string]any.
// Values are decoded as:
// - quoted strings -> string (without quotes)
// - numbers -> int64 or float64
// - true/false/null -> bool/nil
// - arrays -> []any
// - nested objects -> map[string]any
// - everything else -> string (expression token, e.g. args.userId, concat(...))
func parseObjectLiteral(s string) map[string]any {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return nil
	}
	inner := strings.TrimSpace(s[1 : len(s)-1])
	if inner == "" {
		return map[string]any{}
	}
	out := make(map[string]any)

	pos := 0
	for pos < len(inner) {
		// skip whitespace/commas
		for pos < len(inner) && (inner[pos] == ' ' || inner[pos] == '\t' || inner[pos] == '\n' || inner[pos] == '\r' || inner[pos] == ',') {
			pos++
		}
		if pos >= len(inner) {
			break
		}

		// ctx-shorthand: `{ctx.name}` -> `{name: ctx.name}`.
		// Single-segment restriction (dotted paths like
		// `ctx.user.id` route through the verbose `key: value` form).
		// Authored as `args.name` in mutation source -- the
		// rewriter translates `args.X` to `ctx.X` before this point.
		if key, rawExpr, next, ok := tryParseShorthandCtx(inner, pos); ok {
			out[key] = rawExpr
			pos = next
			continue
		}

		// parse key (identifier or quoted string)
		key, newPos := parseObjectKey(inner, pos)
		if key == "" {
			return nil
		}
		pos = newPos
		for pos < len(inner) && (inner[pos] == ' ' || inner[pos] == '\t' || inner[pos] == '\n' || inner[pos] == '\r') {
			pos++
		}
		if pos >= len(inner) || inner[pos] != ':' {
			return nil
		}
		pos++ // skip ':'
		val, next := parseLiteralOrExpr(inner, pos)
		out[key] = val
		pos = next
	}

	return out
}

// tryParseShorthandCtx recognises `ctx.<bareIdent>` with no `key:`
// prefix in an object literal and infers the
// key from the ctx path. Dotted suffixes (`ctx.user.id`) are rejected
// so they fall through to the normal key:value parser, matching the
// arg() restriction.
func tryParseShorthandCtx(s string, pos int) (string, string, int, bool) {
	start := pos
	for start < len(s) && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	const prefix = "ctx."
	if start+len(prefix) > len(s) || s[start:start+len(prefix)] != prefix {
		return "", "", pos, false
	}
	scan := start + len(prefix)
	// Consume the path: letters / digits / underscore / dot.
	pathStart := scan
	for scan < len(s) {
		c := s[scan]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '.' {
			scan++
			continue
		}
		break
	}
	path := s[pathStart:scan]
	if !baseparser.IsSimpleIdentifier(path) {
		// Path is empty or has a dot — not eligible for the shorthand.
		return "", "", pos, false
	}
	// Lookahead: after the path must be whitespace, `,`, `}`, or EOF.
	// `:` means this was actually `ctx.name: value`, route through
	// the normal parser; anything else rules out the shorthand.
	probe := scan
	for probe < len(s) && (s[probe] == ' ' || s[probe] == '\t' || s[probe] == '\n' || s[probe] == '\r') {
		probe++
	}
	if probe < len(s) {
		c := s[probe]
		if c != ',' && c != '}' {
			return "", "", pos, false
		}
	}
	rawExpr := strings.TrimSpace(s[start:scan])
	return path, rawExpr, scan, true
}

func parseObjectKey(s string, pos int) (string, int) {
	// skip whitespace
	for pos < len(s) && (s[pos] == ' ' || s[pos] == '\t' || s[pos] == '\n' || s[pos] == '\r') {
		pos++
	}
	if pos >= len(s) {
		return "", pos
	}
	// quoted key
	if s[pos] == '"' {
		lit, next, ok := scanQuotedString(s, pos)
		if !ok {
			return "", pos
		}
		return lit, next
	}
	start := pos
	for pos < len(s) && s[pos] != ':' && s[pos] != ' ' && s[pos] != '\t' && s[pos] != '\n' && s[pos] != '\r' {
		pos++
	}
	return strings.TrimSpace(s[start:pos]), pos
}

func parseLiteralOrExpr(s string, pos int) (any, int) {
	// skip whitespace
	for pos < len(s) && (s[pos] == ' ' || s[pos] == '\t' || s[pos] == '\n' || s[pos] == '\r') {
		pos++
	}
	if pos >= len(s) {
		return nil, pos
	}

	switch s[pos] {
	case '{':
		obj, next := scanBalanced(s, pos, '{', '}')
		if next == pos {
			return nil, pos
		}
		return parseObjectLiteral(obj), next
	case '[':
		arr, next := scanBalanced(s, pos, '[', ']')
		if next == pos {
			return nil, pos
		}
		return parseArrayLiteral(arr), next
	case '"':
		lit, next, ok := scanQuotedString(s, pos)
		if !ok {
			return nil, pos
		}
		return lit, next
	default:
		// read until comma or closing brace/bracket at depth 0
		start := pos
		depth := 0
		inString := false
		escaped := false
		for pos < len(s) {
			ch := s[pos]
			if escaped {
				escaped = false
				pos++
				continue
			}
			if inString && ch == '\\' {
				escaped = true
				pos++
				continue
			}
			if ch == '"' {
				inString = !inString
				pos++
				continue
			}
			if !inString {
				switch ch {
				case '(':
					depth++
				case ')':
					if depth > 0 {
						depth--
					}
				case ',':
					if depth == 0 {
						raw := strings.TrimSpace(s[start:pos])
						return classifyScalarOrExpr(raw), pos
					}
				case '}', ']':
					if depth == 0 {
						raw := strings.TrimSpace(s[start:pos])
						return classifyScalarOrExpr(raw), pos
					}
				}
			}
			pos++
		}
		raw := strings.TrimSpace(s[start:pos])
		return classifyScalarOrExpr(raw), pos
	}
}

func parseArrayLiteral(s string) []any {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		return nil
	}
	inner := strings.TrimSpace(s[1 : len(s)-1])
	if inner == "" {
		return []any{}
	}
	var out []any
	pos := 0
	for pos < len(inner) {
		// skip whitespace/commas
		for pos < len(inner) && (inner[pos] == ' ' || inner[pos] == '\t' || inner[pos] == '\n' || inner[pos] == '\r' || inner[pos] == ',') {
			pos++
		}
		if pos >= len(inner) {
			break
		}
		val, next := parseLiteralOrExpr(inner, pos)
		out = append(out, val)
		pos = next
	}
	return out
}

func classifyScalarOrExpr(raw string) any {
	if raw == "" {
		return nil
	}
	switch raw {
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	}
	if i, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return i
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		return f
	}
	return raw
}

func scanQuotedString(s string, pos int) (string, int, bool) {
	if pos >= len(s) || s[pos] != '"' {
		return "", pos, false
	}
	pos++
	start := pos
	escaped := false
	for pos < len(s) {
		ch := s[pos]
		if escaped {
			escaped = false
			pos++
			continue
		}
		if ch == '\\' {
			escaped = true
			pos++
			continue
		}
		if ch == '"' {
			lit := s[start:pos]
			pos++
			// unescape via strconv.Unquote for correctness
			u, err := strconv.Unquote(`"` + lit + `"`)
			if err != nil {
				return "", pos, false
			}
			return u, pos, true
		}
		pos++
	}
	return "", pos, false
}

func scanBalanced(s string, pos int, open, close byte) (string, int) {
	if pos >= len(s) || s[pos] != open {
		return "", pos
	}
	start := pos
	depth := 0
	inString := false
	escaped := false
	for pos < len(s) {
		ch := s[pos]
		if escaped {
			escaped = false
			pos++
			continue
		}
		if inString && ch == '\\' {
			escaped = true
			pos++
			continue
		}
		if ch == '"' {
			inString = !inString
			pos++
			continue
		}
		if !inString {
			switch ch {
			case open:
				depth++
			case close:
				depth--
				if depth == 0 {
					pos++
					return s[start:pos], pos
				}
			}
		}
		pos++
	}
	return "", start
}

func isQuotedString(s string) bool {
	s = strings.TrimSpace(s)
	return len(s) >= 2 && strings.HasPrefix(s, "\"") && strings.HasSuffix(s, "\"")
}

func unquoteSafe(s string) string {
	s = strings.TrimSpace(s)
	u, err := strconv.Unquote(s)
	if err != nil {
		return strings.Trim(s, "\"")
	}
	return u
}

func parseSingleStringArg(expr string, fn string) (string, bool, error) {
	raw, ok, err := parseSingleArg(expr, fn)
	if err != nil {
		return "", false, err
	}
	if !ok {
		return "", false, nil
	}
	raw = strings.TrimSpace(raw)
	if !isQuotedString(raw) {
		return "", false, fmt.Errorf("%s(): argument must be a quoted string", fn)
	}
	return unquoteSafe(raw), true, nil
}

func parseSingleArg(expr string, fn string) (string, bool, error) {
	args, err := parseArgList(expr, fn)
	if err != nil {
		return "", false, err
	}
	if len(args) == 0 {
		return "", false, nil
	}
	if len(args) != 1 {
		return "", false, fmt.Errorf("%s() requires exactly one argument", fn)
	}
	return args[0], true, nil
}

func parseArgList(expr string, fn string) ([]string, error) {
	prefix := fn + "("
	if !strings.HasPrefix(expr, prefix) || !strings.HasSuffix(expr, ")") {
		return nil, fmt.Errorf("%s(): invalid call syntax", fn)
	}
	inner := strings.TrimSpace(expr[len(prefix) : len(expr)-1])
	if inner == "" {
		return []string{}, nil
	}
	return splitArgs(inner), nil
}

func splitArgs(s string) []string {
	var args []string
	var cur strings.Builder
	depth := 0
	inString := false
	escaped := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if escaped {
			escaped = false
			cur.WriteByte(ch)
			continue
		}
		if inString && ch == '\\' {
			escaped = true
			cur.WriteByte(ch)
			continue
		}
		if ch == '"' {
			inString = !inString
			cur.WriteByte(ch)
			continue
		}
		if !inString {
			switch ch {
			case '(':
				depth++
			case ')':
				if depth > 0 {
					depth--
				}
			case ',':
				if depth == 0 {
					args = append(args, cur.String())
					cur.Reset()
					continue
				}
			}
		}
		cur.WriteByte(ch)
	}
	if cur.Len() > 0 {
		args = append(args, cur.String())
	}
	return args
}
