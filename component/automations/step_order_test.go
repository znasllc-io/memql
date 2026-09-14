package automations

// step_order_test.go -- the order an automation's steps run in (memql#5367).
//
// The compiler orders the steps with a STABLE topological sort
// (compiler.topoSortSteps): every step runs after the steps it reads, and is
// otherwise in the order it was written. So when no step reads a step
// written after it, the steps run in source order exactly -- what the
// statement form of a body means (D12), and what the legacy runtime did.
//
// The sort it replaced was breadth-first, and after the flip that reversed
// two writes in forge's routeRequest: `advance` reads `steps.decide`, so it
// became ready one round after `persistRouted`, which reads nothing, and
// persistRouted ran first. The legacy build had run them in source order
// only because its reference scan missed that edge -- it credited a dotted
// path to its head, and `steps.decide.result`'s head is `steps`.
//
// The references checked here are read off the PREPARED automation -- the
// parsed nodes the runtime evaluates -- not off the compiler's own reference
// walk, so a dependency that walk misses is caught too: a step that reads a
// step running after it would read nothing.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/memql/baseloader"
	"github.com/znasllc-io/memql/core/repowalk"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// treeSlice is one automation slice of a DSL tree, with the file it was cut
// from as the author wrote it (for compileUnifiedSlice's positions).
type treeSlice struct {
	automationSlice
	path     string
	authored string
}

// treeAutomationSlices is every automation slice in tree's `<domain>/...`
// files, keyed by the origin the unified loader stamps: the same walk, the
// same per-file front end and terse lowering (loadFromTree). allFiles reads
// every .memql file; otherwise only the `automations.memql` files the loader
// reads.
func treeAutomationSlices(t *testing.T, tree fs.FS, allFiles bool) map[string]treeSlice {
	t.Helper()
	lines, _ := memql.ResolveLanguageLines(tree)
	out := map[string]treeSlice{}
	err := fs.WalkDir(tree, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if base := d.Name(); base != "." && (strings.HasPrefix(base, "_") || strings.HasPrefix(base, ".")) {
				return fs.SkipDir
			}
			return nil
		}
		wanted := strings.HasSuffix(path, "/automations.memql") || (allFiles && strings.HasSuffix(path, ".memql"))
		if !wanted || memqldsl.SkipsBehavioralLoad(path) {
			return nil
		}
		data, err := fs.ReadFile(tree, path)
		if err != nil {
			return err
		}
		data, err = lines.Prepare(path, data)
		if errors.Is(err, languageParser.ErrLanguageLineRefused) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		lowered, err := languageParser.NormaliseTerseAutomationSource(string(data))
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		for _, slice := range extractAutomationSlices(lowered) {
			out["unified:"+path+":"+slice.Name] = treeSlice{automationSlice: slice, path: path, authored: string(data)}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the tree: %v", err)
	}
	return out
}

// bundleTrees is every DSL bundle the repository ships beside the engine
// tree, each as the tree the engine mounts it as: deploy/fleet/dsl, whose
// subdirectories are its domains, and every examples/**/dsl, a pack's tree
// mounted as one domain named for the pack. Nothing else compiles their
// automations -- memqllint does not, and the fleet's bundle test checks
// structure only -- which is how every fleet automation came to fail to
// compile unnoticed (memql#5367).
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

// mountedAs is fsys as the single domain `domain/` of a tree.
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

// sourceStepOrder is the order the steps of an automation slice are written
// in: the parsed body the compiler is handed (parseAutomationFile, after the
// precondition blocks are stripped as compileMemQL strips them).
func sourceStepOrder(t *testing.T, source string) []string {
	t.Helper()
	_, stripped, err := extractPreconditions(source)
	if err != nil {
		t.Fatalf("preconditions: %v", err)
	}
	file, _, err := parseAutomationFile(source, stripped)
	if err != nil || file == nil {
		t.Fatalf("parse: %v", err)
	}
	for _, d := range file.Definitions {
		fd, ok := d.(*languageParser.FunctionDef)
		if !ok {
			continue
		}
		body, ok := fd.Body.(*languageParser.AutomationDef)
		if !ok {
			continue
		}
		var ids []string
		for i := range body.Steps {
			if body.Steps[i].ID != "_return" {
				ids = append(ids, body.Steps[i].ID)
			}
		}
		return ids
	}
	t.Fatalf("no automation in the slice")
	return nil
}

