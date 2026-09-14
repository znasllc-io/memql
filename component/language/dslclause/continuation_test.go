package dslclause

import (
	"reflect"
	"testing"
)

func TestContinuesClause(t *testing.T) {
	cases := []struct {
		acc, next string
		want      bool
	}{
		// The codemod's wrapped filter: continuation lines open with the
		// connective.
		{`filter  row => row.a == args.a`, `&& row.b == args.b`, true},
		{`filter  row => a`, `|| b`, true},
		// A dangling operator, an open guard, a lambda arrow or a spec's `=`
		// left at the end of a line.
		{`filter  a == 1 &&`, `b == 2`, true},
		{`filter  when(args.x) {`, `x == args.x }`, true},
		{`spec agent richPredicate = row =>`, `row.a == 1`, true},
		{`spec agent richPredicate =`, `row => row.a == 1`, true},
		// A new clause.
		{`filter  a == 1`, `shape  thingFull`, false},
		{`shape  thingFull`, `sort "row.createdAt", "desc"`, false},
		// A `{` inside a string does not open anything.
		{`filter  a == "{"`, `shape x`, false},
		// Edition 2026 (memql#5364): a conditional broken around its branches,
		// and a lambda whose arrow opens the next line.
		{`filter  row => args.x != nil`, `? row.a == args.x : true`, true},
		{`filter  row => args.x != nil ?`, `row.a == args.x : true`, true},
		{`filter  row`, `=> row.a == 1`, true},
	}
	for _, c := range cases {
		if got := ContinuesClause(c.acc, c.next); got != c.want {
			t.Errorf("ContinuesClause(%q, %q) = %v, want %v", c.acc, c.next, got, c.want)
		}
	}
}

func TestClauseExtent(t *testing.T) {
	lines := []string{
		`query x y {`,                           // 0
		`  filter  row => row.a == args.a`,      // 1
		``,                                      // 2
		`          && row.b == args.b`,          // 3
		`  shape   thingFull`,                   // 4
		`  filter  when(args.x) {`,              // 5
		`    x == args.x`,                       // 6
		`  }`,                                   // 7: closes the guard, not the query
		`}`,                                     // 8
		`  filter  row => row.ownerUserId == 1`, // 9
		`}`,                                     // 10: the query's own brace
	}
	for _, c := range []struct{ open, want int }{
		{1, 3},   // a blank line inside the clause neither ends nor joins it
		{4, 4},   // a new clause keyword is never swallowed
		{5, 7},   // an open guard runs to its own closing brace
		{9, 9},   // ...and the query's brace is never part of the clause
		{-1, -1}, // out of range is returned unchanged
	} {
		if got := ClauseExtent(lines, c.open); got != c.want {
			t.Errorf("ClauseExtent(lines, %d) = %d, want %d", c.open, got, c.want)
		}
	}
}

func TestOpensLambda(t *testing.T) {
	yes := []string{
		`row => row.a == 1`,
		`  row=>row.a`,
		`actor => actor.role == "owner"`,
		`(a, b) => a + b`,
		`( x ) => x`,
		`r1 => r1.id == args.id`,
		// The forms the parser accepts and the struct-query rewriter's
		// refine check always did: no parameters, a trailing comma, and an
		// arrow on the next line.
		`() => 1`,
		`(x,) => x`,
		"row\n  => row.a == 1",
	}
	no := []string{
		`ownerUserId==actor.userId`,
		`(ownerUserId==actor.userId || actor.isClusterOwner==true)`,
		`when(args.x) { x == args.x }`,
		`isActiveRecord`,
		`row.status == args.status`,
		`(a, 1) => a`,
		`(,) => a`,
		`1row => x`,
		`row >= 1`,
		``,
	}
	for _, s := range yes {
		if !OpensLambda(s) {
			t.Errorf("OpensLambda(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if OpensLambda(s) {
			t.Errorf("OpensLambda(%q) = true, want false", s)
		}
	}
}

func TestSplitLambdaHeader(t *testing.T) {
	params, body, ok := SplitLambdaHeader(`(a, b) => a + b`)
	if !ok || !reflect.DeepEqual(params, []string{"a", "b"}) || body != "a + b" {
		t.Errorf("SplitLambdaHeader = %v %q %v", params, body, ok)
	}
	params, body, ok = SplitLambdaHeader(`  row => row.a == 1 && isX(row)`)
	if !ok || !reflect.DeepEqual(params, []string{"row"}) || body != "row.a == 1 && isX(row)" {
		t.Errorf("SplitLambdaHeader = %v %q %v", params, body, ok)
	}
	if _, _, ok := SplitLambdaHeader(`a == b`); ok {
		t.Error("a clause with no header split")
	}
	params, body, ok = SplitLambdaHeader(`(x,) => x`)
	if !ok || !reflect.DeepEqual(params, []string{"x"}) || body != "x" {
		t.Errorf("SplitLambdaHeader(trailing comma) = %v %q %v", params, body, ok)
	}
	params, _, ok = SplitLambdaHeader(`() => 1`)
	if !ok || len(params) != 0 {
		t.Errorf("SplitLambdaHeader(no params) = %v %v", params, ok)
	}
}
