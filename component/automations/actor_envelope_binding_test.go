package automations

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
)

// memql#2801: an UNBOUND actor root is fail-open, so every evaluator
// that can reach an `actor.*` read must bind the denying envelope.
//
// The evaluator renders an unresolved dotted path as its own path TEXT,
// so with `actor` unbound `actor.isClusterOwner` evaluates to the
// non-empty string "actor.isClusterOwner" -- truthy. A negated gate
// (`actor.isClusterOwner != false`) therefore read TRUE on a request
// with no auth context, which is the same fail-open the envelope's nil
// default was fixed for.
//
// This is the coverage the first attempt at that fix shipped without:
// reverting the binding left the whole suite green.
func TestBindActorEnvelope_UnboundActorIsFailOpen(t *testing.T) {
	// Baseline: what an UNBOUND actor root does. This is not asserting
	// desired behaviour -- it documents why binding is mandatory.
	bare := NewEvaluator()
	got, err := bare.EvaluateCondition("actor.isClusterOwner != false")
	if err != nil {
		t.Fatalf("unbound evaluate: %v", err)
	}
	if !got {
		t.Skip("unbound path no longer renders as literal text; the fail-open premise changed -- revisit memql#2801")
	}

	// Bound: the denying envelope closes it.
	bound := NewEvaluator()
	bindActorEnvelope(context.Background(), bound)
	got, err = bound.EvaluateCondition("actor.isClusterOwner != false")
	if err != nil {
		t.Fatalf("bound evaluate: %v", err)
	}
	if got {
		t.Error("`actor.isClusterOwner != false` is TRUE with no auth context -- the admin gate is fail-open (memql#2801)")
	}

	// The positive form must deny too.
	got, err = bound.EvaluateCondition("actor.isClusterOwner == true")
	if err != nil {
		t.Fatalf("bound evaluate ==: %v", err)
	}
	if got {
		t.Error("`actor.isClusterOwner == true` must be false with no auth context")
	}
}

// A real owner must still pass, or the fix is a denial-of-service on the
// admin surface rather than a gate.
func TestBindActorEnvelope_RealOwnerStillPasses(t *testing.T) {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "u1", Role: auth.RoleOwner,
	})
	ev := NewEvaluator()
	bindActorEnvelope(ctx, ev)

	got, err := ev.EvaluateCondition("actor.isClusterOwner == true")
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !got {
		t.Error("a real cluster owner must pass the gate")
	}
}

// The no-caller spelling (event triggers, scheduler ticks) must deny
// identically -- it is the same envelope, stated explicitly.
func TestBindNoCallerActorEnvelope_Denies(t *testing.T) {
	ev := NewEvaluator()
	bindNoCallerActorEnvelope(ev)
	for _, cond := range []string{
		"actor.isClusterOwner != false",
		"actor.isClusterOwner == true",
	} {
		got, err := ev.EvaluateCondition(cond)
		if err != nil {
			t.Fatalf("%s: %v", cond, err)
		}
		if got {
			t.Errorf("%s must be false for a trigger with no caller (memql#2801)", cond)
		}
	}
}

