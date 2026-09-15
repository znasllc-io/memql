package automations

import (
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// mustLambda parses a one-parameter lambda for the loop tests.
func mustLambda(t *testing.T, src string) *ast.LambdaExpr {
	t.Helper()
	lam, err := languageParser.ParseV1Lambda(src)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	return lam
}

// TestUntilInFilter: an @loop's until is covered when its automation's
// @filter holds the negation of until's predicate as a top-level conjunct --
// `!(P)`, or P's inverted comparison -- in the filter's own parameter.
func TestUntilInFilter(t *testing.T) {
	cases := []struct {
		name          string
		until, filter string
		want          bool
	}{
		{"the inverted comparison", `row => row.status == "done"`, `row => row.status != "done"`, true},
		{"the negation, beside another conjunct", `row => row.status == "done"`, `row => !(row.status == "done") && row.kind == "a"`, true},
		{"a filter that says something else", `row => row.status == "done"`, `row => row.status == "open"`, false},
		{"the filter's parameter is another name", `row => row.status == "done"`, `x => x.status != "done"`, true},
		{"until's parameter is another name", `t => t.status == "done"`, `row => row.status != "done"`, true},
		{"< inverts to >=", `row => row.n >= 3`, `row => row.n < 3`, true},
		{"> inverts to <=", `row => row.n > 3`, `row => row.n <= 3`, true},
		{"<= inverts to >", `row => row.n <= 3`, `row => row.n > 3`, true},
		{"!= inverts to ==", `row => row.status != "open"`, `row => row.status == "open"`, true},
		{"the last conjunct of three", `row => row.status == "done"`, `row => row.a == 1 && row.b == 2 && row.status != "done"`, true},
		{"a parenthesised conjunct", `row => row.status == "done"`, `row => (row.status != "done") && row.a == 1`, true},
		{"a parenthesised predicate", `row => (row.status == "done")`, `row => row.status != "done"`, true},
		{"a double negation", `row => !row.pending`, `row => row.pending`, true},
		{"a negated boolean field", `row => row.closed`, `row => !row.closed`, true},
		// Inside an || the negation narrows nothing on its own: a row with
		// kind "a" passes the filter whatever its status.
		{"the negation inside an ||", `row => row.status == "done"`, `row => row.status != "done" || row.kind == "a"`, false},
		{"the same comparison, not its negation", `row => row.status == "done"`, `row => row.status == "done"`, false},
		{"a different value", `row => row.status == "done"`, `row => row.status != "closed"`, false},
		// in and startsWith have no inverse operator; only !(P) covers them.
		{"in, negated", `row => row.status in ["done", "closed"]`, `row => !(row.status in ["done", "closed"])`, true},
		{"in, with no negation", `row => row.status in ["done", "closed"]`, `row => row.status == "open"`, false},
		// A lambda inside until that binds the same name is not renamed.
		{"a nested lambda shadowing the parameter", `row => row.items.any(row => row.done)`, `x => !(x.items.any(row => row.done))`, true},
		{"a nested lambda wrongly renamed", `row => row.items.any(row => row.done)`, `x => !(x.items.any(x => x.done))`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := untilInFilter(mustLambda(t, tc.filter), mustLambda(t, tc.until))
			if got != tc.want {
				t.Errorf("untilInFilter(filter %s, until %s) = %v, want %v", tc.filter, tc.until, got, tc.want)
			}
		})
	}

	// No filter covers nothing, and nothing is covered by no until.
	if untilInFilter(nil, mustLambda(t, `row => row.status == "done"`)) {
		t.Error("an automation with no @filter covered its until")
	}
	if untilInFilter(mustLambda(t, `row => row.status != "done"`), nil) {
		t.Error("an absent until was reported covered")
	}
}

// TestUntilInFilterDoesNotMutateUntil: the rename works on a copy -- the
// until lambda is the automation's own, and Task 8's graph reads it again.
func TestUntilInFilterDoesNotMutateUntil(t *testing.T) {
	until := mustLambda(t, `t => t.status == "done"`)
	before := ast.FormatExpr(until)
	if !untilInFilter(mustLambda(t, `row => row.status != "done"`), until) {
		t.Fatal("the renamed until was not found in the filter")
	}
	if after := ast.FormatExpr(until); after != before {
		t.Errorf("untilInFilter changed the until lambda: %s, was %s", after, before)
	}
}

// TestNegateComparison: the six comparisons invert to their complements;
// anything else has no inversion.
func TestNegateComparison(t *testing.T) {
	for src, want := range map[string]string{
		`row.a == 1`:   `row.a != 1`,
		`row.a != 1`:   `row.a == 1`,
		`row.a < 1`:    `row.a >= 1`,
		`row.a >= 1`:   `row.a < 1`,
		`row.a > 1`:    `row.a <= 1`,
		`row.a <= 1`:   `row.a > 1`,
		`(row.a == 1)`: `row.a != 1`,
	} {
		n, err := languageParser.ParseV1Expression(src)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		got, ok := negateComparison(n)
		if !ok {
			t.Errorf("negateComparison(%s): no inversion, want %s", src, want)
			continue
		}
		if s := ast.FormatExpr(got); s != want {
			t.Errorf("negateComparison(%s) = %s, want %s", src, s, want)
		}
	}
	for _, src := range []string{`row.a in [1, 2]`, `row.a startsWith "x"`, `row.a && row.b`, `row.a`, `!row.a`} {
		n, err := languageParser.ParseV1Expression(src)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		if got, ok := negateComparison(n); ok {
			t.Errorf("negateComparison(%s) = %s, want no inversion", src, ast.FormatExpr(got))
		}
	}
}

func TestUntilRenameCannotCaptureOuterRow(t *testing.T) {
	until, err := languageParser.ParseV1Lambda(`x => x.items.some(row => row.id == x.id)`)
	if err != nil {
		t.Fatal(err)
	}
	filter, err := languageParser.ParseV1Lambda(`row => !row.items.some(row => row.id == row.id)`)
	if err != nil {
		t.Fatal(err)
	}
	if untilInFilter(filter, until) {
		t.Fatal("captured outer row falsely proved convergence")
	}
}
