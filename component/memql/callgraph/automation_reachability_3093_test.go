package callgraph

// memql#3093: the automation-condition rules (P4, memql#2371) were live in
// ConstructFindings and UNREACHABLE from CheckFile, because restrictedKinds had
// no "automation" entry -- so CheckTree, the whole-tree CI gate, analysed none
// of the tree's automations.
//
// Why nothing caught it: every existing test called ConstructFindings("automation",
// ...) DIRECTLY (automation_conditions_test.go), proving the rules correct
// against a code path the gate never takes. So these tests all drive findings
// through CheckFile -- the entry point whose absence from the test suite was the
// actual defect. A test that calls ConstructFindings cannot fail for this bug,
// no matter how thorough it is.

import (
	"strings"
	"testing"
)

func noSideEffects(string) bool { return false }

// TestCheckFile_ReachesAutomationConditions is the assertion whose absence hid
// memql#3093. It must go through CheckFile, and the file name must be the one
// the tree actually uses, because kind inference is `singular(basename)` and
// that inference is half of what was broken.
func TestCheckFile_ReachesAutomationConditions(t *testing.T) {
	src := `@trigger(event="node.updated", concept="v1:forge:request", partition="*")
automation probe {
  step apply {
    if submitterRole == "admin" || submitterRole == "writer" {
      mutation advanceRequest ( requestId: id )
    }
  }
}`

	findings := CheckFile("forge/automations.memql", src, noSideEffects)
	if len(findings) == 0 {
		t.Fatal("CheckFile found nothing in an automation with a literal-vocabulary condition -- the automation arm is unreachable from the tree walk (memql#3093)")
	}
	var got []string
	for _, f := range findings {
		got = append(got, f.Rule)
		if f.Kind != "automation" {
			t.Errorf("finding kind = %q, want %q", f.Kind, "automation")
		}
		if f.Construct != "probe" {
			t.Errorf("finding construct = %q, want %q", f.Construct, "probe")
		}
	}
	if !containsRule(findings, "automation-condition-vocabulary") {
		t.Errorf("want an automation-condition-vocabulary finding, got rules %v", got)
	}
}

// TestCheckFile_ReachesBodilessAutomationFilter is the other half of
// reachability, and the half that a restrictedKinds entry alone does NOT buy.
//
// The braced-header matcher anchors on `{`. Ten of the tree's 31 walked
// automations are the bodiless `automation <name> @trigger(...) => logic <name>`
// delegation, and @filter -- one of the three condition surfaces the P4 rules
// inspect -- is where their conditions live. Two of the tree's three live
// @filter annotations sit on that form. Without the bodiless matcher the arm
// reports "reachable" while skipping the majority of the tree's automation
// conditions, which is memql#3043's failure mode reproduced inside the fix for
// memql#3093.
func TestCheckFile_ReachesBodilessAutomationFilter(t *testing.T) {
	src := `@trigger(event="node.created", concept="v1:data:record", partition="*")
@filter(coalesce(payload.kind, "regular") == "daily")
automation conflictDetection @trigger(schedule="0 0 2 * * *") => logic conflictDetection
`

	findings := CheckFile("data/automations.memql", src, noSideEffects)
	if !containsRule(findings, "automation-condition-builtin") {
		t.Fatalf("a bodiless `=> logic` automation's @filter condition must be inspected; got %d findings: %v", len(findings), ruleNames(findings))
	}
	for _, f := range findings {
		if f.Construct != "conflictDetection" {
			t.Errorf("finding construct = %q, want %q", f.Construct, "conflictDetection")
		}
	}
}

// TestSplitConstructs_AttributesAnnotationsToTheRightAutomation pins the
// interleaving hazard the two matchers create. Each construct's text is
// "everything since the previous construct ended", so if the two shapes were
// collected without being re-sorted into SOURCE order, a braced construct's
// preamble would swallow an earlier bodiless declaration -- and that
// declaration's @filter would be reported against the wrong construct.
// dsl/identity/automations.memql interleaves both shapes, so this is the real
// tree's layout, not a synthetic one.
func TestSplitConstructs_AttributesAnnotationsToTheRightAutomation(t *testing.T) {
	src := `@trigger(schedule="0 5 9 * * *")
@filter(coalesce(payload.kind, "regular") == "daily")
automation firstBodiless @trigger(schedule="0 5 9 * * *") => logic firstBodiless

@trigger(event="node.created", concept="v1:identity:user", partition="*")
automation secondBraced {
  step apply {
    logic doThing ( event: event )
  }
}
`
	constructs := splitConstructs("automation", src)
	if len(constructs) != 2 {
		t.Fatalf("want 2 automations split, got %d: %+v", len(constructs), names(constructs))
	}
	if constructs[0].name != "firstBodiless" || constructs[1].name != "secondBraced" {
		t.Fatalf("wrong names/order: %v", names(constructs))
	}
	// The @filter belongs to the FIRST declaration and must not leak into the
	// second's text.
	if !strings.Contains(constructs[0].text, "@filter") {
		t.Error("the bodiless declaration lost its own @filter annotation")
	}
	if strings.Contains(constructs[1].text, "@filter") {
		t.Error("the braced declaration absorbed the previous declaration's @filter -- source-order interleaving is broken")
	}

	// And the finding lands on the right construct.
	findings := CheckFile("identity/automations.memql", src, noSideEffects)
	if !containsRule(findings, "automation-condition-builtin") {
		t.Fatalf("interleaved file: want the bodiless @filter finding, got %v", ruleNames(findings))
	}
	for _, f := range findings {
		if f.Rule == "automation-condition-builtin" && f.Construct != "firstBodiless" {
			t.Errorf("automation-condition-builtin attributed to %q, want %q", f.Construct, "firstBodiless")
		}
	}
}

// TestCheckFile_SanctionedAutomationShapesAreClean guards the other direction:
// making the arm reachable must not start reporting the shapes P4 explicitly
// sanctions. This is what lets the tree's own 31 automations come back clean
// and be believed.
func TestCheckFile_SanctionedAutomationShapesAreClean(t *testing.T) {
	src := `@trigger(event="node.updated", concept="v1:identity:user", partition="*")
@filter(event.node.payload.preferences.computerUseEnabled == false)
automation killSwitchSuspendsRunningPlans {
  step decide {
    logic killSwitchSuspendsRunningPlans ( event: event )
  }
  step apply {
    forEach item in decide.nodes() {
      updatePlanStatus ( planId: item.id, status: "paused" )
    }
  }
}`
	if findings := CheckFile("worker/automations.memql", src, noSideEffects); len(findings) != 0 {
		t.Errorf("sanctioned automation shape reported %d findings: %v", len(findings), ruleNames(findings))
	}
}

func containsRule(findings []Finding, rule string) bool {
	for _, f := range findings {
		if f.Rule == rule {
			return true
		}
	}
	return false
}

func ruleNames(findings []Finding) []string {
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, f.Rule+"/"+f.Construct)
	}
	return out
}

func names(cs []construct) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.name)
	}
	return out
}
