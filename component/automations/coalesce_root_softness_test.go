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
	e.SetInput(map[string]any{"seeded": "yes"})
	e.SetStepResult("real", &StepResult{StepId: "real", Status: "success",
		Result: map[string]any{"x": "v"}})
	e.SetStepResult("rows", &StepResult{StepId: "rows", Status: "success",
		Result: []any{map[string]any{"id": "row-1"}, map[string]any{"id": "row-2"}}})
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
	{"missing input key", "input.nosuchkey"},
	// `item` is the forEach loop variable. Unbound outside a loop, which is
	// the state a coalesce in a non-loop position sees.
	{"missing item key", "item.nosuchkey"},
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
		{"step method call resolves", "steps.rows.first.id", "row-1"},
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
// TestUnresolvedBarePathConditionIsALoudError replaces a test that swallowed
// errors with `continue` and therefore PASSED against the unfixed code -- the
// review confirmed it would have stayed green through a full revert. It was the
// test the commit called "the predicate case that makes it a security bug",
// and it proved nothing.
//
// The real requirement is not "falsy". It is LOUD, and it belongs to memql#2819:
// an unresolved bare-path condition must ERROR, because "a silent false would
// trade a filter that always fires for one that never fires -- equally wrong
// and harder to notice."
//
// #2851 and #2819 pull opposite ways and both are right. Returning nil for an
// unresolved path (so coalesce reaches its fallback) silently defeated #2819's
// guard, which detects non-resolution by sniffing for the path's own text and
// cannot see a nil. explicitRootDidNotResolve is the companion that keeps the
// error.
func TestUnresolvedBarePathConditionIsALoudError(t *testing.T) {
	for _, expr := range []string{
		"var.killSwitch", "systemVar.missing", "secret.missing", "systemSecret.missing",
		"automation.bogus", "steps.nope.enabled", "steps.real.nosuchfield",
	} {
		t.Run(expr, func(t *testing.T) {
			e := softnessEvaluator(t)
			got, err := e.EvaluateCondition(expr)
			if err == nil {
				t.Errorf("EvaluateCondition(%q) = %v with NO error.\n\nAn unresolved bare-path "+
					"condition must fail LOUD (memql#2819). A silent false trades a filter that "+
					"always fires for one that never fires -- equally wrong and harder to notice. "+
					"Making the path resolve to nil for coalesce's sake must not cost this error.",
					expr, got)
			}
		})
	}
}

// TestMapBackedRootsStayQuietInAPredicate records the boundary deliberately.
//
// A missing key in a MAP-backed root resolves to (nil, nil) -- not a failure --
// so it stays silent, exactly as it was before #2851. #2819's text sniff never
// caught these either. Whether it SHOULD is a real question, but it is #2819's
// to answer; pinning the current boundary here stops #2851 from moving it by
// accident in either direction.
func TestMapBackedRootsStayQuietInAPredicate(t *testing.T) {
	for _, expr := range []string{"actor.nonexistent", "args.missing", "ctx.nosuchkey"} {
		t.Run(expr, func(t *testing.T) {
			e := softnessEvaluator(t)
			got, err := e.EvaluateCondition(expr)
			if err != nil {
				t.Errorf("EvaluateCondition(%q) errored: %v\nA missing key in a map-backed root "+
					"is a resolved absence, not a resolution failure; it was quiet before #2851 "+
					"and must stay quiet.", expr, err)
			}
			if got {
				t.Errorf("EvaluateCondition(%q) = true -- fail-OPEN", expr)
			}
		})
	}
}

// TestUnresolvedRootsAppearInTheResolutionMatrix keeps this file honest against
// the thing #2851 actually asked for: the resolver-backed roots belong in the
// enumerated matrix, not in a one-off test that the next root can skip.
func TestUnresolvedRootsAppearInTheResolutionMatrix(t *testing.T) {
	covered := map[string]bool{}
	for _, tc := range unresolvablePaths {
		if i := strings.IndexByte(tc.path, '.'); i > 0 {
			covered[tc.path[:i]] = true
		}
	}
	// Derived from explicitPathRoots, NOT a hardcoded list. The first version
	// hardcoded the nine roots that existed and therefore COULD NOT FAIL:
	// adding "brandNewRoot" to the shared map left this test -- and the whole
	// package -- green. It was the one thing memql#2851 explicitly asked for,
	// and it was decorative.
	for root := range explicitPathRoots {
		if !covered[root] {
			t.Errorf("root %q is in explicitPathRoots but has no row in unresolvablePaths.\n\n"+
				"Every explicit root must be enumerated in the matrix, or the next root added to "+
				"the shared list silently reintroduces memql#2851 for itself. Add a "+
				"`%s.<missing>` row.", root, root)
		}
	}
	// `actor` is not in explicitPathRoots (it resolves by a different route)
	// but is the root the whole #2380 family is about, so it is required
	// explicitly rather than left to the loop above.
	if !covered["actor"] {
		t.Error("the matrix must keep an actor.* row: it is the map-backed control that proves " +
			"the resolver-backed roots are being compared against something that already works.")
	}
}

