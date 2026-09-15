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
type registrySource struct {
	reg       work.Registry
	functions map[string]*memql.Function
}

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
		return registrySource{reg: reg}
	}
	for name, fn := range fns.LookupIndex() {
		if fn == nil {
			continue
		}
		if t, ok := functionTarget(fn, concepts); ok {
			reg[name] = t
		}
	}
	return registrySource{reg: reg, functions: fns.LookupIndex()}
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

// logicCalls is the names a logic's statements call, sorted: every call
// statement in its compiled body (fn.LogicBody), through its loops, its
// parallel branches and the blocks an if or a switch compiles to. A construct
// call is always a statement of its own, so no expression holds one; a
// catalog function is not a call statement and contributes nothing.
func logicCalls(fn *memql.Function) []string {
	set := map[string]bool{}
	var walk func(steps []map[string]any)
	walk = func(steps []map[string]any) {
		for _, st := range steps {
			if f, ok := st["function"].(map[string]any); ok {
				if name, _ := f["name"].(string); strings.TrimSpace(name) != "" {
					set[strings.TrimSpace(name)] = true
				}
			}
			if fe, ok := st["forEach"].(map[string]any); ok {
				walk(stepMaps(fe["do"]))
			}
			if p, ok := st["parallel"].(map[string]any); ok {
				walk(stepMaps(p["branches"]))
			}
			if b, ok := st["block"].(map[string]any); ok {
				walk(stepMaps(b["steps"]))
			}
		}
	}
	walk(fn.LogicBody)
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

// stepMaps reads a compiled step list, which the compiler builds as
// []map[string]any and a JSON round trip leaves as []any.
func stepMaps(v any) []map[string]any {
	switch l := v.(type) {
	case []map[string]any:
		return l
	case []any:
		out := make([]map[string]any, 0, len(l))
		for _, x := range l {
			if m, ok := x.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}
