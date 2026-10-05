package planner

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

func TestRepairStopsRepeatedSourceCyclesAndUnchangedDiagnostics(t *testing.T) {
	base := memql.SandboxConstruct{Kind: "spec", Name: "broken", Source: "spec broken A"}
	change := func(source string) memql.SandboxConstruct { c := base; c.Source = source; return c }
	for _, tc := range []struct {
		name      string
		repairs   []memql.SandboxConstruct
		wantCalls int
		wantError string
	}{
		{"unchanged", []memql.SandboxConstruct{base}, 1, "repeated source"},
		{"cycle", []memql.SandboxConstruct{change("spec broken B"), base}, 2, "repeated source"},
		{"unchanged diagnostics", []memql.SandboxConstruct{change("spec broken B"), change("spec broken C"), change("spec broken D")}, 2, "without diagnostic progress"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			responses := []string{}
			for _, c := range tc.repairs {
				responses = append(responses, emitJSON(t, []memql.SandboxConstruct{c}))
			}
			eng := emitFakeEngine(emitJSON(t, []memql.SandboxConstruct{automationCon, base}), responses, nil)
			sandbox := &fakeSandbox{reports: []memql.SandboxReport{failReport(base.Kind, base.Name, "unknown binding", automationCon, base)}}
			_, _, clean, err := newDesignLoop(eng).emitAndRepairBundle(context.Background(), unboundedBudget, "digest", designPlanWith(), sandbox)
			if clean || err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("unexpected result: clean=%v err=%v", clean, err)
			}
			_, calls, _ := eng.snapshot()
			if n := countContains(calls, "authoringRepair"); n != tc.wantCalls {
				t.Fatalf("made %d repairs, want %d", n, tc.wantCalls)
			}
		})
	}
}

func TestRepairCannotOverwritePassingConstructOrReturnDuplicates(t *testing.T) {
	broken := specCon
	changed := automationCon
	changed.Source = "automation dailyDigest { publish invented }"
	for _, repaired := range [][]memql.SandboxConstruct{{changed}, {broken, broken}} {
		if err := validateRepair([]memql.SandboxConstruct{automationCon, broken}, []memql.SandboxConstruct{broken}, repaired); err == nil {
			t.Fatal("unsafe repair accepted")
		}
	}
	if err := validateRepair([]memql.SandboxConstruct{automationCon, broken}, []memql.SandboxConstruct{broken}, []memql.SandboxConstruct{specCon}); err != nil {
		t.Fatal(err)
	}
}

func TestCancelledAuthoringMakesNoModelCalls(t *testing.T) {
	eng := emitFakeEngine("", nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _, err := newDesignLoop(eng).emitAndRepairBundle(ctx, unboundedBudget, "digest", designPlanWith(), &fakeSandbox{})
	if err == nil {
		t.Fatal("cancelled authoring proceeded")
	}
	_, calls, _ := eng.snapshot()
	if len(calls) != 0 {
		t.Fatalf("cancelled work invoked models: %v", calls)
	}
}
