package memql

import (
	"context"
	"reflect"
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
)

// expr_object_test.go pins ExprObject (memql#5367): a scope value that
// answers its own member reads and stands for an underlying value everywhere
// else. The automations runtime's RunScope hands one out for every step -- a
// step's `.status` and `.count` are views over one stored result, and its
// bare name means the result -- so each place an evaluation can meet a value
// is checked here: a member read, an operand, a method receiver, a `??` arm,
// a container element, a named construct-call argument, and the value
// EvalExpr returns.

// testStepObject stands for rows, and answers `status` and `count` itself.
type testStepObject struct {
	rows   []any
	status string
}

func (o *testStepObject) ExprMember(field string) (any, bool) {
	switch field {
	case "status":
		return o.status, true
	case "count":
		return int64(len(o.rows)), true
	}
	return nil, false
}

func (o *testStepObject) ExprValue() any { return o.rows }

// testAbsentObject stands for the Absent sentinel: a step that never ran.
type testAbsentObject struct{}

func (testAbsentObject) ExprMember(string) (any, bool) { return nil, false }
func (testAbsentObject) ExprValue() any                { return Absent }

// testChainObject stands for another object: resolution follows the chain.
type testChainObject struct{ next ExprObject }

func (o *testChainObject) ExprMember(string) (any, bool) { return nil, false }
func (o *testChainObject) ExprValue() any                { return o.next }

func TestExprObject(t *testing.T) {
	rows := []any{
		map[string]any{"id": "a", "active": true},
		map[string]any{"id": "b", "active": false},
	}
	scope := MapScope{
		"rows":    &testStepObject{rows: rows, status: "success"},
		"none":    &testStepObject{rows: nil, status: "skipped"},
		"chained": &testChainObject{next: &testStepObject{rows: rows, status: "success"}},
		"never":   testAbsentObject{},
	}
	runExprCases(t, []exprCase{
		// A member the object answers.
		{"answered member", xmem(xid("rows"), "status"), scope, EvalOptions{}, "success"},
		{"answered count", xmem(xid("rows"), "count"), scope, EvalOptions{}, int64(2)},
		// A member it does not answer is read from what it stands for: an
		// index into the rows.
		{"deferred member", xmem(xmem(xid("rows"), "0"), "id"), scope, EvalOptions{}, "a"},
		// A method receiver is the value.
		{"method receiver", xmeth(xid("rows"), "count"), scope, EvalOptions{}, int64(2)},
		{"method with lambda", xmeth(xmeth(xid("rows"), "where", xlam("r", xpath("r", "active"))), "count"), scope, EvalOptions{}, int64(1)},
		{"empty over no rows", xmeth(xid("none"), "empty"), scope, EvalOptions{}, true},
		{"negated empty", xnot(xmeth(xid("rows"), "empty")), scope, EvalOptions{}, true},
		// An operand is the value: a list equals the list it stands for.
		{"operand", xbin("==", xmeth(xid("rows"), "count"), xlit(int64(2))), scope, EvalOptions{}, true},
		// `??` sees through the object: nil rows are unset, and so is an
		// object standing for absent.
		{"coalesce over nil rows", xbin("??", xid("none"), xlit("fallback")), scope, EvalOptions{}, "fallback"},
		{"coalesce over absent", xbin("??", xid("never"), xlit("fallback")), scope, EvalOptions{}, "fallback"},
		{"member of absent", xmem(xid("never"), "x"), scope, EvalOptions{}, Absent},
		// A chain of objects resolves to the last one's value.
		{"chained member", xmeth(xid("chained"), "count"), scope, EvalOptions{}, int64(2)},
	})

	// The value EvalExpr returns is never an object: a bare name comes back
	// as the rows, and a container holding one holds the rows.
	for _, c := range []struct {
		name string
		expr ast.ExpressionNode
		want any
	}{
		{"bare name", xid("rows"), rows},
		{"in a list", xlist(xid("rows")), []any{rows}},
		{"in a map", xmap("r", xid("rows")), map[string]any{"r": rows}},
		// The container rule applies to what the object stands for: an
		// object standing for absent is omitted, as an absent read is.
		{"absent in a list is omitted", xlist(xid("never"), xlit("x")), []any{"x"}},
		{"absent in a map is omitted", xmap("a", xid("never"), "b", xlit(int64(1))), map[string]any{"b": int64(1)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := EvalExpr(context.Background(), c.expr, scope, EvalOptions{})
			if err != nil {
				t.Fatalf("%s: %v", ast.FormatExpr(c.expr), err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("%s = %#v, want %#v", ast.FormatExpr(c.expr), got, c.want)
			}
		})
	}

	// IsAbsent reads through the object.
	if !IsAbsent(&testStepObject{}) {
		t.Error("IsAbsent(object standing for nil rows) = false")
	}
	if IsAbsent(&testStepObject{rows: rows}) {
		t.Error("IsAbsent(object standing for rows) = true")
	}
}
