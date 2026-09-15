package automations

// loop_graph_filter.go -- deciding a trigger filter against what a write is
// known to set (memql#5381; D-I of the loop protection plan).
//
// The static loop graph draws an edge from A to B when one of A's writes
// publishes a topic B's trigger matches -- unless B's @filter cannot hold on
// what that write puts in the row. decideFilter is that "cannot": a partial
// evaluation of the filter lambda over a row of which some fields are known
// (a literal the mutation template writes, or an argument the call site
// passes as one), some are known absent (a new row the template does not set
// them on) and the rest are not known until the write runs.
//
// THREE VALUES, AND ONLY FALSE REMOVES AN EDGE. true and unknown both keep
// it, so every rule here is chosen so that a false is one the runtime would
// give for every row the write can produce. The rules mirror the runtime's
// own evaluator (component/memql/expr_eval.go) node for node -- D8's absence
// table for ==, != and the ordered comparisons, membership as ==, the
// blank-coalescing ??, and Kleene logic for && || ! -- and
// TestDecideFilterAgreesWithTheRuntime holds every decided answer to that
// evaluator over completions of the unknown fields. A node the rules do not
// cover is unknown, never a guess, and the reason says which.

import (
	"fmt"
	"math"
	"strings"

	"github.com/znasllc-io/memql/component/language/ast"
)

// tri is a three-valued truth.
type tri int8

const (
	triFalse tri = iota
	triTrue
	triUnknown
)

func (t tri) String() string {
	switch t {
	case triFalse:
		return "false"
	case triTrue:
		return "true"
	}
	return "unknown"
}

func triOf(b bool) tri {
	if b {
		return triTrue
	}
	return triFalse
}

// knownRow is what a write is known to put in the row its event carries.
type knownRow struct {
	Concept      string
	Fields       map[string]any  // known literal values
	Absent       map[string]bool // known absent (an insert of a new row that does not set them)
	OthersKnown  bool            // true when every unset field is absent (NewRow insert)
	FirstVersion tri
	// Unknown are the fields the write sets to a value only the run knows.
	// They are never absent, whatever OthersKnown says: a new row that sets
	// status from a step's result has a status, just not a known one.
	Unknown map[string]bool
	// explain says why a field (or "firstVersion") is unknown, in the words
	// an undecided edge prints: what the write does to it. Nil says only that
	// the run knows it.
	explain func(field string) string
}

// why is the reason field is unknown.
func (r knownRow) why(field string) string {
	if r.explain != nil {
		if s := r.explain(field); s != "" {
			return s
		}
	}
	return fmt.Sprintf("%s is known only at run time", field)
}

// decideFilter decides a trigger filter -- a one-parameter lambda over the
// triggering row -- against what a write is known to set. A nil filter holds
// for every write. An unknown answer carries the reason: the first field the
// filter reads that the write leaves unknown, in the row's words, or the node
// the rules do not cover.
func decideFilter(filter *ast.LambdaExpr, row knownRow) (tri, string) {
	if filter == nil {
		return triTrue, ""
	}
	if len(filter.Params) != 1 {
		return triUnknown, fmt.Sprintf("%s is not a one-parameter lambda", ast.FormatExpr(filter))
	}
	d := filterDecider{param: filter.Params[0], row: row}
	return d.cond(filter.Body)
}

type filterDecider struct {
	param string
	row   knownRow
}

// pkind is how much is known about one value.
type pkind int8

const (
	pKnown   pkind = iota // v holds it
	pAbsent               // a read of a field the row does not have
	pUnknown              // only the run knows; why says so
)

// pval is a value as far as the load knows it. A known nil is the author's
// `nil` or a field written null; pAbsent is a missing field. The two are one
// value to every comparison and differ only inside a list literal, where an
// absent element contributes nothing and a nil is kept -- as at run time.
type pval struct {
	kind pkind
	v    any
	why  string
}

