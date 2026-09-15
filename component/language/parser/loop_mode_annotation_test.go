package parser

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/annotations"
	"github.com/znasllc-io/memql/component/language/ast"
)

// loopModeProbe is an event-triggered automation carrying head.
func loopModeProbe(head string) string {
	return `@trigger(event="node.created", concept="v1:probe:ticket")
` + head + `
automation probe {
  step first {
    logic other(x: 1)
  }
}`
}

// wantAnnotationRefusal holds err to a refusal carrying code on an
// *annotations.Refusal, last in its message, and saying each fragment.
func wantAnnotationRefusal(t *testing.T, err error, code string, fragments ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("parsed; want the %s refusal", code)
	}
	var ref *annotations.Refusal
	if !errors.As(err, &ref) {
		t.Fatalf("want an *annotations.Refusal with code %s, got %T: %v", code, err, err)
	}
	if ref.Code != code {
		t.Fatalf("refused with %s, want %s: %v", ref.Code, code, err)
	}
	if !strings.HasSuffix(err.Error(), "["+code+"]") {
		t.Errorf("the refusal does not end with [%s]: %v", code, err)
	}
	for _, f := range fragments {
		if !strings.Contains(err.Error(), f) {
			t.Errorf("the refusal does not say %q: %v", f, err)
		}
	}
}

// TestLoopAnnotationParses: @loop's keys fold onto AutomationDef.Loop -- the
// depth as an int, the until as the lambda itself, in either order.
func TestLoopAnnotationParses(t *testing.T) {
	for _, head := range []string{
		`@loop(maxDepth=2, until=row => row.x == 1)`,
		`@loop(until=row => row.x == 1, maxDepth=2)`,
	} {
		t.Run(head, func(t *testing.T) {
			auto := automationBody(t, mustParseV1Authored(t, loopModeProbe(head)))
			if auto.Loop == nil {
				t.Fatal("AutomationDef.Loop is nil")
			}
			if auto.Loop.MaxDepth != 2 || !auto.Loop.MaxDepthSet {
				t.Errorf("MaxDepth = %d (set %v), want 2 (set)", auto.Loop.MaxDepth, auto.Loop.MaxDepthSet)
			}
			if auto.Loop.Until == nil || len(auto.Loop.Until.Params) != 1 {
				t.Fatalf("Until = %+v, want a one-parameter lambda", auto.Loop.Until)
			}
			if got, want := ast.FormatExpr(auto.Loop.Until), "row => row.x == 1"; got != want {
				t.Errorf("Until = %s, want %s", got, want)
			}
			if auto.Mode != nil {
				t.Errorf("Mode = %+v, want nil", auto.Mode)
			}
		})
	}
}

// TestModeAnnotationParses: @mode's bare keys are its flags, in the order
// written, and max= its bound.
func TestModeAnnotationParses(t *testing.T) {
	for head, want := range map[string]ModeDef{
		`@mode(queued, max=5)`:   {Flags: []string{"queued"}, Max: 5, MaxSet: true},
		`@mode(single)`:          {Flags: []string{"single"}},
		`@mode(max=3, parallel)`: {Flags: []string{"parallel"}, Max: 3, MaxSet: true},
		// Two flags, or none, parse: the load refuses them (mode_flags), by
		// the names written.
		`@mode(single, queued)`: {Flags: []string{"single", "queued"}},
		`@mode(max=3)`:          {Max: 3, MaxSet: true},
	} {
		t.Run(head, func(t *testing.T) {
			auto := automationBody(t, mustParseV1Authored(t, loopModeProbe(head)))
			if auto.Mode == nil {
				t.Fatal("AutomationDef.Mode is nil")
			}
			if !reflect.DeepEqual(*auto.Mode, want) {
				t.Errorf("Mode = %+v, want %+v", *auto.Mode, want)
			}
			if auto.Loop != nil {
				t.Errorf("Loop = %+v, want nil", auto.Loop)
			}
		})
	}
}

