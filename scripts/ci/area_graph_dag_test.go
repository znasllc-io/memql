// Static guard: the area-level dependency graph is a DAG
// (znasllc-io/memql#3164).
//
// # Why an AREA graph and not the package graph
//
// The package graph is already acyclic -- Go enforces that. The thing a module
// boundary is drawn around is a DIRECTORY, and directories aggregate packages.
// So the question that decides whether `go.mod` can be introduced is whether
// the graph is acyclic once packages are aggregated to area level.
//
// Before the moves this file guards, it was not. `component/language/parser`
// imported `component/memql/baseparser` while `component/memql` imported
// `component/language/parser`. Both legal Go, and fatal for a boundary drawn
// at `component/memql/`: the two directories import each other, so the two
// modules would import each other, and the toolchain rejects that.
//
// The audit found one 6-area strongly-connected component:
// component/{database,harness,actions,language,memql} + dsl -- an artifact of
// five L0 leaves (baseparser, baseregistry, dslfs, literalparity,
// liveknowledge) being misfiled inside the engine directory, plus
// component/memql importing integrations/audio, an mp3/wav codec that is a
// utility rather than an integration.
//
// # Why this is a test and not a one-off script
//
// The design marked "these two moves alone resolve the SCC" as an ESTIMATE.
// An estimate that gates a module split should be executable, and it should
// stay executable: nothing stops a future import from re-forming a cycle
// between two directories, and it would not be noticed until whoever tries the
// module split hits an unresolvable go.mod months later.
//
// So the property is asserted continuously, at the point where it is cheap to
// fix rather than the point where it blocks an epic.
package ci

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

const areaModulePath = "github.com/znasllc-io/memql"

func areaRepoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

// areaOf aggregates an import path to its area, matching the model in the
// design document's appendix.
//
// `component/*/gen` is deliberately its own area ("wire"): it is the generated
// protobuf surface, it is L0 with 21 importers, and it is targeted to become an
// independently versioned module. Folding it into `component/grpc` would
// manufacture a cycle that does not exist.
func areaOf(importPath string) (string, bool) {
	if !strings.HasPrefix(importPath, areaModulePath+"/") {
		return "", false // stdlib or third-party
	}
	rel := strings.TrimPrefix(importPath, areaModulePath+"/")
	seg := strings.Split(rel, "/")

	if len(seg) >= 3 && seg[0] == "component" && seg[2] == "gen" {
		return "wire", true
	}
	if len(seg) >= 2 {
		switch seg[0] {
		case "component", "integrations", "core", "cmd", "examples", "scripts", "editors", "sdk", "test":
			return seg[0] + "/" + seg[1], true
		}
	}
	return seg[0], true
}

// areaGraph builds area -> set(area) from `go list`, dropping self-edges.
func areaGraph(t *testing.T) map[string]map[string]bool {
	t.Helper()
	cmd := exec.Command("go", "list", "-f", "{{.ImportPath}}{{range .Imports}} {{.}}{{end}}", "./...")
	cmd.Dir = areaRepoRoot(t)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	g := map[string]map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		from, ok := areaOf(fields[0])
		if !ok {
			continue
		}
		if g[from] == nil {
			g[from] = map[string]bool{}
		}
		for _, imp := range fields[1:] {
			to, ok := areaOf(imp)
			if !ok || to == from {
				continue
			}
			g[from][to] = true
		}
	}
	if len(g) == 0 {
		t.Fatal("built an empty area graph -- this guard cannot verify what it was " +
			"written to verify, so it fails closed")
	}
	return g
}

// stronglyConnectedComponents runs Tarjan's algorithm and returns only the
// components with more than one member (a self-edge is not a directory cycle).
func stronglyConnectedComponents(g map[string]map[string]bool) [][]string {
	var (
		index   = map[string]int{}
		low     = map[string]int{}
		onStack = map[string]bool{}
		stack   []string
		next    int
		out     [][]string
	)

	nodes := make([]string, 0, len(g))
	for n := range g {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes) // deterministic output

	var strongConnect func(string)
	strongConnect = func(v string) {
		index[v] = next
		low[v] = next
		next++
		stack = append(stack, v)
		onStack[v] = true

		succ := make([]string, 0, len(g[v]))
		for w := range g[v] {
			succ = append(succ, w)
		}
		sort.Strings(succ)

		for _, w := range succ {
			if _, seen := index[w]; !seen {
				strongConnect(w)
				if low[w] < low[v] {
					low[v] = low[w]
				}
			} else if onStack[w] {
				if index[w] < low[v] {
					low[v] = index[w]
				}
			}
		}

		if low[v] == index[v] {
			var comp []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				comp = append(comp, w)
				if w == v {
					break
				}
			}
			if len(comp) > 1 {
				sort.Strings(comp)
				out = append(out, comp)
			}
		}
	}

	for _, n := range nodes {
		if _, seen := index[n]; !seen {
			strongConnect(n)
		}
	}
	return out
}

// TestAreaGraphIsADAG is the guard that unblocks (and keeps unblocked) the
// module split.
func TestAreaGraphIsADAG(t *testing.T) {
	sccs := stronglyConnectedComponents(areaGraph(t))
	if len(sccs) == 0 {
		return
	}
	for _, comp := range sccs {
		t.Errorf("area-level dependency CYCLE across %d directories: %s\n\n"+
			"These directories import each other, so a module boundary drawn around any "+
			"of them creates a genuine module cycle the Go toolchain rejects -- which is "+
			"what blocked memql#3165 until the misfiled L0 leaves were extracted.\n"+
			"Fix by moving the shared leaf DOWN (into core/) rather than by adding an "+
			"interface: the cycle is a filing error, not a design one.",
			len(comp), strings.Join(comp, " <-> "))
	}
}
