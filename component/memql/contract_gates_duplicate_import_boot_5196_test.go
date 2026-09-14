package memql

import (
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/baseloader"
	"github.com/znasllc-io/memql/component/memql/dslgate"
)

// contract_gates_duplicate_import_boot_5196_test.go -- memql#5196.
//
// A .memql file that imports the SAME bare name from two different domains gets
// a positional pick with no error and no warning. The gate that refuses it must
// reach STRICT BOOT, not just this repository's conformance run: a product DSL
// bundle mounted at MEMQL_DSL_PATH is the tree no Go test in this repo walks and
// the primary delivery path (memql#2472), so a gate that only ever ran over dsl/
// would leave the more dangerous corpus uncovered.
//
// WHY THESE TESTS HAVE TO EXIST rather than trusting the tree. The embedded
// corpus has ZERO instances -- 305 .memql files, 205 `use { }` lines, and the
// three names declared in two domains each (`account`, `invocation`, `run`) all
// resolve to a single source corpus-wide. So every observation of this gate on
// the real tree is a null result, and a null result cannot tell a gate that runs
// and finds nothing from a gate that does not run. The fixture below is what
// makes the instrument move.

// duplicateImportCorpus is one file binding `invocation` from both the domains
// that declare it.
func duplicateImportCorpus() []baseloader.RawFile {
	return []baseloader.RawFile{
		{
			Path: "worker/queries.memql",
			Content: `use worker.concepts.{ invocation }
use observability.concepts.{ invocation }

@description("Reads whichever invocation won.")
query invocation recentInvocations {
  args {
    ownerUserId  string  @required
  }
  filter  row => row.ownerUserId == args.ownerUserId
}
`,
		},
	}
}

// TestDuplicateImportNameGateIsRecordedOnTheLoadReport: the gate reaches the
// report Init refuses a boot on, not merely a test in this repository.
func TestDuplicateImportNameGateIsRecordedOnTheLoadReport(t *testing.T) {
	report := newLoadReport()
	violations := recordContractGateProblems(report, duplicateImportCorpus(), nil)

	var found *dslgate.Violation
	for i := range violations {
		if violations[i].Gate == dslgate.GateDuplicateImportName {
			found = &violations[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("the duplicate-import gate did not fire through recordContractGateProblems; got %v", violations)
	}
	if found.Construct != "invocation" {
		t.Errorf("Construct = %q, want %q", found.Construct, "invocation")
	}

	if !report.HasProblems() {
		t.Fatal("report.HasProblems() is false, so strict boot would ADMIT a corpus whose bare names resolve by import order")
	}

	wantPhase := "contract-gate:" + string(dslgate.GateDuplicateImportName)
	var skipped bool
	for _, s := range report.Skipped {
		if s.Phase == wantPhase {
			skipped = true
			if s.File != "worker/queries.memql" {
				t.Errorf("skip attributed to %q, want worker/queries.memql", s.File)
			}
			if s.Keyword != "use" {
				t.Errorf("skip Keyword = %q, want \"use\" -- an empty Kind is defaulted to \"filter\" and files the skip under the wrong construct", s.Keyword)
			}
			if !strings.Contains(s.Err, "line 2") {
				t.Errorf("skip does not carry the line of the losing import: %q", s.Err)
			}
		}
	}
	if !skipped {
		t.Errorf("no skip recorded under phase %q", wantPhase)
	}
}

// TestDuplicateImportGateAdmitsACleanCorpus is the control: without it, a gate
// that fires on every file passes the test above.
func TestDuplicateImportGateAdmitsACleanCorpus(t *testing.T) {
	clean := []baseloader.RawFile{
		{
			Path: "worker/queries.memql",
			Content: `use worker.concepts.{ invocation }
use observability.concepts.{ codeMetric }

@description("Reads worker invocations.")
query invocation recentInvocations {
  args {
    ownerUserId  string  @required
  }
  filter  row => row.ownerUserId == args.ownerUserId
}
`,
		},
	}
	for _, v := range recordContractGateProblems(newLoadReport(), clean, nil) {
		if v.Gate == dslgate.GateDuplicateImportName {
			t.Fatalf("the gate fired on a corpus with no colliding local name: %s", v.Detail)
		}
	}
}

// TestImportResolutionIsFirstWins pins the DIRECTION the gate's message asserts.
//
// This is the test the issue asked for by name. Every instinct from "a symbol
// table is a map" says the later import overwrites the earlier one; it does not,
// and a fix written from that assumption would pass a test written from the same
// assumption and look correct. So the direction is asserted against the resolver
// itself rather than against the gate's prose: if resolution ever becomes
// last-wins, this fails and the gate's message is then known to be wrong.
func TestImportResolutionIsFirstWins(t *testing.T) {
	uses := []*languageParser.UseDeclaration{
		{Path: "worker.concepts", Parts: []string{"worker", "concepts"}, Names: []string{"invocation"}},
		{Path: "observability.concepts", Parts: []string{"observability", "concepts"}, Names: []string{"invocation"}},
	}

	ns, source := namespaceHintForName(uses, "invocation")
	if ns != "worker" {
		t.Fatalf("namespaceHintForName returned %q; the FIRST import must win. If this now says \"observability\", resolution became last-wins and dslgate's duplicate-import message states the wrong direction", ns)
	}
	if source != "invocation" {
		t.Errorf("source name = %q, want %q", source, "invocation")
	}
}

// TestAliasKeepsBothImportsReachable is why the gate's remedy is an alias rather
// than "delete one of them": the symbol table is keyed by the LOCAL name, so
// aliasing changes the KEY and both entries survive. Re-keying that table by the
// SOURCE name would reopen import capture (memql#3802) AND break this remedy.
func TestAliasKeepsBothImportsReachable(t *testing.T) {
	uses := []*languageParser.UseDeclaration{
		{Path: "worker.concepts", Parts: []string{"worker", "concepts"}, Names: []string{"invocation"}},
		{
			Path:    "observability.concepts",
			Parts:   []string{"observability", "concepts"},
			Names:   []string{"invocation"},
			Aliases: map[string]string{"invocation": "observedInvocation"},
		},
	}

	if ns, _ := namespaceHintForName(uses, "invocation"); ns != "worker" {
		t.Errorf("bare `invocation` resolved to %q, want worker", ns)
	}
	ns, source := namespaceHintForName(uses, "observedInvocation")
	if ns != "observability" {
		t.Errorf("aliased name resolved to %q, want observability -- the alias did not make the second import reachable", ns)
	}
	if source != "invocation" {
		t.Errorf("aliased source name = %q, want %q: the module still spells it invocation", source, "invocation")
	}
}
