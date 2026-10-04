package planner

import (
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	workintegration "github.com/znasllc-io/memql/integrations/work"
)

// replanKeepsPrefix decides exactly what resume will do with the template.
func TestReplanKeepsPrefixMirrorsWhereTheRunResumes(t *testing.T) {
	compile := func(t *testing.T, body string) *automations.Automation {
		t.Helper()
		a, err := automations.NewLoader(automations.LoaderOptions{}).CompileSource("@template\nautomation probe {\n"+body+"}\n", "probe.memql")
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		return a
	}
	rc := workintegration.ReplanContext{
		CompletedSteps: []map[string]any{{"key": "a"}, {"key": "b"}},
		Recorded:       map[string]string{"a": "done", "b": "done", "c": "failed", "broke": "failed"},
	}
	for _, tc := range []struct {
		name, body string
		ok         bool
	}{
		{"the prefix, then new steps", "  a := builtin one()\n  b := builtin two()\n  x := builtin three()\n", true},
		{"the failed step kept and run again", "  a := builtin one()\n  b := builtin two()\n  broke := builtin three()\n", true},
		{"a new step before a completed one would run that completed step again", "  a := builtin one()\n  x := builtin three()\n  b := builtin two()\n  y := builtin four()\n", false},
		{"a completed step dropped", "  a := builtin one()\n  x := builtin three()\n", false},
		{"nothing left to run", "  a := builtin one()\n  b := builtin two()\n", false},
		{"the failed step continued past", "  a := builtin one()\n  b := builtin two()\n  builtin broke() on error continue\n", false},
		{"an earlier failure no longer continued stops the run before b", "  a := builtin one()\n  c := builtin five()\n  b := builtin two()\n  x := builtin three()\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := replanKeepsPrefix(compile(t, tc.body), rc, "broke")
			if (err == nil) != tc.ok {
				t.Fatalf("replanKeepsPrefix = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}