func knownP(v any) pval        { return pval{kind: pKnown, v: v} }
func unknownP(why string) pval { return pval{kind: pUnknown, why: why} }

var absentP = pval{kind: pAbsent}

// undecidable is the reason for a node the rules do not evaluate.
func undecidable(n ast.ExpressionNode) string {
	return fmt.Sprintf("%s is not evaluated at load", ast.FormatExpr(n))
}

// cond decides n in condition position: a bool is itself, absent is false,
// and any other value is not true -- a stored value of the wrong type answers
// false at run time, and a computed one is refused, which does not fire
// either.
func (d filterDecider) cond(n ast.ExpressionNode) (tri, string) {
	switch e := n.(type) {
	case *ast.ParenExpr:
		if e != nil {
			return d.cond(e.Inner)
		}
	case *ast.UnaryExpr:
		if e != nil && e.Op == "!" {
			t, why := d.cond(e.Operand)
			switch t {
			case triTrue:
				return triFalse, ""
			case triFalse:
				return triTrue, ""
			}
			return triUnknown, why
		}
	case *ast.BinaryExpr:
		if e == nil {
			break
		}
		switch e.Op {
		case "&&":
			l, lwhy := d.cond(e.Left)
			if l == triFalse {
				return triFalse, ""
			}
			r, rwhy := d.cond(e.Right)
			switch {
			case r == triFalse:
				return triFalse, ""
			case l == triTrue && r == triTrue:
				return triTrue, ""
			case l == triUnknown:
				return triUnknown, lwhy
			}
			return triUnknown, rwhy
		case "||":
			l, lwhy := d.cond(e.Left)
			if l == triTrue {
				return triTrue, ""
			}
			r, rwhy := d.cond(e.Right)
			switch {
			case r == triTrue:
				return triTrue, ""
			case l == triFalse && r == triFalse:
				return triFalse, ""
			case l == triUnknown:
				return triUnknown, lwhy
			}
			return triUnknown, rwhy
		case "==", "!=", "<", "<=", ">", ">=":
			return d.compare(e)
		case "in":
			return d.in(e)
		case "startsWith":
			return d.startsWith(e)
		}
	}
	v := d.value(n)
	switch v.kind {
	case pUnknown:
		return triUnknown, v.why
	case pAbsent:
		return triFalse, ""
	}
	if b, ok := v.v.(bool); ok {
		return triOf(b), ""
	}
	return triFalse, ""
}

// value evaluates n to what the load knows of it.
func (d filterDecider) value(n ast.ExpressionNode) pval {
	switch e := n.(type) {
	case *ast.ParenExpr:
		if e != nil {
			return d.value(e.Inner)
		}
	case *ast.LiteralExpr:
		if e != nil {
			return knownP(normalizeLoopValue(e.Value))
		}
	case *ast.NilExpr:
		return knownP(nil)
	case *ast.MemberExpr:
		if e != nil {
			return d.member(e)
		}
	case *ast.UnaryExpr:
		if e != nil && e.Op == "!" {
			return d.condValue(e)
		}
	case *ast.BinaryExpr:
		if e == nil {
			break
		}
		switch e.Op {
		case "??":
			return d.coalesce(e)
		case "==", "!=", "<", "<=", ">", ">=", "in", "startsWith", "&&", "||":
			return d.condValue(e)
		}
	}
	return unknownP(undecidable(n))
}

// condValue is a condition used as a value: its bool.
func (d filterDecider) condValue(n ast.ExpressionNode) pval {
	t, why := d.cond(n)
	if t == triUnknown {
		return unknownP(why)
	}
	return knownP(t == triTrue)
}

