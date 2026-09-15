package automations

// loop_tree_measure_test.go -- the static loop check measured over the trees
// this repository ships (memql#5381): the embedded dsl/, and each bundle
// beside it -- deploy/fleet/dsl and every examples/<pack>/dsl -- mounted over
// the embedded tree the way MEMQL_DSL_PATH mounts one at boot, so the check
// sees what a node that mounts it sees.
//
// Each tree boots offline, as memqllint's engine-parity pass does
// (component/memql/lint_parity.go): the concepts load, the engine's Init
// builds the function registry, and the automation loader walks the tree
// with it. Every problem and the check's coverage are printed; run it with
// -v. The check is report-only until the tree's cycles are fixed, so this
// test fails only when the measurement goes blind -- a walk that loads no
// automations, or a registry that resolves no call, would read as a tree
// with no cycles.

import (
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/component"
	"github.com/znasllc-io/memql/core/repowalk"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// bundleTrees is every DSL bundle the repository ships beside the engine
// tree, each as the tree the engine mounts it as: deploy/fleet/dsl, whose
// subdirectories are its domains, and every examples/**/dsl, a pack's tree
// mounted as one domain named for the pack. Nothing else compiles their
// automations -- memqllint does not, and the fleet's bundle test checks
// structure only -- which is how every fleet automation came to fail to
// compile unnoticed (memql#5367). Ported from the pre-flip step_order_test.go
// (deleted in the statements flip, memql#5372/commit 96cbd0360, along with
// the legacy step-order helpers that had no successor) since this is a plain
// filesystem-mounting helper with no dependency on the retired step grammar,
// and this file is its only caller left in the package.
func bundleTrees(t *testing.T) map[string]fs.FS {
	t.Helper()
	root := filepath.Join("..", "..")
	trees := map[string]fs.FS{"deploy/fleet/dsl": os.DirFS(filepath.Join(root, "deploy", "fleet", "dsl"))}
	examples := os.DirFS(filepath.Join(root, "examples"))
	err := fs.WalkDir(examples, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() || path == "." {
			return nil
		}
		if base := d.Name(); strings.HasPrefix(base, "_") || strings.HasPrefix(base, ".") || repowalk.SkipDir(base) {
			return fs.SkipDir
		}
		if d.Name() != "dsl" {
			return nil
		}
		sub, err := fs.Sub(examples, path)
		if err != nil {
			return err
		}
		pack := filepath.Base(filepath.Dir(path))
		trees["examples/"+path] = mountedAs(pack, sub)
		return fs.SkipDir
	})
	if err != nil {
		t.Fatalf("walk examples: %v", err)
	}
	return trees
}

// mountedFS is fsys as the single domain `domain/` of a tree.
type mountedFS struct {
	domain string
	fsys   fs.FS
}

func mountedAs(domain string, fsys fs.FS) fs.FS { return mountedFS{domain, fsys} }