// TestLoopAndModeParseRefusals: what the parser can decide from the source
// alone -- the until's form, a depth or a bound that is not a whole number,
// a key left out -- is refused here, with the rule id the load uses.
func TestLoopAndModeParseRefusals(t *testing.T) {
	for _, tc := range []struct {
		head, code string
		fragments  []string
	}{
		{`@loop(maxDepth=2, until="done")`, "loop_until_form", []string{"until=row =>", "as in @loop(maxDepth=4, until=row => row.status =="}},
		{`@loop(maxDepth=2, until=row.status == "done")`, "loop_until_form", []string{"until=row =>"}},
		{`@loop(maxDepth=2, until=(a, b) => a == b)`, "loop_until_form", []string{"one parameter", "got 2"}},
		{`@loop(maxDepth=2)`, "loop_until_form", []string{"automation \"probe\"", "until=row =>"}},
		{`@loop(maxDepth="2", until=row => row.x == 1)`, "loop_max_depth_range", []string{"automation \"probe\"", "whole number", `"2"`}},
		{`@loop(maxDepth=2.5, until=row => row.x == 1)`, "loop_max_depth_range", []string{"whole number", "2.5"}},
		{`@loop(until=row => row.x == 1)`, "loop_max_depth_range", []string{"automation \"probe\"", "maxDepth="}},
		{`@mode(queued, max=0)`, "mode_max_range", []string{"automation \"probe\"", "max=0", "at least 1"}},
		{`@mode(queued, max="3")`, "mode_max_range", []string{"whole number", `"3"`}},
		// The registry decides the names, forms and keys, before any of the
		// above: a key @loop or @mode does not have is annotation_key.
		{`@loop(depth=4, until=row => row.x == 1)`, "annotation_key", []string{"@loop on an automation has no key depth"}},
		{`@mode(serial)`, "annotation_key", []string{"@mode on an automation has no key serial"}},
		{`@mode(single="yes")`, "annotation_key", []string{"single is a flag and takes no value"}},
		{`@loop(4)`, "annotation_form", []string{"@loop on an automation takes keyword arguments"}},
		{`@mode("queued")`, "annotation_form", []string{"@mode on an automation takes keyword arguments"}},
	} {
		t.Run(tc.head, func(t *testing.T) {
			_, err := parseV1Authored(t, loopModeProbe(tc.head))
			wantAnnotationRefusal(t, err, tc.code, tc.fragments...)
		})
	}
}

// TestLoopUntilFormIsPlacedOnTheValue: the until refusal points at the value
// the author wrote after `until=`, in the author's line and column.
func TestLoopUntilFormIsPlacedOnTheValue(t *testing.T) {
	src := loopModeProbe(`@loop(maxDepth=2, until="done")`)
	_, err := parseV1Authored(t, src)
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("the refusal carries no position: %v", err)
	}
	wantLine, wantCol := authoredAt(t, src, `"done"`, 1)
	if line, col := pe.Position(); line != wantLine || col != wantCol {
		t.Errorf("the refusal is at %d:%d, want %d:%d (the value after until=): %v", line, col, wantLine, wantCol, err)
	}
}

// TestLoopOnATerseAutomation: the terse header's arrow is the last top-level
// `=>`, so until's lambda, inside @loop's parentheses, does not end the
// header early.
func TestLoopOnATerseAutomation(t *testing.T) {
	src := `automation probe @trigger(event="node.created", concept="v1:probe:ticket") @filter(row => row.status != "done") @loop(maxDepth=3, until=row => row.status == "done") @mode(queued) => logic probe`
	auto := automationBody(t, mustParseV1Authored(t, src))
	if auto.Loop == nil || auto.Loop.MaxDepth != 3 || ast.FormatExpr(auto.Loop.Until) != `row => row.status == "done"` {
		t.Fatalf("Loop = %+v", auto.Loop)
	}
	if auto.Mode == nil || !reflect.DeepEqual(auto.Mode.Flags, []string{"queued"}) {
		t.Fatalf("Mode = %+v", auto.Mode)
	}
	if auto.Trigger == nil || auto.Trigger.Filter != `row => row.status != "done"` {
		t.Fatalf("Trigger = %+v", auto.Trigger)
	}
}
