package automations

// run_scope.go -- an automation run's state as its expressions read it (epic
// memql#5363, memql#5367, memql#5370).
//
// RunScope is a memql.ExprScope over the run's *Evaluator (run_state.go),
// which the executor, resume and the LogicRunner seed. Every expression of a
// run -- a condition, an argument, a source, a filter -- is evaluated by
// EvalExpr over it.
//
// A bare name resolves in two tiers:
//
//  1. inside a body, the names its statements bound (statement_scope.go):
//     the frame of the list running, then the frames around it -- a name a
//     statement is declared to bind and did not reads absent;
//  2. the roots the run binds before its first statement: `args`, `actor`,
//     `event` (an automation's trigger), `config` and `partition`. `actor`,
//     when the run seeded none, is the DENYING envelope -- the one a request
//     with no auth context binds (auth.ActorEnvelopeMap(nil)): owner bits
//     false, identity empty. Absent would make `actor.isClusterOwner !=
//     false` true under the absence table, the fail-open memql#2801 closed.
//     Any other root the run did not seed is absent (memql#2851). `now` is
//     left to EvalExpr, which answers it from the run clock.
//
// Anything else is an unknown name, which EvalExpr refuses (unknown_name)
// instead of reading the name as its own text. A trigger filter and a
// precondition read the roots alone: they are evaluated before a statement
// binds anything.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/memql"
)

// RunScope is a run's state as a memql.ExprScope. It is cheap to build: one
// per evaluation.
type RunScope struct {
	e *Evaluator
}

// RunScope returns the run's state as a memql.ExprScope.
func (e *Evaluator) RunScope() *RunScope {
	return &RunScope{e: e}
}

// Lookup resolves one bare name (see the file comment for the order).
func (s *RunScope) Lookup(name string) (any, bool) {
	e := s.e
	if e == nil || name == "" {
		return nil, false
	}
	if e.names != nil {
		if v, ok := e.names.lookup(name); ok {
			return v, true
		}
	}
	switch name {
	case "row":
		v, ok := e.custom[name]
		return v, ok
	case "actor":
		if v, ok := e.custom[name]; ok {
			return v, true
		}
		return auth.ActorEnvelopeMap(nil), true
	case "args", "event", "config", "partition":
		if v, ok := e.custom[name]; ok {
			return v, true
		}
		return memql.Absent, true
	}
	return nil, false
}

// variableResolverFor returns the resolver behind a var-family function.
func (e *Evaluator) variableResolverFor(kind string) VariableResolver {
	switch kind {
	case "var":
		return e.variableResolver
	case "systemVar":
		return e.systemVariableResolver
	case "secret":
		return e.secretResolver
	case "systemSecret":
		return e.systemSecretResolver
	}
	return nil
}

// ---------------------------------------------------------------------------
// evaluating over the run
// ---------------------------------------------------------------------------

// ExprOptions is the memql.EvalOptions of an evaluation over this run: the
// run clock, the variable resolvers and the canonicalId resolver the run was
// seeded with. Predicates stays nil until the engine exposes spec and trait
// bodies as v1 lambdas; a predicate application then fails as an unknown
// function rather than meaning something by accident.
func (e *Evaluator) ExprOptions() memql.EvalOptions {
	if e == nil {
		// No run: a literal evaluates, and every name is unknown.
		return memql.EvalOptions{}
	}
	opts := memql.EvalOptions{
		Now: e.runClock(),
		Vars: func(ctx context.Context, kind, name string) (string, error) {
			r := e.variableResolverFor(kind)
			if r == nil {
				return "", fmt.Errorf("%s(%q): this run has no %s resolver", kind, name, kind)
			}
			return r(ctx, name)
		},
	}
	if resolve := e.canonicalIdResolver; resolve != nil {
		opts.CanonicalID = func(ctx context.Context, value any, concept string) (string, error) {
			return resolve(ctx, V1Text(value), concept)
		}
	}
	return opts
}