// member reads `<param>.f...` or `args.f...`; any other root (actor, event,
// config, a name the run binds) is unknown.
func (d filterDecider) member(e *ast.MemberExpr) pval {
	root, fields, ok := ast.MemberPath(e)
	if !ok || len(fields) == 0 {
		return unknownP(undecidable(e))
	}
	var v pval
	switch root {
	case d.param:
		v = d.rowField(fields[0])
	case "args":
		v = d.argField(fields[0])
	default:
		return unknownP(undecidable(e))
	}
	// A read through an absent object is absent, and through an unknown one
	// unknown; through a value that is not an object the load does not
	// decide.
	for _, f := range fields[1:] {
		switch v.kind {
		case pUnknown, pAbsent:
			return v
		}
		if v.v == nil {
			return absentP
		}
		obj, isObj := v.v.(map[string]any)
		if !isObj {
			return unknownP(undecidable(e))
		}
		x, has := obj[f]
		if !has {
			return absentP
		}
		v = knownP(normalizeLoopValue(x))
	}
	return v
}

// rowIntrinsics are the row's columns (memql's intrinsic fields), which a
// payload field of the same name never shadows. Matched case-insensitively,
// as the engine resolves them.
var rowIntrinsics = map[string]bool{"concept": true, "id": true, "type": true, "createdat": true, "createdby": true, "provenance": true}

// rowField reads the row: its concept is the write's concept, its other
// columns are the run's, and every other name is a payload field.
func (d filterDecider) rowField(f string) pval {
	if lower := strings.ToLower(f); rowIntrinsics[lower] {
		if lower == "concept" {
			return knownP(d.row.Concept)
		}
		return unknownP(fmt.Sprintf("the row's %s is known only at run time", f))
	}
	return d.payloadField(f)
}

// graphEventEnvelopeKeys are the keys a graph event carries beside the flattened
// payload. firstVersion has its own answer; concept is the write's own
// unless a stored field of that name shadows it; the rest only the run
// knows.
var graphEventEnvelopeKeys = map[string]bool{"id": true, "nodeId": true, "nodeType": true, "actor": true, "createdAt": true, "oldStatus": true, "payload": true}

// argField reads an args binding, which binds from the event payload by
// name: the written payload, flattened, plus the envelope.
func (d filterDecider) argField(f string) pval {
	switch {
	case f == "firstVersion":
		switch d.row.FirstVersion {
		case triTrue:
			return knownP(true)
		case triFalse:
			return knownP(false)
		}
		return unknownP(d.row.why("firstVersion"))
	case f == "concept":
		// The envelope's concept is set before the payload is flattened
		// over it, so a stored `concept` field would win. Decided only when
		// the write says what that field holds or that no field is set.
		if _, set := d.row.Fields[f]; set || d.row.Unknown[f] || d.row.Absent[f] {
			return d.payloadField(f)
		}
		if d.row.OthersKnown {
			return knownP(d.row.Concept)
		}
		return unknownP(d.row.why(f))
	case graphEventEnvelopeKeys[f]:
		return unknownP(fmt.Sprintf("the event's %s is known only at run time", f))
	}
	return d.payloadField(f)
}

// payloadField reads one field of the written payload.
func (d filterDecider) payloadField(f string) pval {
	if v, ok := d.row.Fields[f]; ok {
		return knownP(normalizeLoopValue(v))
	}
	if d.row.Unknown[f] {
		return unknownP(d.row.why(f))
	}
	if d.row.Absent[f] || d.row.OthersKnown {
		return absentP
	}
	return unknownP(d.row.why(f))
}

// coalesce is `a ?? b`: b when a is absent, nil or blank (empty or
// whitespace-only); false, 0, [] and {} are kept. The final arm is returned
// even when blank, and an absent final arm is nil (coalesceSelect).
func (d filterDecider) coalesce(e *ast.BinaryExpr) pval {
	l := d.value(e.Left)
	switch l.kind {
	case pUnknown:
		return l
	case pKnown:
		if !loopBlank(l.v) {
			return l
		}
	}
	r := d.value(e.Right)
	if r.kind == pAbsent {
		return knownP(nil)
	}
	return r
}

