package conformance

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/znasllc-io/memql/component/automations"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// aggregateProbeTree is a tree whose boot the strict gate refuses: its one
// query binds a concept no domain declares, so the construct is skipped.
var aggregateProbeTree = fstest.MapFS{
	"aggprobe/memql.toml": {Data: []byte("memql = \"1.0\"\nedition = \"2026\"\n")},
	"aggprobe/queries.memql": {Data: []byte(
		"/// Nothing declares its concept.\nquery aggProbeGhost aggProbeQuery {\n  paginate 10\n}\n")},
}

// TestTheAutomationsPassLeavesTheStrictBootAggregateToTheLintPass pins why a
// batch of refusing cases boots once rather than once per case (PR #5693).
//
// The automations pass boots the Init the lint pass has just run over the same
// tree. When the strict gate refuses that boot, its error is an AGGREGATE of
// the skips the lint pass already reported one by one -- and returned as one
// line it named every refusing case in the batch, so no case could claim it
// and the whole batch fell back to one boot per case: 102 boots on this
// branch's corpus, where TestCorpusVerdicts went from ~309s to ~34s once the
// aggregate was left to the lint pass. The skip must still be reported, by the
// lint pass, naming the construct.
func TestTheAutomationsPassLeavesTheStrictBootAggregateToTheLintPass(t *testing.T) {
	// The positive control: this tree's boot IS refused by the strict gate,
	// so the assertion below is about the aggregate rather than about a tree
	// that happened to boot.
	func() {
		_, _, unmount := memqldsl.MountOverlayDomains(corpusQuiet, aggregateProbeTree)
		defer func() {
			unmount()
			memoryNodes.ReplaceAll(nil)
			_, _ = memql.LoadUnifiedConcepts(corpusQuiet)
		}()
		if _, err := memql.LoadUnifiedConcepts(corpusQuiet); err != nil {
			t.Fatalf("concepts: %v", err)
		}
		_, initErr := automations.NewOfflineEngine(corpusQuiet, memoryNodes.DefaultRegistry())
		if initErr == nil || !strings.Contains(initErr.Error(), "strict DSL boot refused") {
			t.Fatalf("the probe tree must be refused by the strict gate; Init returned %v", initErr)
		}
	}()

	diags, _, err := memql.LintUnifiedTree(corpusQuiet, aggregateProbeTree)
	if err != nil {
		t.Fatalf("LintUnifiedTree: %v", err)
	}
	reported := false
	for _, d := range diags {
		if strings.Contains(d.Message, `"aggProbeQuery"`) && d.File == "aggprobe/queries.memql" {
			reported = true
		}
	}
	if !reported {
		t.Fatalf("the lint pass must report the skipped query by itself, in its file; got %v", diags)
	}

	for _, line := range corpusAutomationProblems(t, aggregateProbeTree) {
		if strings.Contains(line, "strict DSL boot refused") {
			t.Errorf("the automations pass returned the strict-boot aggregate, which names every refusing case at once "+
				"and sends the batch to one boot per case:\n%s", line)
		}
	}
}
