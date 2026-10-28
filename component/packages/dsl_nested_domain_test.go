package packages

import (
	"io/fs"
	"testing"
)

// TestADomainOfOnlySubNamespacesIsAnalyzedAndStaged (memql#5426): a package
// deploys the DSL domains it ships. A domain holding only sub-namespace
// directories was neither checked nor staged -- the analysis mounted only a
// directory with a direct .memql file, and stageAndRoll stages exactly the
// domains the analysis reports -- so the deploy succeeded having deployed none
// of that DSL. It is analyzed, counted and staged now, as a domain with a
// direct file is.
func TestADomainOfOnlySubNamespacesIsAnalyzedAndStaged(t *testing.T) {
	tree := validPackage()
	delete(tree, "dsl/acme/concepts.memql")
	// A concept's namespace is a colon-separated identifier, so a nested
	// directory holding concepts pins the one it declares under, as
	// dsl/shopify/generated does.
	tree["dsl/acme/billing/concepts.memql"] = file(validConcepts)
	tree["dsl/acme/billing/namespace.pin"] = file("acme\n")

	rep, err := Analyze(tree, Options{SourceVersion: "abc123"})
	if err != nil || !rep.OK {
		t.Fatalf("the package does not analyze clean: %v (problems: %+v)", err, rep.Problems)
	}
	if len(rep.DslDomains) != 1 || rep.DslDomains[0].Domain != "acme" {
		t.Fatalf("want the one domain 'acme' discovered, got %+v", rep.DslDomains)
	}
	if got := rep.DslDomains[0].Constructs["concept"]; got != 1 {
		t.Fatalf("want the nested concept counted in acme, got %d (%+v)", got, rep.DslDomains[0].Constructs)
	}

	// A changed DSL domain now needs the durable checkpoint, conditional
	// pointer and exact rollout observation. Use the real two-replica journal
	// fixture instead of pretending the legacy in-memory roller can satisfy it.
	c := newContinuationFixtureSource(t, tree)
	c.start(t)
	if len(c.stager.staged) != 1 || c.stager.staged[0] != "acme" {
		t.Fatalf("want the domain staged, got %v", c.stager.staged)
	}
	if c.stager.written != 1 || c.roller.calls != 1 {
		t.Fatalf("new DSL must move the pointer and roll once: written=%d rolls=%d", c.stager.written, c.roller.calls)
	}
	c.roller.ready = true
	c.resume(t)
	if rowString(c.row(t), "status") != StatusSucceeded || c.stager.written != 1 || c.roller.calls != 1 {
		t.Fatal("the observed nested-domain rollout must complete without restaging or restarting")
	}
	// What was staged is the whole domain tree, the nested file included.
	sub, err := fs.Sub(tree, "dsl/acme")
	if err != nil {
		t.Fatal(err)
	}
	_, files, err := hashTree(sub)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files["billing/concepts.memql"]; !ok {
		t.Fatalf("the staged tree does not carry the nested namespace: %v", keysOfFiles(files))
	}
	prefix, err := c.stager.PrefixFor("acme", sub)
	if err != nil || c.stager.active["acme"] != prefix {
		t.Fatalf("the active pointer does not name the complete nested tree: %v", err)
	}
}

func keysOfFiles(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
