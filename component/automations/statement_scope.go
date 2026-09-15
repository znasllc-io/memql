package automations

// statement_scope.go -- names and values in a statement body (epic memql#5370,
// task memql#5372; D12 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
//
// A statement body resolves a bare name differently from every automation
// before it, and the difference is the point of the epic:
//
//   - a statement's NAME is its value (`rows := query q(...)`, then
//     `rows.count()`): there is no `steps` root, no `.result` climb and no
//     step accessor;
//   - an argument is read `args.x`: there is no bare-args tier (G2) and no
//     implicit `event.` retry;
//   - a loop names its own variable, and it exists only inside the loop;
//   - a name bound in a branch that did not run is ABSENT, never unknown.
//
// So a statement run carries a stack of name frames on its Evaluator, and
// RunScope answers from them first and from the reserved roots after; the
// legacy resolution order never runs. CheckBody refused every name that could
// not resolve when the body compiled, so a name missing here is one a skipped
// statement did not bind: each frame declares, when its sequence starts, every
// name its steps bind, and a declared name that was never bound reads absent.
//
// # Values
//
// A name holds what its statement produced, shaped for reading (D12): a query
// result is a list of rows, a mutation's value is the row it wrote, a logic's
// is its return value, a builtin's its result unwrapped. A row is read the way
// a filter reads one -- `row.id`, `row.concept`, `row.type`, `row.createdAt`,
// `row.createdBy`, `row.provenance` are the intrinsics and every other name is
// a payload field (`rows.first().email`) -- and it is passed on as the map it
// arrived as, so a row handed to a call or a published payload is the value a
// consumer saw before.

import (
	"strings"
	"sync"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
)

// nameFrame is one scope of a statement run: the body's own, a loop
// iteration's, a parallel branch's. Once-blocks open none -- their statements
// bind in the enclosing frame (the owner's answer of 2026-09-13).
type nameFrame struct {
	parent *nameFrame

	mu       sync.RWMutex
	names    map[string]any
	declared map[string]bool
}

func newNameFrame(parent *nameFrame) *nameFrame {
	return &nameFrame{parent: parent, names: map[string]any{}, declared: map[string]bool{}}
}

// declare records a name this frame's steps may bind; until one does, the
// name reads absent.
func (f *nameFrame) declare(name string) {
	if name == "" {
		return
	}
	f.mu.Lock()
	f.declared[name] = true
	f.mu.Unlock()
}

// bind sets a name in this frame.
func (f *nameFrame) bind(name string, v any) {
	if name == "" {
		return
	}
	f.mu.Lock()
	f.names[name] = v
	f.declared[name] = true
	f.mu.Unlock()
}

// lookup answers a name from this frame or an enclosing one. A declared name
// no statement bound is absent.
func (f *nameFrame) lookup(name string) (any, bool) {
	for fr := f; fr != nil; fr = fr.parent {
		fr.mu.RLock()
		v, bound := fr.names[name]
		declared := fr.declared[name]
		fr.mu.RUnlock()
		if bound {
			return v, true
		}
		if declared {
			return memql.Absent, true
		}
	}
	return nil, false
}

// statementMode reports whether this Evaluator resolves names as a statement
// body does.
func (e *Evaluator) statementMode() bool { return e != nil && e.names != nil }

// enterStatements puts the Evaluator in statement mode with a fresh body
// frame.
func (e *Evaluator) enterStatements() {
	e.names = newNameFrame(nil)
}

// childFrame returns a clone of the Evaluator whose names live in a new frame
// inside this one's: a loop iteration or a parallel branch.
func (e *Evaluator) childFrame() *Evaluator {
	c := e.Clone()
	c.names = newNameFrame(e.names)
	return c
}

// rowIntrinsics are the names a row answers from its columns rather than its
// payload -- the filter surface's `row.` namespace.
var rowIntrinsics = map[string]bool{
	"id": true, "concept": true, "type": true, "createdAt": true, "createdBy": true, "provenance": true,
}

