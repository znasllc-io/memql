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
//
// # WHY IT LIVES IN THE ROOT PACKAGE (moved from scripts/ci in memql#3165)
//
// Because caching this test is never sound, and where it used to live it was
// cached exactly when it mattered.
//
// This gate reads the ANSWER A SUBPROCESS RETURNED -- a repo-wide `go list`
// over 183 packages. Go's test cache records the files a test OPENS; it has no
// way to record that answer. So in any cacheable package this reports a stale
// green over an import graph it never looked at: the memql#2972 fail-open,
// reopened by caching, and the same reasoning memql#3162 recorded when it kept
// the root package on `-count=1` and embed_inventory_test.go applied when it
// was placed here.
//
// In `scripts/ci` that was not theoretical. That package sits in the 169-
// package cacheable complement the go-tests lane runs WITHOUT `-count=1`, and
// the lane gating inverts perfectly against this guard's purpose:
//
//	RUN_GATES (uncached, gate-inputs step) fires only when NO .go file changed
//	RUN_GO    (cached, complement step)    fires exactly when .go files changed
//
// -- so it ran uncached only on the PRs where a cycle cannot have been
// introduced. Demonstrated on 2026-08-06 by introducing a genuine area-level
// cycle in real source (component/safety/approval -> component/mcp ->
// component/memql -> component/safety; legal Go, no package cycle): run as
// CI's complement step runs it, `ok (cached)`; forced with `-count=1`,
// `FAIL: area-level dependency CYCLE across 5 directories`.
//
// The root package is run uncached in BOTH lanes -- `go test -count=1 .` in
// go-tests under RUN_GO, and `./` in go-checks' gate-inputs step under
// RUN_GATES -- so here the guard executes against a fresh graph whatever the
// PR touched. That is also why it is not fixed by adding a `-count=1` step for
// scripts/ci instead: a new lane step is a new thing to keep, guarded by
// nothing, while the root package's uncached execution is already load-bearing
// for seven `git ls-files` sweeps and the embed inventory.
package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

const areaModulePath = "github.com/znasllc-io/memql"

// areaRepoRoot is this file's own directory -- the repo root, since this file
// lives in the root package. Derived from runtime.Caller rather than the
// working directory so `go list`'s meaning does not depend on how the test was
// invoked.
func areaRepoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(thisFile)
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

// requiredAreas is the area set this guard requires to be PRESENT, pinned by
// measurement rather than counted.
//
// # Why a set and not a floor
//
// The first version of this check was `len(g) >= 80` against a measured 120,
// and it could not fire. A floor asks "is the graph big enough", but the thing
// that disarms this guard is a MODULE BOUNDARY, and a boundary removes a named
// group of directories, not a random 41 of them. Every planned module was
// measured on 2026-08-06 with its areas dropped:
//
//	engine       -> 114 areas remain   CLEARS a floor of 80
//	platform     -> 113                CLEARS
//	base         -> 108                CLEARS
//	app          -> 106                CLEARS
//	integrations ->  86                CLEARS   <-- the largest planned module
//
// So the floor cleared in every case the split can actually produce, and the
// gate that exists to protect the split would have reported ok over a graph
// missing exactly the packages a cycle runs through. Demonstrated, not
// argued: reinstating the exact pre-memql#3164 SCC with the engine module's
// six areas dropped gave `ok`.
//
// A set has no such gap. `component/memql` leaving the module pattern is one
// missing string, and one missing string is red.
//
// # Why this is a REQUIRED SUBSET and not exact in both directions
//
// A deliberate difference from embed_inventory_test.go, which pins exact
// counts. That guard asserts WHAT SHIPS IN THE BINARY, where a file appearing
// is as much an event as a file vanishing. This one asserts SELECTOR
// COVERAGE: an area that appears is more of the repo being seen, which can
// never be the failure. Only disappearance fails open. Pinning additions too
// would put this list in the way of every new integration/ or cmd/ directory,
// and a guard that reddens unrelated PRs is a guard that gets edited away.
//
// Measured on 2026-08-06: 120 areas, 391 edges. To re-measure, run this test
// and paste the block it prints on failure:
//
//	go test -count=1 -run TestAreaGraphIsADAG github.com/znasllc-io/memql
//
// If an area legitimately goes away (a directory retired, two merged), delete
// its line and say so in the commit. Do not delete the check.
var requiredAreas = []string{
	"app",
	"cmd/action-upgrade",
	"cmd/admin-preview",
	"cmd/deploy-gate-check",
	"cmd/docs-gen",
	"cmd/envscan",
	"cmd/genesis-seal",
	"cmd/harness-eval",
	"cmd/healthcheck",
	"cmd/memql-arch",
	"cmd/memql-lsp",
	"cmd/memqlfmt",
	"cmd/memqllint",
	"cmd/memqlmigrate",
	"component",
	"component/actions",
	"component/architecture",
	"component/auth",
	"component/automations",
	"component/bus",
	"component/config",
	"component/database",
	"component/deploycontrol",
	"component/events",
	"component/fileprocessor",
	"component/genesis",
	"component/grpc",
	"component/harness",
	"component/healing",
	"component/identity",
	"component/inbound",
	"component/language",
	"component/mcp",
	"component/memql",
	"component/metadata",
	"component/metrics",
	"component/node",
	"component/observe",
	"component/outbound",
	"component/planner",
	"component/polyphon",
	"component/provenance",
	"component/router",
	"component/safety",
	"component/secret",
	"component/server",
	"component/service",
	"component/worker",
	"core/audio",
	"core/baseparser",
	"core/baseregistry",
	"core/common",
	"core/dslfs",
	"core/env",
	"core/grpctls",
	"core/httptls",
	"core/id",
	"core/literalparity",
	"core/liveknowledge",
	"core/logger",
	"docs",
	"dsl",
	"examples/deploypack",
	"examples/referencepack",
	"integrations",
	"integrations/actionsearch",
	"integrations/agent",
	"integrations/agentdef",
	"integrations/agents",
	"integrations/auth",
	"integrations/avatardirect",
	"integrations/avatarvendor",
	"integrations/azureblob",
	"integrations/chat",
	"integrations/cognition",
	"integrations/dailyspace",
	"integrations/database",
	"integrations/deployversion",
	"integrations/email",
	"integrations/embedding",
	"integrations/fileprocessor",
	"integrations/harnessrecall",
	"integrations/harnesstrace",
	"integrations/identity",
	"integrations/knowledge",
	"integrations/library",
	"integrations/liveknowledge",
	"integrations/openai",
	"integrations/openairealtime",
	"integrations/planner",
	"integrations/rbac",
	"integrations/router",
	"integrations/similarity",
	"integrations/stt",
	"integrations/telephony",
	"integrations/timeutil",
	"integrations/voice",
	"integrations/workbench",
	"scripts/audit-pagination",
	"scripts/ci",
	"scripts/cidb",
	"scripts/citags",
	"scripts/cluster",
	"scripts/dedupe-peruser-seeds",
	"scripts/deploy",
	"scripts/dev",
	"scripts/k3d",
	"scripts/lib",
	"scripts/make",
	"scripts/migrate-concepts",
	"scripts/migrations",
	"scripts/release",
	"scripts/restructure-by-construct",
	"scripts/sdk-gen",
	"scripts/secrets",
	"sdk/gen",
	"sdk/go",
	"test/conformance",
	"wire",
}

