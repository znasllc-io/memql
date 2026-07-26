package automations

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// TestBarePathConditionAgreesWithComparison is the regression evidence for
// memql#2819. A bare-path condition was unconditionally TRUE, because an
// unresolved path renders as its own text and a non-empty string is truthy:
//
//	@filter(actor.isClusterOwner)          -> TRUE   even with no caller
//	@filter(actor.isClusterOwner == true)  -> false  (correct)
//
// #2801 could not reach it. That change makes an absent actor DENY and binds
// the envelope everywhere, which fixes both comparison spellings -- but the
// truthiness here was decided on the rendered string before any resolved value
// participated, so binding changed nothing.
//
// Two defects met at this fall-through and both had to go:
//
//  1. the WRONG RESOLVER -- EvaluateValue does not resolve the ambient roots
//     (that is EvaluateFilterValue's job), which is exactly why the comparison
//     operands resolved and the bare path did not;
//  2. no failure mode for a path that genuinely does not resolve -- it fell
//     through to truthy-on-its-own-text.
//
// The property asserted is agreement: the bare form and the comparison form
// must answer the same way, for the same envelope, in both directions.
func TestBarePathConditionAgreesWithComparison(t *testing.T) {
	owner := func() *Evaluator {
		e := NewEvaluator()
		bindActorEnvelope(auth.ContextWithAccess(context.Background(),
			&auth.AccessContext{UserId: "v1:identity:user:u-1", Role: auth.RoleOwner}), e)
		return e
	}
	noCaller := func() *Evaluator {
		e := NewEvaluator()
		bindNoCallerActorEnvelope(e)
		return e
	}

	cases := []struct {
		name     string
		eval     func() *Evaluator
		bare     string
		compared string
		want     bool
	}{
		{"owner", owner, `actor.isClusterOwner`, `actor.isClusterOwner == true`, true},
		// The fail-open case: this returned TRUE for a caller that does not
		// exist, which is the whole issue.
		{"no caller", noCaller, `actor.isClusterOwner`, `actor.isClusterOwner == true`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bare, err := tc.eval().evaluateAtomicCondition(tc.bare)
			if err != nil {
				t.Fatalf("bare %q: unexpected error: %v", tc.bare, err)
			}
			compared, err := tc.eval().evaluateAtomicCondition(tc.compared)
			if err != nil {
				t.Fatalf("compared %q: unexpected error: %v", tc.compared, err)
			}
			if bare != tc.want {
				t.Errorf("%q = %v, want %v -- a bare path must not be truthy on its own rendered text", tc.bare, bare, tc.want)
			}
			if bare != compared {
				t.Errorf("%q = %v but %q = %v; the two spellings of the same gate must agree", tc.bare, bare, tc.compared, compared)
			}
		})
	}
}

// TestUnresolvedBarePathConditionFailsClosed pins the second half: a path that
// does not resolve is an authoring error, not a match.
//
// Erroring rather than returning false is deliberate. A silent false would
// trade a filter that always fires for one that never fires -- equally wrong
// and harder to notice, since a filter that never matches looks like "the
// event just didn't happen".
func TestUnresolvedBarePathConditionFailsClosed(t *testing.T) {
	e := NewEvaluator()
	bindNoCallerActorEnvelope(e)

	// These render as their own text: nothing resolves them in this context,
	// which is precisely the shape that used to be unconditionally true.
	for _, cond := range []string{
		`payload.someMissingFlag`,
		`totally.bogus.path`,
	} {
		got, err := e.evaluateAtomicCondition(cond)
		if err == nil {
			t.Errorf("%q = %v with no error; an unresolved bare path must not silently decide a filter", cond, got)
			continue
		}
		if got {
			t.Errorf("%q returned true alongside its error; the filter must not match", cond)
		}
		// The message has to name the trap, or the next author reads
		// "does not resolve" and adds a field instead of a comparison.
		if !strings.Contains(err.Error(), "match EVERYTHING") {
			t.Errorf("%q error does not explain the consequence: %v", cond, err)
		}
	}

	// The boundary worth pinning: a path whose ROOT is bound but whose field
	// is absent is NOT the same thing. `actor.noSuchField` resolves to nil --
	// the envelope is a map and the key is missing -- so it is a resolved
	// falsy value, not unrendered text, and evaluating it to false without an
	// error is correct. Erroring here instead would reject a legitimate
	// "is this optional field set" gate.
	//
	// Stated as a case because the distinction is invisible from the outside:
	// both spellings look like "the field isn't there", and only one of them
	// was ever the fail-open bug.
	got, err := e.evaluateAtomicCondition(`actor.noSuchField`)
	if err != nil {
		t.Errorf("actor.noSuchField: unexpected error %v; a bound root with an absent field resolves to nil, which is falsy, not unresolved", err)
	}
	if got {
		t.Error("actor.noSuchField = true; an absent field must not be truthy")
	}
}

