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

// A call nested in a statement body's block, and bound to a name, is read off
// the parsed body: no line of it opens with `automation`, which is all the
// text pattern can see (epic memql#5370).
func TestUnresolvedSubAutomationInABlockIsReported(t *testing.T) {
	got := subGateOn(t, map[string]string{
		"deployment/automations.memql": `automation blockCaller {
  args {
    go  boolean
  }
  if args.go {
    started := automation missingInABlock(x: 1)
  }
}
`,
	})
	if len(got) != 1 || got[0].Construct != "blockCaller" || got[0].Line != 6 || !strings.Contains(got[0].Detail, "missingInABlock") {
		t.Fatalf("violations = %+v, want the one call to missingInABlock, in blockCaller, on line 6", got)
	}
}

// TestUnresolvedSubAutomationIsReported is the rule, in the exact shape the
// issue reported: a call whose callee is declared nowhere.
func TestUnresolvedSubAutomationIsReported(t *testing.T) {
	got := subGateOn(t, map[string]string{
		"deployment/automations.memql": `automation deliberatelyBroken {
  s := automation thisAutomationDoesNotExist(foo: 1)
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
	if v.Line != 2 {
		t.Errorf("line = %d, want 2 (the call site)", v.Line)
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
  p := action provisionAzureInfrastructure(dryRun: true)
}

automation bringUpInstance {
  substrate := automation provisionInstance(
    instanceId: instanceId,
    dryRun: dryRun
  )
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
		"cognition/automations.memql":  "automation childVerb {\n  s := logic noop(x: 1)\n}\n",
		"deployment/automations.memql": "automation parentVerb {\n  s := automation childVerb(note: \"x\")\n}\n",
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
		{"strict", "automation calleeVerb {\n  s := logic noop(x: 1)\n}\n"},
		{"loose brace on the next line", "automation calleeVerb\n{\n  step s {\n    logic noop( x: 1 )\n  }\n}\n"},
		{"terse", "automation calleeVerb @trigger(event=\"a.b\") => logic noop\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := subGateOn(t, map[string]string{
				"a/automations.memql": tc.decl,
				"b/automations.memql": "automation caller {\n  s := automation calleeVerb(x: 1)\n}\n",
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
  // automation notYetWritten( x: 1 )
  s := logic noop(x: 1)
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
		"_reference/_automation.memql": "automation demo {\n  s := automation someOtherAutomation(x: 1)\n}\n",
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
  s := logic noop(x: 1)
}

@trigger(event="a.b")
automation second {
  logic noop(event: event)
}

automation third {
  s := automation missingVerb(x: 1)
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