// stepReads is, per top-level step, the top-level steps it reads: a free
// name that is a step id, or `steps.<id>`, in any expression the step's
// prepared nodes hold -- its condition, its expression fields, its argument
// and payload leaves, and those of the steps it contains (a forEach body
// with its loop variable and `index` bound, parallel branches, switch
// cases), since a container runs them as part of itself.
func stepReads(a *Automation) map[string]map[string]bool {
	ids := map[string]bool{}
	for _, s := range a.Steps {
		ids[s.ID] = true
	}
	out := map[string]map[string]bool{}
	for _, s := range a.Steps {
		reads := map[string]bool{}
		collectStepReads(s, nil, ids, reads)
		delete(reads, s.ID)
		out[s.ID] = reads
	}
	return out
}

func collectStepReads(s *Step, bound map[string]bool, ids, reads map[string]bool) {
	if s == nil {
		return
	}
	node := func(n ast.ExpressionNode, b map[string]bool) { nodeStepReads(n, b, ids, reads) }
	if x := s.Exprs; x != nil {
		for _, n := range []ast.ExpressionNode{x.Condition, x.Query, x.Source, x.Subject, x.ID, x.Parent, x.AliasOf, x.URL, x.Topic, x.CardType, x.PartitionID, x.ConceptRef} {
			node(n, bound)
		}
		for _, n := range x.Headers {
			node(n, bound)
		}
	}
	for _, m := range []map[string]any{functionArgs(s), actionArgs(s), subAutomationArgs(s), mutationPayload(s), webhookBody(s), eventPayload(s), cardData(s)} {
		valueStepReads(m, bound, ids, reads)
	}
	switch {
	case s.ForEach != nil:
		inner := map[string]bool{"index": true}
		for k := range bound {
			inner[k] = true
		}
		as := s.ForEach.As
		if as == "" {
			as = "item"
		}
		inner[as] = true
		if s.Exprs != nil {
			node(s.Exprs.Filter, inner)
		}
		for _, c := range s.ForEach.Do {
			collectStepReads(c, inner, ids, reads)
		}
	case s.Parallel != nil:
		for _, c := range s.Parallel.Branches {
			collectStepReads(c, bound, ids, reads)
		}
	case s.Switch != nil:
		for _, sc := range s.Switch.Cases {
			for _, c := range caseSteps(sc) {
				collectStepReads(c, bound, ids, reads)
			}
		}
		for _, c := range caseSteps(s.Switch.Default) {
			collectStepReads(c, bound, ids, reads)
		}
	}
}

func valueStepReads(v any, bound, ids, reads map[string]bool) {
	switch x := v.(type) {
	case *ExprLeaf:
		nodeStepReads(x.Node, bound, ids, reads)
	case map[string]any:
		for _, el := range x {
			valueStepReads(el, bound, ids, reads)
		}
	case []any:
		for _, el := range x {
			valueStepReads(el, bound, ids, reads)
		}
	}
}

func nodeStepReads(n ast.ExpressionNode, bound, ids, reads map[string]bool) {
	ast.WalkV1(n, func(e ast.ExpressionNode) bool {
		switch x := e.(type) {
		case *ast.IdentExpr:
			if ids[x.Name] && !bound[x.Name] {
				reads[x.Name] = true
			}
		case *ast.MemberExpr:
			if root, ok := x.Object.(*ast.IdentExpr); ok && root.Name == "steps" && !bound["steps"] {
				if ids[x.Field] {
					reads[x.Field] = true
				}
				return false
			}
		case *ast.LambdaExpr:
			inner := map[string]bool{}
			for k := range bound {
				inner[k] = true
			}
			for _, p := range x.Params {
				inner[p] = true
			}
			nodeStepReads(x.Body, inner, ids, reads)
			return false
		}
		return true
	})
}

func functionArgs(s *Step) map[string]any {
	if s.Function != nil {
		return s.Function.Args
	}
	return nil
}

func actionArgs(s *Step) map[string]any {
	if s.Action != nil {
		return s.Action.Args
	}
	return nil
}