// areaGraph builds area -> set(area) from `go list`, dropping self-edges.
//
// The selector is the MODULE pattern, not `./...`, and that is the whole point
// of this guard existing (memql#3165). `./...` means "every package in the
// module rooted at the working directory". The moment the split this test
// gates puts a second `go.mod` beneath the root, `./...` stops seeing the
// packages that moved into it -- silently, exit 0, no diagnostic. This guard
// would then build an area graph over a fraction of the tree, find no cycle in
// that fraction, and report success: the gate protecting the module split
// would disarm itself at exactly the moment the split began. Naming the module
// keeps the selector meaning "the whole module" no matter where boundaries get
// drawn, and the requiredAreas check below catches the case where it stops
// meaning that anyway.
func areaGraph(t *testing.T) map[string]map[string]bool {
	t.Helper()
	cmd := exec.Command("go", "list", "-f", "{{.ImportPath}}{{range .Imports}} {{.}}{{end}}", areaModulePath+"/...")
	cmd.Dir = areaRepoRoot(t)
	out, err := cmd.Output()
	if err != nil {
		// Print `go list`'s stderr. Without it this reads `go list: exit
		// status 1` and nothing else, at precisely the moment it fires --
		// and the message it is swallowing ("cannot embed directory X: in
		// different module", "no required module provides package Y") is
		// the entire diagnosis. Mirrors embed_inventory_test.go.
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		t.Fatalf("go list %s/...: %v\n%s", areaModulePath, err, stderr)
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
	requireFullAreaCoverage(t, g)
	return g
}

// requireFullAreaCoverage fails closed when the graph is missing any area the
// repo is measured to have.
//
// This runs inside areaGraph rather than as its own test on purpose: every
// consumer of the graph must fail closed, and a coverage check a caller has to
// remember to invoke is a coverage check that eventually is not invoked. A
// PARTIAL graph is the dangerous shape -- it passes every cycle check by
// simply not containing the packages a cycle would run through -- and an EMPTY
// graph is just the extreme case, caught here as "all 120 areas missing".
func requireFullAreaCoverage(t *testing.T, g map[string]map[string]bool) {
	t.Helper()

	var missing []string
	for _, area := range requiredAreas {
		if _, ok := g[area]; !ok {
			missing = append(missing, area)
		}
	}
	if len(missing) == 0 {
		return
	}

	observed := make([]string, 0, len(g))
	for area := range g {
		observed = append(observed, area)
	}
	sort.Strings(observed)

	var paste strings.Builder
	for _, area := range observed {
		fmt.Fprintf(&paste, "\t%q,\n", area)
	}

	t.Fatalf("the area graph is MISSING %d of the %d areas this repo is measured to have, so "+
		"it covers only part of the tree -- this guard cannot verify what it was written to "+
		"verify and fails closed.\n\n"+
		"MISSING:\n  %s\n\n"+
		"The usual cause is the package selector no longer covering the whole repo. A "+
		"`go.mod` added beneath the root makes `go list %s/...` silently drop everything "+
		"inside the new module: exit 0, no diagnostic, and an area graph with no cycle in it "+
		"because the directories a cycle runs through are simply absent. Check with:\n"+
		"    git ls-files | grep '/go.mod$'\n"+
		"    go list %s/... | wc -l\n\n"+
		"If those areas genuinely went away (a directory retired, two merged), delete their "+
		"lines from requiredAreas and say so in the commit. Do NOT delete the check, and do "+
		"NOT replace it with a count -- a count of 114 against 120 is what let the "+
		"pre-memql#3164 cycle pass green.\n\n"+
		"Freshly measured set, formatted for paste into requiredAreas:\n%s",
		len(missing), len(requiredAreas), strings.Join(missing, "\n  "),
		areaModulePath, areaModulePath, paste.String())
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
