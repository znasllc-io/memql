package automations

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// TestActorResolvesIdenticallyInEveryPosition is the matrix memql#2848 asked
// for: enumerate the evaluator positions an `actor.*` reference can appear in
// and assert every one resolves it the same way.
//
// The bug class is not any single site. Actor resolution was reimplemented per
// position, and each position had to learn about `actor` separately:
//
//	mutation_templates.go AST path   `update { id: actor.userId }`      #2841
//	logic_runner.go coalesce-arg     `role := actor.role ?? ""`         #2818
//	evaluator.go $-form coalesce-arg `$coalesce(actor.role, "")`        #2848 (this)
//	evaluator.go cond predicate      `cond(actor.isClusterOwner, ...)`  #2819
//
// Three had already been fixed one at a time. A table is the thing that would
// have caught all of them at once, so the table is the deliverable -- patching
// the fourth site alone just waits for a fifth.
//
// Why it is a security bug and not a correctness nit: an unresolved dotted
// path renders as its own SOURCE TEXT (memql#2380). `"actor.isClusterOwner"`
// is a non-empty string, and a non-empty string is truthy -- so in a predicate
// the failure is fail-OPEN, and in a value slot it is a silently wrong value
// with no diagnostic.
//
// WHAT THIS TABLE DOES NOT COVER YET, stated so the coverage is not overread:
// the `cond(...)` predicate position (memql#2819) is a different surface --
// EvaluateFilterValue does not evaluate `cond(...)` at all, returning the
// whole call as a literal string, so it needs its own entry point rather than
// a row here. Adding it belongs with that issue's fix; this table should grow
// a `cond` row then, which is precisely the point of having a table.
func TestActorResolvesIdenticallyInEveryPosition(t *testing.T) {
	newEval := func() *Evaluator {
		e := NewEvaluator()
		bindActorEnvelope(auth.ContextWithAccess(context.Background(), &auth.AccessContext{
			UserId: "v1:identity:user:u-1",
			Role:   auth.RoleOwner,
		}), e)
		return e
	}

	// Every position that can carry an `actor.<field>` reference, with the
	// value it must produce for the owner envelope seeded above.
	positions := []struct {
		name string
		eval func(e *Evaluator) (any, error)
		want any
	}{
		{
			name: "bare filter value",
			eval: func(e *Evaluator) (any, error) { return e.EvaluateFilterValue(`actor.role`) },
			want: "owner",
		},
		{
			name: "bare coalesce arg",
			eval: func(e *Evaluator) (any, error) { return e.EvaluateFilterValue(`coalesce(actor.role, "")`) },
			want: "owner",
		},
		{
			// The reported defect: a second coalesce resolver that never
			// learned the ambient roots and fell through to "unquoted string
			// literal", yielding the path text.
			name: "$-form coalesce arg",
			eval: func(e *Evaluator) (any, error) { return e.EvaluateFilterValue(`$coalesce(actor.role, "")`) },
			want: "owner",
		},
		{
			name: "$-form value",
			eval: func(e *Evaluator) (any, error) { return e.EvaluateValue(`$actor.role`) },
			want: "owner",
		},
	}

	for _, pos := range positions {
		t.Run(pos.name, func(t *testing.T) {
			got, err := pos.eval(newEval())
			if err != nil {
				t.Fatalf("%s: unexpected error: %v", pos.name, err)
			}
			if got != pos.want {
				extra := ""
				if s, ok := got.(string); ok && strings.Contains(s, "actor.") {
					extra = "  <- resolved to its own SOURCE TEXT, which is truthy: fail-open in a predicate, silently wrong in a value slot"
				}
				t.Errorf("%s = %#v, want %#v%s", pos.name, got, pos.want, extra)
			}
		})
	}
}

// TestActorResolutionIsNotPositionDependent states the invariant directly: the
// same reference must not mean different things depending on where it is
// written. Kept separate from the table above because THIS is the property --
// the table is only the enumeration that makes it checkable.
func TestActorResolutionIsNotPositionDependent(t *testing.T) {
	for _, field := range []string{"role", "userId", "isClusterOwner"} {
		e := NewEvaluator()
		bindActorEnvelope(auth.ContextWithAccess(context.Background(), &auth.AccessContext{
			UserId: "v1:identity:user:u-1",
			Role:   auth.RoleOwner,
		}), e)

		bare, err := e.EvaluateFilterValue("actor." + field)
		if err != nil {
			t.Fatalf("bare actor.%s: %v", field, err)
		}
		dollar, err := e.EvaluateFilterValue(fmt.Sprintf(`$coalesce(actor.%s, "SENTINEL")`, field))
		if err != nil {
			t.Fatalf("$coalesce(actor.%s): %v", field, err)
		}
		if fmt.Sprint(bare) != fmt.Sprint(dollar) {
			t.Errorf("actor.%s resolves to %#v bare but %#v through $coalesce; the same reference must not depend on the position it is written in",
				field, bare, dollar)
		}
	}
}

// TestUnresolvedActorPathIsNeverItsOwnText guards the failure MODE rather than
// any one position: whatever a position does with an actor reference, it must
// never hand back the path text. That string is truthy, so returning it turns
// an authorization predicate into a constant true.
func TestUnresolvedActorPathIsNeverItsOwnText(t *testing.T) {
	// The denying envelope -- no caller. Even here the reference must resolve
	// to the envelope's value (false / ""), never to "actor.isClusterOwner".
	e := NewEvaluator()
	bindNoCallerActorEnvelope(e)

	for _, expr := range []string{
		`actor.isClusterOwner`,
		`coalesce(actor.isClusterOwner, false)`,
		`$coalesce(actor.isClusterOwner, false)`,
	} {
		got, err := e.EvaluateFilterValue(expr)
		if err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		if s, ok := got.(string); ok && strings.Contains(s, "actor.") {
			t.Errorf("%s = %q -- the raw path text, which is a non-empty (truthy) string; an authorization predicate reading this is fail-OPEN", expr, s)
		}
		if got == true {
			t.Errorf("%s = true with NO caller; the denying envelope must not read as owner", expr)
		}
	}
}