// TestBarePathGuardLeavesOtherConditionShapesAlone guards the blast radius:
// the new branch must only intercept a bare dotted path. Everything else --
// comparisons, exists(), negation, literals, and a bare single identifier
// (the legitimate bare-step-variable shape) -- has to reach its existing
// handler untouched.
func TestBarePathGuardLeavesOtherConditionShapesAlone(t *testing.T) {
	for _, tc := range []struct {
		expr string
		want bool
	}{
		{`true`, true},
		{`false`, false},
		{`"nonempty"`, true},
		{`1 > 0`, true},
		{`1 < 0`, false},
	} {
		e := NewEvaluator()
		bindNoCallerActorEnvelope(e)
		got, err := e.evaluateAtomicCondition(tc.expr)
		if err != nil {
			t.Errorf("%q: unexpected error: %v", tc.expr, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%q = %v, want %v", tc.expr, got, tc.want)
		}
	}

	// A single identifier is NOT a dotted path and must not be intercepted --
	// it is how a bare step variable is written.
	if isBareDottedPath("toStatus") {
		t.Error("isBareDottedPath(\"toStatus\") = true; a single identifier is the bare-step-variable shape, not a path")
	}
	for _, notAPath := range []string{`a == b`, `exists(payload.x)`, `!flag`, `"a.b"`, `f(a.b)`, `a.b == c.d`, ``} {
		if isBareDottedPath(notAPath) {
			t.Errorf("isBareDottedPath(%q) = true; only a plain dotted reference may be intercepted", notAPath)
		}
	}
	for _, path := range []string{`actor.isClusterOwner`, `payload.x`, `a.b.c.d`, ` payload.x `} {
		if !isBareDottedPath(path) {
			t.Errorf("isBareDottedPath(%q) = false; this is the shape the guard exists for", path)
		}
	}

	// A condition does not always reach the evaluator as the author typed it.
	// The `forEach ... where` parser rebuilds the clause with
	// strings.Join(parts, " "), so `where a.b` arrives as `a . b` -- and an
	// anchored no-space pattern silently did not match it, leaving this exact
	// fail-open alive on that path. No such clause exists in dsl/ today, which
	// is why it needs a test rather than a corpus check.
	for _, spaced := range []string{`a . b`, `actor . isClusterOwner`, "a\t.\tb", `a. b`, `a .b`} {
		if !isBareDottedPath(spaced) {
			t.Errorf("isBareDottedPath(%q) = false; the where-clause parser space-joins tokens, so this reaches the evaluator and must still be guarded", spaced)
		}
	}
}

// TestSpaceJoinedBarePathStillFailsClosed is the end-to-end half of the
// whitespace case: matching the shape only helps if the condition then
// refuses to fire.
func TestSpaceJoinedBarePathStillFailsClosed(t *testing.T) {
	e := NewEvaluator()
	bindNoCallerActorEnvelope(e)

	got, err := e.evaluateAtomicCondition(`actor . isClusterOwner`)
	if err == nil {
		t.Fatalf("`actor . isClusterOwner` = %v with no error; the space-joined spelling must not slip the guard", got)
	}
	if got {
		t.Error("`actor . isClusterOwner` returned true alongside its error")
	}
}
