package memql

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql/baseloader"
	"github.com/znasllc-io/memql/component/memql/dslgate"
)

// contract_gates_crossns_boot_4051_test.go -- memql#4051.
//
// The cross-namespace-import gate (memql#3803) was real, correct, and enforced
// over this repository's dsl/ tree at PR time by a conformance test. It was NOT
// part of strict boot, because MemQLEngine.Init drove dslgate.ScanSource ONCE
// PER FILE and the rule is corpus-level by construction: whether a reference
// crosses a namespace boundary depends on where the referenced name is
// DECLARED, which one file cannot answer.
//
// The consequence was the asymmetry component/memql/dslgate exists to abolish. A
// product DSL bundle mounted at MEMQL_DSL_PATH -- which no Go test in this repo
// walks, and which platform consolidation (memql#2472) makes the PRIMARY
// delivery path -- could carry a cross-namespace violation, boot cleanly, and be
// caught by nothing, while the identical violation in dsl/ failed CI.
//
// WHY THIS TEST HAS TO EXIST, rather than trusting that the tree is clean. The
// embedded corpus has ZERO cross-namespace violations, so every observation of
// the gate on the real tree is a null result, and a null result cannot tell a
// gate that runs and finds nothing from a gate that does not run. These tests
// supply the corpus that makes the instrument move: TestCrossNamespaceImportGate
// IsRecordedOnTheLoadReport fails against the per-file loop this issue replaced,
// and TestBootContractGatesAdmitACleanCorpus is its control, so neither a gate
// that never fires nor a gate that always fires can pass the pair.
//
// It exercises recordContractGateProblems -- the function Init calls -- rather
// than dslgate directly, because the defect was never in the rule. It was in
// which entry point boot reached it through.

// crossNamespaceCorpus is two files: `common` declares a builtin, and
// `cognition` calls it with no `use` naming it. That is the rule's canonical
// violation, and it is invisible to any per-file scan of EITHER file.
func crossNamespaceCorpus() []baseloader.RawFile {
	return []baseloader.RawFile{
		{
			Path:    "common/builtins.memql",
			Content: "builtin trackPresence {\n  x string\n}\n",
		},
		{
			Path: "cognition/automations.memql",
			Content: `automation a {
  step s {
    builtin trackPresence(x: "1")
  }
}
`,
		},
	}
}

// TestCrossNamespaceImportGateIsRecordedOnTheLoadReport: the gate reaches the
// LoadReport, which is what makes strict boot refuse it.
func TestCrossNamespaceImportGateIsRecordedOnTheLoadReport(t *testing.T) {
	report := newLoadReport()

	violations := recordContractGateProblems(report, crossNamespaceCorpus(), nil)

	var found *dslgate.Violation
	for i := range violations {
		if violations[i].Gate == dslgate.GateCrossNamespaceImport {
			found = &violations[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("recordContractGateProblems returned no %s violation over a corpus that "+
			"contains exactly one.\n"+
			"This is the memql#4051 defect: boot ran the per-file gates only, so a corpus-level "+
			"rule could not refuse a boot and a MEMQL_DSL_PATH product bundle ran it nowhere.\n"+
			"got: %v", dslgate.GateCrossNamespaceImport, violations)
	}
	if !strings.Contains(found.Detail, "use common.builtins.{ trackPresence }") {
		t.Errorf("violation detail does not name the import that fixes it: %q", found.Detail)
	}

	// The LoadReport half is the half that refuses the boot. A violation the
	// function returns but does not record would log loudly and start anyway.
	if !report.HasProblems() {
		t.Fatal("the violation was returned but not recorded on the LoadReport, so strict boot " +
			"would start the node anyway -- returning it only reaches the log line")
	}
	var skipped bool
	for _, s := range report.Skipped {
		if s.Phase == "contract-gate:"+string(dslgate.GateCrossNamespaceImport) {
			skipped = true
			if s.File != "cognition/automations.memql" {
				t.Errorf("skip attributed to %q, want the REFERENCING file -- attributing a "+
					"cross-namespace violation to the declaring file sends the author to the "+
					"wrong end of the edge", s.File)
			}
		}
	}
	if !skipped {
		t.Errorf("no skip recorded under phase contract-gate:%s; got %+v",
			dslgate.GateCrossNamespaceImport, report.Skipped)
	}
}

// TestBootContractGatesAdmitACleanCorpus is the control for the test above: a
// gate that reported unconditionally would pass that one and be an outage.
//
// Same two files, plus the one import line the rule asks for.
func TestBootContractGatesAdmitACleanCorpus(t *testing.T) {
	report := newLoadReport()
	corpus := crossNamespaceCorpus()
	corpus[1].Content = "use common.builtins.{ trackPresence }\n\n" + corpus[1].Content

	for _, v := range recordContractGateProblems(report, corpus, nil) {
		if v.Gate == dslgate.GateCrossNamespaceImport {
			t.Fatalf("the import is present and the gate still fired: %s", v.String())
		}
	}
	if report.HasProblems() {
		t.Fatalf("a compliant corpus recorded problems: %+v", report.Skipped)
	}
}

// TestBootContractGatesStillRunThePerFileRules pins that widening the entry
// point to the whole corpus did not drop the per-file gates on the way.
//
// A retired operator form is per-FILE and needs no corpus at all, so it is the
// cheapest witness that both tiers of rule still run through one call.
func TestBootContractGatesStillRunThePerFileRules(t *testing.T) {
	report := newLoadReport()
	corpus := []baseloader.RawFile{{
		Path: "cognition/queries.memql",
		Content: `use cognition.concepts.{ space }

query space listSpaces {
  filter  status=="active"; ownerUserId==actor.userId
}
`,
	}}

	var sawPerFileGate bool
	for _, v := range recordContractGateProblems(report, corpus, nil) {
		if v.Gate != dslgate.GateCrossNamespaceImport {
			sawPerFileGate = true
		}
	}
	if !sawPerFileGate {
		t.Fatal("no per-file gate fired on a file carrying a retired `;`-AND separator. " +
			"memql#4051 replaced the per-file ScanSource loop with a single ScanFiles call; " +
			"if that call stopped running the per-file rules, this is how it shows up")
	}
}
