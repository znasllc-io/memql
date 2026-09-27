package selection

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
)

var synthDBTrees = []string{"app", "component/memql", "component/packages", "dbx"}

// synthGraph builds a graph big enough to clear the floors: the root, app,
// the memql tree, a node package, a leaf module and its importer, plus goN
// go-lane fillers and dbN db-lane fillers. It returns the graph and the
// complement db-gated-packages.sh would print for it.
func synthGraph(t *testing.T, goN, dbN int) (*Graph, []string) {
	t.Helper()
	pkgs := []Package{
		{ImportPath: ip("."), Dir: ".", ModuleDir: ".", Files: []string{"main.go"},
			Deps: []string{ip("app"), ip("component/node"), ip("component/memql")}},
		{ImportPath: ip("app"), Dir: "app", ModuleDir: ".", Files: []string{"app/app.go"},
			Deps: []string{ip("component/node"), ip("component/memql")}},
		{ImportPath: ip("component/node"), Dir: "component/node", ModuleDir: "component/node", Files: []string{"component/node/node.go"}},
		{ImportPath: ip("component/memql"), Dir: "component/memql", ModuleDir: "component/memql", Files: []string{"component/memql/engine.go"}},
		{ImportPath: ip("component/memql/offline"), Dir: "component/memql/offline", ModuleDir: "component/memql",
			Files: []string{"component/memql/offline/offline.go"}, Deps: []string{ip("component/memql")}},
		{ImportPath: ip("component/packages"), Dir: "component/packages", ModuleDir: ".", Files: []string{"component/packages/p.go"},
			Deps: []string{ip("component/memql")}},
		{ImportPath: ip("component/work"), Dir: "component/work", ModuleDir: "component/work", Files: []string{"component/work/w.go"}},
		{ImportPath: ip("integrations/work"), Dir: "integrations/work", ModuleDir: "integrations", Files: []string{"integrations/work/w.go"},
			Deps: []string{ip("component/work")}},
		{ImportPath: ip("scripts/ci"), Dir: "scripts/ci", ModuleDir: ".", Files: []string{"scripts/ci/ci.go"}},
	}
	for i := 0; i < goN; i++ {
		d := fmt.Sprintf("tools/g%03d", i)
		pkgs = append(pkgs, Package{ImportPath: ip(d), Dir: d, ModuleDir: ".", Files: []string{d + "/g.go"}})
	}
	for i := 0; i < dbN; i++ {
		d := fmt.Sprintf("dbx/d%03d", i)
		pkgs = append(pkgs, Package{ImportPath: ip(d), Dir: d, ModuleDir: ".", Files: []string{d + "/d.go"},
			Deps: []string{ip("component/memql")}})
	}
	g, err := NewGraph(pkgs)
	if err != nil {
		t.Fatal(err)
	}
	var complement []string
	for _, p := range pkgs {
		inDB := false
		for _, tree := range synthDBTrees {
			if p.Dir == tree || strings.HasPrefix(p.Dir, tree+"/") {
				inDB = true
			}
		}
		if !inDB {
			complement = append(complement, p.ImportPath)
		}
	}
	sort.Strings(complement)
	return g, complement
}

func synthInput(t *testing.T, event string, changed []string) PlanInput {
	t.Helper()
	g, complement := synthGraph(t, 180, 45)
	return PlanInput{
		Event:        event,
		Changed:      changed,
		Graph:        g,
		DBTrees:      synthDBTrees,
		Complement:   complement,
		Classes:      mustClasses(t, ciClasses),
		Timings:      Timings{"db": {"component/memql": 496, "component/packages": 162}, "go": {".": 111}},
		GatePatterns: []string{"./", "./scripts/..."},
		TagTrees:     []string{"app", "component/node", "component/server"},
		CPUs:         4,
	}
}

func dirsOf(shards []Shard) map[string]bool {
	out := map[string]bool{}
	for _, s := range shards {
		for _, d := range s.Dirs {
			out[d] = true
		}
	}
	return out
}

func TestMakePlanIsFullForEveryEventButAPullRequest(t *testing.T) {
	for _, event := range []string{"push", "merge_group", "workflow_dispatch", ""} {
		in := synthInput(t, event, []string{"component/work/w.go"})
		p, err := MakePlan(in)
		if err != nil {
			t.Fatalf("%q: %v", event, err)
		}
		if p.Mode != ModeFull || !p.Tags {
			t.Errorf("%q: mode %s tags %v, want full with the tag passes", event, p.Mode, p.Tags)
		}
		got := len(dirsOf(p.GoShards)) + len(dirsOf(p.DBShards))
		if got != in.Graph.Len() {
			t.Errorf("%q: a full plan places %d of %d packages", event, got, in.Graph.Len())
		}
		if len(p.Modules) != len(in.Graph.Modules()) {
			t.Errorf("%q: a full plan checks %d of %d modules", event, len(p.Modules), len(in.Graph.Modules()))
		}
	}
}

