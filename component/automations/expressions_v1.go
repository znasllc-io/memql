package automations

// expressions_v1.go -- a v1 automation's expressions, parsed once at load
// (epic memql#5363, memql#5367).
//
// THE COMPILED SHAPE. An automation the compiler produced from the
// edition-2026 grammar carries a top-level `"expressions": "v1"`. Within it,
// two encodings, and every position uses exactly one:
//
//   - an EXPRESSION FIELD -- a position that is always an expression -- holds
//     canonical v1 source (ast.FormatExpr) as a bare string: a step
//     `condition`, `forEach.source`, `forEach.filter`, `switch.expression`,
//     `shape.source`, `detectLeadSignal.source`, a `query.query`, a
//     precondition `check`, the `trigger.filter` lambda and a logic's
//     `_return`;
//   - a VALUE LEAF -- a position that holds a value -- is plain JSON when it
//     is a literal and `{"$expr": "<canonical v1 source>"}` when it is an
//     expression (compiler.EncodeValueLeaf; a map or list literal is walked,
//     so a leaf inside one is encoded by the same rule). Every entry of a
//     value map is one -- function / action / automation `args`, mutation and
//     event `payload`, webhook `body`, concept-card `data` -- and so is every
//     string-typed field that holds a value: `event.topic`, `webhook.url` and
//     each `webhook.headers` value, `mutation.id` / `parent` / `aliasOf`, and
//     the concept card's `cardType` / `partitionId` / `conceptRef`. A Go
//     string field cannot hold the object form, so those configs lift it on
//     decode (value_leaves.go). `$expr` is not an identifier, so no authored
//     map key collides with the marker.
//
// PrepareExpressions parses each of those once, refuses the automation on a
// parse error or when an expression's static cost estimate exceeds
// tiers.MaxStaticCost (D11), and caches the nodes: on Step.Exprs (a literal in
// a string-typed value field becomes a literal node there, so the executors
// read one kind of thing), on TriggerConfig.FilterLambda, on
// Precondition.checkExpr, and -- for a value-map leaf -- as an *ExprLeaf in
// place of the `{"$expr": ...}` map. The executors therefore never re-parse,
// and a non-nil Step.Exprs is what tells a step executor that its value
// leaves are literals and expressions, not string-evaluator text.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/language/tiers"
	"github.com/znasllc-io/memql/component/memql"
)

// ExpressionsV1 is the value of an automation's `"expressions"` key that marks
// it as compiled from the edition-2026 expression grammar.
const ExpressionsV1 = "v1"

// exprLeafKey is the one key of a compiled value leaf that is an expression.
const exprLeafKey = "$expr"

// IsV1 reports whether the automation was compiled from the edition-2026
// expression grammar.
func (a *Automation) IsV1() bool {
	return a != nil && a.Expressions == ExpressionsV1
}