// runClock is the run's one clock: the `now` the executor, resume and the
// LogicRunner seed. Zero (unseeded) lets EvalExpr read the wall clock.
func (e *Evaluator) runClock() time.Time {
	if s, ok := e.custom["now"].(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// EvalV1 evaluates one parsed v1 expression over the run.
func (e *Evaluator) EvalV1(ctx context.Context, n ast.ExpressionNode) (any, error) {
	return memql.EvalExpr(ctx, n, e.RunScope(), e.ExprOptions())
}

// EvalV1Condition evaluates one parsed v1 condition over the run: a boolean,
// with absent read as false, a stored row field of another type not true, and
// anything else refused (condition_not_boolean).
func (e *Evaluator) EvalV1Condition(ctx context.Context, n ast.ExpressionNode) (bool, error) {
	return memql.EvalCondition(ctx, n, e.RunScope(), e.ExprOptions())
}

// StepCondition decides whether a step runs: its condition, parsed at load,
// through EvalCondition over the run. A step without a condition runs, and
// a condition its automation never prepared is refused -- there is nothing
// else to read its text with.
func (e *Evaluator) StepCondition(ctx context.Context, step *Step) (bool, error) {
	if step == nil {
		return true, nil
	}
	if step.Exprs != nil && step.Exprs.Condition != nil {
		return e.EvalV1Condition(ctx, step.Exprs.Condition)
	}
	if strings.TrimSpace(step.Condition) != "" {
		return false, fmt.Errorf("step %q: its condition was never prepared (automations.PrepareExpressions)", step.ID)
	}
	return true, nil
}

// ResolveV1Value resolves a v1 value: an *ExprLeaf evaluates, a map or list
// resolves each entry, and anything else is a literal and is returned as it
// is. CONTAINER RULE (rule 30's last paragraph, memql#3627): an entry whose
// expression evaluates to the Absent sentinel contributes nothing -- its key
// or element is omitted -- while an explicit nil is kept. A TOP-LEVEL absent
// value is returned as memql.Absent for the caller to decide about.
func (e *Evaluator) ResolveV1Value(ctx context.Context, v any) (any, error) {
	out, err := resolveV1(ctx, v, e.RunScope(), e.ExprOptions())
	if err != nil {
		return out, err
	}
	// A statement body's rows leave it as the maps they arrived as.
	return unwrapStatementValue(out), nil
}

// ResolveV1Map resolves a v1 value map (see ResolveV1Value). Nil stays nil.
func (e *Evaluator) ResolveV1Map(ctx context.Context, m map[string]any) (map[string]any, error) {
	if m == nil {
		return nil, nil
	}
	v, err := resolveV1(ctx, m, e.RunScope(), e.ExprOptions())
	if err != nil {
		return nil, err
	}
	if e.statementMode() {
		// A statement body's rows leave it as the maps they arrived as.
		v = unwrapStatementValue(v)
	}
	out, _ := v.(map[string]any)
	return out, nil
}

func resolveV1(ctx context.Context, v any, scope *RunScope, opts memql.EvalOptions) (any, error) {
	switch x := v.(type) {
	case *ExprLeaf:
		val, err := memql.EvalExpr(ctx, x.Node, scope, opts)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", x.Src, err)
		}
		return val, nil
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, item := range x {
			r, err := resolveV1(ctx, item, scope, opts)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			if r == memql.Absent {
				continue
			}
			out[k] = r
		}
		return out, nil
	case []any:
		out := make([]any, 0, len(x))
		for i, item := range x {
			r, err := resolveV1(ctx, item, scope, opts)
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			if r == memql.Absent {
				continue
			}
			out = append(out, r)
		}
		return out, nil
	}
	return v, nil
}

// V1Text is a v1 value as the text a string-typed position takes (a topic, a
// URL, an id): absent is "", and everything else is FormatValue's rendering.
func V1Text(v any) string {
	if memql.IsAbsent(v) {
		return ""
	}
	return FormatValue(v)
}

// ---------------------------------------------------------------------------
// the trigger filter
// ---------------------------------------------------------------------------

// bindScope binds one name over a parent scope: a lambda parameter.
type bindScope struct {
	name   string
	value  any
	parent memql.ExprScope
}

// Lookup answers the bound name, else asks the parent.
func (s *bindScope) Lookup(name string) (any, bool) {
	if name == s.name {
		return s.value, true
	}
	return s.parent.Lookup(name)
}

// evaluateTriggerFilterV1 evaluates a v1 `@filter(row => ...)`: the lambda's
// parameter is the triggering ROW, the rest of the scope is the filter's
// run state (the event envelope, the args binding, the denying actor).
func evaluateTriggerFilterV1(lam *ast.LambdaExpr, event *events.Event, evaluator *Evaluator) (bool, error) {
	scope := &bindScope{name: lam.Params[0], value: TriggerRow(event), parent: evaluator.RunScope()}
	return memql.EvalCondition(context.Background(), lam.Body, scope, evaluator.ExprOptions())
}

// TriggerRow is the row a trigger filter's parameter binds: the triggering
// event read as a stored row.
//
// A graph CDC event (graph.node.<action>.<concept>, executor_mutation.go)
// carries `id` / `nodeId`, `concept`, `nodeType`, `actor`, `createdAt` (on
// .created), the stored payload FLATTENED onto the top level -- so a payload
// key could shadow any of those -- and the full stored payload again under
// `payload`. So the row is built from the parts that cannot be shadowed where
// that is possible: its payload is the `payload` object, its id the `nodeId`
// alias, its concept the topic's concept segment, its creator the actor the
// bus stamped in the event metadata, and its createdAt the payload's
// `createdAt`, else the event's own time (an .updated event carries none).
//
// Any other event has no row: its whole payload becomes the row's payload,
// with no intrinsics, so `row.<field>` still reads what the event carried.
func TriggerRow(event *events.Event) memql.ExprRow {
	if event == nil {
		return memql.ExprRow{}
	}
	payload := event.Payload
	stored, isGraphRow := payload["payload"].(map[string]any)
	if !strings.HasPrefix(event.Topic, "graph.node.") || !isGraphRow {
		return memql.ExprRow{Payload: payload}
	}
	row := memql.ExprRow{Payload: stored}
	if id, ok := payload["nodeId"].(string); ok {
		row.ID = id
	} else if id, ok := payload["id"].(string); ok {
		row.ID = id
	}
	if segs := strings.SplitN(event.Topic, ".", 4); len(segs) == 4 {
		row.Concept = segs[3]
	} else if c, ok := payload["concept"].(string); ok {
		row.Concept = c
	}
	if t, ok := payload["nodeType"].(string); ok {
		row.Type = t
	}
	if a := event.Metadata["actor"]; a != "" {
		row.CreatedBy = a
	} else if a, ok := payload["actor"].(string); ok {
		row.CreatedBy = a
	}
	if s, ok := payload["createdAt"].(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			row.CreatedAt = t
		}
	}
	if row.CreatedAt.IsZero() && !event.Timestamp.IsZero() {
		row.CreatedAt = event.Timestamp
	}
	return row
}
