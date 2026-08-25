package memql

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql/baseloader"
	"github.com/znasllc-io/memql/component/memql/dslgate"
)

// contract_gates_subautomation_boot_4471_test.go -- memql#4471.
//
// The rule itself is unit-tested in component/memql/dslgate. What these two
// gates prove is the half that makes it MATTER: that the violation reaches the
// LoadReport, because a violation that is only returned reaches the log line
// and the node starts anyway.
//
// They exercise recordContractGateProblems -- the function Init calls -- and
// they come in a pair on purpose. A gate that never fires passes the control; a
// gate that always fires passes the rule. Only both together say anything.

// unresolvedSubAutomationCorpus is the issue's own repro, split across two
// files so the corpus-level half is exercised too: `deployment` calls a verb
// that `cognition` does not declare (it declares a differently-spelled one).
func unresolvedSubAutomationCorpus() []baseloader.RawFile {
	return []baseloader.RawFile{
		{
			Path:    "cognition/automations.memql",
			Content: "automation childVerb {\n  step s {\n    logic noop( x: 1 )\n  }\n}\n",
		},
		{
			Path: "deployment/automations.memql",
			Content: `automation parentVerb {
  step s {
    automation childVerbTypo( note: "x" )
  }
}
`,
		},
	}
}

// TestUnresolvedSubAutomationIsRecordedOnTheLoadReport: the gate refuses a boot.
func TestUnresolvedSubAutomationIsRecordedOnTheLoadReport(t *testing.T) {
	report := newLoadReport()

	violations := recordContractGateProblems(report, unresolvedSubAutomationCorpus(), nil)

	var found *dslgate.Violation
	for i := range violations {
		if violations[i].Gate == dslgate.GateUnresolvedSubAutomation {
			found = &violations[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("recordContractGateProblems returned no %s violation over a corpus containing "+
			"exactly one.\nBefore memql#4471 this corpus loaded clean and failed at RUN time, on the "+
			"step -- which for an orchestration like bringUpInstance means the earlier steps had "+
			"already provisioned infrastructure.\ngot: %v", dslgate.GateUnresolvedSubAutomation, violations)
	}
	if !strings.Contains(found.Detail, "childVerbTypo") {
		t.Errorf("violation detail does not name the unresolved callee: %q", found.Detail)
	}

	// The LoadReport half is the half that refuses the boot.
	if !report.HasProblems() {
		t.Fatal("the violation was returned but not recorded on the LoadReport, so strict boot " +
			"would start the node anyway -- returning it only reaches the log line")
	}
	var skipped bool
	for _, s := range report.Skipped {
		if s.Phase != "contract-gate:"+string(dslgate.GateUnresolvedSubAutomation) {
			continue
		}
		skipped = true
		if s.File != "deployment/automations.memql" {
			t.Errorf("skip attributed to %q, want the CALLING file -- attributing it to the "+
				"declaring side sends the author to the wrong end of the edge", s.File)
		}
		if s.Keyword != "automation" {
			t.Errorf("skip keyword = %q, want \"automation\". recordContractGateProblems defaults "+
				"an empty Violation.Kind to \"filter\", which files an automation problem under a "+
				"construct that has none", s.Keyword)
		}
		if s.Name != "parentVerb" {
			t.Errorf("skip name = %q, want the calling automation \"parentVerb\"", s.Name)
		}
	}
	if !skipped {
		t.Errorf("no skip recorded under phase contract-gate:%s; got %+v",
			dslgate.GateUnresolvedSubAutomation, report.Skipped)
	}
}

// TestSubAutomationGateAdmitsAResolvedCall is the control. The gate refuses a
// boot, so a version of it that fired unconditionally would not be a bad test
// result -- it would be every node in the fleet failing to start.
func TestSubAutomationGateAdmitsAResolvedCall(t *testing.T) {
	report := newLoadReport()
	corpus := unresolvedSubAutomationCorpus()
	corpus[1].Content = strings.Replace(corpus[1].Content, "childVerbTypo", "childVerb", 1)

	violations := recordContractGateProblems(report, corpus, nil)

	for _, v := range violations {
		if v.Gate == dslgate.GateUnresolvedSubAutomation {
			t.Fatalf("gate fired over a corpus whose callee IS declared, in another file: %v\n"+
				"Resolution must run over the merged corpus after every automation is known; "+
				"per-file resolution would make A-calls-B pass or fail on directory order", v)
		}
	}
	if report.HasProblems() {
		t.Fatalf("clean corpus recorded problems: %s", report.Detail())
	}
}
