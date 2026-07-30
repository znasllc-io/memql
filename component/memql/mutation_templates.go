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

	"github.com/znasllc-io/memql/component/auth"
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

	// PayloadOverlayTemplate carries explicit insert-block fields that
	// must overlay on top of the splatted PayloadTemplate at render
	// time. Populated only when the insert block mixes `args.<arg>`
	// (which sets PayloadTemplate to the whole evaluated object) with
	// other explicit fields like `ownerUserId: actor.userId`. The
	// overlay wins on key collision so authz-relevant server-side
	// stamps cannot be displaced by a caller's payload (memql#401).
	// Each entry's value is an expression that the standard
	// mutationTemplateEvaluator resolves -- same surface as the
	// regular payload object literal.
	PayloadOverlayTemplate map[string]any

	// ParentTemplate is an optional parent relationship hint.
	ParentTemplate any

	// AliasOfTemplate is an optional aliasOf relationship hint.
	AliasOfTemplate any

	// AppendFields carries the mutation's @appendFields("a", "b")
	// annotation: array-typed payload fields whose partial-write
	// elements the update executor APPENDS to the stored array instead
	// of replacing it wholesale. Only valid on update-kind mutations
	// (memql#2240).
	AppendFields []string

	// MergeFields carries the mutation's @mergeFields("a", "b")
	// annotation: object-typed payload fields that the update executor
	// deep-merges into the stored object instead of replacing it
	// wholesale. Only valid on update-kind mutations (enforced at
	// load time in function_loader.go). See memql#1339.
	MergeFields []string

	// CreateOnlyFields carries the mutation's @createOnly("a", "b")
	// annotation: payload fields written ONLY on create. When the target
	// id already exists, executeWrite drops them from the delta before the
	// read-merge so the stored value is preserved. Only valid on
	// insert-kind mutations (enforced at load time in function_loader.go).
	// See fylo#63.
	CreateOnlyFields []string

	// ScrubPii carries the mutation's @scrubPii annotation: when set,
	// the update executor enumerates every @pii-annotated field on the
	// bound concept and zeroes it after the partial payload merges,
	// making the hard-delete PII scrub annotation-driven instead of a
	// hand-maintained field list. Only valid on update-kind mutations
	// (enforced at load time in function_loader.go). See memql#1711.
	ScrubPii bool
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

	// Overlay explicit insert-block fields onto the splatted payload.
	// Used when the insert block mixes `args.<arg>` (which became the
	// PayloadTemplate) with explicit per-field stamps like
	// `ownerUserId: actor.userId`. Overlay-wins precedence so authz-
	// relevant server-side values can't be displaced by a caller's
	// splat payload (memql#401).
	if len(tmpl.PayloadOverlayTemplate) > 0 {
		// Copy-on-write: if PayloadTemplate happens to alias the args
		// map (legal in the no-args.payload code path), writing the
		// overlay into payloadMap would mutate the caller's args. Take
		// a shallow clone before overlaying. No size hint on the make:
		// CodeQL's "size computation may overflow" check fires on
		// summed lengths, and the perf delta of pre-sizing isn't worth
		// silencing it -- payloads are small (handful of fields).
		overlaid := make(map[string]any)
		for k, v := range payloadMap {
			overlaid[k] = v
		}
		for k, tpl := range tmpl.PayloadOverlayTemplate {
			val, err := eval.evalValue(ctx, tpl)
			if err != nil {
				return MutationNode{}, fmt.Errorf("evaluate payload overlay field %q: %w", k, err)
			}
			overlaid[k] = val
		}
		payloadMap = overlaid
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
		Kind:             kind,
		Concept:          concept,
		ID:               strings.TrimSpace(id),
		PayloadRaw:       string(payloadJSON),
		CreatedAt:        createdAtRef,
		ParentRef:        parentRef,
		AliasOfRef:       aliasRef,
		MergeFields:      tmpl.MergeFields,
		AppendFields:     tmpl.AppendFields,
		CreateOnlyFields: tmpl.CreateOnlyFields,
		ScrubPii:         tmpl.ScrubPii,
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

// evalOperandString evaluates a BARE (unquoted) operand pulled out of an
// expression -- a coalesce / concat / cond argument. Quoted operands are
// filtered by the callers (via isQuotedString) before reaching here, so a bare
// `now` token here is unambiguously the reserved clock keyword and resolves to
// the current timestamp. This is the only place a bare `now` becomes the clock
// without first being rewritten to the `now()` call form: classifyScalarOrExpr
// rewrites top-level bare scalars (`createdAt: now` -> `now()`), but it sees
// the whole `coalesce(args.createdAt, now)` string as one expression and never
// reaches the inner `now`. Resolving it here (rather than in the shared
// evalString) keeps a top-level QUOTED `"now"` literal -- which arrives at
// evalString as the bare text "now" with its quotes already stripped -- from
// being mistaken for the clock. (epic #2298 / #2301.)
func (e *mutationTemplateEvaluator) evalOperandString(ctx context.Context, raw string) (any, error) {
	if strings.TrimSpace(raw) == "now" {
		return e.now.Format(time.RFC3339Nano), nil
	}
	return e.evalString(ctx, raw)
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

	// args.X / args.X.Y -- caller-argument reference (author-facing
	// form). `ctx.X` is the legacy runtime form still emitted by the
	// struct-form rewriter for non-Logic receivers; both prefixes
	// resolve identically against e.args. Returns missingValue{} for
	// absent paths so optional arguments still drop their object field.
	var argPrefix string
	switch {
	case strings.HasPrefix(trimmed, "args."):
		argPrefix = "args."
	case strings.HasPrefix(trimmed, "ctx."):
		argPrefix = "ctx."
	}
	if argPrefix != "" {
		path := strings.TrimSpace(strings.TrimPrefix(trimmed, argPrefix))
		if path == "" {
			return nil, fmt.Errorf("%s: argument path is required", strings.TrimSuffix(argPrefix, "."))
		}
		// Disallow embedded whitespace / call-shaped suffixes: only a
		// dotted identifier path is valid here. Anything else is a
		// composite expression that should be tokenised by the caller.
		for i := 0; i < len(path); i++ {
			c := path[i]
			if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '.' {
				continue
			}
			return nil, fmt.Errorf("%s<path>: invalid character %q in %q", argPrefix, c, trimmed)
		}
		val, ok := getNestedValue(e.args, path)
		if !ok {
			return missingValue{}, nil
		}
		return val, nil
	}

	// actor.X -- auth-context accessor. Resolved from the caller's
	// auth.AccessContext (set by the gRPC middleware), NOT from
	// caller-passed args. Without this case `actor.userId` in a
	// mutation insert payload would fall through as the literal string
	// "actor.userId" (memql#401 finding) -- which is exactly the bug
	// `mutationCreateSpace`'s server-side ownerUserId stamp was meant
	// to avoid. Matches the comparison-position resolution in
	// component/memql/executor.go:resolveActorPath; supported paths
	// are userId / identityId / role / primaryEmail / isOwner.
	if strings.HasPrefix(trimmed, "actor.") {
		return resolveActorReference(ctx, trimmed)
	}

	// The clock. classifyScalarOrExpr rewrites a bare top-level `now` to the
	// canonical call form `now()`, and the automation generator emits
	// `timestamp()`, so the clock reaches this evaluator as a call form here.
	// A bare `now` operand nested in an expression (e.g.
	// `coalesce(args.createdAt, now)`) is resolved one level up by
	// evalOperandString before it reaches this path -- NOT here, because a
	// standalone top-level value of literally "now" only ever originates from
	// a QUOTED string literal (`label: "now"`) that must stay the literal
	// string, never the clock.
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

	// shortId(value) -- strip the canonical concept prefix from an
	// id-shaped value and return the bare short id. The inverse of
	// canonicalId. Idempotent: a value already bare (no version-tagged
	// concept prefix) is returned unchanged, so it is a no-op on a
	// tool-path short id. Used to keep foreign-key / audit fields (e.g.
	// v1:forge:requestEvent.requestId) in one consistent short form
	// regardless of whether the caller passes a canonical node id
	// (automation path: args.event.payload.id) or a bare slug (tool
	// path: args.requestId). See #1859.
	if strings.HasPrefix(trimmed, "shortId(") && strings.HasSuffix(trimmed, ")") {
		return e.evalShortId(ctx, trimmed)
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
	//     canonicalId(ctx.partitionId, "v1:cognition:space"), ":",
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
			return unwrapMalformedNullCoalesce(arr), nil
		}
	}

	// Object literals (e.g. {}, {key: "value"}) - must be checked before treating as string
	// to correctly type coalesce fallback values like coalesce(ctx.x, {}).
	if strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}") {
		if obj := parseObjectLiteral(trimmed); obj != nil {
			return unwrapMalformedNullCoalesce(obj), nil
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
		// Evaluate as expression string (supports arg(), var(), now, concat(), hash())
		ev, err := e.evalOperandString(ctx, raw)
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

// resolveActorReference resolves an `actor.<path>` reference through the auth
// envelope on ctx, NOT through caller-passed args.
//
// Shared by the string-operand path and the lowered-AST path. They resolved it
// independently until memql#2840, and only the string one implemented it --
// which is why `update { id: actor.userId }` silently selected nothing. One
// implementation means a future position that reaches either path is already
// correct.
func resolveActorReference(ctx context.Context, ref string) (any, error) {
	trimmed := strings.TrimSpace(ref)
	path := strings.TrimSpace(strings.TrimPrefix(trimmed, "actor."))
	if path == "" {
		return nil, fmt.Errorf("actor: actor path is required")
	}
	for i := 0; i < len(path); i++ {
		c := path[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '.' {
			continue
		}
		return nil, fmt.Errorf("actor.<path>: invalid character %q in %q", c, trimmed)
	}
	ac, _ := auth.AccessFromContext(ctx)
	// One canonical envelope (#2623): auth.ActorEnvelopeFields.
	if v, ok := auth.ActorEnvelopeValue(ac, path); ok {
		return v, nil
	}
	return nil, fmt.Errorf("unsupported actor reference path %q (valid: %s)", path, auth.ActorEnvelopeValidNames())
}

func (e *mutationTemplateEvaluator) evalParserExpression(ctx context.Context, expr languageParser.ExpressionNode) (any, error) {
	switch t := expr.(type) {
	case nil:
		return nil, nil
	case *languageParser.LiteralExpr:
		return t.Value, nil
	case *languageParser.ArgRefExpr:
		// The parser deliberately routes BOTH `args.X` and `actor.X` through
		// ArgRefExpr, with the prefix as the only discriminator (see
		// component/language/compiler/automation_generator.go). Resolving the
		// path against the caller args unconditionally therefore looked up the
		// literal key "actor.userId" in the args map, missed, and yielded an
		// empty value -- so `update { id: actor.userId }` selected no row at
		// all and the write failed with "update() requires an explicit id"
		// (memql#2840).
		//
		// The string-operand path has handled this since memql#401; its own
		// comment describes exactly this failure. The AST path never got the
		// same treatment, and nothing caught it because until #2840 no
		// construct in the tree put an actor reference in a lowered-AST slot.
		// Both now share resolveActorReference so they cannot drift again.
		if strings.HasPrefix(t.Path, "actor.") {
			return resolveActorReference(ctx, t.Path)
		}
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
	case *languageParser.ShortIdExpr:
		// The inverse of CanonicalIdExpr below, and the normaliser
		// authoring-rules.md §20 asks for on a hashed FK arg: canonical or
		// bare in, bare out (#1859). Idempotent for the normal shape, but
		// NOT in general -- a value with more than one v<digits> segment
		// strips further on a second application (memql#2981).
		//
		// Missing from this switch until memql#2925, which made the builtin
		// lint clean in an `id:` slot and then die at render with
		// "unsupported expression in mutation template" -- so it worked in
		// payload positions and not in the id derivation, the one position §20
		// is about.
		//
		// Delegates to engine.shortIdValue, the same entry point the string
		// path (evalShortId) and RuntimeEvaluator.EvaluateShortId both use, so
		// this is not a third implementation of the same normalisation.
		ev, err := e.evalParserExpression(ctx, t.Target)
		if err != nil {
			return nil, err
		}
		if ev == nil || isMissing(ev) {
			return "", nil
		}
		raw := strings.TrimSpace(fmt.Sprintf("%v", ev))
		if raw == "" {
			return "", nil
		}
		return e.engine.shortIdValue(raw), nil
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
		// #1614 selection, matching evalCoalesce and the automation
		// evaluator's coalesceFromArgs: the FINAL argument is the
		// ultimate fallback and is returned even when it resolves to "",
		// while every NON-final argument is skipped when it is nil,
		// missing, or a blank string.
		//
		// This slot (id / createdAt / parent / aliasOf templates)
		// previously skipped only nil/missing, so a blank non-final arm
		// won and `id: args.roleId ?? args.slug` yielded "" when roleId
		// was the empty string clients send for an absent optional --
		// which makes the engine mint a RANDOM id instead of the
		// deterministic fallback, silently turning idempotent upserts
		// into duplicate rows (memql#2772 review).
		for i, a := range t.Args {
			ev, err := e.evalParserExpression(ctx, a)
			if err != nil {
				return nil, err
			}
			if i == len(t.Args)-1 {
				// Normalise the sentinel away: a missing final arm is
				// "no value", matching coalesceFromArgs. (evalCoalesce
				// returns "" rather than nil for the blank-then-missing
				// corner; not observable here, since evalStringMaybe maps
				// nil and missingValue{} alike to "" for these slots.)
				// The normalisation matters because
				// isTruthy(missingValue{}) is TRUE, so leaking the
				// sentinel would make an all-missing coalesce read as
				// truthy inside And/Or/Not.
				if isMissing(ev) {
					return nil, nil
				}
				return ev, nil
			}
			if ev == nil || isMissing(ev) {
				continue
			}
			if s, isStr := ev.(string); isStr && strings.TrimSpace(s) == "" {
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
	case *languageParser.BinaryComparisonExpr:
		// The one comparison shape the builtin-arg grammar emits since
		// memql#2654 (relationals before it, ==/!= joined by the
		// normalization). Template cond() predicates land here.
		left, err := e.evalParserExpression(ctx, t.Left)
		if err != nil {
			return nil, err
		}
		right, err := e.evalParserExpression(ctx, t.Right)
		if err != nil {
			return nil, err
		}
		cmp := runtimeCompareValues(left, right)
		switch t.Operator {
		case languageParser.OpEq:
			return cmp == 0, nil
		case languageParser.OpNe:
			return cmp != 0, nil
		case languageParser.OpLt:
			return cmp < 0, nil
		case languageParser.OpLe:
			return cmp <= 0, nil
		case languageParser.OpGt:
			return cmp > 0, nil
		case languageParser.OpGe:
			return cmp >= 0, nil
		default:
			return nil, fmt.Errorf("unsupported comparison operator in mutation template: %s", t.Operator)
		}
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

// evalShortId resolves shortId(value) into the bare short id, stripping
// any `<concept>:` prefix. The inverse of evalCanonicalId. Idempotent:
// an already-bare value passes through unchanged. Empty/missing input
// yields "". Single argument, mirrors evalHash. See #1859.
func (e *mutationTemplateEvaluator) evalShortId(ctx context.Context, expr string) (string, error) {
	arg, ok, err := parseSingleArg(expr, "shortId")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("shortId() requires one argument")
	}
	raw := strings.TrimSpace(arg)
	var input string
	if isQuotedString(raw) {
		input = unquoteSafe(raw)
	} else {
		ev, err := e.evalString(ctx, raw)
		if err != nil {
			return "", err
		}
		if ev == nil || isMissing(ev) {
			return "", nil
		}
		input = fmt.Sprintf("%v", ev)
	}
	return e.engine.shortIdValue(input), nil
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
			ev, err = e.evalOperandString(ctx, raw)
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
	return e.evalOperandString(ctx, chosen)
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

// mutationMergeFields extracts the @mergeFields("a", "b") annotation
// from a mutation definition into the field-name list the update
// executor consults. Only meaningful on update-kind mutations (insert
// writes a full payload -- there is nothing stored to merge against),
// so its presence on an insert mutation is a load-time error rather
// than a silent no-op. See memql#1339.
func mutationMergeFields(funcDef *languageParser.FunctionDef, kind ast.MutationKind) ([]string, error) {
	var fields []string
	for _, attr := range funcDef.Attributes {
		if attr == nil || attr.Name != languageParser.AttrMergeFields {
			continue
		}
		switch v := attr.Value.(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				fields = append(fields, s)
			}
		case []string:
			for _, raw := range v {
				if s := strings.TrimSpace(raw); s != "" {
					fields = append(fields, s)
				}
			}
		}
		if len(fields) == 0 {
			return nil, fmt.Errorf(`@mergeFields requires at least one field name, e.g. @mergeFields("preferences")`)
		}
	}
	if len(fields) > 0 && kind != ast.MutationKindUpdate {
		return nil, fmt.Errorf("@mergeFields is only valid on update mutations (insert writes the full payload; there is nothing stored to merge against)")
	}
	return fields, nil
}

// mutationAppendFields extracts the @appendFields("a", "b") annotation
// from a mutation definition into the field-name list the update
// executor appends (rather than replaces). Only meaningful on update-kind
// mutations -- an insert writes the full payload, so there is no stored
// array to append to -- so its presence on an insert is a load-time error
// rather than a silent no-op. See memql#2240.
func mutationAppendFields(funcDef *languageParser.FunctionDef, kind ast.MutationKind) ([]string, error) {
	var fields []string
	for _, attr := range funcDef.Attributes {
		if attr == nil || attr.Name != languageParser.AttrAppendFields {
			continue
		}
		switch v := attr.Value.(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				fields = append(fields, s)
			}
		case []string:
			for _, raw := range v {
				if s := strings.TrimSpace(raw); s != "" {
					fields = append(fields, s)
				}
			}
		}
		if len(fields) == 0 {
			return nil, fmt.Errorf(`@appendFields requires at least one field name, e.g. @appendFields("attachmentIds")`)
		}
	}
	if len(fields) > 0 && kind != ast.MutationKindUpdate {
		return nil, fmt.Errorf("@appendFields is only valid on update mutations (insert writes the full payload; there is nothing stored to append to)")
	}
	return fields, nil
}

// mutationCreateOnlyFields extracts the @createOnly("a", "b") annotation
// from a mutation definition into the field-name list executeWrite drops
// from the delta when the target id already exists (memql#1709 read-merge),
// so those fields keep their stored value on a re-stage instead of being
// clobbered. The inverse of @mergeFields/@appendFields: it is only
// meaningful on insert-kind (create-or-upsert) mutations -- an update()
// always targets an existing row, so a create-only field would ALWAYS be
// dropped and could never be written, a silent footgun. Its presence on an
// update mutation is therefore a load-time error. See fylo#63.
func mutationCreateOnlyFields(funcDef *languageParser.FunctionDef, kind ast.MutationKind) ([]string, error) {
	var fields []string
	for _, attr := range funcDef.Attributes {
		if attr == nil || attr.Name != languageParser.AttrCreateOnly {
			continue
		}
		switch v := attr.Value.(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				fields = append(fields, s)
			}
		case []string:
			for _, raw := range v {
				if s := strings.TrimSpace(raw); s != "" {
					fields = append(fields, s)
				}
			}
		}
		if len(fields) == 0 {
			return nil, fmt.Errorf(`@createOnly requires at least one field name, e.g. @createOnly("status", "attempts")`)
		}
	}
	if len(fields) > 0 && kind == ast.MutationKindUpdate {
		return nil, fmt.Errorf("@createOnly is only valid on insert mutations (an update always targets an existing row, so a create-only field could never be written)")
	}
	return fields, nil
}

// mutationScrubPii reports whether the mutation carries the @scrubPii
// annotation, which opts an update-kind mutation into engine-side
// generic PII scrubbing (memql#1711). Only meaningful on update-kind
// mutations -- an insert writes the full payload and has nothing stored
// to scrub against -- so its presence on an insert mutation is a
// load-time error rather than a silent no-op. The bound concept supplies
// the field set at execution time via Concept.PIIFields(), so no field
// names are listed on the annotation itself: that is the whole point --
// the scrub stays in sync with the schema automatically.
func mutationScrubPii(funcDef *languageParser.FunctionDef, kind ast.MutationKind) (bool, error) {
	var present bool
	for _, attr := range funcDef.Attributes {
		if attr == nil || attr.Name != languageParser.AttrScrubPii {
			continue
		}
		present = true
	}
	if present && kind != ast.MutationKindUpdate {
		return false, fmt.Errorf("@scrubPii is only valid on update mutations (insert writes the full payload; there is nothing stored to scrub against)")
	}
	return present, nil
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
	// A malformed `??` (a dangling `a ??`, a doubled `a ?? ?? b`) is
	// marked with a typed sentinel while the values are classified.
	// Refuse at LOAD: the runtime value evaluator has no parser behind
	// it, so the operator would otherwise reach it as opaque text
	// (memql#2772). Only EXPRESSION text is ever classified, so a quoted
	// literal containing `??` -- ordinary prose -- cannot trip this.
	if field, raw, bad := firstMalformedNullCoalesce(obj); bad {
		return nil, fmt.Errorf("field %q: malformed `??` in %q (an operand is missing)", field, raw)
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
		val, next, ok := parseLiteralOrExpr(inner, pos)
		if !ok {
			// Malformed value. Deliberately NO position check here: this loop
			// cannot spin, because parseObjectKey either advances or returns
			// "" (which bails), so every iteration makes progress regardless of
			// what the value scan returned. A position check WOULD change
			// `{k:}` from a null value into a load error while leaving
			// `{ k: }` -- the same input plus a space -- untouched, because
			// parseLiteralOrExpr skips that space internally and so advances.
			return nil
		}
		out[key] = val
		pos = next
	}

	return out
}

// tryParseShorthandCtx recognises `args.<bareIdent>` or `ctx.<bareIdent>`
// with no `key:` prefix in an object literal and infers the key from
// the path. Dotted suffixes (`args.user.id` / `ctx.user.id`) are
// rejected so they fall through to the normal key:value parser,
// matching the arg() restriction.
func tryParseShorthandCtx(s string, pos int) (string, string, int, bool) {
	start := pos
	for start < len(s) && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	var prefix string
	switch {
	case start+len("args.") <= len(s) && s[start:start+len("args.")] == "args.":
		prefix = "args."
	case start+len("ctx.") <= len(s) && s[start:start+len("ctx.")] == "ctx.":
		prefix = "ctx."
	default:
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

// parseLiteralOrExpr returns the parsed value, the position after it, and
// whether the input was WELL-FORMED.
//
// The ok result exists because a caller could not otherwise tell a malformed
// literal from a legitimately-empty value: both came back as `(nil, pos)`
// (memql#2785). That ambiguity is why a first cut of the fix turned the
// issue's own repro from a hang into a SILENT NULL -- `{ k: [}{] }` loaded
// `k: null` with err=nil instead of failing the strict-boot gate. A nested
// object/array that fails to parse propagates ok=false rather than passing its
// nil up as a value.
func parseLiteralOrExpr(s string, pos int) (any, int, bool) {
	// skip whitespace
	for pos < len(s) && (s[pos] == ' ' || s[pos] == '\t' || s[pos] == '\n' || s[pos] == '\r') {
		pos++
	}
	if pos >= len(s) {
		// Empty remainder -- `{k:}` is not malformed, it is a null value.
		return nil, pos, true
	}

	switch s[pos] {
	case '{':
		obj, next := scanBalanced(s, pos, '{', '}')
		if next == pos {
			return nil, pos, false
		}
		parsed := parseObjectLiteral(obj)
		if parsed == nil {
			return nil, pos, false
		}
		return parsed, next, true
	case '[':
		arr, next := scanBalanced(s, pos, '[', ']')
		if next == pos {
			return nil, pos, false
		}
		parsed := parseArrayLiteral(arr)
		if parsed == nil {
			return nil, pos, false
		}
		return parsed, next, true
	case '"':
		lit, next, ok := scanQuotedString(s, pos)
		if !ok {
			return nil, pos, false
		}
		return lit, next, true
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
				// Brace and bracket openers count toward depth exactly
				// as parens do (memql#2772). Without them a literal arm
				// in an unparenthesised expression ended the value at
				// its own closer: `ctx.labels ?? {}` truncated to
				// `ctx.labels ?? {`. The `coalesce(ctx.labels, {})`
				// spelling never hit this because its braces sat inside
				// the call's parens.
				case '(', '{', '[':
					depth++
				case ')':
					if depth > 0 {
						depth--
					}
				case ',':
					if depth == 0 {
						raw := strings.TrimSpace(s[start:pos])
						return classifyScalarOrExpr(raw), pos, true
					}
				case '}', ']':
					if depth == 0 {
						raw := strings.TrimSpace(s[start:pos])
						return classifyScalarOrExpr(raw), pos, true
					}
					depth--
				}
			}
			pos++
		}
		raw := strings.TrimSpace(s[start:pos])
		return classifyScalarOrExpr(raw), pos, true
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
	// Non-nil from the start: nil is this parser's "malformed" signal, and an
	// input of only separators (`[,]`) appends nothing while being perfectly
	// well-formed. Leaving it nil made the ok-propagation read it as a parse
	// failure (memql#2785 review).
	out := []any{}
	pos := 0
	for pos < len(inner) {
		// skip whitespace/commas
		for pos < len(inner) && (inner[pos] == ' ' || inner[pos] == '\t' || inner[pos] == '\n' || inner[pos] == '\r' || inner[pos] == ',') {
			pos++
		}
		if pos >= len(inner) {
			break
		}
		val, next, ok := parseLiteralOrExpr(inner, pos)
		if !ok || next <= pos {
			// No progress. parseLiteralOrExpr returns its input pos unchanged
			// on an unbalanced literal (`[}{]`) and on a stray depth-0 closer
			// (`[}]`), so without this the loop appends nil forever -- an
			// unbounded memory grow and a hang, not a panic (memql#2785). It
			// matters at LOAD: parsePayloadRawToTemplate runs while the tree is
			// read, so a malformed payload literal in an authored .memql file
			// would hang the node at boot instead of failing the strict-boot
			// gate.
			//
			// The position check is safe to treat as malformed HERE (unlike in
			// parseObjectLiteral) because the loop's leading skip has already
			// consumed commas and whitespace, so a non-advancing return cannot
			// be a legitimately-empty element.
			//
			// `!ok` decides NOTHING today and is deliberately kept: every
			// ok=false return in parseLiteralOrExpr returns the
			// whitespace-skipped pos, and this loop's leading skip has already
			// consumed that whitespace, so !ok always implies next == pos and
			// the position check alone would suffice. It stays because that is
			// a property of a DIFFERENT function -- if parseLiteralOrExpr ever
			// grows an ok=false path that DOES advance, the position check
			// silently stops covering it and a malformed element becomes a
			// silent nil. TestArrayOkImpliesNoProgress pins the invariant so
			// the day it changes is a red test rather than a silent wrong
			// value. (Contrast the two `newPos < pos` guards deleted in this
			// same change: those were unreachable by construction within one
			// expression, so nothing could ever revive them.)
			return nil
		}
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
	case "now":
		// Bare `now` is the reserved engine identifier for the current
		// timestamp. It reaches here from the UNQUOTED object-literal
		// (mutation insert/update value) path -- a quoted "now" comes through
		// scanQuotedString and is never reclassified -- so mapping it to the
		// canonical call form lets the existing evalString("now()") path stamp
		// a real timestamp at render time. Without this, an authored
		// `createdAt: now` was stored as the literal string "now" and rejected
		// by date-time validation (memql#1629). The `timestamp` spelling is
		// retired (epic #2298 / #2301): only bare `now` is the clock; a bare
		// `timestamp` is now an ordinary identifier. Quoted "now" string
		// literals are preserved.
		return raw + "()"
	}
	if i, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return i
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		return f
	}
	// Store the lowered coalesce() spelling so the runtime value
	// evaluator -- a string-prefix dispatcher over a closed set of forms
	// -- never meets the `??` token (memql#2772). No-op when the value
	// carries no live `??`; yields a malformedNullCoalesce sentinel when
	// an operand is missing, which parsePayloadRawToTemplate refuses.
	return lowerNullCoalesceValue(raw)
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
