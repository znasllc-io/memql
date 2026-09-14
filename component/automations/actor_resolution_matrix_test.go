package automations

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// actorPosition is one position an `actor.*` reference can be written in,
// evaluated the way the runtime evaluates that position.
type actorPosition struct {
	name string
	eval func(t *testing.T, e *Evaluator, field string) any
}

// evalActorValue parses a v1 expression and evaluates it over e.
func evalActorValue(t *testing.T, e *Evaluator, src string) any {
	t.Helper()
	n, err := languageParser.ParseV1Expression(src)
	if err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}
	got, err := e.EvalV1(context.Background(), n)
	if err != nil {
		t.Fatalf("%s: %v", src, err)
	}
	return got
}

// actorPositions enumerates the positions of a run: a value expression (a
// query step's expression, a return), a `??` operand, and a value leaf of a
// step's argument map. A condition and a ternary predicate are asserted
// below, since they answer something other than the field's value.
//
// The string evaluator resolved `actor` separately in each position, and each
// had to learn about the root on its own (#2818, #2819, #2841, #2848). Every
// position is now one evaluator -- EvalExpr over RunScope -- so the table
// holds that as a fact to be checked rather than a promise.
func actorPositions() []actorPosition {
	return []actorPosition{
		{"value", func(t *testing.T, e *Evaluator, f string) any { return evalActorValue(t, e, "actor."+f) }},
		{"?? operand", func(t *testing.T, e *Evaluator, f string) any {
			return evalActorValue(t, e, "actor."+f+` ?? "SENTINEL"`)
		}},
		{"argument value leaf", func(t *testing.T, e *Evaluator, f string) any {
			t.Helper()
			src := "actor." + f
			n, err := languageParser.ParseV1Expression(src)
			if err != nil {
				t.Fatalf("parse %s: %v", src, err)
			}
			args, err := e.ResolveV1Map(context.Background(), map[string]any{"x": &ExprLeaf{Src: src, Node: n}})
			if err != nil {
				t.Fatalf("%s: %v", src, err)
			}
			return args["x"]
		}},
	}
}

// TestActorResolvesIdenticallyInEveryPosition is the matrix memql#2848 asked
// for: every position an `actor.*` reference can appear in resolves it the
// same way, to the envelope's value.
//
// Why it is a security bug and not a correctness nit: the string evaluator
// rendered an unresolved dotted path as its own SOURCE TEXT (memql#2380).
// `"actor.isClusterOwner"` is a non-empty string, and a non-empty string is
// truthy -- so in a predicate the failure was fail-OPEN, and in a value slot
// a silently wrong value with no diagnostic.
func TestActorResolvesIdenticallyInEveryPosition(t *testing.T) {
	e := NewEvaluator()
	bindActorEnvelope(auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "v1:identity:user:u-1",
		Role:   auth.RoleOwner,
	}), e)

	want := map[string]any{"role": "owner", "isClusterOwner": true}
	for _, pos := range actorPositions() {
		for field, w := range want {
			t.Run(pos.name+"/"+field, func(t *testing.T) {
				got := pos.eval(t, e, field)
				if got != w {
					extra := ""
					if s, ok := got.(string); ok && strings.Contains(s, "actor.") {
						extra = "  <- resolved to its own SOURCE TEXT, which is truthy: fail-open in a predicate, silently wrong in a value slot"
					}
					t.Errorf("actor.%s in a %s = %#v, want %#v%s", field, pos.name, got, w, extra)
				}
			})
		}
	}

	// The #2819 shape: a predicate over the actor choosing a value.
	if got := evalActorValue(t, e, `actor.isClusterOwner ? "ALLOW" : "DENY"`); got != "ALLOW" {
		t.Errorf("a ternary over the owner envelope = %#v, want \"ALLOW\"", got)
	}
	// And the condition position, which answers a boolean.
	n, err := languageParser.ParseV1Expression(`actor.role == "owner" && actor.isClusterOwner == true`)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := e.EvalV1Condition(context.Background(), n); err != nil || !ok {
		t.Errorf("the condition position did not read the owner envelope: ok=%v err=%v", ok, err)
	}
}

// TestActorResolutionIsNotPositionDependent states the invariant directly:
// the same reference must not mean different things depending on where it is
// written. The value read, the `??` read and the argument-leaf read must
// agree, for every field of the envelope.
func TestActorResolutionIsNotPositionDependent(t *testing.T) {
	e := NewEvaluator()
	bindActorEnvelope(auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "v1:identity:user:u-1",
		Role:   auth.RoleOwner,
	}), e)
	for _, field := range []string{"role", "userId", "isClusterOwner"} {
		var got []string
		for _, pos := range actorPositions() {
			got = append(got, fmt.Sprintf("%#v", pos.eval(t, e, field)))
		}
		for _, g := range got[1:] {
			if g != got[0] {
				t.Errorf("actor.%s resolves differently by position (value, ??, argument leaf): %v; the same reference must not depend on where it is written", field, got)
				break
			}
		}
	}
}

// TestUnresolvedActorPathIsNeverItsOwnText guards the failure MODE rather than
// any one position: whatever a position does with an actor reference, it must
// never hand back the path text, and with no caller it must never read as
// owner.
func TestUnresolvedActorPathIsNeverItsOwnText(t *testing.T) {
	// The denying envelope -- no caller -- and an unseeded run, which RunScope
	// answers with the same envelope (run_scope.go).
	noCaller := NewEvaluator()
	bindNoCallerActorEnvelope(noCaller)
	for name, e := range map[string]*Evaluator{"no caller": noCaller, "unseeded": NewEvaluator()} {
		for _, pos := range actorPositions() {
			got := pos.eval(t, e, "isClusterOwner")
			if s, ok := got.(string); ok && strings.Contains(s, "actor.") {
				t.Errorf("%s: actor.isClusterOwner in a %s = %q -- the raw path text, which is truthy; an authorization predicate reading this is fail-OPEN", name, pos.name, s)
			}
			if got == true {
				t.Errorf("%s: actor.isClusterOwner in a %s = true with NO caller; the denying envelope must not read as owner", name, pos.name)
			}
		}
		if got := evalActorValue(t, e, `actor.isClusterOwner ? "ALLOW" : "DENY"`); got != "DENY" {
			t.Errorf("%s: a ternary over the actor = %#v with NO caller, want \"DENY\" (memql#2819)", name, got)
		}
	}
}
