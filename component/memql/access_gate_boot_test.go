package memql

import (
	"testing"
	"testing/fstest"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// access_gate_boot_test.go -- a booting node PUBLISHES its gate registry.
//
// Every other test of the registry drives loadAccessGates by hand, so each picks
// the publish value it likes and none can see the one line that decides whether
// a running node serves a gate at all: the call in initFromTree. Passing false
// there leaves every node with an empty registry, every visitor with a 404, and
// the rest of the suite green. This is the test that goes red.
//
// It is shaped like strict_boot_test.go: a real Init over the embedded tree plus
// a throwaway pack, which is the path a product bundle loads through.
func TestAccessGateInitPublishesTheRegistry(t *testing.T) {
	const domain = "accessgateboot"

	// The registry is process-wide and the pack's concept lands in the process-
	// wide concept registry, so both are put back as found.
	restoreAccessGatesAfter(t)
	before := memorynodes.All()
	t.Cleanup(func() { memorynodes.ReplaceAll(before) })

	mounted := false
	t.Cleanup(func() {
		if mounted {
			memqldsl.UnregisterTree(domain)
		}
	})
	memqldsl.RegisterTree(domain, withLanguageLine(fstest.MapFS{
		"concepts.memql": {Data: []byte(gateConceptSource)},
		"shapes.memql":   {Data: []byte("@row\nshape deskBrief deskBriefFull { }\n")},
		"queries.memql":  {Data: []byte(gateQuery(gateDashboard, "deskBriefs", codeArgs, codeFilter))},
	}))
	mounted = true

	// Start empty, so a hit below can only be this Init's doing.
	SetAccessGatesForTest()

	eng := newQuietEngine(t)
	if err := eng.Init(loadedConceptRegistry(t)); err != nil {
		t.Fatalf("engine.Init over a tree carrying one clean gated construct failed: %v", err)
	}
	if eng.loadReport.HasProblems() {
		t.Fatalf("the tree should load clean, got problems:\n%s", eng.loadReport.Detail())
	}

	got := AccessGateFor("client-desk", "dashboard", "deskBriefs", []string{domain})
	if got == nil {
		t.Fatal("a node booted over a gated construct and serves no gate: Init did not publish the registry")
	}
	if got.Kind != AccessKindQuery || got.Domain != domain || got.Namespace != domain ||
		got.Call() != "query accessgateboot.deskBriefs" {
		t.Fatalf("the published entry is not the declared construct: %+v (call %q)", got, got.Call())
	}

	// And the next boot REBUILDS it: a domain dropped from the bundle stops
	// opening anything, through the real Init and not only the loader.
	memqldsl.UnregisterTree(domain)
	mounted = false
	memorynodes.ReplaceAll(before)
	eng = newQuietEngine(t)
	if err := eng.Init(loadedConceptRegistry(t)); err != nil {
		t.Fatalf("engine.Init over the tree without the pack failed: %v", err)
	}
	if got := AccessGateFor("client-desk", "dashboard", "deskBriefs", []string{domain}); got != nil {
		t.Fatalf("a gate survived a boot of a tree that no longer declares it: %+v", got)
	}
	if entries := AccessGateEntries(); len(entries) != 0 {
		t.Fatalf("a boot of a tree with no gated construct left entries behind: %+v", entries)
	}
}