func subAutomationArgs(s *Step) map[string]any {
	if s.Automation != nil {
		return s.Automation.Args
	}
	return nil
}

func mutationPayload(s *Step) map[string]any {
	if s.Mutation != nil {
		return s.Mutation.Payload
	}
	return nil
}

func webhookBody(s *Step) map[string]any {
	if s.Webhook != nil {
		return s.Webhook.Body
	}
	return nil
}

func eventPayload(s *Step) map[string]any {
	if s.Event != nil {
		return s.Event.Payload
	}
	return nil
}

func cardData(s *Step) map[string]any {
	if s.EmitConceptCard != nil {
		return s.EmitConceptCard.Data
	}
	return nil
}

// stepOrderProblems checks a compiled automation's step order against the
// order its steps were written in:
//
//   - every step runs after every step it reads (a step that reads one
//     running after it reads nothing -- a dependency the sort missed);
//   - when no step reads a step written after it, the steps run in source
//     order exactly.
//
// forward reports whether some step reads a step written after it, the one
// case in which the order may differ from the source.
func stepOrderProblems(a *Automation, source []string) (problems []string, forward bool) {
	ran := make([]string, 0, len(a.Steps))
	runAt := map[string]int{}
	for i, s := range a.Steps {
		ran = append(ran, s.ID)
		runAt[s.ID] = i
	}
	writtenAt := map[string]int{}
	for i, id := range source {
		writtenAt[id] = i
	}
	reads := stepReads(a)
	for _, id := range ran {
		for r := range reads[id] {
			if runAt[r] > runAt[id] {
				problems = append(problems, fmt.Sprintf("%s: step %q reads step %q, which runs after it", a.Name, id, r))
			}
			if writtenAt[r] > writtenAt[id] {
				forward = true
			}
		}
	}
	if !forward && strings.Join(ran, ",") != strings.Join(source, ",") {
		problems = append(problems, fmt.Sprintf("%s: no step reads a step written after it, so the steps run in source order -- ran %v, written %v", a.Name, ran, source))
	}
	return problems, forward
}