// StepExprs is one v1 step's expressions, parsed at load. A nil field is a
// position the step does not use.
type StepExprs struct {
	// Condition gates the step.
	Condition ast.ExpressionNode
	// Query is a query step's expression: a construct call the executor
	// renders and runs, or any other expression, whose value is the result.
	Query ast.ExpressionNode
	// Source is a forEach / shape / detectLeadSignal step's source.
	Source ast.ExpressionNode
	// Filter is a forEach step's per-item filter.
	Filter ast.ExpressionNode
	// Subject is a switch step's expression.
	Subject ast.ExpressionNode
	// ID, Parent and AliasOf are a mutation step's string-typed expressions.
	ID, Parent, AliasOf ast.ExpressionNode
	// URL is a webhook step's; Headers its header values.
	URL     ast.ExpressionNode
	Headers map[string]ast.ExpressionNode
	// Topic is an event step's.
	Topic ast.ExpressionNode
	// CardType, PartitionID and ConceptRef are a concept-card step's.
	CardType, PartitionID, ConceptRef ast.ExpressionNode
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

// PrepareExpressions parses every expression of a v1 automation once and
// caches the nodes (see the file comment for where). A legacy automation is
// left as it is -- unless its JSON carries a v1 value leaf, which is refused. Every path that turns compiled JSON into an executable
// Automation calls it -- the tree loader, CompileSource and the LogicRunner's
// body compile -- and the executor refuses a v1 automation that skipped it.
//
// An error names the step and the position: a parse error, or an expression
// whose static cost estimate exceeds tiers.MaxStaticCost.
func PrepareExpressions(a *Automation) error {
	if !a.IsV1() {
		return refuseLegacyLeaves(a)
	}
	p := &exprPreparer{automation: a.Name}
	if a.Trigger != nil && strings.TrimSpace(a.Trigger.Filter) != "" {
		lam, err := languageParser.ParseV1Lambda(a.Trigger.Filter)
		if err != nil {
			return fmt.Errorf("automation %q: trigger filter: %w", a.Name, err)
		}
		if len(lam.Params) != 1 {
			return fmt.Errorf("automation %q: trigger filter %q must be a one-parameter lambda, as in row => row.status == \"active\"", a.Name, a.Trigger.Filter)
		}
		if err := p.checkCost("trigger filter", lam.Body); err != nil {
			return err
		}
		a.Trigger.FilterLambda = lam
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
	for _, hook := range []*Step{a.OnComplete, a.OnError} {
		if hook != nil {
			if err := p.step(hook); err != nil {
				return err
			}
		}
	}
	a.exprsPrepared = true
	return nil
}

// requirePreparedExpressions refuses a v1 automation whose expressions were
// never parsed. Without the parsed nodes a v1 step has no Exprs, so its
// executor would take the LEGACY path and read v1 source as string-evaluator
// expressions -- wrong silently, which is the one outcome this refusal
// exists to rule out.
func requirePreparedExpressions(a *Automation) error {
	if a.IsV1() && !a.exprsPrepared {
		return fmt.Errorf("automation %q is compiled from the v1 expression grammar but its expressions were never prepared (automations.PrepareExpressions) -- it would run its v1 source through the legacy string evaluator", a.Name)
	}
	return nil
}

// bindV1RunAmbient binds, for a v1 automation run, the ambient roots the
// executor never seeded for the string evaluator: `config` (the allow-listed
// configuration envelope, component/config/policy_exposable.go) and
// `partition`, from the engine's one canonical envelope -- the source the
// LogicRunner already binds them from (bindEngineAmbientEnvelope). The run's
// clock stays the seeded `timestamp`, so `now` is not rebound here. A legacy
// run is left exactly as it was.
func bindV1RunAmbient(ctx context.Context, engine *memql.MemQLEngine, evaluator *Evaluator, a *Automation) {
	if evaluator == nil || !a.IsV1() {
		return
	}
	envelope := engine.AmbientEnvelope(ctx)
	for _, key := range []string{"config", "partition"} {
		if value, ok := envelope[key]; ok {
			evaluator.SetCustom(key, value)
		}
	}
}

// bindV1OnErrorRun gives a v1 onError hook the run it is handling: the
// automation's bound `args` and the steps the run recorded, so `args.x`,
// `steps.s.error` and `automation.errors` read the failed run. The legacy hook
// evaluates over a fresh evaluator holding only the event, the error and the
// clock -- there `automation.errors` is always empty -- and is left exactly as
// it was.
func bindV1OnErrorRun(evaluator *Evaluator, a *Automation, exec *AutomationExecution, triggeringEvent *events.Event) {
	if evaluator == nil || !a.IsV1() {
		return
	}
	if boundArgs, _, err := bindEventArgs(a, triggeringEvent); err == nil && boundArgs != nil {
		evaluator.SetCustom("args", boundArgs)
		evaluator.SetCustom("argsDeclared", declaredArgsSet(a))
	}
	if exec == nil {
		return
	}
	for id, sr := range exec.Steps {
		if sr != nil {
			evaluator.SetStepResult(id, sr)
		}
	}
}

// exprPreparer carries the automation's name into every refusal.
type exprPreparer struct {
	automation string
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
	case s.Query != nil:
		if x.Query, err = p.parse(at("query"), s.Query.Query); err != nil {
			return err
		}
	case s.Mutation != nil:
		m := s.Mutation
		if x.ID, err = p.leaf(at("mutation id"), m.ID, m.leaves["id"]); err != nil {
			return err
		}
		if x.Parent, err = p.leaf(at("mutation parent"), m.Parent, m.leaves["parent"]); err != nil {
			return err
		}
		if x.AliasOf, err = p.leaf(at("mutation aliasOf"), m.AliasOf, m.leaves["aliasOf"]); err != nil {
			return err
		}
		if m.Payload, err = p.valueMap(at("mutation payload"), m.Payload); err != nil {
			return err
		}
	case s.Webhook != nil:
		w := s.Webhook
		if x.URL, err = p.leaf(at("webhook url"), w.URL, w.leaves["url"]); err != nil {
			return err
		}
		if x.URL == nil {
			return fmt.Errorf("automation %q: %s is empty", p.automation, at("webhook url"))
		}
		names := map[string]bool{}
		for k := range w.Headers {
			names[k] = true
		}
		for key := range w.leaves {
			if name, isHeader := strings.CutPrefix(key, "headers."); isHeader {
				names[name] = true
			}
		}
		if len(names) > 0 {
			x.Headers = make(map[string]ast.ExpressionNode, len(names))
			for _, k := range sortedKeys(names) {
				node, err := p.leaf(at("webhook header "+k), w.Headers[k], w.leaves["headers."+k])
				if err != nil {
					return err
				}
				if node == nil {
					// An empty header value is the empty string, as it was.
					node = &ast.LiteralExpr{Value: ""}
				}
				x.Headers[k] = node
			}
		}
		if w.Body, err = p.valueMap(at("webhook body"), w.Body); err != nil {
			return err
		}
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
	case s.Switch != nil:
		if x.Subject, err = p.parse(at("switch expression"), s.Switch.Expression); err != nil {
			return err
		}
		for _, k := range sortedCaseKeys(s.Switch.Cases) {
			if err := p.steps(caseSteps(s.Switch.Cases[k])); err != nil {
				return err
			}
		}
		if err := p.steps(caseSteps(s.Switch.Default)); err != nil {
			return err
		}
	case s.Shape != nil:
		if x.Source, err = p.parse(at("shape source"), s.Shape.Source); err != nil {
			return err
		}
	case s.DetectLeadSignal != nil:
		if x.Source, err = p.parse(at("detectLeadSignal source"), s.DetectLeadSignal.Source); err != nil {
			return err
		}
	case s.EmitConceptCard != nil:
		c := s.EmitConceptCard
		if x.CardType, err = p.leaf(at("concept card cardType"), c.CardType, c.leaves["cardType"]); err != nil {
			return err
		}
		if x.PartitionID, err = p.leaf(at("concept card partitionId"), c.PartitionId, c.leaves["partitionId"]); err != nil {
			return err
		}
		if x.ConceptRef, err = p.leaf(at("concept card conceptRef"), c.ConceptRef, c.leaves["conceptRef"]); err != nil {
			return err
		}
		if c.Data, err = p.valueMap(at("concept card data"), c.Data); err != nil {
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

func sortedCaseKeys(m map[string]*SwitchCase) []string {
	return sortedKeys(m)
}
