package automations

// run_scope.go -- an automation run's state as its expressions read it (epic
// memql#5363, memql#5367).
//
// RunScope is a memql.ExprScope over the run's *Evaluator (run_state.go),
// which the executor, resume, the forEach clone and the LogicRunner seed.
// Every expression of a run -- a condition, an argument, a source, a filter --
// is evaluated by EvalExpr over it.
//
// Name resolution follows the order the automations runtime has always
// served, root by root:
//
//  1. the forEach loop variable, under its own name (`item`, or `as`);
//  2. the fixed roots: `item` (the current item; nil outside a loop),
//     `steps`, `input`, `automation` (its `errors`), and `var` / `systemVar`
//     / `secret` / `systemSecret` (`var.NAME` reads the resolver, and an
//     unresolved one is absent, memql#2851);
//  3. every root the run seeded: `event` (the envelope), `ctx`, `args`,
//     `actor`, `config`, `timestamp`, `error` (onError), `index` (forEach),
//     `partition` / `now` (the LogicRunner's ambient envelope);
//  4. `actor`, when the run seeded none, is the DENYING envelope -- the one a
//     request with no auth context binds (auth.ActorEnvelopeMap(nil)): owner
//     bits false, identity empty. Absent would make `actor.isClusterOwner !=
//     false` true under the absence table, the fail-open memql#2801 closed.
//     It is answered here, before any later tier can bind the name: the
//     event envelope carries an `actor` key of its own (`{id}`, the
//     emitter's stamp), and reading that as the actor would be the same
//     fail-open;
//  5. a step id -- a bare step name stands for the step's RESULT, and its
//     members are the step accessors (`result`, `status`, `error`,
//     `metadata`, `count`, `nodes`, `empty`, `first`, `last`, `Ran`),
//     exactly as `stepId.x` means `steps.stepId.x`;
//  6. the G2 bare-args tier (memql#2364): an args field, and a declared but
//     absent optional field as nil;
//  7. the implicit envelope read for any other root: a key of the event
//     envelope (`payload`, `topic`, ...);
//  8. any other reserved root the run did not seed (`args` with no args
//     block, `error` outside onError) is absent (memql#2851).
//
// Anything else is an unknown name, which EvalExpr refuses (unknown_name)
// instead of reading the name as its own text.
//
// A step's value: a result with a flat output (an object literal or a scalar
// `return`) is that value (UnwrapStepResult, #2271); a Bundle-backed query or
// mutation result is its node list (GetStepNodes -- node maps with id,
// concept and payload), so `rows.count()`, `rows.first().payload.x` and
// `rows.where(r => ...)` read the rows rather than the envelope; any other
// result is itself.

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

// RunScope is a run's state as a memql.ExprScope. It is cheap to build --
// one per evaluation -- and caches the node list of each step it reads for
// the life of that evaluation, so `rows.count` and `rows.first` in one
// expression convert the stored result once.
type RunScope struct {
	e     *Evaluator
	nodes map[string]runStepNodes
}

type runStepNodes struct {
	nodes []any
	ok    bool
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
	// A statement body resolves names its own way, and none of the tiers
	// below exist in it (statement_scope.go).
	if e.statementMode() {
		return s.statementLookup(name)
	}
	if e.item != nil && name == e.itemName {
		return e.item, true
	}
	switch name {
	case "item":
		return e.item, true
	case "steps":
		return &runSteps{scope: s}, true
	case "input":
		return e.input, true
	case "automation":
		return &runAutomation{e: e}, true
	case "var", "systemVar", "secret", "systemSecret":
		return &runVarRoot{e: e, kind: name}, true
	case "argsDeclared":
		// The G2 bookkeeping set, seeded beside `args`; not a name.
		return nil, false
	}
	if v, ok := e.custom[name]; ok {
		return v, true
	}
	// No actor seeded: the denying envelope, never absent and never a later
	// tier's `actor` (see the file comment).
	if name == "actor" {
		return auth.ActorEnvelopeMap(nil), true
	}
	if sr, ok := e.steps[name]; ok && sr != nil {
		return &runStep{scope: s, id: name, result: sr, asResult: true}, true
	}
	if bound, gated := e.custom["args"].(map[string]any); gated {
		if v, ok := bound[name]; ok {
			return v, true
		}
		if declared, _ := e.custom["argsDeclared"].(map[string]bool); declared[name] {
			return nil, true
		}
	}
	if envelope, ok := e.custom["event"].(map[string]any); ok {
		if v, ok := envelope[name]; ok {
			return v, true
		}
	}
	// Any other reserved root this run did not seed -- `args` in an automation
	// with no args block, `error` outside an onError hook -- is ABSENT, not an
	// unknown name (memql#2851), so `args.limit ?? 10` reaches its fallback.
	// `now` is left to EvalExpr, which answers it from the run clock.
	if reservedAutomationRoots[name] && name != "now" {
		return memql.Absent, true
	}
	return nil, false
}

