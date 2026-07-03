package memql_test

// authoring_deployment_bundle_test.go -- the E1 keystone e2e (epic memql#2354
// #2372): the ACTUAL deployment bundle (dsl/deployment/automations.memql +
// actions.memql + the capability decls they import from
// dsl/capabilities/capabilities.memql), fed verbatim through the authoring
// splitter + Gate-1 sandbox, must classify + compile every construct with
// per-construct OK and ZERO skips.
//
// This lives in the EXTERNAL test package so it can blank-import
// component/automations, whose init() registers the automation-compile hook the
// sandbox uses for the `automation` kind. Without that hook the automation
// would report as skipped -- which is precisely the regression this test
// guards against (a deployment bundle is MADE of automations/actions).

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"

	// Side-effect: registers the automation-compile hook (SetSandboxAutomationCompiler).
	_ "github.com/znasllc-io/memql/component/automations"
)

// readEmbeddedDSL reads a file from the embedded DSL tree, failing the test if
// it is missing (the deployment bundle files must exist for this e2e).
func readEmbeddedDSL(t *testing.T, path string) string {
	t.Helper()
	raw, err := fs.ReadFile(memqldsl.Tree(), path)
	if err != nil {
		t.Fatalf("read embedded DSL %q: %v", path, err)
	}
	return string(raw)
}

// TestDeploymentBundle_AllKindsCompileNoSkips feeds the real deployment bundle
// through SplitBundleSource + SandboxCompileBundle and asserts every construct
// -- the automation, all nine actions, and every imported capability -- compiles
// to OK with zero skips and zero unrecognized regions.
func TestDeploymentBundle_AllKindsCompileNoSkips(t *testing.T) {
	automations := readEmbeddedDSL(t, "deployment/automations.memql")
	actions := readEmbeddedDSL(t, "deployment/actions.memql")
	capabilities := readEmbeddedDSL(t, "capabilities/capabilities.memql")

	bundle := strings.Join([]string{automations, actions, capabilities}, "\n\n")

	constructs := memql.SplitBundleSource(bundle)
	if len(constructs) == 0 {
		t.Fatal("SplitBundleSource returned no constructs for the deployment bundle")
	}

	// Every deployment kind must be represented -- proof the splitter now covers
	// automation + action + capability (the E1 gap).
	byKind := map[string]int{}
	for _, c := range constructs {
		byKind[c.Kind]++
		if c.Kind == "unrecognized" {
			t.Errorf("deployment bundle produced an unrecognized construct: %q", c.Name)
		}
	}
	if byKind["automation"] < 1 {
		t.Errorf("expected >=1 automation, got %d", byKind["automation"])
	}
	if byKind["action"] < 9 {
		t.Errorf("expected 9 deployment actions, got %d", byKind["action"])
	}
	if byKind["capability"] < 1 {
		t.Errorf("expected >=1 capability, got %d", byKind["capability"])
	}

	report := memql.SandboxCompileBundle(constructs)
	if !report.OK {
		for _, d := range report.Diagnostics {
			if !d.OK {
				t.Errorf("construct %s %q failed: skipped=%v error=%q", d.Kind, d.Name, d.Skipped, d.Error)
			}
		}
		t.Fatal("deployment bundle must compile clean (OK), see per-construct failures above")
	}

	// Belt-and-suspenders: zero skips across the whole report (the automation
	// compiler is linked, so nothing may be skipped).
	for _, d := range report.Diagnostics {
		if d.Skipped {
			t.Errorf("construct %s %q was SKIPPED -- no deployment construct may be skipped: %q", d.Kind, d.Name, d.Error)
		}
		if !d.OK {
			t.Errorf("construct %s %q not OK: %q", d.Kind, d.Name, d.Error)
		}
	}

	// The headline automation is present and OK.
	var sawAutomation bool
	for _, d := range report.Diagnostics {
		if d.Kind == "automation" && d.Name == "deployEngineCluster" {
			sawAutomation = true
			if !d.OK || d.Skipped {
				t.Errorf("deployEngineCluster automation must compile clean, got %+v", d)
			}
		}
	}
	if !sawAutomation {
		t.Error("deployEngineCluster automation missing from the compile report")
	}
}
