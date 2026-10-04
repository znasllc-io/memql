package pipelines

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
)

// Package is one Go package of a repository, as selection sees it.
type Package struct {
	ImportPath string
	Dir        string // repo-relative, "." for the root
	// Imports are the first-party import paths -- the repository's own, under
	// a module its tree declares -- sorted, deduplicated, test imports
	// included. One no package of the tree answers stays: it is a package a
	// change deleted or moved away, and this package is what that change
	// breaks.
	Imports []string
}

// Graph is a repository's Go import graph, read from source (decision 5 of
// the pipelines-seam plan). Build one with ScanGoTree.
//
// It is read from source, not from `go list`, because the driver holds a
// tarball, not a toolchain: a customer repository's packages are compiled by
// the step that tests them, in that step's image, and nowhere before. Source
// is enough for selection, because an import is a line in a file.
type Graph struct {
	pkgs    map[string]*Package // import path -> package
	byDir   map[string]string   // package directory -> import path
	modules map[string]string   // module root directory -> module path, "" when its go.mod names none
	// importers is import path -> the packages importing it, sorted. Its keys
	// include first-party import paths no package answers (Package.Imports).
	importers  map[string][]string
	incomplete []string
}

// ScanGoTree reads the import graph from source. Every go.mod names a
// module; a directory holding .go files is a package of the nearest module
// above it; every import any file declares -- any build tag, test files
// included -- is an edge. Directories named testdata or vendor, or starting
// with "." or "_", are skipped, as the go tool skips them. A file that does
// not parse marks the graph Incomplete rather than failing the scan.
//
// The union over build tags is a superset of what `go list` reports for any
// one build: an edge some build lacks makes a selection wider, never
// narrower, and too wide is the direction that costs minutes rather than a
// red merge. A .go file under no go.mod belongs to no package a module build
// can name, and is not one. Only an error reading fsys fails the scan.
//
// An edge is an import of the repository's own code: a path under a module
// the tree declares. One that resolves to no package directory is kept, not
// dropped -- at the head of a change that deletes package a, b still imports
// a, and b is exactly what that change breaks; Affected reaches b through it.
// Every other import (the standard library, a dependency) is no edge.
func ScanGoTree(fsys fs.FS) (*Graph, error) {
	modules := map[string]string{}
	goFiles := map[string][]string{} // directory -> its .go files
	var incomplete []string

	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if p != "." && skippedByTheGoTool(name) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		switch {
		case name == "go.mod":
			data, err := fs.ReadFile(fsys, p)
			if err != nil {
				return err
			}
			modPath, ok := modulePath(data)
			if !ok {
				// A module whose path cannot be read cannot name its
				// packages. It still owns its directory, recorded with no
				// path: read as part of the module above, its packages would
				// get names no build has.
				incomplete = append(incomplete, p)
			}
			modules[path.Dir(p)] = modPath
		case strings.HasSuffix(name, ".go") && !strings.HasPrefix(name, ".") && !strings.HasPrefix(name, "_"):
			// The go tool ignores a file whose name begins with "." or "_".
			goFiles[path.Dir(p)] = append(goFiles[path.Dir(p)], p)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reading the Go tree: %w", err)
	}

	g := &Graph{
		pkgs:      map[string]*Package{},
		byDir:     map[string]string{},
		modules:   modules,
		importers: map[string][]string{},
	}
	fset := token.NewFileSet()
	declared := map[string]map[string]bool{} // import path -> everything its files import
	for _, dir := range sortedMapKeys(goFiles) {
		importPath, ok := importPathOf(modules, dir)
		if !ok {
			continue // outside every module, or inside one whose path is unknown
		}
		if other, taken := g.pkgs[importPath]; taken {
			// Two directories come out as one import path (two modules
			// claiming one path). Neither can be trusted to be the one an
			// import means.
			incomplete = append(incomplete, other.Dir, dir)
			continue
		}
		g.pkgs[importPath] = &Package{ImportPath: importPath, Dir: dir}
		g.byDir[dir] = importPath

		imports := map[string]bool{}
		for _, file := range goFiles[dir] {
			src, err := fs.ReadFile(fsys, file)
			if err != nil {
				return nil, fmt.Errorf("reading %s: %w", file, err)
			}
			// ImportsOnly stops after the import block: what the graph
			// needs, and a file broken further down still has readable
			// imports. A file whose imports cannot be read is incomplete.
			f, err := parser.ParseFile(fset, file, src, parser.ImportsOnly)
			if err != nil {
				incomplete = append(incomplete, file)
				continue
			}
			for _, spec := range f.Imports {
				if imported, err := strconv.Unquote(spec.Path.Value); err == nil {
					imports[imported] = true
				}
			}
		}
		declared[importPath] = imports
	}

	for importPath, imports := range declared {
		var firstParty []string
		for imported := range imports {
			// Not g.pkgs[imported]: a first-party import no package answers
			// is the edge a deleted package leaves behind (ScanGoTree).
			if imported != importPath && isFirstParty(modules, imported) {
				firstParty = append(firstParty, imported)
			}
		}
		slices.Sort(firstParty)
		g.pkgs[importPath].Imports = firstParty
		for _, imported := range firstParty {
			g.importers[imported] = append(g.importers[imported], importPath)
		}
	}
	for _, list := range g.importers {
		slices.Sort(list)
	}
	slices.Sort(incomplete)
	g.incomplete = slices.Compact(incomplete)
	return g, nil
}

