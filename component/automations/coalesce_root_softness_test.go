package automations

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// coalesce_root_softness_test.go -- memql#2851.
//
// `coalesce` is documented as intentionally soft: the comment at
// evaluator.go's coalesce case says it exists so an author can write
// `$coalesce(steps.x.result[0].y, "fallback")` without separate guards. That
// promise held only for MAP-BACKED roots.
//
// EvaluateFilterValue returned the expression's own SOURCE TEXT for an
// explicit-root path that failed to resolve. resolvePath returns (nil, nil)
// for a missing key in a map-backed root, so `actor.*` / `ctx.*` / `item.*`
// and a seeded `args.*` were genuinely soft. The RESOLVER-backed roots
// (`var.` / `systemVar.` / `secret.` / `systemSecret.` / `automation.`) and an
// unresolvable `steps.` sub-path returned their text instead, and coalesce read
// that as a PRESENT value -- so the fallback was never reached.
//
// The returned text is non-empty and therefore TRUTHY. That is the #2380
// hazard: a default that exists precisely to be safe is skipped.
//
//	enabled := var.killSwitch ?? false   // -> "var.killSwitch" -> truthy
//
// A kill switch written that way is ON when its variable is missing, which is
// the opposite of what the author wrote. Fail-OPEN.
//
// MEASURED CORRECTIONS TO #2851'S OWN TABLE, so the record is accurate:
//
//   - the issue lists `$coalesce(args.missing, "FB") => "args.missing"`. That
//     is only true when `args` is not seeded at all. The executor always calls
//     SetCustom("args", boundArgs) (executor.go, scheduler.go, logic_runner.go),
//     and once `args` is a map -- even a nil or empty one -- the root is
//     map-backed and already soft. Verified all four shapes below.
//   - `steps.` is soft for a RESOLVABLE path (`steps.s.result.x` -> the value)
//     and hard only for an unresolvable one, including a bad sub-path on a step
//     that exists.

// softnessEvaluator builds an evaluator with every resolver seeded and every
// resolver-backed lookup failing, which is the state an automation is in when
// it references a variable/secret that was never set.
func softnessEvaluator(t *testing.T) *Evaluator {
	t.Helper()
	e := NewEvaluator()
	bindActorEnvelope(auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "v1:identity:user:u-1", Role: auth.RoleOwner,
	}), e)
	fail := func(kind string) VariableResolver {
		return func(ctx context.Context, name string) (string, error) {
			return "", fmt.Errorf("no such %s %q", kind, name)
		}
	}
	e.SetVariableResolver(fail("variable"))
	e.SetSystemVariableResolver(fail("system variable"))
	e.SetSecretResolver(fail("secret"))
	e.SetSystemSecretResolver(fail("system secret"))
	// Seeded exactly as the executor seeds it.
	e.SetCustom("args", map[string]any{"present": "yes"})
	e.SetCustom("ctx", map[string]any{"input": map[string]any{}})
	e.SetStepResult("real", &StepResult{StepId: "real", Status: "success",
		Result: map[string]any{"x": "v"}})
	return e
}

// unresolvablePaths is every explicit-root shape that does NOT resolve. Each
// must be treated as ABSENT, so a coalesce fallback is taken and a bare
// reference yields nil rather than truthy text.
var unresolvablePaths = []struct{ name, path string }{
	{"missing variable", "var.missing"},
	{"missing system variable", "systemVar.missing"},
	{"missing secret", "secret.missing"},
	{"missing system secret", "systemSecret.missing"},
	{"unknown automation field", "automation.bogus"},
	{"missing step", "steps.nope.x"},
	{"bad sub-path on a step that EXISTS", "steps.real.nosuchfield"},
	{"missing key in a map-backed root", "args.missing"},
	{"missing actor field", "actor.nonexistent"},
	{"missing ctx key", "ctx.nosuchkey"},
}

// TestUnresolvedPathIsAbsentNotItsOwnText is the property, stated once.
func TestUnresolvedPathIsAbsentNotItsOwnText(t *testing.T) {
	for _, tc := range unresolvablePaths {
		t.Run(tc.name, func(t *testing.T) {
			e := softnessEvaluator(t)
			got, err := e.EvaluateFilterValue(tc.path)
			if err != nil {
				t.Fatalf("EvaluateFilterValue(%q): %v", tc.path, err)
			}
			if got != nil {
				t.Errorf("EvaluateFilterValue(%q) = %#v, want nil.\n\n"+
					"An unresolved explicit-root path renders as its own SOURCE TEXT, which is "+
					"non-empty and therefore TRUTHY (memql#2380). In a predicate that is "+
					"fail-OPEN; in a value slot it is a silently wrong value with no "+
					"diagnostic (memql#2851).", tc.path, got)
			}
		})
	}
}