// TestComparisonVerdictsThatCHANGE is the honest record of what this change
// costs, and it exists because the commit first claimed the opposite.
//
// I verified `== "success"` / `!= "success"` -- the ONE operand pair that
// happens to be invariant -- and generalised that to "both comparison verdicts
// are unchanged, provably not a weakening." Review enumerated all six
// operators in both positions and found FOUR that flip. Only `==` / `!=`
// against a NON-EMPTY literal are actually invariant.
//
// The flips are all the same shape: nil compares as absent/zero where the path
// text compared as a non-empty string. Every one moves from a verdict computed
// on a MEANINGLESS string ("steps.nosuch.status" is not data) to one computed
// on absence, so the new answers are the correct ones -- but they ARE changes,
// and pinning them here is the difference between a documented semantics change
// and an undocumented one.
func TestComparisonVerdictsThatCHANGE(t *testing.T) {
	for _, tc := range []struct {
		cond string
		want bool
		note string
	}{
		// The invariant pair -- the only one the original claim covered.
		{`steps.nosuch.status == "success"`, false, "invariant"},
		{`steps.nosuch.status != "success"`, true, "invariant"},

		// Empty-literal comparisons flip. This is the #2257 kill-switch shape.
		{`steps.nosuch.status == ""`, true, "FLIPPED from false"},
		{`steps.nosuch.status != ""`, false, "FLIPPED from true"},

		// Ordering flips: compareOrdered reads nil as 0 rather than
		// lexicographically comparing the path text.
		{`steps.nosuch.status > "success"`, false, "FLIPPED from true"},
		{`steps.nosuch.status <= "success"`, true, "FLIPPED from false"},
		{`5 > var.missing`, true, "FLIPPED from false"},

		// The natural "is it absent" spelling, and a distinct author idiom
		// from the empty-string pair above.
		{`steps.nosuch.status == null`, true, "FLIPPED from false"},
		{`steps.nosuch.status != null`, false, "FLIPPED from true"},
	} {
		t.Run(tc.cond, func(t *testing.T) {
			e := softnessEvaluator(t)
			got, err := e.EvaluateCondition(tc.cond)
			if err != nil {
				t.Fatalf("EvaluateCondition(%q): %v", tc.cond, err)
			}
			if got != tc.want {
				t.Errorf("EvaluateCondition(%q) = %v, want %v (%s)", tc.cond, got, tc.want, tc.note)
			}
		})
	}
}

// TestShippedTreeGateThatChanges pins the one live construct this change
// affects, which the PR claimed did not exist.
//
// `dsl/cognition/automations.memql` generateResponse gates two chat inserts on
// `steps.decide.result != ""`. When the decide step never ran, that read as
// "steps.decide.result" != "" -> TRUE, so the automation posted a chat
// utterance whose visible body was the literal string "steps.decide.result".
// The gate is now false and nothing is posted.
//
// I checked for exposure by grepping for `$coalesce` in dsl/, which was the
// wrong instrument entirely -- the exposure is through comparison operands. The
// resolver-backed roots do have zero live uses, so that half of the claim held;
// the `steps.` half did not.
func TestShippedTreeGateThatChanges(t *testing.T) {
	const gate = `steps.decide.result != ""`

	unrun := softnessEvaluator(t)
	got, err := unrun.EvaluateCondition(gate)
	if err != nil {
		t.Fatalf("EvaluateCondition(%q): %v", gate, err)
	}
	if got {
		t.Errorf("the generateResponse gate is TRUE with no `decide` step.\n\nThat is how a chat "+
			"utterance whose body was the literal text %q reached users: the unresolved path was "+
			"a non-empty string, so `!= \"\"` passed (memql#2851).", "steps.decide.result")
	}

	// And it must still fire when the step DID produce text, or the fix has
	// silenced a working feature rather than a bug.
	ran := softnessEvaluator(t)
	ran.SetStepResult("decide", &StepResult{StepId: "decide", Status: "success", Result: "hello"})
	got, err = ran.EvaluateCondition(gate)
	if err != nil {
		t.Fatalf("EvaluateCondition(%q) with a real result: %v", gate, err)
	}
	if !got {
		t.Error("the generateResponse gate is FALSE for a step that produced text -- the fix has " +
			"disabled a working chat path, not just the empty case.")
	}
}

