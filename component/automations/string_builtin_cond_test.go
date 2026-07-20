package automations

import (
	"strings"
	"testing"
)

// #2656: the admitted-set / behavior matrix. Every builtin the AST
// converter accepts in a logic-body positional slot must EVALUATE
// correctly here, or fail loudly -- never load green and evaluate
// wrong. Before this landed, isPositionalBuiltinName admitted only
// coalesce/cond plus the date set, so a string builtin in a cond
// predicate was never resolved locally and the comparison could not
// match: `lower(args.b) == "y"` was always-false, silently.
func TestStringBuiltinsEvaluateInCondPredicate(t *testing.T) {
	for _, tc := range []struct {
		name      string
		predicate string
		want      bool
	}{
		{"lower matches", `lower($v) == "abc"`, true},
		{"lower non-match", `lower($v) == "zzz"`, false},
		{"upper matches", `upper($v) == "ABC"`, true},
		{"trim matches", `trim($padded) == "abc"`, true},
		{"concat matches", `concat($v, "-", $v) == "AbC-AbC"`, true},
		{"nested in coalesce", `coalesce(lower($v), "") == "abc"`, true},
	} {
		e := NewEvaluator()
		e.SetCustom("v", "AbC")
		e.SetCustom("padded", "  abc  ")
		got, err := evaluateCondPredicate(tc.predicate, e)
		if err != nil {
			t.Errorf("%s: %q errored: %v", tc.name, tc.predicate, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: %q = %v, want %v (an always-false result here is the #2656 silent-wrong shape)", tc.name, tc.predicate, got, tc.want)
		}
	}
}

// The whole accepted set must be admitted by the runner, so no builtin
// can be load-green and locally unresolvable. This is the sweep the DoD
// asks for: the converter's positional set vs the runner's admit set.
func TestPositionalBuiltinAdmitSetCoversConverterSet(t *testing.T) {
	// The positional builtins component/memql/ast_converter.go accepts in
	// logic-body slots (convertPositionalBuiltin call sites).
	converterAccepts := []string{
		"coalesce", "concat", "cond", "first", "hash",
		"last", "lower", "shortId", "trim", "upper",
	}
	var unadmitted []string
	for _, name := range converterAccepts {
		if !isPositionalBuiltinName(name) {
			unadmitted = append(unadmitted, name)
		}
	}
	// first/last are COLLECTION accessors resolved by the collection-chain
	// path, not the positional-builtin path, so they are legitimately
	// outside this set. Everything else must be admitted.
	expectedOutside := map[string]bool{"first": true, "last": true}
	for _, name := range unadmitted {
		if !expectedOutside[name] {
			t.Errorf("builtin %q is accepted by the AST converter in positional slots but NOT admitted by the logic runner -- it will load green and evaluate wrong (#2656)", name)
		}
	}
}

// A builtin admitted by name but lacking a handler must error loudly
// rather than fall through to the literal source text.
func TestAdmittedBuiltinWithoutHandlerErrorsLoudly(t *testing.T) {
	e := NewEvaluator()
	_, err := e.evaluateStringBuiltinCall("notARealBuiltin", `"x"`)
	if err == nil {
		t.Fatal("an unknown string builtin must error, not return a value")
	}
	if !strings.Contains(err.Error(), "notARealBuiltin") {
		t.Errorf("error must name the builtin: %v", err)
	}
	// Arity is enforced: lower() takes exactly one argument.
	if _, err := e.evaluateStringBuiltinCall("lower", `"a", "b"`); err == nil {
		t.Error("lower() with two args must error")
	}
}

// The issue names THREE shapes that were all always-false: the EqExpr
// door, the parenthesised comparison, and bind-then-compare. All three
// route operand resolution through evaluateOperand, so the fix covers
// each; pin the bind-then-compare shape explicitly since it is the
// documented workaround that also failed.
func TestStringBuiltinBindThenCompare(t *testing.T) {
	e := NewEvaluator()
	e.SetCustom("b", "AbC")
	// z := lower($b) resolves to "abc"; then cond(z == "abc", ...) matches.
	z, err := e.evaluateOperand(`lower($b)`)
	if err != nil {
		t.Fatalf("bind step lower($b): %v", err)
	}
	if z != "abc" {
		t.Fatalf("lower($b) = %v, want abc", z)
	}
	e.SetCustom("z", z)
	got, err := evaluateCondPredicate(`$z == "abc"`, e)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if got != true {
		t.Errorf("bind-then-compare on a string builtin must match, got %v", got)
	}
}
