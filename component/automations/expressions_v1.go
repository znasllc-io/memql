package automations

// expressions_v1.go -- an automation's expressions, parsed once at load
// (epic memql#5363, memql#5367).
//
// THE COMPILED SHAPE. Every automation is compiled from its statements
// (component/language/compiler/body_compile.go), and within its JSON there
// are two encodings, every position using exactly one:
//
//   - an EXPRESSION FIELD -- a position that is always an expression -- holds
//     canonical v1 source (ast.FormatExpr) as a bare string: a step
//     `condition`, `forEach.source`, `forEach.filter`, an `expression`, a
//     `return.value`, a precondition `check` and the `trigger.filter`
//     lambda;
//   - a VALUE LEAF -- a position that holds a value -- is plain JSON when it
//     is a literal and `{"$expr": "<canonical v1 source>"}` when it is an
//     expression (compiler.EncodeValueLeaf; a map or list literal is walked,
//     so a leaf inside one is encoded by the same rule). Every entry of a
//     value map is one -- function / action / automation `args`, an event's
//     `payload` -- and so is `event.topic`, the one string-typed field that
//     holds a value. A Go string field cannot hold the object form, so its
//     config lifts it on decode (value_leaves.go). `$expr` is not an
//     identifier, so no authored map key collides with the marker.
//
// PrepareExpressions parses each of those once, refuses the automation on a
// parse error or when an expression's static cost estimate exceeds
// tiers.MaxStaticCost (D11), and caches the nodes: on Step.Exprs (a literal in
// a string-typed value field becomes a literal node there, so the executors
// read one kind of thing), on TriggerConfig.FilterLambda, on
// Precondition.checkExpr, and -- for a value-map leaf -- as an *ExprLeaf in
// place of the `{"$expr": ...}` map. The executors therefore never re-parse.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/language/tiers"
	"github.com/znasllc-io/memql/component/memql"
)

// exprLeafKey is the one key of a compiled value leaf that is an expression.
const exprLeafKey = "$expr"

// StepExprs is one step's expressions, parsed at load. A nil field is a
// position the step does not use.
type StepExprs struct {
	// Condition gates the step.
	Condition ast.ExpressionNode
	// Source is a forEach step's source.
	Source ast.ExpressionNode
	// Filter is a forEach step's per-item filter.
	Filter ast.ExpressionNode
	// Topic is an event step's.
	Topic ast.ExpressionNode
	// Value is an expression step's expression, or a return step's value
	// (nil for a bare return).
	Value ast.ExpressionNode
}

// ExprLeaf is a value-map leaf that is an expression: `{"$expr": src}` in the
// compiled JSON, parsed once at load. It marshals back to the same shape, so
// a step that is re-serialised (a journal row, a test fixture) round-trips.
type ExprLeaf struct {
	Src  string
	Node ast.ExpressionNode
}

// MarshalJSON writes the compiled form.
func (l *ExprLeaf) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{exprLeafKey: l.Src})
}

// PrepareExpressions parses every expression of an automation once and
// caches the nodes (see the file comment for where). Every path that turns
// compiled JSON into an executable Automation calls it -- the tree loader and
// the LogicRunner's statement logic -- and the executor prepares an
// automation built in Go before its first run (ensurePrepared).
//
// An error names the step and the position: a parse error, or an expression
// whose static cost estimate exceeds tiers.MaxStaticCost.
//
// PrepareExpressions has no concept registry, so it cannot check a trigger
// filter against its concept's declared field types; the loader, which has
// one, calls prepareExpressions with it.
func PrepareExpressions(a *Automation) error {
	return prepareExpressions(a, nil)
}

