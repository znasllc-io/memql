package pipelines

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
)

// FullTriggers are the paths whose change selects everything, beside a
// pipeline's own select.full globs.
var FullTriggers = []string{"go.mod", "go.sum", "go.work", "go.work.sum"} // matched by base name anywhere

// ManifestPath is the manifest a pipeline is declared in; its change selects
// everything, because it can change what every step runs.
const ManifestPath = "memql-package.yaml" // matched at the root

// Affected selects what changed touches: the package each changed path
// belongs to (its directory or the nearest package above), plus every
// package importing one of them, transitively. Full when: the change list
// is empty, a full trigger or a select.full glob matches, or the graph is
// incomplete.
//
// Every one of those is a change the graph cannot reason about, and the
// answer to "cannot say" is everything, never nothing: a selection that is
// too wide costs minutes, one that is too narrow merges a red build, and the
// full run in the merge queue is the backstop only for the first kind of
// mistake (design record D1, section 5). An empty change list in particular
// is read as a diff that could not be read, not as a change that touched
// nothing. Vendored code at a module root counts as a full trigger too: the
// graph skips vendor directories, as the go tool does, so it cannot say who a
// vendored change reaches.
//
// select.full is read with PathSet, as Validate admits it: a path is in it
// when a plain glob matches and no `!` glob does. A list PathSet cannot read
// is an error, never a list that matches nothing.
//
// A changed path that no package owns (PackageAt) seeds nothing. That is how
// a change confined to files no Go package can see selects no packages, and
// the steps selecting packages skip.
func Affected(g *Graph, changed []string, fullGlobs []string) (Selection, error) {
	if g == nil {
		return Selection{}, errors.New("there is no import graph to select from")
	}
	var inFull func(string) bool
	if len(fullGlobs) > 0 {
		set, err := PathSet(fullGlobs)
		if err != nil {
			return Selection{}, fmt.Errorf("select.full: %w", err)
		}
		inFull = set
	}
	everything := func(reason string) (Selection, error) {
		return Selection{Full: true, Reason: reason, Packages: g.importPaths()}, nil
	}

	if len(changed) == 0 {
		return everything("the change list is empty, which is a diff that could not be read: narrowing to nothing would skip every step")
	}
	if unread := g.Incomplete(); len(unread) > 0 {
		return everything(firstAndCount(unread) + " could not be read, so the graph cannot say what a change reaches")
	}
	seeds := map[string]bool{}
	for _, raw := range changed {
		p, ok := repoPath(raw)
		if !ok {
			return everything(fmt.Sprintf("changed path %q is not repository-relative", clip(raw)))
		}
		if reason, ok := g.fullTrigger(p); ok {
			return everything(reason)
		}
		if inFull != nil && inFull(p) {
			return everything(p + " is under select.full")
		}
		if importPath, ok := g.PackageAt(p); ok {
			seeds[importPath] = true
		}
	}

	selected := map[string]bool{}
	queue := sortedMapKeys(seeds)
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		if selected[next] {
			continue
		}
		selected[next] = true
		queue = append(queue, g.importers[next]...)
	}
	sel := Selection{Seeds: sortedMapKeys(seeds), Packages: sortedMapKeys(selected)}
	if len(sel.Seeds) == 0 {
		sel.Reason = fmt.Sprintf("%s, none in a Go package", count(len(changed), "changed path", "changed paths"))
	} else {
		sel.Reason = fmt.Sprintf("%s seed %s; with their importers, %d of %d are selected",
			count(len(changed), "changed path", "changed paths"), count(len(sel.Seeds), "package", "packages"),
			len(sel.Packages), len(g.pkgs))
	}
	return sel, nil
}

// fullTrigger reports why a changed path selects everything on its own, if
// it does.
func (g *Graph) fullTrigger(p string) (string, bool) {
	if slices.Contains(FullTriggers, path.Base(p)) {
		return p + " changes the module graph the selection is computed from", true
	}
	if p == ManifestPath {
		return ManifestPath + " can change what every step runs", true
	}
	for root := range g.modules {
		vendor := "vendor/"
		if root != "." {
			vendor = root + "/vendor/"
		}
		if strings.HasPrefix(p, vendor) {
			return p + " is vendored code, which the import graph does not read", true
		}
	}
	return "", false
}

// GraphSelector adapts a graph and its selection to Compile's Selector.
func GraphSelector(g *Graph, s Selection) Selector {
	return graphSelector{g: g, sel: s}
}

type graphSelector struct {
	g   *Graph
	sel Selection
}

// All is every package of the graph, a fresh slice each call.
func (s graphSelector) All() []string { return s.g.importPaths() }

// Affected is the selection, copied so the caller cannot change it.
func (s graphSelector) Affected() Selection {
	out := s.sel
	out.Seeds = slices.Clone(s.sel.Seeds)
	out.Packages = slices.Clone(s.sel.Packages)
	return out
}

// DirOf is a package's directory, "" for one the graph does not hold.
func (s graphSelector) DirOf(importPath string) string { return s.g.dirOf(importPath) }

// firstAndCount names the first of a list and how many more follow.
func firstAndCount(list []string) string {
	if len(list) == 1 {
		return list[0]
	}
	return fmt.Sprintf("%s and %d more", list[0], len(list)-1)
}

// count is a number with its noun, singular or plural.
func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
