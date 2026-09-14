package ast

import (
	"reflect"
	"strings"
	"testing"
)

func TestMemberPath(t *testing.T) {
	cases := []struct {
		n      ExpressionNode
		root   string
		fields []string
		ok     bool
	}{
		{path("row", "status"), "row", []string{"status"}, true},
		{path("row", "credentials", "keyHash"), "row", []string{"credentials", "keyHash"}, true},
		{opt(path("row", "lineage"), "planId"), "row", []string{"lineage", "planId"}, true},
		{id("now"), "now", nil, true},
		{mem(call("first", id("xs")), "id"), "", nil, false},
		{lit("s"), "", nil, false},
	}
	for _, c := range cases {
		root, fields, ok := MemberPath(c.n)
		if root != c.root || !reflect.DeepEqual(fields, c.fields) || ok != c.ok {
			t.Errorf("MemberPath(%s) = %q %v %v, want %q %v %v", FormatExpr(c.n), root, fields, ok, c.root, c.fields, c.ok)
		}
	}
}

func TestMemberPathsReportsMaximalChainsOnly(t *testing.T) {
	// row.ownerUserId == actor.userId && row.items.first().id == args.x && isX(row)
	n := bin("&&",
		bin("&&",
			bin("==", path("row", "ownerUserId"), path("actor", "userId")),
			bin("==", mem(&CallExpr{Receiver: path("row", "items"), Name: "first"}, "id"), path("args", "x"))),
		call("isX", id("row")))
	var got []string
	MemberPaths(n, func(root string, fields []string) {
		got = append(got, root+"."+strings.Join(fields, "."))
	})
	want := []string{"row.ownerUserId", "actor.userId", "row.items", "args.x"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MemberPaths = %v, want %v", got, want)
	}
}

// owner is the leaf the authz gates ask about: a comparison naming actor.userId.
func owner(n ExpressionNode) bool {
	found := false
	MemberPaths(n, func(root string, fields []string) {
		if root == "actor" && len(fields) == 1 && fields[0] == "userId" {
			found = true
		}
	})
	return found
}

func TestGuarantees(t *testing.T) {
	own := bin("==", path("row", "ownerUserId"), path("actor", "userId"))
	other := bin("==", path("row", "status"), path("args", "status"))
	absent := bin("==", path("args", "x"), &NilExpr{})
	present := bin("!=", path("args", "x"), &NilExpr{})
	cases := []struct {
		name string
		n    ExpressionNode
		want bool
	}{
		{"a lone leaf", own, true},
		{"a conjunct guarantees", bin("&&", other, own), true},
		{"a disjunct does not", bin("||", other, own), false},
		{"both arms of a disjunction", bin("||", own, &ParenExpr{Inner: own}), true},
		{"parentheses are transparent", &ParenExpr{Inner: bin("&&", own, other)}, true},
		{"negation never guarantees", &UnaryExpr{Op: "!", Operand: own}, false},
		// The optional-argument guard in both positions the codemod writes it.
		{"a guard under && is conditional", bin("&&", other, &ParenExpr{Inner: bin("||", absent, own)}), false},
		{"a guard under || admits its rows only when present", bin("||", &ParenExpr{Inner: bin("&&", present, own)}, own), true},
		{"a conditional needs both branches", &TernaryExpr{Condition: path("args", "mine"), Then: own, Else: other}, false},
		{"a conditional with both branches scoped", &TernaryExpr{Condition: path("args", "mine"), Then: own, Else: own}, true},
		{"the condition alone is no guarantee", &TernaryExpr{Condition: own, Then: other, Else: other}, false},
		{"nil is no guarantee", nil, false},
	}
	for _, c := range cases {
		if got := Guarantees(c.n, owner); got != c.want {
			t.Errorf("%s: Guarantees(%s) = %v, want %v", c.name, FormatExpr(c.n), got, c.want)
		}
	}
}

func TestPredicateLeavesAndConjuncts(t *testing.T) {
	a := bin("==", path("row", "a"), lit(int64(1)))
	b := call("isB", id("row"))
	c := bin("in", path("row", "c"), &ListExpr{Elems: []ExpressionNode{lit("x")}})
	n := bin("&&", &ParenExpr{Inner: bin("||", a, &UnaryExpr{Op: "!", Operand: b})}, c)

	var leaves []string
	PredicateLeaves(n, func(l ExpressionNode) { leaves = append(leaves, FormatExpr(l)) })
	if want := []string{`row.a == 1`, `isB(row)`, `row.c in ["x"]`}; !reflect.DeepEqual(leaves, want) {
		t.Errorf("PredicateLeaves = %v, want %v", leaves, want)
	}

	var conj []string
	for _, e := range Conjuncts(n) {
		conj = append(conj, FormatExpr(e))
	}
	if want := []string{`row.a == 1 || !isB(row)`, `row.c in ["x"]`}; !reflect.DeepEqual(conj, want) {
		t.Errorf("Conjuncts = %v, want %v", conj, want)
	}
}