// compare is == != < <= > >=.
func (d filterDecider) compare(e *ast.BinaryExpr) (tri, string) {
	l, r := d.value(e.Left), d.value(e.Right)
	switch e.Op {
	case "==", "!=":
		if l.kind == pUnknown {
			return triUnknown, l.why
		}
		if r.kind == pUnknown {
			return triUnknown, r.why
		}
		eq := loopEqual(l, r)
		if e.Op == "!=" {
			eq = !eq
		}
		return triOf(eq), ""
	}
	// An ordered comparison is false unless both sides are numbers or both
	// are strings, so one known side that is neither decides it whatever the
	// other holds.
	if l.kind != pUnknown && !loopOrderable(l) {
		return triFalse, ""
	}
	if r.kind != pUnknown && !loopOrderable(r) {
		return triFalse, ""
	}
	if l.kind == pUnknown {
		return triUnknown, l.why
	}
	if r.kind == pUnknown {
		return triUnknown, r.why
	}
	return triOf(loopOrdered(e.Op, l.v, r.v)), ""
}

// in is `v in list` over a list literal: `v == a || v == b`, an absent
// element contributing nothing. An absent or nil right side holds nothing;
// any other right side is not decided.
func (d filterDecider) in(e *ast.BinaryExpr) (tri, string) {
	l := d.value(e.Left)
	list, isList := ast.Unparen(e.Right).(*ast.ListExpr)
	if !isList || list == nil {
		r := d.value(e.Right)
		switch {
		case r.kind == pAbsent, r.kind == pKnown && r.v == nil:
			return triFalse, ""
		case r.kind == pUnknown:
			return triUnknown, r.why
		}
		return triUnknown, undecidable(e.Right)
	}
	out, why := triFalse, ""
	for _, el := range list.Elems {
		ev := d.value(el)
		if ev.kind == pAbsent {
			continue
		}
		if l.kind == pUnknown || ev.kind == pUnknown {
			if out == triFalse {
				out = triUnknown
				why = l.why
				if l.kind != pUnknown {
					why = ev.why
				}
			}
			continue
		}
		if loopEqual(l, ev) {
			return triTrue, ""
		}
	}
	return out, why
}

// startsWith is `s startsWith p`: p a string or a list of strings, blank
// prefixes dropped, an empty set matching nothing, and a subject that is not
// a string false.
func (d filterDecider) startsWith(e *ast.BinaryExpr) (tri, string) {
	l := d.value(e.Left)
	if l.kind == pAbsent || (l.kind == pKnown && !isLoopString(l.v)) {
		return triFalse, ""
	}
	var prefixes []string
	if list, isList := ast.Unparen(e.Right).(*ast.ListExpr); isList && list != nil {
		for _, el := range list.Elems {
			ev := d.value(el)
			switch {
			case ev.kind == pUnknown:
				return triUnknown, ev.why
			case ev.kind == pAbsent, ev.v == nil:
				continue
			}
			s, ok := ev.v.(string)
			if !ok {
				return triFalse, "" // refused at run time: it does not fire
			}
			prefixes = append(prefixes, s)
		}
	} else {
		r := d.value(e.Right)
		switch {
		case r.kind == pUnknown:
			return triUnknown, r.why
		case r.kind == pAbsent, r.v == nil:
			return triFalse, ""
		}
		s, ok := r.v.(string)
		if !ok {
			return triFalse, ""
		}
		prefixes = []string{s}
	}
	nonBlank := prefixes[:0]
	for _, p := range prefixes {
		if strings.TrimSpace(p) != "" {
			nonBlank = append(nonBlank, p)
		}
	}
	if len(nonBlank) == 0 {
		return triFalse, ""
	}
	if l.kind == pUnknown {
		return triUnknown, l.why
	}
	s := l.v.(string)
	for _, p := range nonBlank {
		if strings.HasPrefix(s, p) {
			return triTrue, ""
		}
	}
	return triFalse, ""
}