// prepareExpressions is PrepareExpressions against the concept registry the
// automation compiles with -- the engine's at boot, the authoring sandbox's
// clone (bundle concepts overlaid) for an authored one. A trigger filter is
// checked against its concept's declared field types there (see
// checkTriggerFilterFields); a nil registry skips that check and nothing else.
func prepareExpressions(a *Automation, concepts memoryNodes.Registry) error {
	p := &exprPreparer{automation: a.Name, concepts: concepts}
	if a.Trigger != nil && strings.TrimSpace(a.Trigger.Filter) != "" {
		if err := p.triggerFilter(a.Trigger); err != nil {
			return err
		}
	}
	for _, pc := range a.Preconditions {
		if pc == nil {
			continue
		}
		node, err := p.parse("precondition "+pc.ID, pc.Check)
		if err != nil {
			return err
		}
		pc.checkExpr = node
	}
	if err := p.steps(a.Steps); err != nil {
		return err
	}
	a.exprsPrepared = true
	return nil
}

// triggerFilter parses a trigger filter as the one-parameter lambda it must
// be, checks its cost, and caches it on the trigger.
func (p *exprPreparer) triggerFilter(t *TriggerConfig) error {
	lam, err := languageParser.ParseV1Lambda(t.Filter)
	if err != nil {
		return fmt.Errorf("automation %q: trigger filter: %w", p.automation, err)
	}
	if len(lam.Params) != 1 {
		return fmt.Errorf("automation %q: trigger filter %q must be a one-parameter lambda, as in row => row.status == \"active\"", p.automation, t.Filter)
	}
	if err := p.checkCost("trigger filter", lam.Body); err != nil {
		return err
	}
	if err := p.checkTriggerFilterFields(t, lam); err != nil {
		return err
	}
	t.FilterLambda = lam
	return nil
}

// checkTriggerFilterFields refuses a trigger filter that uses a bare field of
// a declared non-boolean type as its condition -- `row => row.title` over a
// string field -- as Lower refuses one in a query filter (memql#5366). The
// filter is decided in process over the triggering row, and there a stored
// non-boolean in condition position is "not true", so the automation would
// load and silently never fire; the author meant a comparison, and the
// refusal names the one it probably was.
//
// The declared types are the trigger concept's, read from the registry the
// automation compiles against. By the time the preparer runs, the compiler
// has resolved the trigger's concept to its canonical id in the topic
// (graph.node.<action>.<concept id>). A trigger that names no concept (a
// schedule, a non-graph event, a wildcard), a concept the registry does not
// hold, or no registry at all leaves nothing to check against, and the
// runtime rule decides.
func (p *exprPreparer) checkTriggerFilterFields(t *TriggerConfig, lam *ast.LambdaExpr) error {
	if p.concepts == nil {
		return nil
	}
	conceptID := conceptIdFromTriggerTopic(t.Event)
	if conceptID == "" {
		return nil
	}
	concept, err := p.concepts.Get(conceptID)
	if err != nil || concept == nil {
		return nil
	}
	if err := memql.CheckConditionFields(lam, concept, tiers.PositionTriggerFilter); err != nil {
		// The lambda was parsed from the compiled JSON's filter text, so the
		// refused node's span is a column of THAT text, not of the author's
		// file. An authoring diagnostic positions a LowerError by its span,
		// and a position that cannot be established is omitted rather than
		// guessed -- the diagnostic then anchors at the construct.
		var le *memql.LowerError
		if errors.As(err, &le) {
			le.Span = ast.Span{}
		}
		return fmt.Errorf("automation %q: trigger filter: %w", p.automation, err)
	}
	return nil
}

// prepareOnDemand serialises ensurePrepared, so two first runs of one
// automation built in Go do not prepare it at once.
var prepareOnDemand sync.Mutex

// ensurePrepared prepares an automation that was built in Go rather than
// loaded, before its first run. A loaded automation was prepared at load and
// is left as it is. Without its parsed nodes a step's expressions would be
// text nothing reads -- a condition that never gates, an argument that is
// never passed -- which is the outcome this rules out.
//
// Its @loop and @mode are held to the rules the load holds them to
// (prepareLoopAndMode). One it refuses is left unprepared, so every run
// refuses it rather than only the first.
func ensurePrepared(a *Automation) error {
	prepareOnDemand.Lock()
	defer prepareOnDemand.Unlock()
	if a.exprsPrepared {
		return nil
	}
	if err := PrepareExpressions(a); err != nil {
		return err
	}
	if err := prepareLoopAndMode(a); err != nil {
		a.exprsPrepared = false
		return err
	}
	return nil
}

