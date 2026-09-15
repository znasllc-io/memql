package automations

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/memql/baseloader"
)

// loopProbe is an automation that closes a self-cycle on its trigger concept,
// with head standing for the annotations under test.
func loopProbe(head string) string {
	return head + `
automation advanceProbe {
  args {
    id any
  }
  advance := mutation advanceTicket(id: args.id, status: "done")
}`
}

// eventHead is the trigger every event-triggered probe carries.
const eventHead = `@trigger(event="node.created", concept="v1:probe:ticket")`

func compileLoopProbe(t *testing.T, head string) (*Automation, error) {
	t.Helper()
	return NewLoader(LoaderOptions{}).CompileSource(loopProbe(head), "test")
}

// wantLoopRefusal holds err to the refusal code, as the last thing in the
// message and as the rule code a load report reads, and to each fragment.
func wantLoopRefusal(t *testing.T, err error, code string, fragments ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("compiled; want the %s refusal", code)
	}
	if !strings.HasSuffix(err.Error(), "["+code+"]") {
		t.Fatalf("the refusal does not end with [%s]:\n%v", code, err)
	}
	if got := baseloader.RuleCode(err); got != code {
		t.Errorf("the refusal carries rule code %q, want %q: %v", got, code, err)
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("the refusal spans lines; the strict loader lists one problem per line:\n%v", err)
	}
	for _, f := range fragments {
		if !strings.Contains(err.Error(), f) {
			t.Errorf("the refusal does not say %q:\n%v", f, err)
		}
	}
}