// The structural invariant, and the reason it exists: four review rounds
// on memql#2801 each found ANOTHER evaluator that left `actor` unbound,
// and then a fifth found the test guarding that was itself checking a
// proxy. Two proxies, in fact -- first "the word bindActorEnvelope
// appears in this function", then "the identifier appears as an argument
// to a binder somewhere in this function". Both are satisfied by code
// that is still fail-open.
//
// An unbound actor root is not neutral: the evaluator renders an
// unresolved dotted path as its own path TEXT, so `actor.isClusterOwner`
// is a non-empty (truthy) string and a negated gate reads TRUE with no
// auth context.
//
// So this checks the property the binder's doc actually promises --
// bound UNCONDITIONALLY, before first use -- with one rule:
//
//	every NewEvaluator() must be the RHS of a plain assignment to a
//	local, and that local must be passed to a binder as a top-level
//	statement of the SAME block, after the construction and before any
//	other use of the variable.
//
// That subsumes the special cases the earlier versions accumulated. A
// binding behind `if`/`switch`/`for` is not top-level, so conditional
// binding fails without a dead-branch carve-out. A binding after first
// use fails on ordering. A construction that is returned, stored in a
// field, or dropped into a composite literal fails the assignment rule
// rather than silently not being counted.
func TestEveryEvaluatorBindsAnActorEnvelope(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}

	checked := 0
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			// Every construction in the file, so one that never reaches
			// the block-level assignment form below is reported rather
			// than silently uncounted.
			all := map[*ast.CallExpr]bool{}
			ast.Inspect(file, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.CallExpr:
					if id, ok := node.Fun.(*ast.Ident); ok && id.Name == "NewEvaluator" {
						all[node] = true
					}
				case *ast.CompositeLit:
					// `&Evaluator{}` / `Evaluator{}` sidesteps a scan keyed
					// on the constructor name, and the zero value evaluates
					// `actor.*` happily -- fail-open (review round 6).
					//
					// evaluator.go is exempt: NewEvaluator and Clone are the
					// type's own constructors and necessarily build the
					// struct literally. Everywhere else must call
					// NewEvaluator so the binding rule applies.
					if strings.HasSuffix(fset.Position(node.Pos()).Filename, "evaluator.go") {
						return true
					}
					if id, ok := node.Type.(*ast.Ident); ok && id.Name == "Evaluator" {
						t.Errorf("%s: Evaluator built as a composite literal. Use NewEvaluator() and "+
							"bind it -- the zero value has no actor root, and an unbound actor.* read "+
							"is TRUTHY (memql#2801).", fset.Position(node.Pos()))
					}
				}
				return true
			})

			ast.Inspect(file, func(n ast.Node) bool {
				// Statement lists come in three shapes: a block, and the
				// bodies of switch/select clauses, which carry []ast.Stmt
				// directly. Missing the latter two sent a construction
				// there to the catch-all with a message stating the
				// opposite of what the code did (review round 6).
				var list []ast.Stmt
				switch node := n.(type) {
				case *ast.BlockStmt:
					list = node.List
				case *ast.CaseClause:
					list = node.Body
				case *ast.CommClause:
					list = node.Body
				default:
					return true
				}
				for i, stmt := range list {
					assign, ok := stmt.(*ast.AssignStmt)
					if !ok {
						continue
					}
					for k, rhs := range assign.Rhs {
						call, ok := rhs.(*ast.CallExpr)
						if !ok || !all[call] {
							continue
						}
						delete(all, call) // accounted for
						checked++

						pos := fset.Position(call.Pos())
						if k >= len(assign.Lhs) {
							t.Errorf("%s: NewEvaluator() result is discarded", pos)
							continue
						}
						target, ok := assign.Lhs[k].(*ast.Ident)
						if !ok {
							t.Errorf("%s: NewEvaluator() must be assigned to a plain local so the "+
								"binding can be verified; assign it and call bindActorEnvelope(ctx, ev)", pos)
							continue
						}
						name := target.Name

						boundAt := -1
						usedAt := -1
						for j := i + 1; j < len(list); j++ {
							if binderCallOn(list[j], name) {
								boundAt = j
								break
							}
							if usesIdent(list[j], name) {
								usedAt = j
								break
							}
						}
						switch {
						case boundAt >= 0:
							// correct
						case usedAt >= 0:
							t.Errorf("%s: evaluator %q is USED at %s before it is bound -- everything "+
								"up to that point evaluates against an unbound actor root, which is "+
								"TRUTHY and fails OPEN (memql#2801)",
								pos, name, fset.Position(list[usedAt].Pos()))
						default:
							t.Errorf("%s: evaluator %q is never passed to bindActorEnvelope / "+
								"bindNoCallerActorEnvelope as a top-level statement of its own block. "+
								"The binding must be UNCONDITIONAL -- a call nested in an if/switch/for "+
								"leaves the no-auth path unbound, which is the memql#2801 bug shape.", pos, name)
						}
					}
				}
				return true
			})

			for call := range all {
				t.Errorf("%s: NewEvaluator() is not assigned to a local, so its binding cannot be "+
					"verified (memql#2801). Assign it to a variable and bind that variable.",
					fset.Position(call.Pos()))
			}
		}
	}

	if checked == 0 {
		t.Fatal("no NewEvaluator() constructions found; the scan must not pass vacuously")
	}
	t.Logf("checked %d evaluator construction(s)", checked)
}

