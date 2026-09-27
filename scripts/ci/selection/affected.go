package selection

import (
	"fmt"
	"path"
	"strings"
)

// Mode says whether a run tests everything or only what a change reaches.
type Mode string

const (
	// ModeFull tests every package. Push and merge_group runs are always
	// full, and so is any pull request whose change the graph cannot reason
	// about.
	ModeFull Mode = "full"
	// ModeAffected tests the packages a pull request's change reaches.
	ModeAffected Mode = "affected"
)

// Selection is what a set of changed files reaches.
type Selection struct {
	Mode Mode
	// Reason says why the run is full, or summarises the narrowing.
	Reason string
	// Seeds are the packages the changed files map onto, in either scope,
	// plus the gate packages when a non-Go file changed (affected mode).
	Seeds []string
	// Packages is the affected set (affected mode): every seed, plus every
	// package whose test binary depends on a package whose BUILD changed.
	Packages []string
	// TagChange is true when a changed file only compiles under a node tag.
	TagChange bool
}

// fullTrigger reports whether a changed file invalidates the premise of
// narrowing, and why. Each entry is a file the graph cannot reason about
// because it changes the graph itself or the machinery that computes it --
// the design record's "a changed go.mod, go.work, the tool itself or ci.yml
// means everything", with ci.yml widened to all of .github/ and the Makefile
// (the `changes` job's `ci` bucket), since a composite action or a make
// target is CI configuration exactly as a workflow is.
func fullTrigger(file string) (string, bool) {
	switch base := path.Base(file); {
	case base == "go.mod" || base == "go.sum" || base == "go.work" || base == "go.work.sum":
		return file + " changes the module graph the selection is computed from", true
	case strings.HasPrefix(file, ".github/"):
		return file + " is CI configuration, which can change what any lane runs", true
	case file == "Makefile":
		return "Makefile is CI configuration, which can change what any lane runs", true
	case strings.HasPrefix(file, "scripts/ci/selection/") || strings.HasPrefix(file, "scripts/ci/affected/"):
		return file + " changes the selector, which must not be trusted to narrow its own change", true
	case file == "scripts/ci/db-gated-packages.sh":
		return file + " defines the db-gated set and its complement", true
	}
	return "", false
}

// Select maps changed files (repo-relative paths) onto the packages whose
// tests they can affect.
//
// A changed file has a SCOPE, and the scope decides how far it reaches:
//
//   - build scope -- a non-test Go source (including one only a node tag
//     compiles, and one that was deleted) or a //go:embed match of the
//     package's own build. It changes what every importer links, so it
//     reaches the package AND every package whose test binary depends on it.
//   - test scope -- a _test.go source, a test build's embed, or any other
//     file under the package's directory that no build names (testdata, a
//     fixture, a README). Only the package's own tests can read it, so it
//     reaches that package and nothing else.
//
// gate lists `go test` package patterns whose tests read repository files by
// PATH rather than through an import (docs gates, manifest gates, the
// path-routing guards), which no graph can see. They are added, in test
// scope, whenever a changed file is not Go source. The CI step passes the
// same list the go-checks gate-inputs step runs.
//
// Every changed path lands on a package or turns the run full; a file is
// never dropped. The order of the rules:
//
//  1. an empty list, or a path that is not repo-relative, is full -- an
//     empty diff on a pull request means the diff failed, and narrowing to
//     nothing would skip every lane;
//  2. a full trigger (fullTrigger) is full;
//  3. a file some package's build or tests name maps to that package, in
//     that scope;
//  4. anything else maps to the package owning the deepest directory above
//     it -- in build scope when it is a non-test Go file (a deleted source),
//     in test scope otherwise. The root is a package, so this always finds
//     one.
func Select(g *Graph, changed []string, gate []string) (Selection, error) {
	if len(changed) == 0 {
		return Selection{Mode: ModeFull, Reason: "the change list is empty; narrowing to nothing would skip every lane"}, nil
	}
	built := map[string]bool{}  // packages whose build changed: reach importers
	tested := map[string]bool{} // packages only whose tests changed
	nonGo, tagChange := false, false
	for _, raw := range changed {
		f := path.Clean(strings.TrimSpace(raw))
		if f == "." || path.IsAbs(f) || f == ".." || strings.HasPrefix(f, "../") {
			return Selection{Mode: ModeFull, Reason: fmt.Sprintf("changed path %q is not repo-relative", raw)}, nil
		}
		if reason, ok := fullTrigger(f); ok {
			return Selection{Mode: ModeFull, Reason: reason}, nil
		}
		isGo := strings.HasSuffix(f, ".go")
		if !isGo {
			nonGo = true
		}
		buildOwners, testOwners := g.owners[f], g.testOwners[f]
		if len(buildOwners) == 0 && len(testOwners) == 0 {
			ip, ok := g.nearest(f)
			if !ok {
				return Selection{Mode: ModeFull, Reason: f + " is under no package directory"}, nil
			}
			if isGo && !strings.HasSuffix(f, "_test.go") {
				buildOwners = []string{ip}
			} else {
				testOwners = []string{ip}
			}
		}
		for _, o := range buildOwners {
			built[o] = true
		}
		for _, o := range testOwners {
			tested[o] = true
		}
		if g.tagged[f] {
			tagChange = true
		}
	}
	if nonGo {
		for _, pat := range gate {
			ips, err := g.Expand(pat)
			if err != nil {
				return Selection{}, fmt.Errorf("gate package pattern: %w", err)
			}
			for _, ip := range ips {
				tested[ip] = true
			}
		}
	}
	seeds := map[string]bool{}
	affected := map[string]bool{}
	for s := range tested {
		seeds[s] = true
		affected[s] = true
	}
	for s := range built {
		seeds[s] = true
		affected[s] = true
		for _, r := range g.rdeps[s] {
			affected[r] = true
		}
	}
	sel := Selection{
		Mode:      ModeAffected,
		Seeds:     sortedKeys(seeds),
		Packages:  sortedKeys(affected),
		TagChange: tagChange,
	}
	sel.Reason = fmt.Sprintf("%d changed file(s) reach %d of %d packages", len(changed), len(sel.Packages), g.Len())
	return sel, nil
}
