// Package selection decides which Go packages a CI run tests and how the two
// long lanes are sharded (epic memql#5476; design record
// docs/superpowers/specs/2026-09-16-pipelines-program-design.md, section 4,
// epic 1).
//
// It is pure over values. A Graph, a list of changed files, a class table and
// a timing table go in; a Plan comes out. The only functions that touch the
// machine are GoList and NodeTags, and they are what the CLI in
// scripts/ci/affected wires in. Every decision is tested on a recorded graph
// fixture (testdata/graph.json) with no toolchain in the loop, and one live
// test holds the loader to the real tree.
//
// # What the graph is a union OF
//
// A package's test binary depends on different packages under different build
// tags: app/build_agent.go imports integrations/agent only when `-tags agent`
// is set. CI tests the default build AND every node tag, so a change that
// reaches app only through an agent-tagged import still has to run app's
// tests. The graph is therefore the union over every configuration CI tests,
// which over-approximates each one. Over-approximating is the safe direction:
// a selection that is too wide costs minutes, one that is too narrow merges a
// red build.
package selection

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Package is one Go package of the workspace, as far as selection cares.
type Package struct {
	// ImportPath is the package's import path.
	ImportPath string `json:"importPath"`
	// Dir is the package directory, repo-relative and slash-separated. The
	// repository root is ".".
	Dir string `json:"dir"`
	// ModuleDir is the directory of the go.mod that owns the package,
	// repo-relative. The root module is ".".
	ModuleDir string `json:"moduleDir"`
	// Files are the repo-relative files some build of the package, or of its
	// tests, names: Go sources under every configuration CI tests, the files
	// the default build ignores, and every //go:embed match.
	Files []string `json:"files"`
	// TaggedFiles is the subset of Files that the default build does not
	// compile and some node-tag build does.
	TaggedFiles []string `json:"taggedFiles,omitempty"`
	// Deps are the first-party packages the package's TEST BINARY depends
	// on, transitively, under the union of every configuration. Transitive
	// is what lets the affected set be computed in one step.
	Deps []string `json:"deps"`
}

// Graph is the union package graph. Build one with NewGraph, ReadGraph or
// LoadGraph; the zero value is not usable.
type Graph struct {
	pkgs   map[string]*Package
	byDir  map[string]string   // dir -> import path
	owners map[string][]string // file -> import paths naming it
	rdeps  map[string][]string // import path -> packages whose Deps contain it
	tagged map[string]bool     // files only a node-tag build compiles
}

// NewGraph validates pkgs and indexes them. It refuses a graph it could
// answer wrongly from: a duplicate import path or directory, a package with
// no directory, or a dependency on a package the graph does not hold.
func NewGraph(pkgs []Package) (*Graph, error) {
	g := &Graph{
		pkgs:   make(map[string]*Package, len(pkgs)),
		byDir:  make(map[string]string, len(pkgs)),
		owners: map[string][]string{},
		rdeps:  map[string][]string{},
		tagged: map[string]bool{},
	}
	for i := range pkgs {
		p := pkgs[i]
		if p.ImportPath == "" || p.Dir == "" || p.ModuleDir == "" {
			return nil, fmt.Errorf("package %d (%q) is missing its import path, directory or module directory", i, p.ImportPath)
		}
		if _, dup := g.pkgs[p.ImportPath]; dup {
			return nil, fmt.Errorf("import path %s appears twice", p.ImportPath)
		}
		if other, dup := g.byDir[p.Dir]; dup {
			return nil, fmt.Errorf("directory %s holds both %s and %s", p.Dir, other, p.ImportPath)
		}
		g.pkgs[p.ImportPath] = &p
		g.byDir[p.Dir] = p.ImportPath
		for _, f := range p.Files {
			g.owners[f] = append(g.owners[f], p.ImportPath)
		}
		for _, f := range p.TaggedFiles {
			g.tagged[f] = true
		}
	}
	for ip, p := range g.pkgs {
		for _, d := range p.Deps {
			if d == ip {
				continue
			}
			if _, ok := g.pkgs[d]; !ok {
				return nil, fmt.Errorf("%s depends on %s, which the graph does not hold", ip, d)
			}
			g.rdeps[d] = append(g.rdeps[d], ip)
		}
	}
	for _, m := range []map[string][]string{g.owners, g.rdeps} {
		for k := range m {
			sort.Strings(m[k])
		}
	}
	return g, nil
}

// graphFile is the on-disk shape of a recorded graph.
type graphFile struct {
	Packages []Package `json:"packages"`
}

// ReadGraph reads a graph recorded by WriteJSON.
func ReadGraph(r io.Reader) (*Graph, error) {
	var f graphFile
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("reading a recorded graph: %w", err)
	}
	return NewGraph(f.Packages)
}