// stepResultValue is what a step stands for (see the file comment).
func (s *RunScope) stepResultValue(id string, sr *StepResult) any {
	raw := sr.Result
	if fo, ok := raw.(flatOutputer); ok {
		if out, has := fo.FlatOutput(); has {
			return out
		}
	}
	switch x := raw.(type) {
	case nil, bool, string, int, int64, float64, []any, []map[string]any:
		return raw
	case map[string]any:
		if _, envelope := x["Bundle"]; !envelope {
			return raw
		}
	}
	if nodes, ok := s.stepNodes(id); ok {
		return nodes
	}
	// A Bundle-backed result GetStepNodes finds no node list in is a result
	// with no rows: an empty bundle's JSON omits `nodes`. The step accessors
	// read it as zero rows, and so does an expression -- `rows.nodes()`,
	// `rows.empty()` and `rows.first()` over an empty read are [], true and
	// absent, never the envelope itself.
	if bundleEnvelope(raw) {
		return []any{}
	}
	return raw
}

// bundleEnvelope reports whether a step result is an engine result whose
// value is its rows: an ExecuteResult without a flat output (the caller has
// already taken a flat one), or the same envelope decoded into a map.
func bundleEnvelope(raw any) bool {
	switch x := raw.(type) {
	case *memql.ExecuteResult:
		return x != nil
	case map[string]any:
		_, ok := x["Bundle"]
		return ok
	}
	return false
}

// stepNodes is GetStepNodes, cached for this scope's life.
func (s *RunScope) stepNodes(id string) ([]any, bool) {
	if c, ok := s.nodes[id]; ok {
		return c.nodes, c.ok
	}
	nodes, ok := s.e.GetStepNodes(id)
	if s.nodes == nil {
		s.nodes = map[string]runStepNodes{}
	}
	s.nodes[id] = runStepNodes{nodes: nodes, ok: ok}
	return nodes, ok
}

// runStep is one step as an expression reads it: its members are the step
// accessors, and -- read by its bare name -- it stands for its result.
// Reached through `steps.<id>` it stands for the step record itself.
type runStep struct {
	scope    *RunScope
	id       string
	result   *StepResult
	asResult bool
}

// ExprMember answers the step accessors. A name it does not know is read from
// the value the step stands for.
func (v *runStep) ExprMember(field string) (any, bool) {
	sr := v.result
	switch field {
	case "result":
		return v.scope.stepResultValue(v.id, sr), true
	case "status":
		return sr.Status, true
	case "error":
		return sr.Error, true
	case "metadata":
		if sr.Metadata == nil {
			return nil, true
		}
		return sr.Metadata, true
	case "Ran":
		// True when the step executed at all, as distinct from `empty`.
		return sr.Status != "", true
	case "count", "nodes", "empty", "first", "last":
		nodes, _ := v.scope.stepNodes(v.id)
		switch field {
		case "count":
			return int64(len(nodes)), true
		case "nodes":
			if nodes == nil {
				return []any{}, true
			}
			return nodes, true
		case "empty":
			return len(nodes) == 0, true
		case "first":
			if len(nodes) == 0 {
				return nil, true
			}
			return nodes[0], true
		default: // last
			if len(nodes) == 0 {
				return nil, true
			}
			return nodes[len(nodes)-1], true
		}
	}
	return nil, false
}

// ExprValue is what the step stands for.
func (v *runStep) ExprValue() any {
	if v.asResult {
		return v.scope.stepResultValue(v.id, v.result)
	}
	return v.result
}

// runSteps is the `steps` root: each member is a recorded step.
type runSteps struct {
	scope *RunScope
}

