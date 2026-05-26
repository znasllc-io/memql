package memql

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	busv1 "github.com/znasllc-io/memql/component/bus/gen"
	"github.com/znasllc-io/memql/component/config"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// jsonMarshal is a thin wrapper that swallows errors — for trace
// persistence we never want a marshal failure to crash the eval; we
// just write an empty string and the caller logs.
func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

// mustJSONMarshal returns the JSON encoding of v, or "{}" on error.
// Used for the inline mutation argument literal where producing an
// invalid JSON shape would break the mutation parse.
func mustJSONMarshal(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// sha256ArgsHash returns the SHA-256 hex of the canonicalised JSON
// encoding of the args map. Used for cache lookup and as the audit
// correlation key on v1:platform:policyTrace rows.
func sha256ArgsHash(args map[string]any) string {
	b, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// PolicyEvalOptions configures a single EvaluatePolicy call.
type PolicyEvalOptions struct {
	// RequiredTier — when non-empty, the policy's tier must match.
	// Auth interceptors pass "core"; gRPC handlers pass "bff".
	// Empty allows any tier.
	RequiredTier string

	// PersistTrace — when true, overrides @traces_persisted for this
	// evaluation and writes a v1:platform:policyTrace row. Phase 7
	// wires the actual write; Phase 3 just plumbs the flag.
	PersistTrace bool

	// ReturnTrace — when true, the result trace is always returned to
	// the caller (overriding @returns_trace).
	ReturnTrace bool
}

// PolicyTrace is the structured trace tree captured during a single
// EvaluatePolicy run. Serialisable to JSON for persistence + the
// gRPC EvaluatePolicy reply path.
type PolicyTrace struct {
	Name        string         `json:"name"`
	Tier        string         `json:"tier"`
	Args        map[string]any `json:"args,omitempty"`
	Result      any            `json:"result,omitempty"`
	DurationMs  float64        `json:"durationMs"`
	StartedAt   time.Time      `json:"startedAt"`
	Subcalls    []*PolicyTrace `json:"subcalls,omitempty"`
	Breadcrumbs []string       `json:"breadcrumbs,omitempty"`
	Error       string         `json:"error,omitempty"`
}

// policyEvalState is the per-evaluation bookkeeping carried via
// context.Context. Used for cycle detection across nested
// policy() calls and for the parent->child trace linkage.
type policyEvalState struct {
	mu          sync.Mutex
	inFlight    map[string]struct{} // policy names currently being evaluated
	parentTrace *PolicyTrace        // when set, sub-call traces append here
}

type policyEvalStateKey struct{}

func withPolicyEvalState(ctx context.Context, state *policyEvalState) context.Context {
	return context.WithValue(ctx, policyEvalStateKey{}, state)
}

func policyEvalStateFrom(ctx context.Context) *policyEvalState {
	if v, ok := ctx.Value(policyEvalStateKey{}).(*policyEvalState); ok {
		return v
	}
	return nil
}

// EvaluatePolicy looks up the named policy under
// engine.PolicyFunctions(), evaluates its body against the caller-
// supplied args + engine-provided ctx fields, and returns the
// computed result plus the structured trace tree.
//
// Phase 3 ships the core evaluation loop:
//   - tier check (opts.RequiredTier)
//   - effective ctx assembly (caller args + caller fields)
//   - body expression evaluation
//   - cycle detection at runtime
//   - trace tree capture
//
// Cache (@cacheable), persisted traces (@traces_persisted), audit
// rows (@audited), and config-allow-list (ctx.config.*) wire in via
// later commits inside the same phase plan.
func (e *MemQLEngine) EvaluatePolicy(ctx context.Context, name string, args map[string]any, opts PolicyEvalOptions) (any, *PolicyTrace, error) {
	if e == nil || e.policyFunctions == nil {
		return nil, nil, fmt.Errorf("policy engine not initialised")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, nil, fmt.Errorf("policy name is required")
	}

	policy, ok := e.policyFunctions.Lookup(name)
	if !ok {
		return nil, nil, fmt.Errorf("policy %q is not registered", name)
	}
	if opts.RequiredTier != "" && !strings.EqualFold(opts.RequiredTier, policy.Tier) {
		return nil, nil, fmt.Errorf("policy %q has tier %q but caller required %q", name, policy.Tier, opts.RequiredTier)
	}

	state := policyEvalStateFrom(ctx)
	if state == nil {
		state = &policyEvalState{inFlight: map[string]struct{}{}}
		ctx = withPolicyEvalState(ctx, state)
	}

	state.mu.Lock()
	if _, busy := state.inFlight[name]; busy {
		state.mu.Unlock()
		return nil, nil, fmt.Errorf("policy %q is already in-flight; cycle detected", name)
	}
	state.inFlight[name] = struct{}{}
	state.mu.Unlock()

	defer func() {
		state.mu.Lock()
		delete(state.inFlight, name)
		state.mu.Unlock()
	}()

	trace := &PolicyTrace{
		Name:      name,
		Tier:      policy.Tier,
		Args:      cloneArgsForTrace(args),
		StartedAt: time.Now().UTC(),
	}

	// Build the effective ctx the body sees. Caller-passed args are
	// merged with engine-provided fields. Sensitive caller fields are
	// resolved here rather than at body-parse time.
	effective := buildPolicyCtx(ctx, e, args)

	// Convert the language-parser AST body into the engine's
	// ExpressionNode. Same conversion path used by specs.
	bodyExpr, ok := policy.FunctionDef.Body.(languageParser.ExpressionNode)
	if !ok || bodyExpr == nil {
		trace.Error = fmt.Sprintf("policy %q has no evaluable body", name)
		trace.DurationMs = msSince(trace.StartedAt)
		return nil, trace, errors.New(trace.Error)
	}

	converter := NewASTConverter()
	memqlExpr, err := converter.ConvertExpression(bodyExpr)
	if err != nil {
		trace.Error = fmt.Sprintf("convert body: %v", err)
		trace.DurationMs = msSince(trace.StartedAt)
		return nil, trace, fmt.Errorf("convert policy %q body: %w", name, err)
	}

	// Set this trace as the parent for any sub-policy() calls made
	// while the body evaluates. The DSL builtin (Phase 4) reads this
	// to append its child trace.
	prevParent := state.parentTrace
	state.mu.Lock()
	state.parentTrace = trace
	state.mu.Unlock()
	defer func() {
		state.mu.Lock()
		state.parentTrace = prevParent
		state.mu.Unlock()
	}()

	result, evalErr := evaluatePolicyExpressionWithCtx(ctx, e, memqlExpr, effective)
	trace.Result = result
	trace.DurationMs = msSince(trace.StartedAt)
	if evalErr != nil {
		trace.Error = evalErr.Error()
	}

	// Stamp the ctx envelope post-eval: the body's return value is
	// ctx.output, the error message lands on ctx.error. The shorthand
	// `ctx.X` / canonical `ctx.input.X` reads remain unchanged from
	// pre-body state. Sub-policy callers continue to see the raw
	// value at value position (no DSLCtx wrapper); explicit
	// `.output` / `.input` / `.error` chained access on a sub-call
	// is deferred to Phase C when the parser learns that path.
	effective["output"] = result
	if evalErr != nil {
		effective["error"] = evalErr.Error()
	}

	// Phase 7 persistence path: when the policy carries
	// @traces_persisted, or the caller passed PersistTrace=true on
	// the options, write the trace tree to v1:platform:policyTrace
	// via the mutationInsertPolicyTrace DSL function. We persist
	// only at the top of the eval stack (not inside nested sub-
	// policy() calls) so one logical evaluation produces exactly
	// one row, with the full nested trace JSON.
	if policy.TracesPersisted || opts.PersistTrace {
		if isTopLevelPolicyEval(state) {
			persistPolicyTrace(ctx, e, policy, args, effective, trace)
		}
	}

	if evalErr != nil {
		return nil, trace, evalErr
	}
	return result, trace, nil
}

// isTopLevelPolicyEval reports whether the current policyEvalState
// has exactly one in-flight policy — the one we're about to
// return from. Sub-policy calls push their name into the map and
// pop on defer, so a >1 count means we're inside another policy's
// body. Persistence runs only at the outermost level.
func isTopLevelPolicyEval(state *policyEvalState) bool {
	if state == nil {
		return true
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return len(state.inFlight) == 1
}

// persistPolicyTrace fires-and-logs a mutationInsertPolicyTrace call
// via engine.Execute. Failures are logged at warn level — we never
// want a debug-trace persistence error to bubble up and break the
// caller's main policy decision.
func persistPolicyTrace(
	ctx context.Context,
	e *MemQLEngine,
	policy *PolicyFunction,
	rawArgs map[string]any,
	effective map[string]any,
	trace *PolicyTrace,
) {
	resultJSON, _ := jsonMarshal(trace.Result)
	traceJSON, _ := jsonMarshal(trace)
	argsHash := sha256ArgsHash(rawArgs)
	actor, _ := effective["actor"].(map[string]any)
	actorUserId, _ := actor["userId"].(string)
	actorRole, _ := actor["role"].(string)
	partition, _ := effective["partition"].(string)

	insertCall := map[string]any{
		"policyName":      policy.Name,
		"tier":            policy.Tier,
		"actorUserId":     actorUserId,
		"actorRole":       actorRole,
		"callerPartition": partition,
		"argsHash":        argsHash,
		"resultJson":      string(resultJSON),
		"traceJson":       string(traceJSON),
		"durationMs":      trace.DurationMs,
		"error":           trace.Error,
	}
	query := fmt.Sprintf(`mutationInsertPolicyTrace(%s)`, mustJSONMarshal(insertCall))
	if _, err := e.Execute(ctx, query); err != nil && e.Logger != nil {
		e.Logger.Warn("policy trace persistence failed",
			"component", ComponentName,
			"policy", policy.Name,
			"error", err.Error(),
		)
	}
}

// buildPolicyCtx assembles the ctx envelope a policy body sees.
//
// Shape (the canonical ctx envelope used across every procedural DSL
// receiver under the ctx-envelope rule):
//
//	ctx.input       caller-passed args (map[string]any)
//	ctx.output      filled by the body; starts nil
//	ctx.error       error string when set; "" otherwise
//	ctx.actor       resolved AccessContext fields (userId / role / ...)
//	ctx.partition   active partition string
//	ctx.now         engine-monotonic timestamp
//	ctx.config      allow-listed config surface (see PolicyExposableConfig)
//	ctx.trace       reserved for future breadcrumb access
//
// In addition, every caller-passed arg key is mirrored at the top
// level so existing files written under the prior convention
// (`ctx.spaceId`, `ctx.vendor`) keep working — `ctx.X` for unknown X
// resolves to `ctx.input.X` via the path-resolution fallback in
// lookupPath. New code should prefer the explicit `ctx.input.X`
// form.
func buildPolicyCtx(ctx context.Context, engine *MemQLEngine, args map[string]any) map[string]any {
	if args == nil {
		args = map[string]any{}
	}
	out := make(map[string]any, len(args)+8)

	// ctx.input — canonical caller args.
	out["input"] = args
	// ctx.output / ctx.error — body fills via assignment under the
	// new convention; the engine reads them post-eval and exposes to
	// callers. Default to nil / empty.
	out["output"] = nil
	out["error"] = ""

	// Mirror caller args at the top level for back-compat with the
	// shorthand `ctx.X` form. The path resolver also auto-falls-back
	// to ctx.input.X when X isn't a known engine field, so both reads
	// land on the same value.
	for k, v := range args {
		// Don't shadow engine fields if a caller happens to pass an
		// arg keyed "input" / "output" / "actor" / etc. — that would
		// confuse the resolver. Skip reserved names from the mirror;
		// the explicit ctx.input.<name> path still resolves them.
		if isReservedCtxKey(k) {
			continue
		}
		out[k] = v
	}

	// actor.* — pulled from auth context if present.
	actor := map[string]any{}
	if access, ok := auth.AccessFromContext(ctx); ok {
		actor["userId"] = access.UserId
		actor["role"] = string(access.Role)
		actor["identityId"] = access.IdentityId
		actor["isClusterOwner"] = access.IsClusterOwner()
	}
	out["actor"] = actor
	out["partition"] = currentPartitionFromContext(ctx)
	out["now"] = time.Now().UTC().Format(time.RFC3339Nano)

	// ctx.config.* — backed by the PolicyExposableConfig allow-list.
	// Sensitive keys collapse to a presence-only bool; non-sensitive
	// keys expose the raw value. Missing snapshot (tests, dev mode)
	// yields the zero-value map.
	var snapshot *busv1.ConfigSnapshot
	if engine != nil {
		if raw := engine.ConfigSnapshot(); raw != nil {
			snapshot, _ = raw.(*busv1.ConfigSnapshot)
		}
	}
	out["config"] = config.BuildPolicyConfigCtx(snapshot)
	return out
}

// reservedCtxKeys is the set of top-level ctx field names the engine
// owns. Caller-passed args sharing one of these names are NOT
// mirrored to the top level; readers reach them via the explicit
// ctx.input.<name> path.
var reservedCtxKeys = map[string]struct{}{
	"input":     {},
	"output":    {},
	"error":     {},
	"actor":     {},
	"partition": {},
	"now":       {},
	"config":    {},
	"trace":     {},
}

func isReservedCtxKey(key string) bool {
	_, ok := reservedCtxKeys[strings.ToLower(strings.TrimSpace(key))]
	return ok
}

func currentPartitionFromContext(ctx context.Context) string {
	// AccessContext doesn't carry the active partition directly —
	// the gRPC envelope's partition is the source of truth. Phase 3
	// leaves this empty; Phase 5's gRPC handler will pass the active
	// partition through the call options once wired.
	_ = ctx
	return ""
}

func cloneArgsForTrace(args map[string]any) map[string]any {
	if len(args) == 0 {
		return nil
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		out[k] = v
	}
	return out
}

func msSince(start time.Time) float64 {
	return float64(time.Since(start).Microseconds()) / 1000.0
}

// evaluatePolicyExpression walks the engine ExpressionNode AST and
// produces a plain Go value, resolving ArgReference / ActorReference
// against the effective ctx. The supported surface is intentionally
// small — enough for the Phase 9 consumers (avatar vendor, role
// check, UI gating). Richer constructs come as needed.
//
// The evaluator threads the same context.Context through nested
// policy() calls so cycle detection, the parent trace pointer, and
// the auth.AccessContext propagate automatically.
func evaluatePolicyExpression(expr ExpressionNode, ctx map[string]any) (any, error) {
	return evaluatePolicyExpressionWithCtx(context.Background(), nil, expr, ctx)
}

func evaluatePolicyExpressionWithCtx(goCtx context.Context, engine *MemQLEngine, expr ExpressionNode, ctx map[string]any) (any, error) {
	if expr == nil {
		return nil, nil
	}
	switch node := expr.(type) {
	case *ComparisonExpression:
		return evaluatePolicyComparison(node, ctx)
	case *LogicalExpression:
		return evaluatePolicyLogicalCtx(goCtx, engine, node, ctx)
	case *FunctionCallExpression:
		return evaluatePolicyFunctionCall(goCtx, engine, node, ctx)
	case *BuiltinFunctionExpression:
		return nil, fmt.Errorf("builtin function %q is not evaluable inside a policy body yet", node.Name)
	}
	return nil, fmt.Errorf("unsupported expression type %T in policy body", expr)
}

// evaluatePolicyFunctionCall handles function-call-shaped expressions
// inside a policy body. Supported calls:
//
//   - `policy("<name>", { ... })`  — recurses through EvaluatePolicy
//     so cycle detection, tier checks,
//     and trace propagation reuse the
//     existing machinery.
//   - `spec("<name>")`             — context-spec lookup; runs the
//     spec's body in-process against
//     the auth context and returns a
//     bool. Row-specs are rejected.
//
// Any other call name is an error — the policy body grammar doesn't
// admit ad-hoc DSL function calls.
func evaluatePolicyFunctionCall(goCtx context.Context, engine *MemQLEngine, call *FunctionCallExpression, ctx map[string]any) (any, error) {
	if call == nil {
		return nil, nil
	}
	name := strings.ToLower(strings.TrimSpace(call.Name))
	switch name {
	case "policy":
		return evaluatePolicySubCall(goCtx, engine, call)
	case "spec":
		return evaluateSpecSubCall(goCtx, engine, call)
	default:
		return nil, fmt.Errorf("function call %q is not callable from a policy body (expected `policy(...)` or `spec(...)`)", call.Name)
	}
}

// evaluatePolicySubCall handles `policy("name", { args })` inside
// a policy body. Returns the inner policy's result.
func evaluatePolicySubCall(goCtx context.Context, engine *MemQLEngine, call *FunctionCallExpression) (any, error) {
	if engine == nil {
		return nil, fmt.Errorf("policy() builtin requires an engine context; call via engine.EvaluatePolicy(...)")
	}
	rawName, ok := call.Args["0"]
	if !ok {
		return nil, fmt.Errorf("policy() requires the policy name as the first argument")
	}
	name, ok := rawName.(string)
	if !ok {
		return nil, fmt.Errorf("policy() first argument must be a string, got %T", rawName)
	}
	var subArgs map[string]any
	if rawArgs, ok := call.Args["1"]; ok {
		if m, ok := rawArgs.(map[string]any); ok {
			subArgs = m
		}
	}
	result, subTrace, err := engine.EvaluatePolicy(goCtx, name, subArgs, PolicyEvalOptions{})
	if state := policyEvalStateFrom(goCtx); state != nil {
		state.mu.Lock()
		if state.parentTrace != nil && subTrace != nil {
			state.parentTrace.Subcalls = append(state.parentTrace.Subcalls, subTrace)
		}
		state.mu.Unlock()
	}
	if err != nil {
		return nil, err
	}
	return result, nil
}

// evaluateSpecSubCall handles `spec("name")` inside a policy body.
// Routes to engine.EvaluateSpec which enforces that the named spec
// is a context-spec (row-specs belong in query filters). Returns
// the spec's boolean result.
func evaluateSpecSubCall(goCtx context.Context, engine *MemQLEngine, call *FunctionCallExpression) (any, error) {
	if engine == nil {
		return nil, fmt.Errorf("spec() builtin requires an engine context; call via engine.EvaluateSpec(...)")
	}
	rawName, ok := call.Args["0"]
	if !ok {
		return nil, fmt.Errorf("spec() requires the spec name as the first argument")
	}
	name, ok := rawName.(string)
	if !ok {
		return nil, fmt.Errorf("spec() first argument must be a string, got %T", rawName)
	}
	return engine.EvaluateSpec(goCtx, name)
}

func evaluatePolicyLogicalCtx(goCtx context.Context, engine *MemQLEngine, expr *LogicalExpression, ctx map[string]any) (any, error) {
	if expr == nil {
		return nil, nil
	}
	lhs, err := evaluatePolicyExpressionWithCtx(goCtx, engine, expr.Left, ctx)
	if err != nil {
		return nil, err
	}
	if expr.Op == LogicalAnd {
		if !policyTruthy(lhs) {
			return false, nil
		}
		rhs, err := evaluatePolicyExpressionWithCtx(goCtx, engine, expr.Right, ctx)
		if err != nil {
			return nil, err
		}
		return policyTruthy(rhs), nil
	}
	if expr.Op == LogicalOr {
		if policyTruthy(lhs) {
			return true, nil
		}
		rhs, err := evaluatePolicyExpressionWithCtx(goCtx, engine, expr.Right, ctx)
		if err != nil {
			return nil, err
		}
		return policyTruthy(rhs), nil
	}
	return nil, fmt.Errorf("unsupported logical operator %v in policy body", expr.Op)
}

func evaluatePolicyComparison(expr *ComparisonExpression, ctx map[string]any) (any, error) {
	if expr == nil {
		return nil, nil
	}
	lhs, err := evaluatePolicyFieldRef(expr.Field, ctx)
	if err != nil {
		return nil, err
	}
	rhs, err := evaluatePolicyValue(expr.Value, ctx)
	if err != nil {
		return nil, err
	}
	switch expr.Operator {
	case OpEq:
		return policyEqual(lhs, rhs), nil
	case OpNe:
		return !policyEqual(lhs, rhs), nil
	}
	return nil, fmt.Errorf("operator %v not supported in policy bodies yet", expr.Operator)
}

func evaluatePolicyLogical(expr *LogicalExpression, ctx map[string]any) (any, error) {
	if expr == nil {
		return nil, nil
	}
	lhs, err := evaluatePolicyExpression(expr.Left, ctx)
	if err != nil {
		return nil, err
	}
	if expr.Op == LogicalAnd {
		if !policyTruthy(lhs) {
			return false, nil
		}
		rhs, err := evaluatePolicyExpression(expr.Right, ctx)
		if err != nil {
			return nil, err
		}
		return policyTruthy(rhs), nil
	}
	if expr.Op == LogicalOr {
		if policyTruthy(lhs) {
			return true, nil
		}
		rhs, err := evaluatePolicyExpression(expr.Right, ctx)
		if err != nil {
			return nil, err
		}
		return policyTruthy(rhs), nil
	}
	return nil, fmt.Errorf("unsupported logical operator %v in policy body", expr.Op)
}

func evaluatePolicyFieldRef(ref FieldReference, ctx map[string]any) (any, error) {
	if len(ref.Parts) == 0 {
		return nil, fmt.Errorf("empty field reference in policy body")
	}
	first := strings.ToLower(strings.TrimSpace(ref.Parts[0]))
	switch first {
	case "payload", "ctx", "args":
		// The parser normalises ctx → payload in field-reference
		// position; `args` is the author-facing form. All three
		// resolve to the policy ctx.
		return lookupPath(ctx, ref.Parts[1:]), nil
	case "actor":
		actor, _ := ctx["actor"].(map[string]any)
		if actor == nil {
			return nil, nil
		}
		return lookupPath(actor, ref.Parts[1:]), nil
	case "caller":
		return nil, fmt.Errorf("caller.X is retired (#221) -- use actor.X (the same auth-context envelope, one canonical spelling)")
	}
	return nil, fmt.Errorf("field reference %q not supported in policy body", ref.Raw)
}

func evaluatePolicyValue(value any, ctx map[string]any) (any, error) {
	switch v := value.(type) {
	case *ArgReference:
		return lookupPath(ctx, strings.Split(v.Path, ".")), nil
	case *ActorReference:
		actor, _ := ctx["actor"].(map[string]any)
		if actor == nil {
			return nil, nil
		}
		return lookupPath(actor, strings.Split(v.Path, ".")), nil
	}
	return value, nil
}

// lookupPath walks a dotted path against the ctx envelope. The
// resolver supports both the canonical `ctx.input.X` form and the
// shorthand `ctx.X` form: when the first path segment isn't a
// reserved engine field (input / output / error / actor / partition
// / now / config / trace), the resolver implicitly walks into
// ctx.input.X. This preserves every existing .memql file written
// under the prior convention while making `ctx.input.X` explicit
// for new code.
func lookupPath(root map[string]any, parts []string) any {
	if root == nil || len(parts) == 0 {
		return nil
	}
	first := strings.ToLower(strings.TrimSpace(parts[0]))
	// Shorthand: `ctx.X` for X that isn't a known engine field
	// reads from ctx.input.X. The canonical form `ctx.input.X` also
	// works because parts[0]=="input" walks into ctx["input"]
	// directly via the regular path-walk below.
	if _, reserved := reservedCtxKeys[first]; !reserved {
		if _, hasInput := root["input"].(map[string]any); hasInput {
			if v, ok := root["input"].(map[string]any)[parts[0]]; ok {
				if len(parts) == 1 {
					return v
				}
				// Continue walking from the input-rooted value.
				return walkValuePath(v, parts[1:])
			}
		}
	}
	return walkValuePath(any(root), parts)
}

// walkValuePath descends parts segments into a value (root may be a
// map; intermediate steps must also be maps). Returns nil on a miss.
func walkValuePath(root any, parts []string) any {
	var current = root
	for _, p := range parts {
		seg := strings.TrimSpace(p)
		if seg == "" {
			return nil
		}
		m, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = m[seg]
	}
	return current
}

func policyEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == b
	}
	// Stringify both sides for the common cross-type comparison
	// (role enums vs strings, ints vs strings) and compare. Phase 3
	// keeps this minimal; richer numeric coercion lands when a
	// consumer needs it.
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

func policyTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return strings.TrimSpace(t) != ""
	case int, int32, int64, float32, float64:
		return fmt.Sprintf("%v", t) != "0"
	}
	return true
}