// TestStepMethodCallInACoalesceArg is the memql#2851-review blocker.
//
// Adding `steps` to the shared root list routed method-call paths down the
// $-form path, which does not understand first() / last() / count(). The
// evaluator interpolated the step value and appended the leftover accessor:
//
//	coalesce(steps.rows.first().id, "FB")  ->  `{"id":"row-1"}().id`
//
// A serialized row object embedded in a string that coalesce still reads as
// PRESENT -- worse than the path text it replaced, and the identical hazard the
// comment above that guard already warned about.
//
// The CALL form is the point. My first fixture used the already-normalized
// dotted spelling (`steps.rows.first.id`), which never reaches the normalizer,
// so removing it left the suite green -- caught only by re-running the
// mutation.
func TestStepMethodCallInACoalesceArg(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		want       any
	}{
		{"first() then field", "steps.rows.first().id", "row-1"},
		{"last() then field", "steps.rows.last().id", "row-2"},
		{"count()", "steps.rows.count", 2},
		{"already-normalized dotted form still works", "steps.rows.first.id", "row-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := softnessEvaluator(t)
			got, err := evaluateCoalesceArgs([]string{tc.path, `"FB"`}, e)
			if err != nil {
				t.Fatalf("evaluateCoalesceArgs(%q): %v", tc.path, err)
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("coalesce(%s, \"FB\") = %#v, want %#v.\n\nA method-call path sent down "+
					"the $-form path without normalization interpolates the step VALUE and "+
					"appends the leftover accessor, producing a serialized object inside a "+
					"string -- still truthy, so the fallback is still skipped (memql#2851).",
					tc.path, got, tc.want)
			}
		})
	}
}

// TestUnresolvedStepMethodCallStillFallsBack is the same shape, unresolved.
func TestUnresolvedStepMethodCallStillFallsBack(t *testing.T) {
	e := softnessEvaluator(t)
	got, err := evaluateCoalesceArgs([]string{"steps.nope.first().id", `"FB"`}, e)
	if err != nil {
		t.Fatalf("evaluateCoalesceArgs: %v", err)
	}
	if got != "FB" {
		t.Errorf("coalesce(steps.nope.first().id, \"FB\") = %#v, want \"FB\"", got)
	}
}

// TestBarePathConditionResolvesExactlyOnce pins the single-pass property.
//
// The first cut of the #2819 guard asked EvaluateFilterValue for the value and
// then asked resolvePath AGAIN for resolvability. resolvePath calls the
// variable/secret resolvers for real, so every `@filter(secret.X)` did two
// secret-store reads -- and because the two reads are independent, a transient
// failure between them could hard-error a condition that resolved, or return a
// silent false for one that did not. The second is #2819's fail-quiet
// reintroduced by the guard added to prevent it.
//
// Counting resolver calls is the only way to see this: every value-level
// assertion passes either way.
func TestBarePathConditionResolvesExactlyOnce(t *testing.T) {
	for _, tc := range []struct {
		name, cond string
		resolves   bool
	}{
		{"resolving variable", "var.enabled", true},
		{"resolving secret", "secret.apiKey", true},
		{"missing variable", "var.missing", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var varCalls, secretCalls int
			e := NewEvaluator()
			e.SetVariableResolver(func(ctx context.Context, name string) (string, error) {
				varCalls++
				if name == "enabled" {
					return "yes", nil
				}
				return "", fmt.Errorf("no variable %q", name)
			})
			e.SetSecretResolver(func(ctx context.Context, name string) (string, error) {
				secretCalls++
				if name == "apiKey" {
					return "k", nil
				}
				return "", fmt.Errorf("no secret %q", name)
			})

			_, err := e.EvaluateCondition(tc.cond)
			if tc.resolves && err != nil {
				t.Fatalf("EvaluateCondition(%q): %v", tc.cond, err)
			}
			if !tc.resolves && err == nil {
				t.Fatalf("EvaluateCondition(%q) did not error for an unresolved path", tc.cond)
			}

			if got := varCalls + secretCalls; got != 1 {
				t.Errorf("EvaluateCondition(%q) invoked the resolvers %d times, want exactly 1.\n\n"+
					"Resolving twice doubles every secret-store read a filter performs, and the "+
					"two reads can disagree: a transient failure between them either hard-errors "+
					"a condition that resolved or silently returns false for one that did not "+
					"(memql#2851 / #2819).", tc.cond, got)
			}
		})
	}
}

// TestUnseededMapRootFailsLoud records the asymmetry the review surfaced: which
// side of the loud/quiet line a map-backed root lands on is decided at RUNTIME,
// not by its name.
//
// With `args` seeded -- which the executor always does -- `args.typo` is a
// resolved absence and stays quiet. With no args block at all the root does not
// exist, resolution FAILS, and the same condition is a loud error. Identical to
// baseline, so this is a record rather than a change; pinning both sides keeps
// it from drifting unnoticed in either direction.
func TestUnseededMapRootFailsLoud(t *testing.T) {
	e := NewEvaluator() // no SetCustom("args", ...)
	if _, err := e.EvaluateCondition("args.typo"); err == nil {
		t.Error("with `args` UNSEEDED, args.typo resolved quietly. The root does not exist, so " +
			"resolution fails and the condition must be loud -- the quiet side is only for a " +
			"missing KEY in a root that IS present.")
	}
}