// rowView is a row as a statement body reads it: the intrinsics from the
// node map's columns, every other name from its payload. Its value is the map
// it arrived as, which is what a call argument or a published payload
// receives.
type rowView struct {
	m map[string]any
}

// ExprMember reads one field of the row.
func (r *rowView) ExprMember(field string) (any, bool) {
	if rowIntrinsics[field] {
		v, ok := r.m[field]
		if !ok {
			return memql.Absent, true
		}
		return v, true
	}
	payload, _ := r.m["payload"].(map[string]any)
	if v, ok := payload[field]; ok {
		return viewRows(v), true
	}
	return memql.Absent, true
}

// ExprValue is the row's map, unchanged.
func (r *rowView) ExprValue() any { return r.m }

// isNodeMap reports whether a map is a stored row: an id and a payload object.
func isNodeMap(m map[string]any) bool {
	_, hasID := m["id"].(string)
	_, hasPayload := m["payload"].(map[string]any)
	return hasID && hasPayload
}

// viewRows wraps every stored row in v -- v itself, or an element of a list --
// in a rowView. Anything else is returned as it is.
func viewRows(v any) any {
	switch x := v.(type) {
	case map[string]any:
		if isNodeMap(x) {
			return &rowView{m: x}
		}
		return x
	case []any:
		out := make([]any, len(x))
		for i, el := range x {
			out[i] = viewRows(el)
		}
		return out
	case []map[string]any:
		out := make([]any, len(x))
		for i, el := range x {
			out[i] = viewRows(el)
		}
		return out
	}
	return v
}

// unwrapStatementValue replaces every rowView in v by its map, so a value
// leaving the body -- a call argument, a published payload, a returned value --
// is the plain value its consumers read.
func unwrapStatementValue(v any) any {
	switch x := v.(type) {
	case *rowView:
		return x.m
	case []any:
		out := make([]any, len(x))
		for i, el := range x {
			out[i] = unwrapStatementValue(el)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, el := range x {
			out[k] = unwrapStatementValue(el)
		}
		return out
	}
	return v
}

// bundleNodeMap is a GraphBundle node as the node map a query's output rows
// already are: its columns and its payload.
func bundleNodeMap(n *memqlv1.MemoryNode) map[string]any {
	m := map[string]any{
		"id":        n.GetId(),
		"concept":   n.GetConcept(),
		"type":      n.GetType(),
		"createdBy": n.GetCreatedBy(),
		"payload":   map[string]any{},
	}
	if ts := n.GetCreatedAt(); ts != nil {
		m["createdAt"] = ts.AsTime().UTC().Format(time.RFC3339Nano)
	}
	if p := n.GetPayload(); p != nil {
		m["payload"] = p.AsMap()
	}
	return m
}

// functionStatementValue is the value a query, mutation, logic or builtin
// call binds: its flat output when it has one (a logic's return value, a
// builtin's result, a query's rows), else its bundle's rows. A mutation binds
// the one row it wrote.
func functionStatementValue(kind string, raw any) any {
	var v any
	switch x := raw.(type) {
	case *memql.ExecuteResult:
		if x == nil {
			return nil
		}
		if out, has := x.FlatOutput(); has {
			v = out
		} else if x.Bundle != nil {
			rows := make([]any, 0, len(x.Bundle.GetNodes()))
			for _, n := range x.Bundle.GetNodes() {
				rows = append(rows, bundleNodeMap(n))
			}
			v = rows
		}
	default:
		if fo, ok := raw.(flatOutputer); ok {
			if out, has := fo.FlatOutput(); has {
				v = out
				break
			}
		}
		v = raw
	}
	v = viewRows(v)
	if strings.EqualFold(kind, "mutation") {
		if list, ok := v.([]any); ok {
			if len(list) == 0 {
				return nil
			}
			return list[0]
		}
	}
	return v
}