func TestMakePlanNarrowsAPullRequest(t *testing.T) {
	p, err := MakePlan(synthInput(t, "pull_request", []string{"component/work/w.go"}))
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode != ModeAffected {
		t.Fatalf("mode %s (%s), want affected", p.Mode, p.Reason)
	}
	goDirs, dbDirs := dirsOf(p.GoShards), dirsOf(p.DBShards)
	if !goDirs["component/work"] || !goDirs["integrations/work"] || len(goDirs) != 2 {
		t.Errorf("go lane = %v, want exactly component/work and integrations/work", goDirs)
	}
	if len(dbDirs) != 0 || len(p.DBShards) != 0 {
		t.Errorf("a change nothing db-gated imports must plan no db shard, got %v", dbDirs)
	}
	if p.Tags {
		t.Error("a change reaching no tag tree and no tagged file must skip the tag passes")
	}
	if want := []string{"component/work", "integrations"}; strings.Join(p.Modules, " ") != strings.Join(want, " ") {
		t.Errorf("modules = %v, want %v", p.Modules, want)
	}
	out, err := p.GitHubOutput()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "db_count=0\n") || !strings.Contains(out, `db_matrix={"include":[]}`) {
		t.Errorf("an empty db lane must read db_count=0 with an empty include list:\n%s", out)
	}
}

func TestMakePlanRunsTheTagPassesWhenATagTreeIsReached(t *testing.T) {
	p, err := MakePlan(synthInput(t, "pull_request", []string{"component/node/node.go"}))
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode != ModeAffected || !p.Tags {
		t.Errorf("a change to component/node must run the tag passes: mode %s tags %v", p.Mode, p.Tags)
	}
}

func TestMakePlanNeverReadsAnUnreadableDiffAsEmpty(t *testing.T) {
	in := synthInput(t, "pull_request", nil)
	in.DiffError = "base commit abc123 is not in the checkout"
	p, err := MakePlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode != ModeFull || !strings.Contains(p.Reason, "abc123") {
		t.Errorf("an unreadable diff must plan a FULL run naming why: %s %q", p.Mode, p.Reason)
	}
	in.DiffError = ""
	in.Changed = nil
	if p, err = MakePlan(in); err != nil || p.Mode != ModeFull {
		t.Errorf("an empty change list on a pull request must plan a full run: %v %v", p.Mode, err)
	}
}

func TestMakePlanRefusesWhatItCannotTrust(t *testing.T) {
	small, smallComplement := synthGraph(t, 100, 45)
	in := synthInput(t, "push", nil)
	in.Graph, in.Complement = small, smallComplement
	if _, err := MakePlan(in); err == nil {
		t.Error("a graph under the package floor must be refused")
	}

	thinDB, thinComplement := synthGraph(t, 200, 10)
	in = synthInput(t, "push", nil)
	in.Graph, in.Complement = thinDB, thinComplement
	if _, err := MakePlan(in); err == nil {
		t.Error("a full plan whose db lane is under its floor must be refused")
	}

	in = synthInput(t, "push", nil)
	in.Complement = in.Complement[1:]
	if _, err := MakePlan(in); err == nil || !strings.Contains(err.Error(), "disagree") {
		t.Errorf("a planner disagreeing with db-gated-packages.sh must refuse, got %v", err)
	}

	in = synthInput(t, "push", nil)
	in.DBTrees = nil
	if _, err := MakePlan(in); err == nil {
		t.Error("no db trees must be refused")
	}
}

func TestGitHubOutputIsWhatTheWorkflowReads(t *testing.T) {
	p, err := MakePlan(synthInput(t, "push", nil))
	if err != nil {
		t.Fatal(err)
	}
	out, err := p.GitHubOutput()
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("output line %q is not key=value", line)
		}
		values[k] = v
	}
	for _, k := range []string{"mode", "reason", "go_matrix", "go_count", "db_matrix", "db_count", "tags", "modules"} {
		if _, ok := values[k]; !ok {
			t.Errorf("output is missing %s", k)
		}
	}
	for _, k := range []string{"go_matrix", "db_matrix"} {
		var m struct {
			Include []map[string]any `json:"include"`
		}
		if err := json.Unmarshal([]byte(values[k]), &m); err != nil {
			t.Fatalf("%s is not JSON: %v", k, err)
		}
		if len(m.Include) == 0 {
			t.Errorf("%s: a full plan has entries", k)
		}
		for _, e := range m.Include {
			for _, field := range []string{"name", "packages", "timeout", "parallel", "uncached"} {
				if _, ok := e[field]; !ok {
					t.Errorf("%s entry %v lacks %s, which the step interpolates", k, e, field)
				}
			}
			if pk, _ := e["packages"].(string); strings.TrimSpace(pk) == "" {
				t.Errorf("%s entry %v has no packages: `go test` with none tests the current directory and passes", k, e)
			}
		}
	}
	if values["mode"] != "full" || values["tags"] != "true" {
		t.Errorf("mode/tags = %s/%s, want full/true", values["mode"], values["tags"])
	}
	if s := p.Summary(); !strings.Contains(s, "db-tests") || !strings.Contains(s, "memql") {
		t.Errorf("the summary must list the shards:\n%s", s)
	}
}
