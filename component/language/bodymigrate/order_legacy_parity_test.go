package bodymigrate

// order_legacy_parity_test.go -- the rewrite's copy of the retired
// compiler's step order, held to the compiler itself (epic memql#5370, task
// memql#5373).
//
// The rewrite reproduces the order the retired engine ran a body in, because
// the migration writes statements in that order. It cannot call the compiler
// to find out -- the flip deletes the sort, and a bundle repository runs this
// rewrite long after -- so it carries its own copy (legacyGraph,
// legacyFlatOrder). This test is what makes the copy trustworthy while the
// original still exists: for every automation and logic in the tree it
// compiles the body with the real compiler many times over (its tie order came
// from Go map iteration) and requires
//
//   - every order the compiler produced to respect the copy's dependency edges
//     (the copy invents no edge), and
//   - when the copy sees no tie, every order the compiler produced to equal the
//     copy's (the copy misses no edge and sorts the same way).
//
// DELETED WITH THE COMPILER'S SORT in the flip (Task 13 of the plan): after
// that there is nothing to hold the copy to, and its goldens carry it.

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/compiler"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/repowalk"
)

func TestLegacyOrderMatchesTheCompiler(t *testing.T) {
	roots := []string{"../../../dsl", "../../../examples", "../../../deploy/fleet/dsl"}
	checked := 0
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() && repowalk.SkipDir(d.Name()) {
				return filepath.SkipDir
			}
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".memql") || strings.Contains(p, "/_") {
				return nil
			}
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				t.Fatal(rerr)
			}
			checked += checkFileOrders(t, p, string(b))
			return nil
		})
	}
	// A reachable positive: the tree holds dozens of bodies with two or more
	// steps; a walk that found none is a walk that looked nowhere.
	if checked < 40 {
		t.Fatalf("checked %d bodies; the walk is not reaching the tree", checked)
	}
}

// checkFileOrders checks every legacy body in one file and returns how many it
// checked.
func checkFileOrders(t *testing.T, file, src string) int {
	t.Helper()
	checked := 0
	for _, c := range findConstructs(src) {
		if c.terse {
			continue
		}
		body := src[c.open+1 : c.close]
		if (c.kind == "automation" && !isLegacyAutomation(body)) || (c.kind == "logic" && !isLegacyLogic(body)) {
			continue
		}
		lb, err := readConstructBody(file, src, c)
		if err != nil {
			t.Errorf("%s: %s %s: the reader refused a body the tree loads: %v", file, c.kind, c.name, err)
			continue
		}
		steps := compiledStepIDs(t, file, src, c)
		if steps == nil {
			continue
		}
		nodes, deps := legacyGraph(lb.stmts)
		if len(nodes) != len(steps.source) {
			t.Errorf("%s: %s %s: the copy flattens to %d steps, the parser to %d (%v)",
				file, c.kind, c.name, len(nodes), len(steps.source), steps.source)
			continue
		}
		flat, ties := legacyFlatOrder(nodes, deps)
		seen := map[string]bool{}
		for run := 0; run < 24; run++ {
			order := steps.compile()
			key := strings.Join(order, ",")
			if seen[key] {
				continue
			}
			seen[key] = true
			pos := map[int]int{}
			for k, id := range order {
				pos[steps.index[id]] = k
			}
			for i, ds := range deps {
				for d := range ds {
					if pos[d] > pos[i] {
						t.Errorf("%s: %s %s: the compiler ran %s before %s, which the copy says it waits for",
							file, c.kind, c.name, steps.source[i], steps.source[d])
					}
				}
			}
			if len(ties) == 0 {
				var want []string
				for _, n := range flat {
					want = append(want, steps.source[n])
				}
				if key != strings.Join(want, ",") {
					t.Errorf("%s: %s %s: the compiler ran %v; the copy says %v", file, c.kind, c.name, order, want)
				}
			}
		}
		checked++
	}
	return checked
}

// compiledSteps is one body as the retired parser and compiler see it.
type compiledSteps struct {
	source  []string       // step ids in source order, without _return
	index   map[string]int // id -> source position
	compile func() []string
}

// compiledStepIDs parses one construct with the retired parser and returns a
// way to compile it -- as the LogicRunner compiled a logic, wrapped in an
// automation -- and read the step ids in the order the compiler emitted them.
func compiledStepIDs(t *testing.T, file, src string, c construct) *compiledSteps {
	t.Helper()
	// The struct-form rewriter and parser take a whole file; hand them the
	// construct alone, with the file's use lines, so an unrelated construct
	// in the file cannot break the parse.
	piece := src[c.start : c.close+1]
	norm, err := langparser.NormaliseAll(piece)
	if err != nil {
		t.Errorf("%s: %s %s: the retired rewriter refused it: %v", file, c.kind, c.name, err)
		return nil
	}
	pf, err := langparser.ParseFile(norm)
	if err != nil {
		t.Errorf("%s: %s %s: the retired parser refused it: %v", file, c.kind, c.name, err)
		return nil
	}
	var def *langparser.FunctionDef
	for _, d := range pf.Definitions {
		if fd, ok := d.(*langparser.FunctionDef); ok && fd.Name == c.name {
			def = fd
		}
	}
	if def == nil {
		t.Errorf("%s: %s %s: the parser returned no definition", file, c.kind, c.name)
		return nil
	}
	body, ok := def.Body.(*langparser.AutomationDef)
	if !ok {
		t.Errorf("%s: %s %s: body is %T", file, c.kind, c.name, def.Body)
		return nil
	}
	cs := &compiledSteps{index: map[string]int{}}
	for _, st := range body.Steps {
		if st.ID == "_return" {
			continue
		}
		cs.index[st.ID] = len(cs.source)
		cs.source = append(cs.source, st.ID)
	}
	fake := &langparser.FunctionDef{Name: c.name, Type: langparser.FunctionTypeAutomation, Body: body}
	cs.compile = func() []string {
		result, err := compiler.NewDefault().CompileFile(&langparser.File{Definitions: []langparser.Node{fake}})
		if err != nil || len(result.Automations) == 0 {
			t.Fatalf("%s: %s %s: compile: %v", file, c.kind, c.name, err)
		}
		var out []string
		steps, _ := result.Automations[0].JSON["steps"].([]map[string]any)
		for _, st := range steps {
			out = append(out, st["id"].(string))
		}
		return out
	}
	return cs
}