// ExprMember answers a recorded step id; an unknown one is absent.
func (v *runSteps) ExprMember(id string) (any, bool) {
	sr, ok := v.scope.e.steps[id]
	if !ok || sr == nil {
		return nil, false
	}
	return &runStep{scope: v.scope, id: id, result: sr}, true
}

// ExprValue is the recorded steps, by id.
func (v *runSteps) ExprValue() any {
	out := make(map[string]any, len(v.scope.e.steps))
	for id, sr := range v.scope.e.steps {
		out[id] = sr
	}
	return out
}

// runAutomation is the `automation` root; its one member is `errors`.
type runAutomation struct {
	e *Evaluator
}

// ExprMember answers `errors`: every failed step as "stepId: error", as
// resolveAutomationMeta built it.
func (v *runAutomation) ExprMember(field string) (any, bool) {
	if field != "errors" {
		return nil, false
	}
	return v.errors(), true
}

func (v *runAutomation) errors() []any {
	out := []any{}
	for _, id := range sortedKeys(v.e.steps) {
		if sr := v.e.steps[id]; sr != nil && sr.Error != "" {
			out = append(out, fmt.Sprintf("%s: %s", sr.StepId, sr.Error))
		}
	}
	return out
}

// ExprValue is the root's one member as a map.
func (v *runAutomation) ExprValue() any {
	return map[string]any{"errors": v.errors()}
}

// runVarRoot is `var` / `systemVar` / `secret` / `systemSecret`: `var.NAME`
// asks the run's resolver. A name the resolver cannot resolve is ABSENT, not
// its own text, so `var.killSwitch ?? false` reaches its fallback
// (memql#2851). The catalog spelling, var("NAME"), reaches the same resolver
// through EvalOptions.Vars and surfaces its error instead.
type runVarRoot struct {
	e    *Evaluator
	kind string
}

// ExprMember resolves NAME through the run's resolver for this root.
func (v *runVarRoot) ExprMember(name string) (any, bool) {
	r := v.e.variableResolverFor(v.kind)
	if r == nil {
		return nil, false
	}
	val, err := r(context.Background(), name)
	if err != nil {
		return nil, false
	}
	return val, true
}

// ExprValue is nothing: the root has members and no value of its own.
func (v *runVarRoot) ExprValue() any { return nil }

// variableResolverFor returns the resolver behind a var-family root or
// function.
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

// runClock is the run's one clock: the LogicRunner's ambient `now`, else the
// executor's seeded `timestamp`. Zero (unseeded) lets EvalExpr read the wall
// clock.
func (e *Evaluator) runClock() time.Time {
	for _, key := range []string{"now", "timestamp"} {
		if s, ok := e.custom[key].(string); ok {
			if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
				return t
			}
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

// InProcessQuery evaluates a query step whose expression runs in process --
// anything but a construct call: a logic body's `total := a + b`,
// `rows.where(r => ...)`, `return {ok: true}` -- over the run, with absent
// read as nil (one notion of unset for a step's value). inProcess is false
// for a construct call (`query activeUsers(...)`), which is the engine's to
// run, and for a step that is not a prepared query step.
func (e *Evaluator) InProcessQuery(ctx context.Context, step *Step) (value any, inProcess bool, err error) {
	if step == nil || step.Exprs == nil || step.Exprs.Query == nil {
		return nil, false, nil
	}
	if call, isCall := ast.Unparen(step.Exprs.Query).(*ast.CallExpr); isCall && call.Kind != "" {
		return nil, false, nil
	}
	v, err := e.EvalV1(ctx, step.Exprs.Query)
	if err != nil {
		return nil, true, err
	}
	if v == memql.Absent {
		v = nil
	}
	return v, true, nil
}

// ResolveV1Value resolves a v1 value: an *ExprLeaf evaluates, a map or list
// resolves each entry, and anything else is a literal and is returned as it
// is. CONTAINER RULE (rule 30's last paragraph, memql#3627): an entry whose
// expression evaluates to the Absent sentinel contributes nothing -- its key
// or element is omitted -- while an explicit nil is kept. A TOP-LEVEL absent
// value is returned as memql.Absent for the caller to decide about.
func (e *Evaluator) ResolveV1Value(ctx context.Context, v any) (any, error) {
	out, err := resolveV1(ctx, v, e.RunScope(), e.ExprOptions())
	if err != nil || !e.statementMode() {
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