func (m mountedFS) Open(name string) (fs.File, error) {
	if name == "." {
		return fs.FS(fstest.MapFS{m.domain: &fstest.MapFile{Mode: fs.ModeDir}}).Open(".")
	}
	if name == m.domain {
		return m.fsys.Open(".")
	}
	if rest, ok := strings.CutPrefix(name, m.domain+"/"); ok {
		return m.fsys.Open(rest)
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

// loopMeasurement is one tree's check.
type loopMeasurement struct {
	graph      *LoopGraph
	problems   []automationLoadProblem
	unresolved []string // "<automation> -> <call>", for the calls the registry does not hold
}

// measureLoops boots the embedded tree -- with bundle mounted over it, when
// given -- offline, and runs the loader's check over every automation the
// merged tree holds. Global state (the mounted domains, the concept registry)
// is restored before it returns.
func measureLoops(t *testing.T, bundle fs.FS) loopMeasurement {
	t.Helper()
	quiet := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if bundle != nil {
		_, skipped, unmount := memqldsl.MountOverlayDomains(quiet, bundle)
		if len(skipped) > 0 {
			t.Logf("  domains skipped for colliding with a core domain: %v", skipped)
		}
		defer func() {
			unmount()
			memorynodes.ReplaceAll(nil)
			_, _ = memql.LoadUnifiedConcepts(quiet)
		}()
	}
	if _, err := memql.LoadUnifiedConcepts(quiet); err != nil {
		t.Fatalf("load concepts: %v", err)
	}
	registry := memorynodes.DefaultRegistry()
	eng, err := memql.New(nil, (&component.Component{}).WithLoggerWriter(io.Discard))
	if err != nil {
		t.Fatalf("construct the engine: %v", err)
	}
	eng.Logger = quiet
	if err := eng.Init(registry); err != nil {
		t.Logf("  engine Init: %v", err)
	}
	if eng.Functions() == nil || len(eng.Functions().LookupIndex()) == 0 {
		t.Fatal("the engine loaded no functions: the measurement would see no writes")
	}

	l := NewLoader(LoaderOptions{Logger: quiet, Registry: registry, Functions: eng.Functions()})
	loaded, err := l.LoadFromTree(memqldsl.Tree())
	if err != nil {
		t.Logf("  load: %v", err)
	}
	g, problems := l.checkLoops(loaded)

	reg := NewFunctionSource(eng.Functions()).Registry()
	var unresolved []string
	for _, a := range loaded {
		for _, c := range collectStepFacts(a).calls {
			if _, ok := reg[c.name]; !ok {
				unresolved = append(unresolved, a.Name+" -> "+c.name)
			}
		}
	}
	sort.Strings(unresolved)
	return loopMeasurement{graph: g, problems: problems, unresolved: unresolved}
}

// logMeasurement prints a measurement, leaving out the problems seen already.
func logMeasurement(t *testing.T, label string, m loopMeasurement, seen map[string]bool) {
	t.Helper()
	c := m.graph.Coverage
	t.Logf("== %s: %d automations, %d edges (%d decided), %d cyclic components; calls: %d resolved, %d unresolved, %d opaque",
		label, c.Automations, len(m.graph.Edges), countDecided(m.graph.Edges), len(m.graph.Cycles), c.Resolved, c.Unresolved, c.Opaque)
	for _, u := range m.unresolved {
		t.Logf("  unresolved call: %s", u)
	}
	for i, cy := range m.graph.Cycles {
		state := "REFUSED"
		if len(cy.PermittedBy) > 0 {
			state = "permitted by " + strings.Join(cy.PermittedBy, ", ")
		}
		t.Logf("  cycle %d: {%s} %s", i, strings.Join(cy.Members, ", "), state)
		// A refusal prints one representative cycle; fixing a component
		// means refuting every edge inside it, so all of them are listed.
		member := map[string]bool{}
		for _, name := range cy.Members {
			member[name] = true
		}
		for _, e := range m.graph.Edges {
			if member[e.From] && member[e.To] {
				t.Logf("    edge %s -> %s (decided %v): %s", e.From, e.To, e.Decided, e.Reason)
			}
		}
	}
	fresh := 0
	for _, p := range m.problems {
		key := p.Path + "\x00" + p.Name + "\x00" + p.Err
		if seen[key] {
			continue
		}
		seen[key] = true
		fresh++
		t.Logf("  PROBLEM %s:%s [%s]\n%s", p.Path, p.Name, p.Phase, indent(p.Err))
	}
	t.Logf("  %d problem(s) (%d new to this tree)", len(m.problems), fresh)
	if len(m.problems) != 0 {
		t.Errorf("%s: shipped tree contains uncovered cycles", label)
	}
}

func countDecided(edges []GraphEdge) int {
	n := 0
	for _, e := range edges {
		if e.Decided {
			n++
		}
	}
	return n
}

func indent(s string) string {
	return "      " + strings.ReplaceAll(s, "\n", "\n      ")
}

func TestLoopCheckMeasureTheTrees(t *testing.T) {
	if testing.Short() {
		t.Skip("boots the engine offline once per tree")
	}
	seen := map[string]bool{}
	base := measureLoops(t, nil)
	logMeasurement(t, "dsl/ (embedded)", base, seen)
	// The tree ships about sixty automations whose steps call more than a
	// hundred constructs; far below that, the walk or the registry went
	// blind and a clean result would mean nothing.
	if c := base.graph.Coverage; c.Automations < 40 || c.Resolved < 60 || len(base.graph.Edges) == 0 {
		t.Fatalf("the embedded measurement went blind: %+v, %d edges", c, len(base.graph.Edges))
	}

	names := []string{}
	trees := bundleTrees(t)
	for name := range trees {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		m := measureLoops(t, trees[name])
		logMeasurement(t, name+" (mounted over dsl/)", m, seen)
		if m.graph.Coverage.Automations < base.graph.Coverage.Automations {
			t.Errorf("%s: the merged tree holds %d automations, fewer than the embedded tree's %d -- the mount went blind",
				name, m.graph.Coverage.Automations, base.graph.Coverage.Automations)
		}
	}
}
