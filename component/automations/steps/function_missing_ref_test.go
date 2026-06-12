package steps

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
)

// These tests pin the fix for memql#1392: a compiler-emitted runtime
// reference (`$args.event.payload.summary`) whose path is MISSING from
// the payload must resolve to nil -- never survive as its own source
// text. Before the fix, resolveArgValueRef kept the original string on
// any resolution miss, renderMemQLValue passed it through unquoted
// (isRuntimeReference), and the literal `$args.event.payload.summary`
// landed in the stored artifact row as the summary.

// seedArgsEvaluator returns an evaluator seeded the way LogicRunner
// seeds a logic invocation: custom var `args` holding the event
// envelope.
func seedArgsEvaluator(payload map[string]any) *automations.Evaluator {
	eval := automations.NewEvaluator()
	eval.SetCustom("args", map[string]any{
		"event": map[string]any{
			"topic":   "v1:library:generatedOutput",
			"kind":    "created",
			"payload": payload,
		},
	})
	return eval
}

// Present path resolves to its value.
func TestCompiledArgReferencePresentPathResolves(t *testing.T) {
	eval := seedArgsEvaluator(map[string]any{
		"title":   "10 best German folklore tales for kids",
		"summary": "A curated list of folklore tales.",
	})

	got, err := resolveArgValueRef("$args.event.payload.summary", eval)
	if err != nil {
		t.Fatalf("resolveArgValueRef failed: %v", err)
	}
	if got != "A curated list of folklore tales." {
		t.Fatalf("expected resolved summary value, got %#v", got)
	}
}

// Missing leaf segment resolves to nil -- the raw reference text must
// never leak into the rendered mutation arg (the #1392 failure shape).
func TestCompiledArgReferenceMissingLeafDropsToNull(t *testing.T) {
	// payload has NO summary field -- the exact producer shape that
	// stamped the literal on staging.
	eval := seedArgsEvaluator(map[string]any{
		"title": "10 best German folklore tales for kids",
	})

	got, err := resolveArgValueRef("$args.event.payload.summary", eval)
	if err != nil {
		t.Fatalf("resolveArgValueRef failed: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for missing payload path, got %#v", got)
	}

	// End-to-end through the renderer: the literal must not appear.
	args := map[string]any{"summary": "$args.event.payload.summary", "title": "$args.event.payload.title"}
	resolved, err := resolveArgsRefs(args, eval)
	if err != nil {
		t.Fatalf("resolveArgsRefs failed: %v", err)
	}
	rendered := renderFunctionArgs(resolved)
	if strings.Contains(rendered, "$args.") {
		t.Fatalf("unresolved reference literal leaked into rendered args: %q", rendered)
	}
	if !strings.Contains(rendered, "summary=null") {
		t.Fatalf("expected missing summary to render as null, got %q", rendered)
	}
	if !strings.Contains(rendered, `title="10 best German folklore tales for kids"`) {
		t.Fatalf("expected present title to render as its value, got %q", rendered)
	}
}

// A missing INTERMEDIATE segment (payload.meta absent, .deep navigated
// off it) also drops to nil.
func TestCompiledArgReferenceMissingIntermediateDropsToNull(t *testing.T) {
	eval := seedArgsEvaluator(map[string]any{
		"title": "t",
	})

	got, err := resolveArgValueRef("$args.event.payload.meta.deep", eval)
	if err != nil {
		t.Fatalf("resolveArgValueRef failed: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for missing intermediate path, got %#v", got)
	}
}

// A reference whose ROOT cannot resolve at all (evaluator error, not
// just a nil walk) also drops to nil rather than leaking the literal.
func TestCompiledArgReferenceUnseededRootDropsToNull(t *testing.T) {
	eval := automations.NewEvaluator() // no args/event seeded

	got, err := resolveArgValueRef("$args.event.payload.summary", eval)
	if err != nil {
		t.Fatalf("resolveArgValueRef failed: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for unresolvable compiled reference, got %#v", got)
	}
}

// User data that merely starts with `$` is NOT a compiler reference and
// must pass through untouched.
func TestNonReferenceDollarStringsUntouched(t *testing.T) {
	eval := automations.NewEvaluator()

	for _, s := range []string{
		"$foo",            // no dot segment -- not a compiled reference shape
		"$100,000 budget", // user data: comma + space
		"$budget.total",   // dotted, but root is not an evaluator-seeded root
	} {
		got, err := resolveArgValueRef(s, eval)
		if err != nil {
			t.Fatalf("resolveArgValueRef(%q) failed: %v", s, err)
		}
		if got != s {
			t.Fatalf("expected %q to pass through untouched, got %#v", s, got)
		}
	}
}

// isCompiledReference shape table.
func TestIsCompiledReference(t *testing.T) {
	cases := map[string]bool{
		"$args.event.payload.summary": true,
		"$event.payload.id":           true,
		"$item.id":                    true,
		"$steps.getRow.result":        true,
		"$ctx.spaceId":                true,
		"$input.x":                    true,
		"$args":                       false, // no path segment
		"$event":                      false, // bare token (pinned by #418 test)
		"$foo.bar":                    false, // unknown root
		"$args.":                      false, // empty rest
		"$args.event payload":         false, // whitespace
		"$coalesce(args.x, 1)":        false, // expression, not a reference
		"args.event.payload.summary":  false, // no $ marker
		"$":                           false,
	}
	for expr, want := range cases {
		if got := isCompiledReference(expr); got != want {
			t.Errorf("isCompiledReference(%q) = %v, want %v", expr, got, want)
		}
	}
}