// TestLoopLoadsOntoTheAutomation: a converging self-cycle compiles with its
// @loop on Automation.Loop, the until parsed once.
func TestLoopLoadsOntoTheAutomation(t *testing.T) {
	t.Setenv(maxChainDepthEnv, "16")
	a, err := compileLoopProbe(t, eventHead+`
@filter(row => row.status != "done")
@loop(maxDepth=4, until=row => row.status == "done")`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if a.Loop == nil {
		t.Fatal("Automation.Loop is nil")
	}
	if a.Loop.MaxDepth != 4 {
		t.Errorf("MaxDepth = %d, want 4", a.Loop.MaxDepth)
	}
	if want := `row => row.status == "done"`; a.Loop.Until != want {
		t.Errorf("Until = %q, want %q", a.Loop.Until, want)
	}
	if a.Loop.UntilLambda == nil || len(a.Loop.UntilLambda.Params) != 1 || a.Loop.UntilLambda.Params[0] != "row" {
		t.Fatalf("UntilLambda = %+v, want the parsed one-parameter lambda", a.Loop.UntilLambda)
	}
	if got := ast.FormatExpr(a.Loop.UntilLambda); got != a.Loop.Until {
		t.Errorf("UntilLambda prints %q, Until is %q", got, a.Loop.Until)
	}
	if a.Mode != nil {
		t.Errorf("Mode = %+v, want nil for an automation with no @mode", a.Mode)
	}
}

// TestLoopRefusals: each @loop refusal, from source, with its code.
func TestLoopRefusals(t *testing.T) {
	t.Setenv(maxChainDepthEnv, "16")
	t.Run("loop_until_not_in_filter: a filter that does not exclude the converged row", func(t *testing.T) {
		_, err := compileLoopProbe(t, eventHead+`
@filter(row => row.status == "open")
@loop(maxDepth=4, until=row => row.status == "done")`)
		wantLoopRefusal(t, err, "loop_until_not_in_filter",
			`advanceProbe`,
			`&& row.status != "done"`,
			`@filter(row => row.status == "open" && row.status != "done")`)
	})
	t.Run("loop_until_not_in_filter: no filter at all", func(t *testing.T) {
		_, err := compileLoopProbe(t, eventHead+`
@loop(maxDepth=4, until=row => row.status == "done")`)
		wantLoopRefusal(t, err, "loop_until_not_in_filter", "has no @filter", `add @filter(row => row.status != "done")`)
	})
	t.Run("loop_until_not_in_filter: the fix keeps an || filter's meaning", func(t *testing.T) {
		_, err := compileLoopProbe(t, eventHead+`
@filter(row => row.a == 1 || row.b == 2)
@loop(maxDepth=4, until=row => row.status == "done")`)
		wantLoopRefusal(t, err, "loop_until_not_in_filter", `@filter(row => (row.a == 1 || row.b == 2) && row.status != "done")`)
	})
	t.Run("loop_until_not_in_filter: the fix is written in the filter's parameter", func(t *testing.T) {
		_, err := compileLoopProbe(t, eventHead+`
@filter(t => t.status == "open")
@loop(maxDepth=4, until=row => row.status == "done")`)
		wantLoopRefusal(t, err, "loop_until_not_in_filter", `@filter(t => t.status == "open" && t.status != "done")`)
	})
	t.Run("loop_max_depth_range: zero", func(t *testing.T) {
		_, err := compileLoopProbe(t, eventHead+`
@filter(row => row.status != "done")
@loop(maxDepth=0, until=row => row.status == "done")`)
		wantLoopRefusal(t, err, "loop_max_depth_range", "maxDepth=0", "MEMQL_AUTOMATION_MAX_CHAIN_DEPTH")
	})
	t.Run("loop_max_depth_range: past the cap", func(t *testing.T) {
		_, err := compileLoopProbe(t, eventHead+`
@filter(row => row.status != "done")
@loop(maxDepth=17, until=row => row.status == "done")`)
		wantLoopRefusal(t, err, "loop_max_depth_range", "maxDepth=17", "16")
	})
	t.Run("loop_max_depth_range: not a whole number", func(t *testing.T) {
		_, err := compileLoopProbe(t, eventHead+`
@filter(row => row.status != "done")
@loop(maxDepth="4", until=row => row.status == "done")`)
		wantLoopRefusal(t, err, "loop_max_depth_range", "whole number")
	})
	t.Run("loop_max_depth_range: not written", func(t *testing.T) {
		_, err := compileLoopProbe(t, eventHead+`
@filter(row => row.status != "done")
@loop(until=row => row.status == "done")`)
		wantLoopRefusal(t, err, "loop_max_depth_range", "maxDepth=")
	})
	t.Run("loop_not_event_triggered: a scheduled automation", func(t *testing.T) {
		_, err := compileLoopProbe(t, `@trigger(schedule="0 0 * * * *")
@loop(maxDepth=4, until=row => row.status == "done")`)
		wantLoopRefusal(t, err, "loop_not_event_triggered", "advanceProbe", "@trigger(event=")
	})
	t.Run("loop_until_form: a string", func(t *testing.T) {
		_, err := compileLoopProbe(t, eventHead+`
@filter(row => row.status != "done")
@loop(maxDepth=4, until="done")`)
		wantLoopRefusal(t, err, "loop_until_form", "until=row =>")
	})
	t.Run("loop_until_form: a predicate with no lambda header", func(t *testing.T) {
		_, err := compileLoopProbe(t, eventHead+`
@filter(row => row.status != "done")
@loop(maxDepth=4, until=row.status == "done")`)
		wantLoopRefusal(t, err, "loop_until_form", "until=row =>")
	})
	t.Run("loop_until_form: two parameters", func(t *testing.T) {
		_, err := compileLoopProbe(t, eventHead+`
@filter(row => row.status != "done")
@loop(maxDepth=4, until=(a, b) => a == b)`)
		wantLoopRefusal(t, err, "loop_until_form", "one parameter")
	})
	t.Run("loop_until_form: not written", func(t *testing.T) {
		_, err := compileLoopProbe(t, eventHead+`
@filter(row => row.status != "done")
@loop(maxDepth=4)`)
		wantLoopRefusal(t, err, "loop_until_form", "until=row =>")
	})
}

// TestLoopDepthCapIsTheEnvValue: maxDepth's upper bound is the depth cap,
// MEMQL_AUTOMATION_MAX_CHAIN_DEPTH -- a value, not a constant.
func TestLoopDepthCapIsTheEnvValue(t *testing.T) {
	head := eventHead + `
@filter(row => row.status != "done")
@loop(maxDepth=17, until=row => row.status == "done")`
	t.Setenv(maxChainDepthEnv, "20")
	if _, err := compileLoopProbe(t, head); err != nil {
		t.Fatalf("maxDepth=17 under a cap of 20: %v", err)
	}
	t.Setenv(maxChainDepthEnv, "12")
	_, err := compileLoopProbe(t, head)
	wantLoopRefusal(t, err, "loop_max_depth_range", "12")
}

// TestMaxChainDepth: the cap reads its env value, defaulting to 16; a value
// that is not a depth -- not a number, or below 1 -- reads as the default.
func TestMaxChainDepth(t *testing.T) {
	for env, want := range map[string]int{"": 16, "20": 20, "1": 1, "zz": 16, "0": 16, "-3": 16} {
		t.Setenv(maxChainDepthEnv, env)
		if got := maxChainDepth(); got != want {
			t.Errorf("%s=%q: maxChainDepth() = %d, want %d", maxChainDepthEnv, env, got, want)
		}
	}
}

// TestModeLoadsOntoTheAutomation: each mode, with and without max.
func TestModeLoadsOntoTheAutomation(t *testing.T) {
	for head, want := range map[string]ModeConfig{
		`@mode(single)`:           {Kind: ModeSingle},
		`@mode(queued)`:           {Kind: ModeQueued},
		`@mode(queued, max=3)`:    {Kind: ModeQueued, Max: 3},
		`@mode(max=3, queued)`:    {Kind: ModeQueued, Max: 3},
		`@mode(restart)`:          {Kind: ModeRestart},
		`@mode(parallel)`:         {Kind: ModeParallel},
		`@mode(parallel, max=12)`: {Kind: ModeParallel, Max: 12},
	} {
		t.Run(head, func(t *testing.T) {
			a, err := compileLoopProbe(t, eventHead+"\n"+head)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			if a.Mode == nil || *a.Mode != want {
				t.Fatalf("Mode = %+v, want %+v", a.Mode, want)
			}
			if a.Loop != nil {
				t.Errorf("Loop = %+v, want nil for an automation with no @loop", a.Loop)
			}
		})
	}
	// A scheduled automation may carry a mode: it governs concurrent fires,
	// whatever fires them.
	a, err := compileLoopProbe(t, `@trigger(schedule="0 0 * * * *")
@mode(single)`)
	if err != nil || a.Mode == nil || a.Mode.Kind != ModeSingle {
		t.Fatalf("a scheduled @mode(single): %+v, %v", a, err)
	}
}

// TestModeRefusals: each @mode refusal, from source, with its code.
func TestModeRefusals(t *testing.T) {
	for _, tc := range []struct {
		head, code string
		fragments  []string
	}{
		{`@mode(single, queued)`, "mode_flags", []string{"single", "queued", "exactly one"}},
		{`@mode(max=3)`, "mode_flags", []string{"names no mode", "exactly one"}},
		{`@mode(single, max=2)`, "mode_max_not_allowed", []string{"single", "max=2", "queued or parallel"}},
		{`@mode(restart, max=1)`, "mode_max_not_allowed", []string{"restart", "max=1"}},
		{`@mode(queued, max=0)`, "mode_max_range", []string{"max=0", "at least 1"}},
		{`@mode(parallel, max="3")`, "mode_max_range", []string{"whole number"}},
		{`@mode(queued, max=2.5)`, "mode_max_range", []string{"whole number"}},
	} {
		t.Run(tc.head, func(t *testing.T) {
			_, err := compileLoopProbe(t, eventHead+"\n"+tc.head)
			wantLoopRefusal(t, err, tc.code, tc.fragments...)
		})
	}
}

// TestLoopAndModeOnAnAutomationBuiltInGo: the executor prepares an automation
// built in Go before its first run (ensurePrepared), and holds its loop and
// mode to the same rules the load does. A refused one stays unprepared, so
// every run refuses rather than the first.
func TestLoopAndModeOnAnAutomationBuiltInGo(t *testing.T) {
	t.Setenv(maxChainDepthEnv, "16")
	build := func(loop *LoopConfig, mode *ModeConfig) *Automation {
		return &Automation{
			Name:    "goBuiltLoop",
			Trigger: &TriggerConfig{Event: "graph.node.created.v1:probe:ticket", Filter: `row => row.status != "done"`},
			Steps:   []*Step{{ID: "noop", Type: StepTypeFunction, Function: &FunctionStepConfig{Name: "q", Kind: "query"}}},
			Loop:    loop,
			Mode:    mode,
		}
	}

	ok := build(&LoopConfig{MaxDepth: 3, Until: `row => row.status == "done"`}, &ModeConfig{Kind: ModeQueued})
	if err := ensurePrepared(ok); err != nil {
		t.Fatalf("ensurePrepared: %v", err)
	}
	if ok.Loop.UntilLambda == nil {
		t.Fatal("ensurePrepared did not parse the until")
	}

	for _, tc := range []struct {
		name string
		a    *Automation
		code string
	}{
		{"an until that does not parse", build(&LoopConfig{MaxDepth: 3, Until: `row =>`}, nil), "loop_until_form"},
		{"an until that is not a lambda", build(&LoopConfig{MaxDepth: 3, Until: `row.status == "done"`}, nil), "loop_until_form"},
		{"no until", build(&LoopConfig{MaxDepth: 3}, nil), "loop_until_form"},
		{"no maxDepth", build(&LoopConfig{Until: `row => row.status == "done"`}, nil), "loop_max_depth_range"},
		{"an until the filter does not exclude", build(&LoopConfig{MaxDepth: 3, Until: `row => row.status == "open"`}, nil), "loop_until_not_in_filter"},
		{"a negative max", build(nil, &ModeConfig{Kind: ModeQueued, Max: -1}), "mode_max_range"},
		{"an unknown mode", build(nil, &ModeConfig{Kind: "serial"}), "mode_flags"},
		{"two modes", build(nil, &ModeConfig{Kind: "single,restart"}), "mode_flags"},
		{"max on restart", build(nil, &ModeConfig{Kind: ModeRestart, Max: 4}), "mode_max_not_allowed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ensurePrepared(tc.a)
			wantLoopRefusal(t, err, tc.code)
			if again := ensurePrepared(tc.a); again == nil {
				t.Error("a second ensurePrepared passed an automation the first refused")
			}
		})
	}

	scheduled := build(&LoopConfig{MaxDepth: 3, Until: `row => row.status == "done"`}, nil)
	scheduled.Trigger, scheduled.Schedule = nil, "0 0 * * * *"
	wantLoopRefusal(t, ensurePrepared(scheduled), "loop_not_event_triggered")
}