// bindRunAmbient binds the ambient roots of a run: `config` (the allow-listed
// configuration envelope, component/config/policy_exposable.go) and
// `partition`, from the engine's one canonical envelope -- the source the
// LogicRunner binds them from (bindEngineAmbientEnvelope). The run's clock
// stays the seeded `timestamp`, so `now` is not rebound here.
func bindRunAmbient(ctx context.Context, engine *memql.MemQLEngine, evaluator *Evaluator) {
	if evaluator == nil {
		return
	}
	envelope := engine.AmbientEnvelope(ctx)
	for _, key := range []string{"config", "partition"} {
		if value, ok := envelope[key]; ok {
			evaluator.SetCustom(key, value)
		}
	}
}

// exprPreparer carries the automation's name into every refusal.
type exprPreparer struct {
	automation string
	// concepts is the registry the automation compiles against, for the
	// trigger filter's declared-type check; nil skips that check.
	concepts memoryNodes.Registry
}

// parse parses one always-expression position and checks its static cost.
func (p *exprPreparer) parse(where, src string) (ast.ExpressionNode, error) {
	if strings.TrimSpace(src) == "" {
		return nil, fmt.Errorf("automation %q: %s is empty", p.automation, where)
	}
	node, err := languageParser.ParseV1Expression(src)
	if err != nil {
		return nil, fmt.Errorf("automation %q: %s: %w", p.automation, where, err)
	}
	if err := p.checkCost(where, node); err != nil {
		return nil, err
	}
	return node, nil
}

// parseOptional parses a position that may be left empty.
func (p *exprPreparer) parseOptional(where, src string) (ast.ExpressionNode, error) {
	if strings.TrimSpace(src) == "" {
		return nil, nil
	}
	return p.parse(where, src)
}

// checkCost refuses an expression whose static estimate exceeds the M tier's
// limit (D11): it is refused before it can run, rather than stopped by the
// step budget after it has run for a while.
func (p *exprPreparer) checkCost(where string, node ast.ExpressionNode) error {
	if cost := memql.EstimateCost(node); cost > tiers.MaxStaticCost {
		return fmt.Errorf("automation %q: %s `%s` has a static cost estimate of %d, above tiers.MaxStaticCost (%d): a collection scan nested in another over lists the loader cannot size -- move the inner scan into a prior step",
			p.automation, where, ast.FormatExpr(node), cost, tiers.MaxStaticCost)
	}
	return nil
}

func (p *exprPreparer) steps(steps []*Step) error {
	for _, s := range steps {
		if s == nil {
			continue
		}
		if err := p.step(s); err != nil {
			return err
		}
	}
	return nil
}

