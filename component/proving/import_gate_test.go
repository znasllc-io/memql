package proving

import (
	"os/exec"
	"sort"
	"strings"
	"testing"
)

// pureSubpackages are the ones that must never reach the engine.
//
// They hold everything the proving suite decides over VALUES: the statistics
// and the honesty rules, the corpus format, the scorecard and its two gates,
// the capability envelope, the fake external world and the recorded responses.
// This package -- and only this package -- touches the engine.
var pureSubpackages = []string{
	"github.com/znasllc-io/memql/component/proving/figure",
	"github.com/znasllc-io/memql/component/proving/scenario",
	"github.com/znasllc-io/memql/component/proving/scorecard",
	"github.com/znasllc-io/memql/component/proving/capability",
	"github.com/znasllc-io/memql/component/proving/world",
	"github.com/znasllc-io/memql/component/proving/cassette",
}

// TestProvingPureSubpackagesImportNothingBeyondStdlib is what makes "these
// cannot reach the engine" a BUILD-GRAPH FACT rather than a promise.
//
// # Why it exists at all
//
// The epic's standard is that the numbers must be honest before they are good.
// A statistics layer that could reach the engine is one nobody can check
// without running the engine, and a corpus loader that could read a row is one
// whose refusals depend on cluster state. Keeping them in separate PACKAGES
// makes the question mechanical: `go list -deps` is the build graph, and the
// build graph does not have opinions.
//
// # Why not a nested Go module
//
// component/work is a leaf module for this reason, and the same shape was
// considered here. A new nested module fires roughly twelve repo-wide gates --
// go.work, two go.mod require/replace pairs, BOTH Dockerfiles, the db-gated
// package script, the module taxonomy, the embed inventory -- three of which
// are invisible to `make test` AND to a plain `go test`. A package boundary
// plus this gate buys the same property at none of that cost, and unlike the
// module it also names, in one place, exactly which packages are covered.
//
// # What "beyond stdlib" allows
//
// The pure packages may import each other -- scenario reads figure's metric
// registry, scorecard reads both -- and nothing else outside the standard
// library. There is no exemption list, deliberately: the first exemption is
// the one that makes the next one arguable.
func TestProvingPureSubpackagesImportNothingBeyondStdlib(t *testing.T) {
	allowed := map[string]bool{}
	for _, p := range pureSubpackages {
		allowed[p] = true
	}

	for _, pkg := range pureSubpackages {
		out, err := exec.Command("go", "list", "-deps", pkg).Output()
		if err != nil {
			t.Fatalf("go list -deps %s: %v", pkg, err)
		}
		var offenders []string
		for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			dep = strings.TrimSpace(dep)
			switch {
			case dep == "" || dep == pkg:
				continue
			case allowed[dep]:
				continue
			case isStdlib(dep):
				continue
			}
			offenders = append(offenders, dep)
		}
		if len(offenders) > 0 {
			sort.Strings(offenders)
			t.Errorf("%s imports outside the standard library:\n  %s\n\n"+
				"This package is one of the proving suite's PURE halves: its whole value is that its "+
				"decisions can be checked without running anything. If the import is genuinely needed, "+
				"the code belongs in component/proving (which may import anything) rather than here.",
				pkg, strings.Join(offenders, "\n  "))
		}
	}
}

// isStdlib reports whether an import path is in the standard library. The
// heuristic is the usual one and it is exact enough for this purpose: a
// standard-library path's first segment carries no dot, because a module path
// outside the standard library begins with a domain.
func isStdlib(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}

// TestTheGateCoversEveryPureSubpackage keeps the list above from silently
// falling behind the tree. A new pure sub-package that nobody adds to
// pureSubpackages is one this gate does not cover, and its absence looks
// exactly like a pass.
func TestTheGateCoversEveryPureSubpackage(t *testing.T) {
	out, err := exec.Command("go", "list", "github.com/znasllc-io/memql/component/proving/...").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	listed := map[string]bool{}
	for _, p := range pureSubpackages {
		listed[p] = true
	}
	for _, pkg := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		pkg = strings.TrimSpace(pkg)
		if pkg == "" || pkg == "github.com/znasllc-io/memql/component/proving" {
			continue
		}
		if !listed[pkg] {
			t.Errorf("%s is a component/proving sub-package and is not in pureSubpackages.\n"+
				"Either it is pure -- add it, and the gate will hold it to that -- or it touches the engine, "+
				"in which case it belongs in component/proving itself rather than beside the pure halves.", pkg)
		}
	}
}