// loopUnset is D8's unset: absent, nil or the empty string. Whitespace is a
// value.
func loopUnset(p pval) bool {
	if p.kind == pAbsent || p.v == nil {
		return true
	}
	s, ok := p.v.(string)
	return ok && s == ""
}

// loopEqual is `==` for two values the load knows: unset equals only unset,
// and two set values compare by type.
func loopEqual(l, r pval) bool {
	lu, ru := loopUnset(l), loopUnset(r)
	if lu || ru {
		return lu && ru
	}
	return loopStrictEqual(l.v, r.v)
}

// loopStrictEqual is typed equality: numbers numerically across integers and
// floats, strings and bools as themselves, lists and maps deeply, and
// different types never equal.
func loopStrictEqual(a, b any) bool {
	a, b = normalizeLoopValue(a), normalizeLoopValue(b)
	switch x := a.(type) {
	case nil:
		return b == nil
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case string:
		y, ok := b.(string)
		return ok && x == y
	case int64, float64:
		c, ok := loopCompareNumbers(x, b)
		return ok && c == 0
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !loopStrictEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, xv := range x {
			yv, has := y[k]
			if !has || !loopStrictEqual(xv, yv) {
				return false
			}
		}
		return true
	}
	return false
}

func loopOrderable(p pval) bool {
	if p.kind != pKnown {
		return false
	}
	switch p.v.(type) {
	case int64, float64, string:
		return true
	}
	return false
}

// loopOrdered is `< <= > >=` over two orderable values: two numbers
// numerically, two strings by byte; a number against a string is false.
func loopOrdered(op string, l, r any) bool {
	var c int
	if ls, ok := l.(string); ok {
		rs, ok := r.(string)
		if !ok {
			return false
		}
		c = strings.Compare(ls, rs)
	} else {
		cc, ok := loopCompareNumbers(l, r)
		if !ok {
			return false
		}
		c = cc
	}
	switch op {
	case "<":
		return c < 0
	case "<=":
		return c <= 0
	case ">":
		return c > 0
	case ">=":
		return c >= 0
	}
	return false
}

// loopCompareNumbers compares two numbers: two integers exactly, anything
// else as floats. ok is false when either is not a number or is NaN.
func loopCompareNumbers(a, b any) (int, bool) {
	ai, aInt := a.(int64)
	bi, bInt := b.(int64)
	if aInt && bInt {
		switch {
		case ai < bi:
			return -1, true
		case ai > bi:
			return 1, true
		}
		return 0, true
	}
	af, aok := loopFloat(a)
	bf, bok := loopFloat(b)
	if !aok || !bok || math.IsNaN(af) || math.IsNaN(bf) {
		return 0, false
	}
	switch {
	case af < bf:
		return -1, true
	case af > bf:
		return 1, true
	}
	return 0, true
}

func loopFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

func isLoopString(v any) bool {
	_, ok := v.(string)
	return ok
}

// loopBlank is what `??` falls through: nil, or a string that is empty or
// whitespace-only.
func loopBlank(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

// normalizeLoopValue folds the Go integer types into int64 and float32 into
// float64, the two number types the comparisons know; everything else is
// returned as it is.
func normalizeLoopValue(v any) any {
	switch x := v.(type) {
	case int:
		return int64(x)
	case int8:
		return int64(x)
	case int16:
		return int64(x)
	case int32:
		return int64(x)
	case uint8:
		return int64(x)
	case uint16:
		return int64(x)
	case uint32:
		return int64(x)
	case uint:
		if uint64(x) > math.MaxInt64 {
			return float64(x)
		}
		return int64(x)
	case uint64:
		if x > math.MaxInt64 {
			return float64(x)
		}
		return int64(x)
	case float32:
		return float64(x)
	}
	return v
}
