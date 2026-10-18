package selection

// live_test.go -- the loader held to the real multi-module tree.
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

// Native node-tag commands are unconditional on PRs and merge candidates.
// scripts/citags now discovers every tagged test and checks actual native
// command coverage; the former TAG_TREES narrowing rule no longer schedules them.
