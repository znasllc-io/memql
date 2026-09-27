package selection

// live_test.go -- the loader and the tag-pass gate held to the REAL tree.
//
// Everything else in this package is tested on a recorded fixture, which is
// what keeps the decisions pure. These two tests are the exception on purpose:
// the fixture cannot tell whether `go list` on this repository still loads, or
// whether the node-tag passes in ci.yml still test only packages the planner's
// tag rule can see. Both take a few seconds.

import (
	"context"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const liveModulePath = "github.com/znasllc-io/memql"

func liveRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.work above %s", filepath.Dir(file))
		}
		dir = parent
	}
}

func liveGraph(t *testing.T) (*Graph, string) {
	t.Helper()
	root := liveRoot(t)
	tags, err := NodeTags(root)
	if err != nil {
		t.Fatal(err)
	}
	g, err := LoadGraph(context.Background(), root, liveModulePath, tags, GoList(root, liveModulePath+"/..."))
	if err != nil {
		t.Fatalf("loading the real tree's graph: %v", err)
	}
	return g, root
}

// TestLoadGraphOnTheRealTree: the loader reads this repository whole -- above
// the planner's floor, every package directory real, and exactly the modules
// the tree tracks.
func TestLoadGraphOnTheRealTree(t *testing.T) {
	g, root := liveGraph(t)
	if g.Len() < minGraphPackages {
		t.Fatalf("the real tree loaded %d packages, under the planner's floor of %d", g.Len(), minGraphPackages)
	}
	rootPkg, ok := g.Package(liveModulePath)
	if !ok || rootPkg.Dir != "." {
		t.Fatalf("the root package is missing or misplaced: %+v", rootPkg)
	}
	for _, ipth := range g.ImportPaths() {
		p, _ := g.Package(ipth)
		if fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(p.Dir))); err != nil || !fi.IsDir() {
			t.Errorf("%s: directory %s does not exist", ipth, p.Dir)
		}
	}
	out, err := exec.Command("git", "-C", root, "ls-files", "go.mod", "*/go.mod").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var tracked []string
	for _, f := range strings.Fields(string(out)) {
		tracked = append(tracked, path.Dir(f))
	}
	sort.Strings(tracked)
	if got := g.Modules(); strings.Join(got, " ") != strings.Join(tracked, " ") {
		t.Errorf("the graph's modules are not the tracked go.mod set:\n  graph:   %v\n  tracked: %v\n"+
			"module-boundaries checks the planner's list on a pull request, so a module the graph misses "+
			"is a module never checked there", got, tracked)
	}
}

// TestTagPassPackagesAreReachedThroughTheTagTrees is the soundness half of
// the tag-pass gate (memql#5483). The planner runs the seven passes when the
// affected set reaches a package under TAG_TREES. That is only right if every
// package a pass TESTS is either under one of those trees or imported -- over
// the union graph, test binaries included -- by a package that is; otherwise a
// change reaching only that package would skip the pass that tests it.
func TestTagPassPackagesAreReachedThroughTheTagTrees(t *testing.T) {
	g, root := liveGraph(t)
	raw, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			Strategy struct {
				Matrix yaml.Node `yaml:"matrix"`
			} `yaml:"strategy"`
			Steps []struct {
				ID  string            `yaml:"id"`
				Env map[string]string `yaml:"env"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatal(err)
	}
	var trees []string
	for _, s := range wf.Jobs["plan"].Steps {
		if s.ID == "plan" {
			trees = strings.Fields(s.Env["TAG_TREES"])
		}
	}
	if len(trees) == 0 {
		t.Fatal("the plan step carries no TAG_TREES")
	}
	var matrix struct {
		Include []struct {
			Tag      string `yaml:"tag"`
			Packages string `yaml:"packages"`
		} `yaml:"include"`
	}
	node := wf.Jobs["go-tests-tags"].Strategy.Matrix
	if err := node.Decode(&matrix); err != nil || len(matrix.Include) == 0 {
		t.Fatalf("reading go-tests-tags' matrix: %v (entries %d)", err, len(matrix.Include))
	}
	underATree := func(dir string) bool {
		for _, tr := range trees {
			if dir == tr || strings.HasPrefix(dir, tr+"/") {
				return true
			}
		}
		return false
	}
	byDir := map[string]string{}
	for _, ipth := range g.ImportPaths() {
		p, _ := g.Package(ipth)
		byDir[p.Dir] = ipth
	}
	for _, e := range matrix.Include {
		for _, arg := range strings.Fields(e.Packages) {
			dir := strings.TrimSuffix(strings.TrimPrefix(arg, "./"), "/")
			ipth, ok := byDir[dir]
			if !ok {
				t.Errorf("tag %s tests %s, which is not a package", e.Tag, arg)
				continue
			}
			if underATree(dir) {
				continue
			}
			reached := false
			for _, importer := range g.rdeps[ipth] {
				if p, _ := g.Package(importer); underATree(p.Dir) {
					reached = true
					break
				}
			}
			if !reached {
				t.Errorf("tag %s tests %s, which is under no TAG_TREES tree %v and imported by no package "+
					"that is: a change reaching only it would skip the pass that tests it. Add its tree to "+
					"TAG_TREES in ci.yml's plan step.", e.Tag, dir, trees)
			}
		}
	}
}
