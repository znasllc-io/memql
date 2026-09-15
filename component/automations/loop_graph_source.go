package automations

// loop_graph_source.go -- the construct call graph the static loop analysis
// walks (memql#5381), read once from the engine's function registry.
//
// component/work holds the walk (UnionFootprint, UnionWrites) and is a pure
// leaf: it knows constructs as names in a work.Registry and nothing of the
// engine. This file is the one adapter from memql.Function to work.Target, so
// the graph, the dedup key's Reads (loop_reads.go) and anything else that
// needs "what does this call reach" read one registry built one way.
//
// A MUTATION'S WRITE is read off its template. After the edition-2026 flip
// (epic memql#5363) a template holds parsed v1 nodes at its leaves --
// mutation_values_v1.go lays the insert/update block out as a map of field to
// node -- so a literal is an *ast.LiteralExpr, `field: args.x` (and the
// `accept { field }` it desugars from) an args member path, and anything else
// a value only the run computes. TestFunctionSource_AMutationIsItsWrite pins
// that reading over forge's advanceRequest as the loader builds it.

import (
	"sort"
	"strings"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
)

// FunctionSource is the construct call graph the loop analysis walks.
type FunctionSource interface {
	Registry() work.Registry
}

// registrySource is a FunctionSource over a registry built once.
type registrySource struct{ reg work.Registry }

func (s registrySource) Registry() work.Registry { return s.reg }

// NewFunctionSource builds the call graph once from every function fns holds,
// keyed by every name a call can use: the qualified `<namespace>.<name>` and,
// where it is unambiguous, the bare name (LookupIndex, as the engine resolves
// a construct reference out of a compiled body). A nil fns is an empty graph,
// in which every call is unresolved.
//
// It knows no concepts, so it cannot see the two fields the engine rewrites
// after a template renders -- a written id in an outgoing @relationship field,
// and a @scrubPii mutation's PII fields; the loader's check reads concepts too
// (newFunctionSource).
func NewFunctionSource(fns *memql.FunctionRegistry) FunctionSource {
	return newFunctionSource(fns, nil)
}

// newFunctionSource is NewFunctionSource reading each mutation's concept from
// concepts, when given: a value the template writes into a field the engine
// rewrites before storing it is not the value the row carries, so such a
// field is read as unknown.
func newFunctionSource(fns *memql.FunctionRegistry, concepts memoryNodes.Registry) FunctionSource {
	reg := work.Registry{}
	if fns == nil {
		return registrySource{reg}
	}
	for name, fn := range fns.LookupIndex() {
		if fn == nil {
			continue
		}
		if t, ok := functionTarget(fn, concepts); ok {
			reg[name] = t
		}
	}
	return registrySource{reg}
}

// functionTarget is one function as a node of the call graph. A kind the
// graph does not model (none today) is left out, so a call to it reads as
// unresolved rather than as a construct that does nothing.
func functionTarget(fn *memql.Function, concepts memoryNodes.Registry) (work.Target, bool) {
	kind := strings.ToLower(strings.TrimSpace(fn.FunctionKind))
	switch {
	case fn.IsBuiltin() || kind == memql.FunctionTypeBuiltin:
		// Empty Effects: a builtin's @effects are not modelled here, and the
		// graph lists a builtin as OPAQUE rather than guessing its writes.
		return work.Target{ConstructKind: work.ConstructBuiltin}, true
	case kind == "" || kind == "query":
		return work.Target{ConstructKind: work.ConstructQuery}, true
	case kind == "mutation":
		// The concept idiom of rowauthz_owner_provenance.go: the bound
		// concept, else the template's.
		concept := strings.TrimSpace(fn.BoundConcept)
		if concept == "" && fn.MutationTemplate != nil {
			concept = strings.TrimSpace(fn.MutationTemplate.Concept)
		}
		var def *memoryNodes.Concept
		if concepts != nil && concept != "" {
			def, _ = concepts.Get(concept)
		}
		return work.Target{ConstructKind: work.ConstructMutation, Concept: concept, Write: writeSpecOf(fn.MutationTemplate, def)}, true
	case kind == "logic":
		return work.Target{ConstructKind: work.ConstructLogic, Calls: logicCalls(fn)}, true
	}
	return work.Target{}, false
}

