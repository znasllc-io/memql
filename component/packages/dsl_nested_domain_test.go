package packages

import (
	"context"
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

	h := newHarness(t, tree, ownerPackage())
	if _, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId:  "v1:platform:package:abc",
		Actor:      mayDeployDsl(),
		Confirmed:  true,
		Placements: firstDeployPlacements(),
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if len(h.stager.staged) != 1 || h.stager.staged[0] != "acme" {
		t.Fatalf("want the domain staged, got %v", h.stager.staged)
	}
	if h.stager.written != 1 || h.roller.rolls != 1 {
		t.Fatalf("new DSL must move the pointer and roll once: written=%d rolls=%d", h.stager.written, h.roller.rolls)
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
}

func keysOfFiles(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
