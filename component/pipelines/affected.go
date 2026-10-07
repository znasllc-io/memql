package pipelines

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
)

// ManifestPath is the manifest a pipeline is declared in; its change selects
const ManifestPath = "memql-package.yaml"

// AnalyzeSelection reports facts a DSL policy uses to choose full or affected
// package coverage. Path normalization, glob matching and graph lookups are
// mechanics; no fallback decision is made here.
func AnalyzeSelection(g *Graph, changed []string, known bool, fullGlobs []string) (SelectionFacts, error) {
	if g == nil {
		return SelectionFacts{}, errors.New("there is no import graph to analyze")
	}
	facts, err := AnalyzeChangedPaths(changed, known, fullGlobs)
	if err != nil {
		return SelectionFacts{}, err
	}
	facts.GraphComplete = len(g.incomplete) == 0
	facts.Incomplete = g.Incomplete()
	facts.AllPackages = g.importPaths()
	seeds := map[string]bool{}
	missing := map[string]bool{} // import paths a changed directory would have, imported where no package is
	for i := range facts.Paths {
		pathFacts := &facts.Paths[i]
		if !pathFacts.RepositoryPath {
			continue
		}
		pathFacts.Vendored = g.isVendoredPath(pathFacts.Path)
		if importPath, ok := g.PackageAt(pathFacts.Path); ok {
			seeds[importPath] = true
		}
		if importPath, importers := g.orphans(pathFacts.Path); len(importers) > 0 {
			missing[importPath] = true
			for _, importer := range importers {
				seeds[importer] = true
			}
		}
	}
	facts.Seeds = sortedMapKeys(seeds)
	facts.Missing = sortedMapKeys(missing)
	return facts, nil
}

// AnalyzeChangedPaths reports facts needed by pipeline policy when a pipeline
// uses changed-path buckets but does not select Go packages. With no import
// graph to consult, any vendor path is conservatively treated as vendored.
// Callers that have a graph should use AnalyzeSelection for its exact module
// boundaries and package seeds.
func AnalyzeChangedPaths(changed []string, known bool, fullGlobs []string) (SelectionFacts, error) {
	var inFull func(string) bool
	if len(fullGlobs) > 0 {
		set, err := PathSet(fullGlobs)
		if err != nil {
			return SelectionFacts{}, fmt.Errorf("select.full: %w", err)
		}
		inFull = set
	}
	facts := SelectionFacts{
		Known: known, ChangedCount: len(changed), GraphComplete: true,
		Paths: make([]ChangedPathFacts, 0, len(changed)),
	}
	for _, raw := range changed {
		p, ok := repoPath(raw)
		if !ok {
			facts.Paths = append(facts.Paths, ChangedPathFacts{Path: raw})
			continue
		}
		pathFacts := ChangedPathFacts{
			Path: p, BaseName: path.Base(p), RepositoryPath: true,
			Vendored: pathHasSegment(p, "vendor"),
		}
		if inFull != nil {
			pathFacts.ConfiguredFull = inFull(p)
		}
		facts.Paths = append(facts.Paths, pathFacts)
	}
	return facts, nil
}

func pathHasSegment(p, segment string) bool {
	for _, part := range strings.Split(p, "/") {
		if part == segment {
			return true
		}
	}
	return false
}

// ResolveSelection applies the full-versus-affected decision made by the
// pinned pipeline DSL, then performs the import-graph traversal. It refuses
// an affected answer when the facts are unsafe to narrow, so a malformed or
// changed policy cannot silently skip required package work.
func ResolveSelection(g *Graph, facts SelectionFacts, decision SelectionDecision) (Selection, error) {
	if g == nil {
		return Selection{}, errors.New("there is no import graph to select from")
	}
	if !decision.Full {
		if !facts.Known || facts.ChangedCount == 0 || !facts.GraphComplete {
			return Selection{}, errors.New("pipeline DSL selected affected packages with an unknown change list or incomplete graph")
		}
		for _, p := range facts.Paths {
			if !p.RepositoryPath {
				return Selection{}, fmt.Errorf("pipeline DSL selected affected packages for non-repository path %q", clip(p.Path))
			}
		}
	}
	if decision.Full {
		reason := strings.TrimSpace(decision.Reason)
		if reason == "" {
			reason = "the pinned pipeline DSL selected full package coverage"
		}
		return Selection{Full: true, Reason: reason, Packages: slices.Clone(facts.AllPackages)}, nil
	}

	selected := map[string]bool{}
	queue := slices.Clone(facts.Seeds)
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		if selected[next] {
			continue
		}
		selected[next] = true
		queue = append(queue, g.importers[next]...)
	}
	sel := Selection{Seeds: slices.Clone(facts.Seeds), Packages: sortedMapKeys(selected)}
	paths := count(facts.ChangedCount, "changed path", "changed paths")
	if len(sel.Seeds) == 0 {
		sel.Reason = paths + ", none in a Go package"
		return sel, nil
	}
	verb := "seed"
	if facts.ChangedCount == 1 {
		verb = "seeds"
	}
	sel.Reason = fmt.Sprintf("%s %s %s; with their importers, %d of %d are selected",
		paths, verb, count(len(sel.Seeds), "package", "packages"), len(sel.Packages), len(facts.AllPackages))
	if len(facts.Missing) > 0 {
		it := "it"
		if len(facts.Missing) > 1 {
			it = "them"
		}
		sel.Reason += "; the change leaves no package at " + firstAndCount(facts.Missing) +
			", and the packages still importing " + it + " are seeds"
	}
	return sel, nil
}

// isVendoredPath reports whether p is under a vendor directory at a module
// root, one of the graph's structural facts. The DSL decides the coverage
// policy for such a path.
func (g *Graph) isVendoredPath(p string) bool {
	for root := range g.modules {
		vendor := "vendor/"
		if root != "." {
			vendor = root + "/vendor/"
		}
		if strings.HasPrefix(p, vendor) {
			return true
		}
	}
	return false
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