// writeSpecOf reads a mutation template's write, against its concept when
// known (nil otherwise). Nil for a mutation with no template, which
// UnionWrites reports as a write it knows nothing about.
//
// A field is known only when the row will carry what the template writes.
// Five things break that, and each such field is read as unknown:
//
//   - on the read-merge path -- every write but a new row's -- @createOnly
//     drops a field over a stored row, @noUnset drops a blank value over a
//     stored one, and @mergeFields / @appendFields / @addToSet /
//     @removeFromSet combine the value with the stored one;
//   - @scrubPii zeroes the concept's PII fields after the merge (every
//     field, when the concept is not known);
//   - an id written into an outgoing @relationship field is stored in
//     canonical form (canonicalizeRelationshipFields), so the literal the
//     template or the call site writes is not the stored value.
func writeSpecOf(t *memql.FunctionMutationTemplate, concept *memoryNodes.Concept) *work.WriteSpec {
	if t == nil {
		return nil
	}
	spec := &work.WriteSpec{Kind: string(t.Kind)}
	if spec.Kind == "" {
		spec.Kind = string(ast.MutationKindInsert) // empty means insert
	}
	fields := map[string]work.FieldValue{}
	splat := false
	switch p := t.PayloadTemplate.(type) {
	case map[string]any:
		for k, v := range p {
			fields[k] = fieldValueOf(v)
		}
	case nil:
	default:
		// `payload: args.x` -- the whole payload is one value the run
		// supplies, so any field may be written.
		splat = true
	}
	for k, v := range t.PayloadOverlayTemplate {
		fields[k] = fieldValueOf(v)
	}
	spec.NewRow = t.IDTemplate == nil && !splat
	unknown := func(names ...string) {
		for _, n := range names {
			if _, set := fields[n]; set {
				fields[n] = work.FieldValue{}
			}
		}
	}
	if !spec.NewRow {
		for _, group := range [][]string{t.CreateOnlyFields, t.MergeFields, t.AppendFields, t.AddToSetFields, t.RemoveFromSetFields} {
			unknown(group...)
		}
		for _, n := range t.NoUnsetFields {
			if fv := fields[n]; !fv.HasLiteral || blankPayloadValue(fv.Literal) {
				unknown(n) // a non-blank literal is written; anything else may not be
			}
		}
	}
	switch {
	case t.ScrubPii && concept == nil:
		for n := range fields {
			fields[n] = work.FieldValue{}
		}
	case t.ScrubPii:
		unknown(concept.PIIFields()...)
	}
	if concept != nil {
		for _, rel := range concept.Relationships {
			if strings.EqualFold(strings.TrimSpace(rel.Direction), "outgoing") {
				top, _, _ := strings.Cut(strings.TrimSpace(rel.Field), ".")
				unknown(top)
			}
		}
	}
	if len(fields) > 0 {
		spec.Fields = fields
	}
	return spec
}

// blankPayloadValue is what @noUnset counts as unset (isEmptyPayloadValue in
// the engine): nil, a blank string, an empty list or object.
func blankPayloadValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(x) == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

// fieldValueOf reads one top-level value of a template. A literal is known
// and `args.x` is exactly an argument; a nested object or list, a call, an
// operator, an actor or config read is a value only the run knows. The two
// encodings a value can arrive in are both read: a parsed v1 node (what the
// loader builds) and a compiled value leaf -- plain JSON for a literal,
// `{"$expr": src}` for an expression (compiler.EncodeValueLeaf).
func fieldValueOf(v any) work.FieldValue {
	switch x := v.(type) {
	case ast.ExpressionNode:
		switch n := ast.Unparen(x).(type) {
		case *ast.LiteralExpr:
			return work.FieldValue{Literal: normalizeLoopValue(n.Value), HasLiteral: true}
		case *ast.NilExpr:
			return work.FieldValue{HasLiteral: true}
		}
		if root, path, ok := ast.MemberPath(ast.Unparen(x)); ok && root == "args" && len(path) == 1 {
			return work.FieldValue{Arg: path[0]}
		}
	case map[string]any:
		if src, ok := exprLeafSource(x); ok {
			if node, err := languageParser.ParseV1Expression(src); err == nil {
				return fieldValueOf(node)
			}
		}
	case nil:
		return work.FieldValue{HasLiteral: true}
	case string, bool, float64, int, int64:
		return work.FieldValue{Literal: normalizeLoopValue(x), HasLiteral: true}
	}
	return work.FieldValue{}
}

