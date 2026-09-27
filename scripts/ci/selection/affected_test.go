package selection

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

// fixtureGate stands in for the gate-inputs package list ci.yml passes.
var fixtureGate = []string{"./", "./scripts/..."}

func ips(dirs ...string) []string {
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		out = append(out, ip(d))
	}
	sort.Strings(out)
	return out
}

func TestSelectRunsFullWhenItCannotNarrow(t *testing.T) {
	g := loadFixture(t)
	cases := map[string][]string{
		"empty change list":           nil,
		"root go.mod":                 {"component/work/compile.go", "go.mod"},
		"nested go.sum":               {"component/memql/go.sum"},
		"go.work":                     {"go.work"},
		"go.work.sum":                 {"go.work.sum"},
		"a workflow":                  {".github/workflows/ci.yml"},
		"a composite action":          {".github/actions/go-cache/action.yml"},
		"the Makefile":                {"Makefile"},
		"the selector itself":         {"scripts/ci/selection/affected.go"},
		"the planner CLI":             {"scripts/ci/affected/main.go"},
		"the db-gated set definition": {"scripts/ci/db-gated-packages.sh"},
		"an absolute path":            {"/etc/passwd"},
		"a path climbing out":         {"../outside/x.go"},
	}
	for name, changed := range cases {
		sel, err := Select(g, changed, fixtureGate)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if sel.Mode != ModeFull {
			t.Errorf("%s: mode %s, want full (reason %q)", name, sel.Mode, sel.Reason)
		}
		if strings.TrimSpace(sel.Reason) == "" {
			t.Errorf("%s: a full run must say why", name)
		}
	}
}

func TestSelectMapsEveryFileOntoAPackage(t *testing.T) {
	g := loadFixture(t)
	cases := []struct {
		name      string
		changed   []string
		wantSeeds []string
		wantAll   []string
		wantTag   bool
	}{
		{
			name:      "a Go file in a leaf module reaches its importers and nothing else",
			changed:   []string{"component/work/compile.go"},
			wantSeeds: ips("component/work"),
			wantAll:   ips(".", "app", "component/work", "integrations/work"),
		},
		{
			name:      "a DELETED Go file lands on the package whose directory held it",
			changed:   []string{"component/work/removed.go"},
			wantSeeds: ips("component/work"),
			wantAll:   ips(".", "app", "component/work", "integrations/work"),
		},
		{
			name:      "a file only a node tag compiles marks a tag change",
			changed:   []string{"component/node/compiled_agent.go"},
			wantSeeds: ips("component/node"),
			wantAll:   ips(".", "app", "component/node", "component/server", "integrations/agent"),
			wantTag:   true,
		},
		{
			name:      "an embedded DSL file reaches everything importing dsl, plus the gate packages",
			changed:   []string{"dsl/agents/concepts.memql"},
			wantSeeds: ips(".", "dsl", "scripts/ci"),
			wantAll: ips(".", "app", "cmd/memqllint", "component/mcp", "component/memql", "component/memql/offline",
				"component/packages", "dsl", "scripts/ci", "test/conformance"),
		},
		{
			name:      "testdata nobody embeds reaches the package that owns the directory, and not its importers",
			changed:   []string{"component/memql/testdata/readiness/case.json"},
			wantSeeds: ips(".", "component/memql", "scripts/ci"),
			wantAll:   ips(".", "component/memql", "scripts/ci"),
		},
		{
			name:      "a test file reaches its own package only: importers never link it",
			changed:   []string{"component/memql/engine_test.go"},
			wantSeeds: ips("component/memql"),
			wantAll:   ips("component/memql"),
		},
		{
			name:      "a DELETED test file reaches its own package only",
			changed:   []string{"component/work/gone_test.go"},
			wantSeeds: ips("component/work"),
			wantAll:   ips("component/work"),
		},
		{
			name:      "the gate packages are run, not propagated: a doc beside a build change adds no importers",
			changed:   []string{"component/work/compile.go", "README.md"},
			wantSeeds: ips(".", "component/work", "scripts/ci"),
			wantAll:   ips(".", "app", "component/work", "integrations/work", "scripts/ci"),
		},
		{
			name:      "a file in a DELETED package directory climbs to the nearest package",
			changed:   []string{"component/gone/gone.go"},
			wantSeeds: ips("."),
			wantAll:   ips("."),
		},
		{
			name:      "a repository document reaches the root and the gate packages",
			changed:   []string{"CLAUDE.md"},
			wantSeeds: ips(".", "scripts/ci"),
			wantAll:   ips(".", "scripts/ci"),
		},
		{
			name:      "a Go-only change does not pay for the gate packages",
			changed:   []string{"integrations/agent/agent.go"},
			wantSeeds: ips("integrations/agent"),
			wantAll:   ips(".", "app", "integrations/agent"),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sel, err := Select(g, c.changed, fixtureGate)
			if err != nil {
				t.Fatal(err)
			}
			if sel.Mode != ModeAffected {
				t.Fatalf("mode %s (%s), want affected", sel.Mode, sel.Reason)
			}
			if !reflect.DeepEqual(sel.Seeds, c.wantSeeds) {
				t.Errorf("seeds = %v\n want %v", sel.Seeds, c.wantSeeds)
			}
			if !reflect.DeepEqual(sel.Packages, c.wantAll) {
				t.Errorf("affected = %v\n    want %v", sel.Packages, c.wantAll)
			}
			if sel.TagChange != c.wantTag {
				t.Errorf("TagChange = %v, want %v", sel.TagChange, c.wantTag)
			}
			for _, s := range sel.Seeds {
				if !contains(sel.Packages, s) {
					t.Errorf("seed %s is missing from the affected set, which must contain every seed", s)
				}
			}
		})
	}
}

func TestSelectRefusesAGatePatternThatMatchesNothing(t *testing.T) {
	g := loadFixture(t)
	if _, err := Select(g, []string{"README.md"}, []string{"./no/such/tree/..."}); err == nil {
		t.Fatal("a gate pattern matching no package must be an error: it would silently stop running the gates")
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