// Packages is every package, sorted by import path.
func (g *Graph) Packages() []Package {
	if g == nil {
		return nil
	}
	out := make([]Package, 0, len(g.pkgs))
	for _, importPath := range g.importPaths() {
		p := *g.pkgs[importPath]
		p.Imports = slices.Clone(p.Imports)
		out = append(out, p)
	}
	return out
}

// Incomplete is the paths the graph could not read -- a .go file that does
// not parse, a go.mod naming no module, a directory whose import path another
// already holds -- sorted. A graph with any cannot say what a change reaches.
func (g *Graph) Incomplete() []string {
	if g == nil {
		return nil
	}
	return slices.Clone(g.incomplete)
}

// PackageAt is the package owning a repository path: the package in the
// path's directory, or the nearest one above it. p is a FILE path; a
// directory path is read as a file in its parent.
//
// The walk stops at the path's module root, because a package owns a file
// only inside its own module: that is the boundary //go:embed cannot cross,
// so a file in a nested module with no package above it is owned by nothing,
// not by whatever package the parent module has at the top.
func (g *Graph) PackageAt(p string) (string, bool) {
	if g == nil {
		return "", false
	}
	p, ok := repoPath(p)
	if !ok {
		return "", false
	}
	for dir := path.Dir(p); ; dir = path.Dir(dir) {
		if importPath, ok := g.byDir[dir]; ok {
			return importPath, true
		}
		if _, isModuleRoot := g.modules[dir]; isModuleRoot || dir == "." {
			return "", false
		}
	}
}

// orphans answers for a repository path whose directory holds no package: the
// import path a package there would have, and the packages importing it all
// the same, sorted. That directory is where a package was until a change
// deleted or moved it, and those importers are what the change breaks. For a
// directory that holds a package, or one under no module, it answers nothing:
// a live package's importers follow from the package itself.
func (g *Graph) orphans(p string) (string, []string) {
	if g == nil {
		return "", nil
	}
	dir := path.Dir(p)
	if _, isPackage := g.byDir[dir]; isPackage {
		return "", nil
	}
	importPath, ok := importPathOf(g.modules, dir)
	if !ok {
		return "", nil
	}
	return importPath, g.importers[importPath]
}

// importPaths is every import path, sorted.
func (g *Graph) importPaths() []string {
	if g == nil {
		return nil
	}
	return sortedMapKeys(g.pkgs)
}

// dirOf is a package's directory, "" for one the graph does not hold.
func (g *Graph) dirOf(importPath string) string {
	if g == nil {
		return ""
	}
	if p := g.pkgs[importPath]; p != nil {
		return p.Dir
	}
	return ""
}

// skippedByTheGoTool reports whether the go tool ignores a directory: testdata
// and vendor, and any name beginning with "." or "_".
func skippedByTheGoTool(name string) bool {
	return name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

// importPathOf is the import path a package in dir has, or would have: the
// path of the module owning dir -- the nearest go.mod at or above it -- plus
// dir below that module's root. False when no module owns dir, or the one
// that does names no path.
func importPathOf(modules map[string]string, dir string) (string, bool) {
	root, ok := moduleRootOf(modules, dir)
	if !ok || modules[root] == "" {
		return "", false
	}
	importPath := modules[root]
	if dir != root {
		rel := dir
		if root != "." {
			rel = strings.TrimPrefix(dir, root+"/")
		}
		importPath += "/" + rel
	}
	return importPath, true
}

// isFirstParty reports whether an import path is the repository's own: a
// module path the tree declares, or a path below one on a "/" boundary, so
// example.test/probes is not under example.test/probe. Whether a package of
// the tree answers it is a separate question, and deliberately not this one.
func isFirstParty(modules map[string]string, importPath string) bool {
	for _, modPath := range modules {
		if modPath != "" && (importPath == modPath || strings.HasPrefix(importPath, modPath+"/")) {
			return true
		}
	}
	return false
}

// moduleRootOf is the root of the module owning dir: dir itself or the
// nearest directory above it holding a go.mod.
func moduleRootOf(modules map[string]string, dir string) (string, bool) {
	for {
		if _, ok := modules[dir]; ok {
			return dir, true
		}
		if dir == "." {
			return "", false
		}
		dir = path.Dir(dir)
	}
}

// modulePath reads the module directive of a go.mod: `module path`, the path
// optionally quoted, a trailing comment allowed.
func modulePath(gomod []byte) (string, bool) {
	for _, line := range strings.Split(string(gomod), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "module" {
			continue
		}
		modPath := fields[1]
		if unquoted, err := strconv.Unquote(modPath); err == nil {
			modPath = unquoted
		}
		if modPath == "" || strings.ContainsAny(modPath, "\"` ") {
			return "", false
		}
		return modPath, true
	}
	return "", false
}

// repoPath cleans a repository-relative path, and reports false for one that
// is not: absolute, above the root, or empty.
func repoPath(p string) (string, bool) {
	p = path.Clean(strings.TrimSpace(p))
	if p == "." || p == ".." || path.IsAbs(p) || strings.HasPrefix(p, "../") {
		return "", false
	}
	return p, true
}

func sortedMapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