// evaluatorSetters is the closed set of Evaluator methods that only
// STORE a root and never resolve a path, so appearing before the binding
// is harmless. An allowlist rather than a `Set` name prefix: the prefix
// waved through anything called Setup() or SetAndEvaluate(), and this is
// the one place the invariant deliberately stops looking (review r6).
var evaluatorSetters = map[string]bool{
	"SetCanonicalIdResolver":    true,
	"SetCustom":                 true,
	"SetInput":                  true,
	"SetItem":                   true,
	"SetLogger":                 true,
	"SetSecretResolver":         true,
	"SetStepResult":             true,
	"SetSystemSecretResolver":   true,
	"SetSystemVariableResolver": true,
	"SetVariableResolver":       true,
}

// binderCallOn reports whether stmt is a top-level call to one of the
// actor-envelope binders passing the named identifier.
func binderCallOn(stmt ast.Stmt, name string) bool {
	expr, ok := stmt.(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := expr.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	fn, ok := call.Fun.(*ast.Ident)
	if !ok || (fn.Name != "bindActorEnvelope" && fn.Name != "bindNoCallerActorEnvelope") {
		return false
	}
	// Must be the package-level binder, not a local of the same name.
	// `bindActorEnvelope := func(context.Context, *Evaluator) {}` is the
	// round-1 decoy relocated from the argument to the callee: it
	// compiles, reads like a binding, and binds nothing (review round 6).
	if fn.Obj != nil {
		return false
	}
	for _, arg := range call.Args {
		if id, ok := arg.(*ast.Ident); ok && id.Name == name {
			return true
		}
	}
	return false
}

// usesIdent reports whether the statement USES the identifier in a way
// that could read `actor.*`.
//
// A `ev.SetX(...)` call is configuration, not evaluation -- it stores a
// root, it never resolves a path -- so seeding a dozen roots before
// binding the envelope is fine and is what newEvaluatorForLogic does.
// Anything else counts: an Evaluate/EvaluateCondition call obviously,
// but also passing the evaluator to another function or returning it,
// since the binding can no longer be verified past that point.
func usesIdent(stmt ast.Stmt, name string) bool {
	if isSetterCallOn(stmt, name) {
		return false
	}
	found := false
	ast.Inspect(stmt, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == name {
			found = true
		}
		return !found
	})
	return found
}

// isSetterCallOn reports whether stmt is exactly `name.SetX(...)`, with
// the identifier appearing nowhere else in the statement.
func isSetterCallOn(stmt ast.Stmt, name string) bool {
	expr, ok := stmt.(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := expr.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || !evaluatorSetters[sel.Sel.Name] {
		return false
	}
	recv, ok := sel.X.(*ast.Ident)
	if !ok || recv.Name != name {
		return false
	}
	// The evaluator must not ALSO be handed to something in the args.
	for _, arg := range call.Args {
		leaked := false
		ast.Inspect(arg, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == name {
				leaked = true
			}
			return !leaked
		})
		if leaked {
			return false
		}
	}
	return true
}

