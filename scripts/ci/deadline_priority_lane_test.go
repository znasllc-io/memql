package ci

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/pipelines"
)

// A test nothing runs is the failure this holds off, and it has happened here:
// scripts/lib/test_host_build_guard.py is named by no step, so the guard it covers
// can go red with every lane green. The deadline promoter's tests are Go so that
// they need no step of their own, which makes "CI runs them" a claim about the
// selector rather than about where the files sit. So this asks the selector, on the
// real tree and the real manifest, what a change to each part of the promoter
// reaches, and whether go-tests keeps it: its YAML (which no import reaches, and
// which `make test` would still cover) most of all.
func TestAChangeToThePromoterRunsItsTestsInGoTests(t *testing.T) {
	const (
		module    = "github.com/znasllc-io/memql"
		component = module + "/deploy/k8s/components/pipelines-capacity"
		promoter  = module + "/integrations/pipelinesteps"
	)
	spec := nativePipeline(t)
	goTests := nativeRequiredStep(t, "go-tests")
	if goTests.Packages != pipelines.PackagesAffected || goTests.Only != "not-db-gated" {
		t.Fatalf("go-tests selects %q, only %q: this gate reads the affected, not-db-gated selection", goTests.Packages, goTests.Only)
	}
	for _, tree := range spec.Select.DBGated {
		for _, dir := range []string{"deploy/k8s/components/pipelines-capacity", "integrations/pipelinesteps"} {
			if dir == tree || strings.HasPrefix(dir, tree+"/") {
				t.Fatalf("%s is under the db-gated tree %s, which go-tests drops and whose lane needs a database these tests do not", dir, tree)
			}
		}
	}
	graph, err := pipelines.ScanGoTree(os.DirFS(RepoRoot()))
	if err != nil {
		t.Fatal(err)
	}
	for changed, want := range map[string][]string{
		"deploy/k8s/components/pipelines-capacity/deadline-priority.yaml": {component},
		"deploy/k8s/components/pipelines-capacity/priorities.yaml":        {component},
		"deploy/k8s/components/pipelines-capacity/queue.yaml":             {component},
		"integrations/pipelinesteps/deadline_priority.go":                 {promoter, component, module},
		"integrations/pipelinesteps/jobspec.go":                           {promoter, component, module},
		"subcommand_pipeline_deadline_priority.go":                        {module},
	} {
		facts, err := pipelines.AnalyzeSelection(graph, []string{changed}, true, spec.Select.Full)
		if err != nil {
			t.Fatalf("%s: %v", changed, err)
		}
		selection, err := pipelines.ResolveSelection(graph, facts, pipelines.SelectionDecision{})
		if err != nil {
			t.Fatalf("%s: %v", changed, err)
		}
		for _, pkg := range want {
			if !slices.Contains(selection.Packages, pkg) {
				t.Errorf("a change to %s does not select %s (%s)", changed, pkg, selection.Reason)
			}
		}
	}
}
