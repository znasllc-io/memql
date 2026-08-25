package dslgate

import (
	"sort"
	"strings"
	"testing"
)

// subautomation_calls_test.go -- memql#4471.
//
// The rule: `automation <name>( ... )` names an automation, and a name that
// resolves to nothing is a LOAD problem rather than a mid-run one. These tests
// pin the rule, the three declaration forms it must recognise, and the two
// mistakes that would make it either useless or an outage.

// subGateOn runs the sub-automation gate over an in-memory corpus. Paths are
// sorted before the scan, matching every real caller (both entry points source
// their file set from dslfs.WalkMemqlFiles, which returns sorted paths).
func subGateOn(t *testing.T, files map[string]string) []Violation {
	t.Helper()
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	corpus := make([]SourceFile, 0, len(paths))
	for _, p := range paths {
		corpus = append(corpus, SourceFile{Path: p, Content: files[p]})
	}
	return scanSubAutomationCalls(corpus)
}

// TestUnresolvedSubAutomationIsReported is the rule, in the exact shape the
// issue reported: a call whose callee is declared nowhere.
func TestUnresolvedSubAutomationIsReported(t *testing.T) {
	got := subGateOn(t, map[string]string{
		"deployment/automations.memql": `automation deliberatelyBroken {
  step s {
    automation thisAutomationDoesNotExist( foo: 1 )
  }
}
`,
	})
	if len(got) != 1 {
		t.Fatalf("violations = %v, want exactly 1 -- before this gate cmd/memqllint reported "+
			"\"OK: no diagnostics\" over this corpus and the failure arrived at run time", got)
	}
	v := got[0]
	if v.Gate != GateUnresolvedSubAutomation {
		t.Errorf("gate = %q, want %q", v.Gate, GateUnresolvedSubAutomation)
	}
	if v.Construct != "deliberatelyBroken" {
		t.Errorf("construct = %q, want the CALLER %q -- a violation that does not name the "+
			"caller makes the operator search for the call site", v.Construct, "deliberatelyBroken")
	}
	if v.Kind != "automation" {
		t.Errorf("kind = %q, want \"automation\"; recordContractGateProblems defaults an empty "+
			"Kind to \"filter\", which would file this under the wrong construct", v.Kind)
	}
	if v.Line != 3 {
		t.Errorf("line = %d, want 3 (the call site)", v.Line)
	}
	if !strings.Contains(v.Detail, "thisAutomationDoesNotExist") {
		t.Errorf("detail does not name the unresolved callee: %q", v.Detail)
	}
	if !strings.Contains(v.Detail, "MEMQL_DSL_PATH") {
		t.Errorf("detail does not mention the other legitimate cause -- a callee that lives in a "+
			"product bundle this node did not mount. Without it the only reading is \"typo\", and "+
			"an operator whose bundle failed to mount is sent to edit correct DSL: %q", v.Detail)
	}
}

// TestResolvedSubAutomationIsSilent is the control. A gate that reported
// unconditionally would pass the test above and refuse every boot.
func TestResolvedSubAutomationIsSilent(t *testing.T) {
	got := subGateOn(t, map[string]string{
		"deployment/automations.memql": `automation provisionInstance {
  step p {
    action provisionAzureInfrastructure( dryRun: true )
  }
}

automation bringUpInstance {
  step substrate {
    automation provisionInstance(
      instanceId,
      dryRun
    )
  }
}
`,
	})
	if len(got) != 0 {
		t.Fatalf("violations = %v, want none -- this is the live shape in "+
			"dsl/deployment/automations.memql and it must load", got)
	}
}

// TestSubAutomationResolvesACROSSFiles is why the gate is corpus-level. Per
// file, neither half is a violation and neither half is resolvable.
func TestSubAutomationResolvesAcrossFiles(t *testing.T) {
	got := subGateOn(t, map[string]string{
		"cognition/automations.memql":  "automation childVerb {\n  step s {\n    logic noop( x: 1 )\n  }\n}\n",
		"deployment/automations.memql": "automation parentVerb {\n  step s {\n    automation childVerb( note: \"x\" )\n  }\n}\n",
	})
	if len(got) != 0 {
		t.Fatalf("violations = %v, want none: childVerb is declared in another file, and "+
			"resolving per file would make this pass or fail on directory order", got)
	}
}

// TestEveryDeclarationFormIsRecognised. Three forms are live in the tree, and a
// declaration index that knows only `automation NAME {` produces a CONFIDENT
// FALSE POSITIVE on the other two -- a refused boot naming an automation that
// is right there in the file.
func TestEveryDeclarationFormIsRecognised(t *testing.T) {
	for _, tc := range []struct {
		name string
		decl string
	}{
		{"strict", "automation calleeVerb {\n  step s {\n    logic noop( x: 1 )\n  }\n}\n"},
		{"loose brace on the next line", "automation calleeVerb\n{\n  step s {\n    logic noop( x: 1 )\n  }\n}\n"},
		{"terse", "automation calleeVerb @trigger(event=\"a.b\") => logic noop\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := subGateOn(t, map[string]string{
				"a/automations.memql": tc.decl,
				"b/automations.memql": "automation caller {\n  step s {\n    automation calleeVerb( x: 1 )\n  }\n}\n",
			})
			if len(got) != 0 {
				t.Fatalf("violations = %v, want none -- the %s declaration form is live in the "+
					"tree, and not recognising it refuses a boot over correct DSL", got, tc.name)
			}
		})
	}
}

// TestACommentedOutCallIsNotAViolation. Blanking comments before matching is
// the difference between a gate and a nuisance.
func TestACommentedOutCallIsNotAViolation(t *testing.T) {
	got := subGateOn(t, map[string]string{
		"a/automations.memql": `automation caller {
  step s {
    // automation notYetWritten( x: 1 )
    logic noop( x: 1 )
  }
}
`,
	})
	if len(got) != 0 {
		t.Fatalf("violations = %v, want none: the call is commented out", got)
	}
}

// TestReferenceSkeletonsAreNotScanned. dsl/_reference exists to SHOW forms,
// including ones that were never meant to resolve, and the loader skips
// `_`-prefixed directories. A gate that fires only under the conformance
// harness -- where ScanTree can reach them -- is a gate nobody can trust.
func TestReferenceSkeletonsAreNotScanned(t *testing.T) {
	got := subGateOn(t, map[string]string{
		"_reference/_automation.memql": "automation demo {\n  step s {\n    automation someOtherAutomation( x: 1 )\n  }\n}\n",
	})
	if len(got) != 0 {
		t.Fatalf("violations = %v, want none: _reference is a soft-disabled directory the "+
			"loader never reads, so the engine could not fail on it", got)
	}
}

// TestTheCallerIsNamedEvenAcrossSeveralAutomations pins the attribution walk:
// the LAST declaration to open before the call site is the enclosing one.
func TestTheCallerIsNamedEvenAcrossSeveralAutomations(t *testing.T) {
	got := subGateOn(t, map[string]string{
		"a/automations.memql": `automation first {
  step s {
    logic noop( x: 1 )
  }
}

automation second @trigger(event="a.b") => logic noop

automation third {
  step s {
    automation missingVerb( x: 1 )
  }
}
`,
	})
	if len(got) != 1 {
		t.Fatalf("violations = %v, want 1", got)
	}
	if got[0].Construct != "third" {
		t.Errorf("construct = %q, want \"third\" -- the terse declaration between them must not "+
			"be skipped when the enclosing automation is resolved", got[0].Construct)
	}
}