// TestAutomationStepsRunInSourceOrder is the gate over the whole tree and the
// bundles shipped beside it: every automation compiles, and its steps run
// after the steps they read, and in source order whenever no step reads a
// later one. forge's routeRequest is named specifically: its `advance` switch
// runs before `persistRouted`. So is the fleet's provisionInstanceOnCreate:
// its two switch steps on one subject, `render` and `settle`, carry their
// authors' names and run in the order written.
func TestAutomationStepsRunInSourceOrder(t *testing.T) {
	loader := NewLoader(LoaderOptions{})
	loaded, err := loader.LoadFromUnifiedTree()
	if err != nil {
		t.Fatalf("load the tree: %v", err)
	}
	slices := treeAutomationSlices(t, memqldsl.Tree(), false)
	var problems, forwardRefs []string
	multiStep := 0
	check := func(a *Automation, slice treeSlice) {
		p, forward := stepOrderProblems(a, sourceStepOrder(t, slice.Source))
		problems = append(problems, p...)
		if forward {
			forwardRefs = append(forwardRefs, a.Name)
		}
		if len(a.Steps) > 1 {
			multiStep++
		}
	}
	byName := map[string]*Automation{}
	for _, a := range loaded {
		slice, ok := slices[a.Origin]
		if !ok {
			t.Fatalf("%s: the loader compiled an automation the walk here does not see -- the two walks differ", a.Origin)
		}
		check(a, slice)
		byName[a.Name] = a
	}
	// The tree ships 58 automations, 18 of them more than one step long; a
	// count far below that means the walk went blind, and a clean result
	// would mean nothing.
	if len(loaded) < 40 || multiStep < 12 {
		t.Fatalf("checked %d automations (%d with more than one step) -- the tree walk is broken, not the tree", len(loaded), multiStep)
	}

	// The bundles: the loader's own walk over each (every automations.memql
	// file, strictly), and then every automation any of their files
	// declares, compiled one by one.
	bundles := 0
	for name, tree := range bundleTrees(t) {
		if _, err := loader.LoadFromTree(tree); err != nil {
			t.Errorf("%s: load: %v", name, err)
		}
		for origin, slice := range treeAutomationSlices(t, tree, true) {
			a, err := loader.compileUnifiedSlice(slice.authored, slice.automationSlice, origin)
			if err != nil {
				t.Errorf("%s: compile: %v", origin, err)
				continue
			}
			check(a, slice)
			byName[a.Name] = a
			bundles++
		}
	}
	// deploy/fleet/dsl declares 16 automations and the example packs 2; the
	// fleet's are the ones that broke.
	if bundles < 12 {
		t.Fatalf("compiled %d bundle automations -- the bundle walk went blind", bundles)
	}
	if len(problems) > 0 {
		t.Errorf("%d step-order problem(s):\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	t.Logf("%d tree and %d bundle automations checked, %d with more than one step; steps reading a later step (so reordered): %v", len(loaded), bundles, multiStep, forwardRefs)

	routeRequest := byName["routeRequest"]
	if routeRequest == nil {
		t.Fatal("forge's routeRequest is not in the tree")
	}
	if got := stepIDs(routeRequest); got != "decide,advance,persistRouted" {
		t.Errorf("routeRequest runs %s, want decide,advance,persistRouted: the `advance` switch carries its author's name and runs before persistRouted, as written", got)
	}

	provision := byName["provisionInstanceOnCreate"]
	if provision == nil {
		t.Fatal("the fleet's provisionInstanceOnCreate was not compiled")
	}
	if got := stepIDs(provision); got != "command,render,settle" {
		t.Errorf("provisionInstanceOnCreate runs %s, want command,render,settle: two switch steps on one subject, each with its author's name, in the order written", got)
	}
}

// stepIDs is an automation's step ids in the order they run.
func stepIDs(a *Automation) string {
	ids := make([]string, 0, len(a.Steps))
	for _, s := range a.Steps {
		ids = append(ids, s.ID)
	}
	return strings.Join(ids, ",")
}

// TestLogicBodyStepsRunInSourceOrder is the same gate over every logic body of
// the tree and of the bundles beside it that runs on the LogicRunner: a logic
// body is compiled to an automation and ordered by the same sort, and its
// `name := call` statements are the statement form whose meaning is the order
// they are written in.
func TestLogicBodyStepsRunInSourceOrder(t *testing.T) {
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("load concepts: %v", err)
	}
	runner := NewLogicRunner(nil, nil, nil)
	var problems []string
	checked, multiStep := 0, 0
	for _, f := range baseloader.ReadAll(nil) {
		for _, slice := range memql.ExtractFunctionSlices(f.Content) {
			if slice.Kind != languageParser.FunctionTypeLogic {
				continue
			}
			fn, err := memql.BuildFunctionConstruct(f.Content, slice.Name, "unified:"+f.Path, memorynodes.DefaultRegistry())
			if err != nil {
				t.Fatalf("%s %s: %v", f.Path, slice.Name, err)
			}
			if fn.LogicSteps == nil {
				continue // a return of a construct call dispatches through fn.Expr: one call, no order
			}
			a, err := runner.compileBodyToAutomation(slice.Name, fn.LogicSteps)
			if err != nil {
				t.Fatalf("%s %s: compile: %v", f.Path, slice.Name, err)
			}
			var source []string
			for i := range fn.LogicSteps.Steps {
				if id := fn.LogicSteps.Steps[i].ID; id != "_return" {
					source = append(source, id)
				}
			}
			// The stitched `_return` runs last by construction; the order under
			// test is the statements'.
			b := *a
			b.Steps = nil
			for _, st := range a.Steps {
				if st.ID != "_return" {
					b.Steps = append(b.Steps, st)
				}
			}
			p, _ := stepOrderProblems(&b, source)
			problems = append(problems, p...)
			checked++
			if len(source) > 1 {
				multiStep++
			}
		}
	}
	if checked < 25 || multiStep < 5 {
		t.Fatalf("checked %d logic bodies (%d with more than one statement) -- the walk went blind", checked, multiStep)
	}

	// The bundles' logic bodies, read the way the tree's are.
	bundleBodies := 0
	for name, tree := range bundleTrees(t) {
		lines, _ := memql.ResolveLanguageLines(tree)
		err := fs.WalkDir(tree, ".", func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil || d.IsDir() || !strings.HasSuffix(path, ".memql") {
				return walkErr
			}
			data, err := fs.ReadFile(tree, path)
			if err != nil {
				return err
			}
			if data, err = lines.Prepare(path, data); err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			for _, slice := range memql.ExtractFunctionSlices(string(data)) {
				if slice.Kind != languageParser.FunctionTypeLogic {
					continue
				}
				fn, err := memql.BuildFunctionConstruct(string(data), slice.Name, "unified:"+path, memorynodes.DefaultRegistry())
				if err != nil {
					t.Errorf("%s: %s %s: %v", name, path, slice.Name, err)
					continue
				}
				if fn.LogicSteps == nil {
					continue
				}
				a, err := runner.compileBodyToAutomation(slice.Name, fn.LogicSteps)
				if err != nil {
					t.Errorf("%s: %s %s: compile: %v", name, path, slice.Name, err)
					continue
				}
				var source []string
				for i := range fn.LogicSteps.Steps {
					if id := fn.LogicSteps.Steps[i].ID; id != "_return" {
						source = append(source, id)
					}
				}
				b := *a
				b.Steps = nil
				for _, st := range a.Steps {
					if st.ID != "_return" {
						b.Steps = append(b.Steps, st)
					}
				}
				p, _ := stepOrderProblems(&b, source)
				problems = append(problems, p...)
				bundleBodies++
			}
			return nil
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// deploy/fleet/dsl and the example packs ship 9 that run on the runner.
	if bundleBodies < 5 {
		t.Fatalf("checked %d bundle logic bodies -- the bundle walk went blind", bundleBodies)
	}
	if len(problems) > 0 {
		t.Errorf("%d step-order problem(s) in logic bodies:\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	t.Logf("%d tree and %d bundle logic bodies checked, %d tree bodies with more than one statement", checked, bundleBodies, multiStep)
}

// TestStepOrderProblemsCatchesAMisorder is the gate's negative control: the
// routeRequest shape, compiled for real, is clean; the breadth-first order
// that shipped with the flip is reported as out of source order; and an
// order that runs a step before the step it reads is reported as reading a
// step that runs after it. A forward reference is not a misorder.
func TestStepOrderProblemsCatchesAMisorder(t *testing.T) {
	const src = `@trigger(event="node.created", concept="v1:forge:request", partition="*")
automation routeProbe {
  args {
    id any
    submitterRole any
  }

  step decide {
    logic requestRouteStatus ( submitterRole )
  }
  step advance {
    switch steps.decide.result {
      case "queued" {
        mutation advanceRequest ( requestId: id, status: "queued" )
      }
      default {
        mutation advanceRequest ( requestId: id, status: steps.decide.result )
      }
    }
  }
  step persistRouted {
    mutation recordRequestEvent ( requestId: id, kind: "routed" )
  }
}`
	// A logic body -- the statement form -- is ordered by the same sort. `c`
	// reads nothing, so a breadth-first sort ran it ahead of `b`, which reads
	// `a`; written in dependency order, the statements run as written.
	body := parseLogicBody(t, `@description("order probe")
logic orderProbe {
  args {
    id string @required
  }
  body {
    a := query thingById( id: args.id )
    b := query thingById( id: a.first().id )
    c := query thingById( id: args.id )
    return c
  }
}
`)
	la, err := NewLogicRunner(nil, nil, nil).compileBodyToAutomation("orderProbe", body)
	if err != nil {
		t.Fatalf("compile the logic body: %v", err)
	}
	var ran []string
	for _, st := range la.Steps {
		ran = append(ran, st.ID)
	}
	if got := strings.Join(ran, ","); got != "a,b,c,_return" {
		t.Errorf("the logic body ran %s, want a,b,c,_return: its statements in the order written", got)
	}

	a, err := NewLoader(LoaderOptions{}).CompileSource(src, "test:routeProbe")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	source := sourceStepOrder(t, src)
	if problems, forward := stepOrderProblems(a, source); len(problems) != 0 || forward {
		t.Fatalf("the compiled order is clean, got %v (forward=%v)", problems, forward)
	}
	if got := []string{a.Steps[0].ID, a.Steps[1].ID, a.Steps[2].ID}; got[2] != "persistRouted" {
		t.Fatalf("compiled %v: persistRouted must run last, as written", got)
	}

	reorder := func(ids ...string) *Automation {
		byID := map[string]*Step{}
		for _, s := range a.Steps {
			byID[s.ID] = s
		}
		b := *a
		b.Steps = nil
		for _, id := range ids {
			b.Steps = append(b.Steps, byID[id])
		}
		return &b
	}
	advance := source[1]

	// The breadth-first order: persistRouted, ready first, ahead of advance.
	if problems, _ := stepOrderProblems(reorder("decide", "persistRouted", advance), source); len(problems) != 1 || !strings.Contains(problems[0], "run in source order") {
		t.Errorf("the breadth-first order was not reported as out of source order: %v", problems)
	}
	// A step ahead of the step it reads.
	if problems, _ := stepOrderProblems(reorder(advance, "decide", "persistRouted"), source); len(problems) == 0 || !strings.Contains(problems[0], `reads step "decide", which runs after it`) {
		t.Errorf("a step running before the step it reads was not reported: %v", problems)
	}

	// A forward reference -- a step reading one written after it -- is the
	// order moving by necessity, not a problem.
	fwd, err := NewLoader(LoaderOptions{}).CompileSource(`@trigger(event="node.created", concept="v1:forge:request", partition="*")
automation forwardProbe {
  args {
    id any
  }

  step first {
    mutation advanceRequest ( requestId: id, status: steps.second.result )
  }
  step second {
    logic requestRouteStatus ( id )
  }
}`, "test:forwardProbe")
	if err != nil {
		t.Fatalf("compile the forward reference: %v", err)
	}
	if problems, forward := stepOrderProblems(fwd, []string{"first", "second"}); len(problems) != 0 || !forward {
		t.Errorf("a forward reference: problems %v, forward=%v; want none, and forward", problems, forward)
	}
	if fwd.Steps[0].ID != "second" {
		t.Errorf("the provider of a forward reference runs first: %v", []string{fwd.Steps[0].ID, fwd.Steps[1].ID})
	}
}

// TestTwoSwitchStepsOnOneSubject is the fleet's shape: two switch steps on one
// subject. Each step's id is its author's name, and the steps run in the
// order written. The parser used to give a bare switch statement an id made
// from its subject, so both were `switch_steps.command.result`; the sort,
// keyed by id, emitted one and counted both, and every fleet automation
// failed to compile with "dependency cycle among steps []" (memql#5367).
//
// Two steps that do share a name are refused, naming both and their lines: an
// id names a step's result and its journal record, so the second would
// overwrite the first.
func TestTwoSwitchStepsOnOneSubject(t *testing.T) {
	const src = `@trigger(event="node.created", concept="v1:forge:request", partition="*")
automation twoSwitches {
  args {
    id any
    submitterRole any
  }

  step command {
    logic requestRouteStatus ( submitterRole )
  }
  step render {
    switch steps.command.result {
      case "queued" {
        mutation advanceRequest ( requestId: id, status: "queued" )
      }
      default { }
    }
  }
  step settle {
    switch steps.command.result {
      case "queued" {
        mutation recordRequestEvent ( requestId: id, kind: "routed" )
      }
      default { }
    }
  }
}`
	a, err := NewLoader(LoaderOptions{}).CompileSource(src, "test:twoSwitches")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if got := stepIDs(a); got != "command,render,settle" {
		t.Fatalf("ran %s, want command,render,settle: each switch step carries its author's name, in the order written", got)
	}
	for _, s := range a.Steps[1:] {
		if s.Type != StepTypeSwitch || s.Switch == nil {
			t.Errorf("step %q is a %q step, want a switch", s.ID, s.Type)
		}
	}
	if problems, forward := stepOrderProblems(a, sourceStepOrder(t, src)); len(problems) != 0 || forward {
		t.Errorf("problems %v, forward=%v; want none", problems, forward)
	}

	// The same automation with both switch steps named `render`.
	dup := strings.Replace(src, "step settle {", "step render {", 1)
	_, err = NewLoader(LoaderOptions{}).CompileSource(dup, "test:twoSwitches")
	if err == nil {
		t.Fatal("two steps named render compiled")
	}
	for _, want := range []string{`two steps have the id "render"`, "the switch step at line 11", "the switch step at line 19"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
}