// constructCallKinds are the call prefixes that run a construct the call
// graph holds. automation, action and capability calls have their own step
// types and are not construct calls of the function registry.
var constructCallKinds = map[string]bool{"query": true, "mutation": true, "logic": true, "builtin": true}

// logicCalls is the names a logic's body calls, sorted: the call its return
// makes (fn.Expr) and its statements' calls (fn.LogicSteps). Epic 3 replaces
// both halves in Task 13 of the loop protection plan, when a logic body is
// statements rather than a step list.
func logicCalls(fn *memql.Function) []string {
	set := map[string]bool{}
	add := func(name string) {
		if name = strings.TrimSpace(name); name != "" {
			set[name] = true
		}
	}
	logicReturnCalls(fn.Expr, add)
	legacyLogicStepCalls(fn.LogicSteps, add)
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// logicReturnCalls reads a logic's fn.Expr: a construct call its return makes
// (convertLogicReturnV1), and any construct call inside a returned
// expression.
func logicReturnCalls(n memql.ExpressionNode, add func(string)) {
	switch e := n.(type) {
	case *memql.FunctionCallExpression:
		if e == nil {
			return
		}
		add(e.Name)
		for _, v := range e.Args {
			if pc, ok := v.(*memql.PlanConstExpression); ok && pc != nil {
				constructCallsIn(pc.Expr, add)
			}
		}
	case *memql.PlanConstExpression:
		if e != nil {
			constructCallsIn(e.Expr, add)
		}
	}
}

// constructCallsIn adds every construct call a v1 expression holds.
func constructCallsIn(n ast.ExpressionNode, add func(string)) {
	ast.WalkV1(n, func(x ast.ExpressionNode) bool {
		if call, ok := x.(*ast.CallExpr); ok && call != nil && call.Receiver == nil && constructCallKinds[call.Kind] {
			add(call.Name)
		}
		return true
	})
}

// legacyLogicStepCalls reads the statements of a logic body the LogicRunner
// runs (fn.LogicSteps): each call statement's name, and the construct a
// statement's expression calls (a `_return` of `query x(...)`), walking
// loops, parallel branches and switch cases. A catalog function or a helper
// (publishEvent) named here is not in the registry and contributes nothing.
// Deleted with LogicSteps by Task 13 of the loop protection plan.
func legacyLogicStepCalls(def *languageParser.AutomationDef, add func(string)) {
	if def == nil {
		return
	}
	var walk func(steps []languageParser.StepDef)
	walk = func(steps []languageParser.StepDef) {
		for i := range steps {
			switch cfg := steps[i].Config.(type) {
			case *languageParser.FunctionStepConfig:
				if cfg != nil {
					add(cfg.Name)
				}
			case *languageParser.QueryStepConfig:
				if cfg != nil && cfg.Query != nil {
					constructCallsIn(cfg.Query, add)
				}
			case *languageParser.ForEachStepConfig:
				if cfg != nil {
					walk(cfg.Do)
				}
			case *languageParser.ParallelStepConfig:
				if cfg != nil {
					walk(cfg.Branches)
				}
			case *languageParser.SwitchStepConfig:
				if cfg == nil {
					continue
				}
				for _, label := range sortedKeys(cfg.Cases) {
					if c := cfg.Cases[label]; c != nil {
						walk(c.Steps)
					}
				}
				if cfg.Default != nil {
					walk(cfg.Default.Steps)
				}
			}
		}
	}
	walk(def.Steps)
	for _, hook := range []*languageParser.StepDef{def.OnComplete, def.OnError} {
		if hook != nil {
			walk([]languageParser.StepDef{*hook})
		}
	}
}