// TestCoalesceFallbackIsReachedForEveryUnresolvedRoot is the same property at
// the position the issue is filed about, in BOTH spellings.
func TestCoalesceFallbackIsReachedForEveryUnresolvedRoot(t *testing.T) {
	for _, tc := range unresolvablePaths {
		// BOTH coalesce surfaces, because they are different code:
		//
		//   $coalesce(...)  -> the evaluator's own builtin case
		//   coalesce(...)   -> logic_runner's evaluateCoalesceArgs, which is
		//                      also where `x ?? y` lands. `??` is a lexer token
		//                      (TokenQuestionQuestion) that the parser lowers to
		//                      a coalesce call, so the operator spelling in
		//                      #2851's motivating example -- `enabled :=
		//                      var.killSwitch ?? false` -- reaches this surface,
		//                      NOT EvaluateFilterValue. Asserting `??` against
		//                      EvaluateFilterValue would test nothing: it does
		//                      not handle the operator and hands the whole
		//                      expression back as a literal.
		for _, form := range []struct {
			label string
			eval  func(e *Evaluator) (any, error)
		}{
			{"$coalesce", func(e *Evaluator) (any, error) {
				return e.EvaluateFilterValue(`$coalesce(` + tc.path + `, "FB")`)
			}},
			{"coalesce (logic body, the ?? lowering)", func(e *Evaluator) (any, error) {
				return evaluateCoalesceArgs([]string{tc.path, `"FB"`}, e)
			}},
		} {
			t.Run(tc.name+"/"+form.label, func(t *testing.T) {
				e := softnessEvaluator(t)
				got, err := form.eval(e)
				if err != nil {
					t.Fatalf("%s(%q): %v", form.label, tc.path, err)
				}
				if got != "FB" {
					t.Errorf("%s / %s = %#v, want \"FB\".\n\ncoalesce is documented as intentionally "+
						"soft so an author can supply a default without separate guards. For a "+
						"resolver-backed root the unresolved path came back as its own text, "+
						"which coalesce read as a PRESENT value -- so the default was skipped and "+
						"the caller got a truthy string. `enabled := var.killSwitch ?? false` "+
						"then yields \"var.killSwitch\", and the kill switch is ON when its "+
						"variable is missing (memql#2851).", tc.path, form.label, got)
				}
			})
		}
	}
}

// TestResolvedPathsAreUnaffected is the direction that keeps the fix honest.
// Making unresolved paths nil must not make RESOLVED ones nil, or every
// automation that reads a step result breaks.
func TestResolvedPathsAreUnaffected(t *testing.T) {
	for _, tc := range []struct {
		name, expr string
		want       any
	}{
		{"step result field", "steps.real.result.x", "v"},
		{"step status", "steps.real.status", "success"},
		{"seeded arg", "args.present", "yes"},
		{"actor field", "actor.role", "owner"},
		{"coalesce takes the resolved value", `$coalesce(steps.real.result.x, "FB")`, "v"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := softnessEvaluator(t)
			got, err := e.EvaluateFilterValue(tc.expr)
			if err != nil {
				t.Fatalf("EvaluateFilterValue(%q): %v", tc.expr, err)
			}
			if got != tc.want {
				t.Errorf("EvaluateFilterValue(%q) = %#v, want %#v", tc.expr, got, tc.want)
			}
		})
	}
}

// TestNonPathLiteralsStillPassThrough is the other honesty direction: only
// EXPLICIT-ROOT paths become nil. A dotted token that is not a known root is an
// ordinary literal and must survive verbatim, or every version string, hostname
// and file name in the corpus turns into nil.
func TestNonPathLiteralsStillPassThrough(t *testing.T) {
	for _, lit := range []string{
		"example.com",
		"v1.2.3",
		"file.memql",
		"1.5",
		"plainword",
		"v1:identity:user:u-1",
	} {
		t.Run(lit, func(t *testing.T) {
			e := softnessEvaluator(t)
			got, err := e.EvaluateFilterValue(lit)
			if err != nil {
				t.Fatalf("EvaluateFilterValue(%q): %v", lit, err)
			}
			if got != lit {
				t.Errorf("EvaluateFilterValue(%q) = %#v, want the literal back. Only an "+
					"EXPLICIT-ROOT path may become nil; a dotted token whose first segment is "+
					"not a known root is an ordinary literal.", lit, got)
			}
		})
	}
}

// TestUnresolvedPathIsFalsyInAPredicate is why this is a security bug rather
// than a correctness nit. The truthy-source-text behaviour makes a guard
// fail OPEN.
func TestUnresolvedPathIsFalsyInAPredicate(t *testing.T) {
	e := softnessEvaluator(t)
	for _, expr := range []string{"var.killSwitch", "secret.missing", "steps.nope.enabled"} {
		got, err := e.EvaluateCondition(expr)
		if err != nil {
			// An error is an acceptable outcome -- it is not fail-open.
			continue
		}
		if got {
			t.Errorf("EvaluateCondition(%q) = true for an UNRESOLVED path.\n\nThat is fail-OPEN: "+
				"the path renders as its own non-empty source text, which is truthy, so a guard "+
				"written to be safe when a value is missing admits instead (memql#2380 / #2851).",
				expr)
		}
	}
}

// TestUnresolvedRootsAppearInTheResolutionMatrix keeps this file honest against
// the thing #2851 actually asked for: the resolver-backed roots belong in the
// enumerated matrix, not in a one-off test that the next root can skip.
func TestUnresolvedRootsAppearInTheResolutionMatrix(t *testing.T) {
	roots := map[string]bool{}
	for _, tc := range unresolvablePaths {
		if i := strings.IndexByte(tc.path, '.'); i > 0 {
			roots[tc.path[:i]] = true
		}
	}
	for _, want := range []string{
		"var", "systemVar", "secret", "systemSecret", "automation", "steps", "args", "actor", "ctx",
	} {
		if !roots[want] {
			t.Errorf("root %q is not covered by unresolvablePaths. Every explicit root "+
				"conditionRootSegment recognises must be enumerated here, or the next root added "+
				"to that switch silently reintroduces memql#2851.", want)
		}
	}
}
