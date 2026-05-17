package extract

import (
	"fmt"
	"go/types"
	"strings"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/callgraph/cha"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"

	"github.com/znasllc-io/memql/component/architecture/model"
)

// ExtractCalls runs static call-graph analysis over the workspace and
// emits EdgeCalls into the model. Foundation for the sequence-diagram
// renderer the cockpit consumes; useful on its own as the "what calls
// what" overlay during drill-down.
//
// Algorithm: Class Hierarchy Analysis (CHA) from
// golang.org/x/tools/go/callgraph/cha. CHA is sound (no false
// negatives), conservative (may report calls that won't actually
// happen at runtime), and fast (linear in program size). It's the
// right default for the static map: the cockpit's L4 view doesn't
// need runtime-exact precision, and the cheaper alternative (RTA /
// VTA) requires extra inputs we don't have at extract time.
//
// Only edges where BOTH caller and callee live in one of the
// workspace's modules are emitted; calls into stdlib / vendor are
// implicit "absent target" edges that the renderer filters out.
//
// Generic instantiations: this pass emits ONE edge per
// (caller-generic-decl, callee-generic-decl) pair, not per
// instantiation -- otherwise a generic function called with N type
// arguments would produce N edges that all mean the same thing on
// the diagram.
func ExtractCalls(m *model.Model, plans []ServicePlan) error {
	// Reload packages with the SSA-required mode set. We don't share
	// state with ExtractTypes because the SSA builder mutates the
	// programs it constructs and we want a clean run.
	var initial []*packages.Package
	for _, plan := range plans {
		cfg := &packages.Config{
			Mode: packages.NeedName |
				packages.NeedFiles |
				packages.NeedCompiledGoFiles |
				packages.NeedImports |
				packages.NeedDeps |
				packages.NeedTypes |
				packages.NeedTypesInfo |
				packages.NeedSyntax |
				packages.NeedModule,
			Dir:   plan.ModuleDir,
			Tests: false,
		}
		patterns := plan.Arch.Roots
		if len(patterns) == 0 {
			patterns = []string{"./..."}
		}
		pkgs, err := packages.Load(cfg, patterns...)
		if err != nil {
			return fmt.Errorf("load packages (calls) for %s: %w", plan.Arch.Service, err)
		}
		initial = append(initial, pkgs...)
	}
	if len(initial) == 0 {
		return nil
	}

	// Build SSA. InstantiateGenerics expands generic functions per
	// instantiation; we coalesce back to the generic decl when we
	// emit edges so the model stays one-edge-per-call-site.
	prog, _ := ssautil.Packages(initial, ssa.InstantiateGenerics)
	prog.Build()

	// CHA call graph.
	cg := cha.CallGraph(prog)
	cg.DeleteSyntheticNodes() // drop init / wrappers / etc. so the diagram talks about real code

	// owner test: is this package one of ours? Built once here so
	// the per-edge filter doesn't re-walk the plans list.
	moduleRoots := make([]string, 0, len(plans))
	for _, p := range plans {
		moduleRoots = append(moduleRoots, p.ModulePath)
	}
	isOurs := func(pkgPath string) bool {
		for _, root := range moduleRoots {
			if pkgPath == root || strings.HasPrefix(pkgPath, root+"/") {
				return true
			}
		}
		return false
	}

	// Dedupe: callgraph.GraphEdge can appear multiple times for
	// distinct call sites within a function. We collapse to one
	// EdgeCalls per (caller, callee) pair; the count goes into
	// Attrs["call_sites"] so renderers can weight the line.
	type key struct{ from, to model.ID }
	counts := map[key]int{}

	err := callgraph.GraphVisitEdges(cg, func(e *callgraph.Edge) error {
		caller, callerOk := ssaFuncToID(e.Caller.Func)
		callee, calleeOk := ssaFuncToID(e.Callee.Func)
		if !callerOk || !calleeOk {
			return nil
		}
		callerPkg := pkgPathFromSSA(e.Caller.Func)
		calleePkg := pkgPathFromSSA(e.Callee.Func)
		if !isOurs(callerPkg) || !isOurs(calleePkg) {
			return nil
		}
		if caller == callee {
			return nil // skip self-loops; renderer treats them as noise at L4
		}
		counts[key{caller, callee}]++
		return nil
	})
	if err != nil {
		return fmt.Errorf("callgraph walk: %w", err)
	}

	for k, c := range counts {
		m.Edges = append(m.Edges, model.Edge{
			From: k.from,
			To:   k.to,
			Kind: model.EdgeCalls,
			Attrs: map[string]string{
				"algorithm":  "cha",
				"call_sites": fmt.Sprintf("%d", c),
			},
		})
	}
	return nil
}

// ssaFuncToID converts an *ssa.Function to the matching model.ID.
// Generic instantiations are coalesced back to their declaring
// generic via fn.Origin(). Anonymous functions and synthetic
// wrappers (init, bound methods we can't address from the diagram)
// return ok=false and are skipped.
func ssaFuncToID(fn *ssa.Function) (model.ID, bool) {
	if fn == nil {
		return "", false
	}
	if fn.Synthetic != "" {
		// init wrappers, range-over-func wrappers, etc.
		return "", false
	}
	// Collapse generic instantiations: foo[int] -> foo.
	if origin := fn.Origin(); origin != nil && origin != fn {
		fn = origin
	}
	pkgPath := pkgPathFromSSA(fn)
	if pkgPath == "" {
		return "", false
	}
	name := fn.Name()
	if name == "" {
		return "", false
	}
	recv := fn.Signature.Recv()
	if recv == nil {
		return model.FuncID(pkgPath, name), true
	}
	recvType, ok := recvNameFromType(recv.Type())
	if !ok {
		return "", false
	}
	return model.MethodID(pkgPath, recvType, name), true
}

// pkgPathFromSSA returns the import path of the function's package.
// Returns "" for functions whose package can't be resolved (mainly
// builtins / lambda-encased closures).
func pkgPathFromSSA(fn *ssa.Function) string {
	if fn == nil || fn.Pkg == nil || fn.Pkg.Pkg == nil {
		return ""
	}
	return fn.Pkg.Pkg.Path()
}

// recvNameFromType unwraps pointer indirection and named type
// parameters to return the bare receiver type name. Mirrors the
// rule in observe_markers.go so call-graph edges line up with the
// Method nodes emitted by the structural pass.
func recvNameFromType(t types.Type) (string, bool) {
	for {
		switch x := t.(type) {
		case *types.Pointer:
			t = x.Elem()
		case *types.Named:
			obj := x.Obj()
			if obj == nil {
				return "", false
			}
			return obj.Name(), true
		default:
			return "", false
		}
	}
}