// step parses one step's expressions and recurses into the steps it holds.
func (p *exprPreparer) step(s *Step) error {
	x := &StepExprs{}
	at := func(pos string) string { return fmt.Sprintf("step %q %s", s.ID, pos) }
	var err error
	if x.Condition, err = p.parseOptional(at("condition"), s.Condition); err != nil {
		return err
	}
	switch {
	case s.Event != nil:
		ev := s.Event
		if x.Topic, err = p.leaf(at("event topic"), ev.Topic, ev.leaves["topic"]); err != nil {
			return err
		}
		if x.Topic == nil {
			return fmt.Errorf("automation %q: %s is empty", p.automation, at("event topic"))
		}
		if ev.Payload, err = p.valueMap(at("event payload"), ev.Payload); err != nil {
			return err
		}
	case s.Function != nil:
		if s.Function.Args, err = p.valueMap(at("args"), s.Function.Args); err != nil {
			return err
		}
	case s.Action != nil:
		if s.Action.Args, err = p.valueMap(at("action args"), s.Action.Args); err != nil {
			return err
		}
	case s.Automation != nil:
		if s.Automation.Args, err = p.valueMap(at("sub-automation args"), s.Automation.Args); err != nil {
			return err
		}
	case s.ForEach != nil:
		if x.Source, err = p.parse(at("forEach source"), s.ForEach.Source); err != nil {
			return err
		}
		if x.Filter, err = p.parseOptional(at("forEach filter"), s.ForEach.Filter); err != nil {
			return err
		}
		if err := p.steps(s.ForEach.Do); err != nil {
			return err
		}
	case s.Parallel != nil:
		if err := p.steps(s.Parallel.Branches); err != nil {
			return err
		}
	case s.Type == StepTypeExpression:
		if x.Value, err = p.parse(at("expression"), s.Expression); err != nil {
			return err
		}
	case s.Return != nil:
		if x.Value, err = p.parseOptional(at("return value"), s.Return.Value); err != nil {
			return err
		}
	case s.Block != nil:
		if err := p.steps(s.Block.Steps); err != nil {
			return err
		}
	}
	s.Exprs = x
	return nil
}

// leaf prepares a string-typed value field (see the file comment): a lifted
// `{"$expr": src}` parses, a lifted number, boolean or null is that literal,
// and the Go string -- what a JSON string decoded into -- is a string
// literal. An empty field is not written (nil).
func (p *exprPreparer) leaf(where, s string, lifted json.RawMessage) (ast.ExpressionNode, error) {
	if lifted == nil {
		if s == "" {
			return nil, nil
		}
		return &ast.LiteralExpr{Value: s}, nil
	}
	var v any
	if err := json.Unmarshal(lifted, &v); err != nil {
		return nil, fmt.Errorf("automation %q: %s: %w", p.automation, where, err)
	}
	switch x := v.(type) {
	case map[string]any:
		src, ok := exprLeafSource(x)
		if !ok {
			return nil, fmt.Errorf("automation %q: %s must be a literal or {\"%s\": <source>}, got an object", p.automation, where, exprLeafKey)
		}
		return p.parse(where, src)
	case nil:
		return &ast.NilExpr{}, nil
	case float64, bool:
		return &ast.LiteralExpr{Value: x}, nil
	}
	return nil, fmt.Errorf("automation %q: %s must be a scalar or an expression, got %T", p.automation, where, v)
}

// valueMap prepares a value map, returning a new map in which every
// `{"$expr": src}` leaf is an *ExprLeaf.
func (p *exprPreparer) valueMap(where string, m map[string]any) (map[string]any, error) {
	if m == nil {
		return nil, nil
	}
	out := make(map[string]any, len(m))
	for _, k := range sortedKeys(m) {
		v, err := p.value(where+"."+k, m[k])
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

// value prepares one value: a `{"$expr": src}` map becomes an *ExprLeaf,
// containers recurse, and every other value is a literal and stays as it is.
func (p *exprPreparer) value(where string, v any) (any, error) {
	switch x := v.(type) {
	case map[string]any:
		if src, ok := exprLeafSource(x); ok {
			node, err := p.parse(where, src)
			if err != nil {
				return nil, err
			}
			return &ExprLeaf{Src: src, Node: node}, nil
		}
		return p.valueMap(where, x)
	case []any:
		out := make([]any, len(x))
		for i, el := range x {
			pv, err := p.value(fmt.Sprintf("%s[%d]", where, i), el)
			if err != nil {
				return nil, err
			}
			out[i] = pv
		}
		return out, nil
	}
	return v, nil
}

// exprLeafSource reports whether m is a compiled expression leaf -- exactly
// one key, `$expr`, holding a string -- and returns its source.
func exprLeafSource(m map[string]any) (string, bool) {
	if len(m) != 1 {
		return "", false
	}
	src, ok := m[exprLeafKey].(string)
	return src, ok
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