// No evaluator may be constructed outside this package: the binders are
// unexported, so a construction elsewhere could neither be seen by the
// invariant above nor use them, and would hand-roll the envelope.
//
// Resolved through the AST with import-qualifier handling rather than a
// substring scan -- a substring both MISSES an aliased or dot import and
// FALSE-POSITIVES on a comment, which is the bug class this whole test
// file exists to stop repeating (review round 5).
func TestNoEvaluatorConstructionOutsideThisPackage(t *testing.T) {
	const pkgPath = "github.com/znasllc-io/memql/component/automations"
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	var outside []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		slash := filepath.ToSlash(path)
		// The package itself is covered by the invariant above; its
		// steps/ subpackage is a different package and IS checked.
		if strings.Contains(slash, "/component/automations/") && !strings.Contains(slash, "/component/automations/steps/") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil || !strings.Contains(string(src), "automations") {
			return nil // cheap pre-filter; the AST below is the decision
		}
		file, perr := parser.ParseFile(fset, path, src, 0)
		if perr != nil {
			return nil
		}

		// How is the package named in THIS file? Default name, alias, or dot.
		local, dot := "", false
		for _, imp := range file.Imports {
			if strings.Trim(imp.Path.Value, `"`) != pkgPath {
				continue
			}
			switch {
			case imp.Name == nil:
				local = "automations"
			case imp.Name.Name == ".":
				dot = true
			case imp.Name.Name == "_":
			default:
				local = imp.Name.Name
			}
		}
		if local == "" && !dot {
			return nil
		}

		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.SelectorExpr:
				// The CONSTRUCTOR matched wherever it appears, not just in
				// call position: `var f = automations.NewEvaluator; f()`
				// is a construction the call-position scan never saw
				// (review round 6). The TYPE is deliberately not matched
				// here -- `func f(ev *automations.Evaluator)` is an
				// ordinary parameter, not a construction.
				if x, ok := node.X.(*ast.Ident); ok && x.Name == local && node.Sel.Name == "NewEvaluator" {
					outside = append(outside, fset.Position(node.Pos()).String())
				}
			case *ast.CompositeLit:
				// ...but a composite literal OF the type is a construction,
				// and every field is unexported so the zero value compiles
				// and evaluates actor.* as truthy.
				if sel, ok := node.Type.(*ast.SelectorExpr); ok {
					if x, ok := sel.X.(*ast.Ident); ok && x.Name == local && sel.Sel.Name == "Evaluator" {
						outside = append(outside, fset.Position(node.Pos()).String())
					}
				}
				if dot {
					if id, ok := node.Type.(*ast.Ident); ok && id.Name == "Evaluator" {
						outside = append(outside, fset.Position(node.Pos()).String())
					}
				}
			case *ast.Ident:
				if dot && node.Name == "NewEvaluator" {
					outside = append(outside, fset.Position(node.Pos()).String())
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	for _, f := range outside {
		t.Errorf("%s constructs an Evaluator outside component/automations, where the actor-envelope "+
			"binders are unexported and the binding invariant cannot see it (memql#2801). Either move "+
			"the construction into the package or export a bound constructor.", f)
	}
}

// End-to-end at the seam the whole memql#2801 narrative is built around:
// an event trigger's @filter. The structural invariant above proves the
// binding is PRESENT at every site; this proves it has the intended
// EFFECT on the path that matters most, because a `@filter` gating on an
// actor field decides whether the automation fires at all.
//
// `@filter(actor.isClusterOwner != false)` loads green (with or without
// @actor) and the compiler does not rewrite the actor root away, so an
// unbound root made this fire on every event.
func TestScheduler_EventFilterOnActor_DoesNotFireWithoutAuth(t *testing.T) {
	bus := events.NewBus()
	defer bus.Close()
	var buf bytes.Buffer
	s := newMinimalScheduler(&buf, bus)

	// Zero steps: a fire completes cleanly without a step registry, and the
	// assertion is on whether the filter admitted the event at all.
	gated := &Automation{
		Name:    "adminOnlySweep",
		Trigger: &TriggerConfig{Event: "node.created", Filter: `actor.isClusterOwner != false`},
	}
	// Control: a filter that does NOT mention actor and is true. Without it
	// this test could pass because the harness never fires anything.
	control := &Automation{
		Name:    "ungatedSweep",
		Trigger: &TriggerConfig{Event: "node.created", Filter: `event.topic != ""`},
	}
	for _, a := range []*Automation{gated, control} {
		s.automations[a.Name] = a
		if err := s.subscribeToEventTrigger(a); err != nil {
			t.Fatalf("subscribeToEventTrigger(%s): %v", a.Name, err)
		}
	}

	bus.PublishSync(events.NewEvent("node.created", events.KindNodeCreated, map[string]any{"id": "x"}))
	log := buf.String()

	// The bus carries no caller, so the denying envelope must make the
	// actor-gated filter false. Before the binding, the unbound
	// `actor.isClusterOwner` rendered as its own path text -- non-empty,
	// therefore truthy -- and this admitted every event.
	if !schedulerLogged(log, "filter not satisfied", gated.Name) {
		t.Errorf("the actor-gated @filter did NOT deny an event with no caller -- the actor root is "+
			"unbound or resolving truthy (memql#2801). Scheduler log:\n%s", log)
	}

	// The control asserts POSITIVELY that the harness fires. A negative
	// "the control was not denied" is satisfied by any early return that
	// skips the deny log -- a filter parse error, for instance -- which
	// would leave the assertion above proving nothing (review round 4).
	if !schedulerLogged(log, "event trigger fired", control.Name) {
		t.Errorf("the control (non-actor) automation did not fire, so the harness admits nothing and "+
			"the assertion above is vacuous. Scheduler log:\n%s", log)
	}
}

// schedulerLogged reports whether the scheduler logged the given message
// for the named automation.
//
// The name is matched on the structured `automation=` field rather than
// anywhere in the line: a bare substring match makes one automation's
// name match another's when it is a prefix (review round 4).
func schedulerLogged(log, msg, automation string) bool {
	for _, line := range strings.Split(log, "\n") {
		if !strings.Contains(line, msg) {
			continue
		}
		if strings.Contains(line, "automation="+automation+" ") ||
			strings.HasSuffix(line, "automation="+automation) ||
			strings.Contains(line, `"automation":"`+automation+`"`) {
			return true
		}
	}
	return false
}

// The binding invariant checks that `actor` is bound before first use.
// It says nothing about what happens AFTER, and the setter exemption
// declares Set* calls uninteresting by construction -- so this passes it:
//
//	bindActorEnvelope(ctx, ev)
//	ev.SetCustom("actor", map[string]any{})   // clobbered back to empty
//
// which is fail-open, and is precisely one of the four pre-fix
// representations of "no actor" the binder's own doc enumerates. A normal
// future edit lands on this, so it is asserted rather than reasoned about
// (review round 6).
func TestActorRootIsSetOnlyByTheBinder(t *testing.T) {
	root := filepath.Join("..", "..")
	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		slash := filepath.ToSlash(path)
		if strings.HasSuffix(slash, "component/automations/actor_envelope_binding.go") {
			return nil // the binder is the one legitimate writer
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, src, 0)
		if perr != nil {
			return nil
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "SetCustom" {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Value != `"actor"` {
				return true
			}
			offenders = append(offenders, fset.Position(call.Pos()).String())
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	for _, o := range offenders {
		t.Errorf("%s sets the `actor` root directly. Only bindActorEnvelope / "+
			"bindNoCallerActorEnvelope may do that -- a hand-rolled or empty envelope is one of the "+
			"four representations of \"no actor\" that made the gate fail OPEN (memql#2801). "+
			"Call a binder instead.", o)
	}
}
