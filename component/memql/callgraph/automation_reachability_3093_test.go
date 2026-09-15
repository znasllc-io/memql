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
	src := `@trigger(event="node.updated", concept="v1:forge:request")
automation probe {
  if submitterRole == "admin" || submitterRole == "writer" {
    apply := mutation advanceRequest(requestId: id)
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

// TestCheckFile_ReachesAutomationFilter is the other half of reachability:
// @filter -- one of the three condition surfaces the P4 rules inspect, and
// the one a trigger's relevance check lives in -- must be inspected through
// CheckFile as well as a statement's if. Reporting the arm "reachable" while
// skipping a surface is memql#3043's failure mode reproduced inside the fix
// for memql#3093.
func TestCheckFile_ReachesAutomationFilter(t *testing.T) {
	src := `@trigger(event="node.created", concept="v1:data:record")
@filter(row => (row.kind ?? "regular") == "daily")
automation conflictDetection {
  logic conflictDetection(event: event)
}
`

	findings := CheckFile("data/automations.memql", src, noSideEffects)
	if !containsRule(findings, "automation-condition-builtin") {
		t.Fatalf("an automation's @filter condition must be inspected; got %d findings: %v", len(findings), ruleNames(findings))
	}
	for _, f := range findings {
		if f.Construct != "conflictDetection" {
			t.Errorf("finding construct = %q, want %q", f.Construct, "conflictDetection")
		}
	}
}

// TestSplitConstructs_AttributesAnnotationsToTheRightAutomation pins source
// order. Each construct's text is "everything since the previous construct
// ended", so an annotation belongs to the declaration it precedes: the first
// automation's @filter must be reported against it, never against the second.
// dsl/identity/automations.memql carries eight automations in one file, so
// this is the real tree's layout, not a synthetic one.
func TestSplitConstructs_AttributesAnnotationsToTheRightAutomation(t *testing.T) {
	src := `@trigger(event="node.created", concept="v1:data:record")
@filter(row => (row.kind ?? "regular") == "daily")
automation first {
  logic first(event: event)
}

@trigger(event="node.created", concept="v1:identity:user")
automation second {
  apply := logic doThing(event: event)
}
`
	constructs := splitConstructs("automation", src)
	if len(constructs) != 2 {
		t.Fatalf("want 2 automations split, got %d: %+v", len(constructs), names(constructs))
	}
	if constructs[0].name != "first" || constructs[1].name != "second" {
		t.Fatalf("wrong names/order: %v", names(constructs))
	}
	// The @filter belongs to the FIRST declaration and must not leak into the
	// second's text.
	if !strings.Contains(constructs[0].text, "@filter") {
		t.Error("the first declaration lost its own @filter annotation")
	}
	if strings.Contains(constructs[1].text, "@filter") {
		t.Error("the second declaration absorbed the previous declaration's @filter -- source order is broken")
	}

	// And the finding lands on the right construct.
	findings := CheckFile("identity/automations.memql", src, noSideEffects)
	if !containsRule(findings, "automation-condition-builtin") {
		t.Fatalf("want the first automation's @filter finding, got %v", ruleNames(findings))
	}
	for _, f := range findings {
		if f.Rule == "automation-condition-builtin" && f.Construct != "first" {
			t.Errorf("automation-condition-builtin attributed to %q, want %q", f.Construct, "first")
		}
	}
}

// TestCheckFile_SanctionedAutomationShapesAreClean guards the other direction:
// making the arm reachable must not start reporting the shapes P4 explicitly
// sanctions. This is what lets the tree's own 31 automations come back clean
// and be believed.
func TestCheckFile_SanctionedAutomationShapesAreClean(t *testing.T) {
	src := `@trigger(event="node.updated", concept="v1:identity:user")
@filter(row => row.preferences.computerUseEnabled == false)
automation killSwitchSuspendsRunningPlans {
  decide := logic killSwitchSuspendsRunningPlans(event: event)
  for item in decide {
    mutation updatePlanStatus(planId: item.id, status: "paused")
  }
}`
	if _, ok := statementConditions(src); !ok {
		t.Fatal("the sanctioned shape must parse, or its conditions are never judged")
	}
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