// WriteJSON records the graph, packages sorted by import path, so a fixture
// can be captured from a real tree and diffed.
func (g *Graph) WriteJSON(w io.Writer) error {
	f := graphFile{Packages: make([]Package, 0, len(g.pkgs))}
	for _, ip := range g.ImportPaths() {
		f.Packages = append(f.Packages, *g.pkgs[ip])
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(f)
}

// Len is the number of packages in the graph.
func (g *Graph) Len() int { return len(g.pkgs) }

// ImportPaths returns every import path, sorted.
func (g *Graph) ImportPaths() []string {
	out := make([]string, 0, len(g.pkgs))
	for ip := range g.pkgs {
		out = append(out, ip)
	}
	sort.Strings(out)
	return out
}

// Package returns the package with the given import path.
func (g *Graph) Package(importPath string) (Package, bool) {
	p, ok := g.pkgs[importPath]
	if !ok {
		return Package{}, false
	}
	return *p, true
}

// Modules returns every module directory that holds at least one package,
// sorted.
func (g *Graph) Modules() []string {
	seen := map[string]bool{}
	for _, p := range g.pkgs {
		seen[p.ModuleDir] = true
	}
	return sortedKeys(seen)
}

// nearest returns the package owning the deepest directory above file. The
// root is a package in this repository, so every repo-relative path has one;
// ok is false only for a graph without a root package.
func (g *Graph) nearest(file string) (string, bool) {
	d := path.Dir(file)
	for {
		if ip, ok := g.byDir[d]; ok {
			return ip, true
		}
		if d == "." {
			return "", false
		}
		d = path.Dir(d)
	}
}

// Expand resolves a `go test` style package pattern against the graph:
// "./" or "." is the root package ONLY, "./x/..." is x and everything under
// it, "./x" is exactly x. A pattern that matches nothing is an error rather
// than an empty answer, because an empty answer is how a selector quietly
// stops selecting. "./..." is refused: it is the whole workspace, which is
// not a selection.
func (g *Graph) Expand(pattern string) ([]string, error) {
	p := strings.TrimSpace(pattern)
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimSuffix(p, "/")
	var out []string
	switch {
	case p == "" || p == ".":
		if ip, ok := g.byDir["."]; ok {
			out = append(out, ip)
		}
	case p == "...":
		return nil, fmt.Errorf("pattern %q is the whole workspace, which is not a selection", pattern)
	case strings.HasSuffix(p, "/..."):
		base := strings.TrimSuffix(p, "/...")
		for dir, ip := range g.byDir {
			if dir == base || strings.HasPrefix(dir, base+"/") {
				out = append(out, ip)
			}
		}
	default:
		if ip, ok := g.byDir[p]; ok {
			out = append(out, ip)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("pattern %q matches no package in the graph", pattern)
	}
	sort.Strings(out)
	return out, nil
}

// ListFunc returns the JSON stream `go list -e -json <pattern>` prints for
// one build configuration. tags is "" for the default build.
type ListFunc func(ctx context.Context, tags string) ([]byte, error)

// GoList returns a ListFunc running the real toolchain in root.
func GoList(root, pattern string) ListFunc {
	return func(ctx context.Context, tags string) ([]byte, error) {
		args := []string{"list", "-e", "-json"}
		if tags != "" {
			args = append(args, "-tags", tags)
		}
		args = append(args, pattern)
		cmd := exec.CommandContext(ctx, "go", args...)
		cmd.Dir = root
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("go %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
		return out, nil
	}
}

// listedPackage is the part of `go list -json` output the loader reads.
type listedPackage struct {
	ImportPath string
	Dir        string
	Module     *struct {
		Path string
		Dir  string
	}
	GoFiles           []string
	CgoFiles          []string
	CFiles            []string
	CXXFiles          []string
	HFiles            []string
	SFiles            []string
	SysoFiles         []string
	IgnoredGoFiles    []string
	IgnoredOtherFiles []string
	TestGoFiles       []string
	XTestGoFiles      []string
	EmbedFiles        []string
	TestEmbedFiles    []string
	XTestEmbedFiles   []string
	Deps              []string
	TestImports       []string
	XTestImports      []string
	Error             *struct{ Err string }
	DepsErrors        []*struct{ Err string }
}

func decodeList(data []byte) ([]listedPackage, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	var out []listedPackage
	for {
		var p listedPackage
		err := dec.Decode(&p)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
}

// LoadGraph builds the union graph over the default build and every tag in
// tags, keeping only packages under prefix (the first-party import path).
//
// It refuses rather than guesses: a package `go list` could not load, a
// dependency it could not load, or a package that moves between directories
// across configurations is an error. A graph built over a broken load would
// answer confidently about a tree it did not see.
func LoadGraph(ctx context.Context, root, prefix string, tags []string, list ListFunc) (*Graph, error) {
	type acc struct {
		pkg   Package
		files map[string]bool
		deps  map[string]bool
	}
	byPath := map[string]*acc{}
	defaultCompiled := map[string]bool{}
	taggedCompiled := map[string]bool{}

	for _, tag := range append([]string{""}, tags...) {
		data, err := list(ctx, tag)
		if err != nil {
			return nil, err
		}
		listed, err := decodeList(data)
		if err != nil {
			return nil, fmt.Errorf("decoding go list output (tags %q): %w", tag, err)
		}
		if len(listed) == 0 {
			return nil, fmt.Errorf("go list (tags %q) listed no packages", tag)
		}
		index := make(map[string]*listedPackage, len(listed))
		for i := range listed {
			index[listed[i].ImportPath] = &listed[i]
		}
		for i := range listed {
			lp := &listed[i]
			if !firstParty(lp.ImportPath, prefix) {
				continue
			}
			if lp.Error != nil {
				return nil, fmt.Errorf("%s (tags %q): %s", lp.ImportPath, tag, lp.Error.Err)
			}
			for _, de := range lp.DepsErrors {
				return nil, fmt.Errorf("%s (tags %q): a dependency failed to load: %s", lp.ImportPath, tag, de.Err)
			}
			if lp.Module == nil {
				return nil, fmt.Errorf("%s (tags %q) belongs to no module", lp.ImportPath, tag)
			}
			dir, err := repoRel(root, lp.Dir)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", lp.ImportPath, err)
			}
			modDir, err := repoRel(root, lp.Module.Dir)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", lp.ImportPath, err)
			}
			a := byPath[lp.ImportPath]
			if a == nil {
				a = &acc{
					pkg:   Package{ImportPath: lp.ImportPath, Dir: dir, ModuleDir: modDir},
					files: map[string]bool{},
					deps:  map[string]bool{},
				}
				byPath[lp.ImportPath] = a
			} else if a.pkg.Dir != dir || a.pkg.ModuleDir != modDir {
				return nil, fmt.Errorf("%s is in %s (module %s) under one configuration and %s (module %s) under tags %q",
					lp.ImportPath, a.pkg.Dir, a.pkg.ModuleDir, dir, modDir, tag)
			}

			for _, f := range concat(lp.GoFiles, lp.CgoFiles, lp.TestGoFiles, lp.XTestGoFiles) {
				rf := joinRel(dir, f)
				a.files[rf] = true
				if tag == "" {
					defaultCompiled[rf] = true
				} else {
					taggedCompiled[rf] = true
				}
			}
			for _, f := range concat(lp.CFiles, lp.CXXFiles, lp.HFiles, lp.SFiles, lp.SysoFiles,
				lp.IgnoredGoFiles, lp.IgnoredOtherFiles, lp.EmbedFiles, lp.TestEmbedFiles, lp.XTestEmbedFiles) {
				a.files[joinRel(dir, f)] = true
			}

			add := func(ip string) {
				if ip != lp.ImportPath && firstParty(ip, prefix) {
					a.deps[ip] = true
				}
			}
			for _, d := range lp.Deps {
				add(d)
			}
			// A test import is linked into the test binary together with
			// everything IT depends on, so both join the package's deps.
			for _, ti := range concat(lp.TestImports, lp.XTestImports) {
				if !firstParty(ti, prefix) {
					continue
				}
				add(ti)
				if tp, ok := index[ti]; ok {
					for _, d := range tp.Deps {
						add(d)
					}
				}
			}
		}
	}

	pkgs := make([]Package, 0, len(byPath))
	for _, a := range byPath {
		a.pkg.Files = sortedKeys(a.files)
		var tagged []string
		for f := range a.files {
			if taggedCompiled[f] && !defaultCompiled[f] {
				tagged = append(tagged, f)
			}
		}
		sort.Strings(tagged)
		a.pkg.TaggedFiles = tagged
		a.pkg.Deps = sortedKeys(a.deps)
		pkgs = append(pkgs, a.pkg)
	}
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].ImportPath < pkgs[j].ImportPath })
	return NewGraph(pkgs)
}

// NodeTags returns the node build tags, read from app/build_<type>.go: the
// canonical node-type list that scripts/ci/node_type_lists_test.go holds every
// other copy to, so the graph cannot miss a tag the images are built with.
func NodeTags(root string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(root, "app", "build_*.go"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range matches {
		base := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), "build_"), ".go")
		if base == "default" || strings.HasSuffix(base, "_test") {
			continue
		}
		out = append(out, base)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no app/build_<type>.go under %s: the node-type layout changed", root)
	}
	sort.Strings(out)
	return out, nil
}

func firstParty(importPath, prefix string) bool {
	return importPath == prefix || strings.HasPrefix(importPath, prefix+"/")
}

// repoRel makes an absolute directory repo-relative and slash-separated, and
// refuses one outside the repository.
func repoRel(root, dir string) (string, error) {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return "", err
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("directory %s is outside the repository %s", dir, root)
	}
	return rel, nil
}

func joinRel(dir, file string) string {
	if dir == "." {
		return path.Clean(file)
	}
	return path.Clean(dir + "/" + file)
}

func concat(lists ...[]string) []string {
	var out []string
	for _, l := range lists {
		out = append(out, l...)
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
