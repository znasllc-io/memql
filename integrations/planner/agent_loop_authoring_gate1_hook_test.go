package planner

import (
	"testing"

	// Register the REAL Gate 1 automation compiler into this test binary
	// (memql#1366). component/automations' init() calls
	// memql.SetSandboxAutomationCompiler; production binaries always link it,
	// but the planner package's test binary did NOT -- so every
	// SandboxCompileBundle call in these tests reported the `automation`
	// kind as Skipped and the *_RealGate1Compiles tests passed VACUOUSLY
	// while the synthesized headline failed the real compiler at runtime.
	// The blank import makes the planner test binary compile automations
	// exactly like production does.
	_ "github.com/znasllc-io/memql/component/automations"

	"github.com/znasllc-io/memql/component/memql"
)

// requireAutomationsActuallyCompiled is the anti-vacuity guard (memql#1366):
// a "compiles through real Gate 1" assertion is meaningless when the
// automation kind was skipped because no compiler hook is linked. Fails
// loudly when any automation diagnostic in the report was skipped, so this
// class of silently-green gap cannot return.
func requireAutomationsActuallyCompiled(t *testing.T, report memql.SandboxReport) {
	t.Helper()
	compiled := 0
	for _, d := range report.Diagnostics {
		if d.Kind != "automation" {
			continue
		}
		if d.Skipped {
			t.Fatalf("automation %q was SKIPPED by the Gate 1 sandbox -- the automation compiler hook is not linked into this test binary, so the compile assertion is vacuous (memql#1366)", d.Name)
		}
		compiled++
	}
	if compiled == 0 {
		t.Fatal("the bundle carried no automation diagnostics; the Gate 1 compile assertion is vacuous")
	}
}

// TestGate1AutomationHookRegistered locks the hook itself: a trivially valid
// automation must come back compiled (not skipped) from the sandbox.
func TestGate1AutomationHookRegistered(t *testing.T) {
	report := memql.SandboxCompileBundle([]memql.SandboxConstruct{
		{Kind: "automation", Name: "hookProbe",
			Source: "@description(\"hook probe\")\nautomation hookProbe {\n  step run {\n    logic hookProbeBody { }\n  }\n}"},
		{Kind: "logic", Name: "hookProbeBody",
			Source: "logic hookProbeBody {\n  body { return now }\n}"},
	})
	requireAutomationsActuallyCompiled(t, report)
	if !report.OK {
		var errs []string
		for _, d := range report.Diagnostics {
			if !d.OK && !d.Skipped {
				errs = append(errs, d.Kind+"/"+d.Name+": "+d.Error)
			}
		}
		t.Fatalf("trivial automation must compile; errors: %v", errs)
	}
}
