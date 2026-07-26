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

// The structural invariant, and the reason it exists: three review rounds
// on memql#2801 each found ANOTHER evaluator that left `actor` unbound,
// and a helper-level test catches none of them -- removing a call site
// leaves the suite green.
//
// An unbound actor root is not neutral. The evaluator renders an
// unresolved dotted path as its own path TEXT, so `actor.isClusterOwner`
// is a non-empty (truthy) string and a negated gate reads TRUE with no
// auth context.
//
// This walks the AST rather than the source text, and keys on the
// CONSTRUCTED VARIABLE. A text scan proved to check the wrong thing: it
// asked "does the word bindActorEnvelope appear nearby", which a decoy
// binding, a commented-out call, an `if false` branch, a string literal,
// or a nested closure all satisfy while the live evaluator escapes
// unbound -- the exact bug class this is guarding (review round 4).
// Closures matter concretely: the scheduler's real site is inside one.
func TestEveryEvaluatorBindsAnActorEnvelope(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}

	type construction struct {
		ident string
		pos   token.Position
	}
	// Keyed by the enclosing function body, so a binding in a sibling
	// function cannot vouch for a construction over here.
	built := map[ast.Node][]construction{}
	bound := map[ast.Node]map[string]bool{}

	// enclosing returns the innermost FuncDecl/FuncLit body containing pos.
	var scopes []ast.Node
	record := func(root ast.Node) {
		ast.Inspect(root, func(n ast.Node) bool {
			switch fn := n.(type) {
			case *ast.FuncDecl:
				if fn.Body != nil {
					scopes = append(scopes, fn.Body)
				}
			case *ast.FuncLit:
				scopes = append(scopes, fn.Body)
			}
			return true
		})
	}

	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			scopes = nil
			record(file)

			// innermostScope finds the tightest body containing pos.
			innermost := func(pos token.Pos) ast.Node {
				var best ast.Node
				for _, sc := range scopes {
					if pos < sc.Pos() || pos > sc.End() {
						continue
					}
					if best == nil || sc.Pos() > best.Pos() {
						best = sc
					}
				}
				return best
			}

			// Bindings inside a provably-dead branch do not count: an
			// `if false { bindActorEnvelope(ctx, ev) }` is a real AST call
			// with the right argument, and would otherwise vouch for an
			// evaluator that is never actually bound at runtime.
			var deadRanges [][2]token.Pos
			ast.Inspect(file, func(n ast.Node) bool {
				ifs, ok := n.(*ast.IfStmt)
				if !ok {
					return true
				}
				if id, ok := ifs.Cond.(*ast.Ident); ok && id.Name == "false" {
					deadRanges = append(deadRanges, [2]token.Pos{ifs.Body.Pos(), ifs.Body.End()})
				}
				return true
			})
			inDeadBranch := func(pos token.Pos) bool {
				for _, r := range deadRanges {
					if pos >= r[0] && pos <= r[1] {
						return true
					}
				}
				return false
			}

			ast.Inspect(file, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.AssignStmt:
					// x := NewEvaluator()
					for i, rhs := range node.Rhs {
						call, ok := rhs.(*ast.CallExpr)
						if !ok {
							continue
						}
						id, ok := call.Fun.(*ast.Ident)
						if !ok || id.Name != "NewEvaluator" {
							continue
						}
						if i >= len(node.Lhs) {
							continue
						}
						target, ok := node.Lhs[i].(*ast.Ident)
						if !ok {
							continue
						}
						sc := innermost(call.Pos())
						built[sc] = append(built[sc], construction{target.Name, fset.Position(call.Pos())})
					}
				case *ast.CallExpr:
					// bindActorEnvelope(ctx, x) / bindNoCallerActorEnvelope(x)
					id, ok := node.Fun.(*ast.Ident)
					if !ok {
						return true
					}
					if id.Name != "bindActorEnvelope" && id.Name != "bindNoCallerActorEnvelope" {
						return true
					}
					if inDeadBranch(node.Pos()) {
						return true
					}
					for _, arg := range node.Args {
						if a, ok := arg.(*ast.Ident); ok {
							sc := innermost(node.Pos())
							if bound[sc] == nil {
								bound[sc] = map[string]bool{}
							}
							bound[sc][a.Name] = true
						}
					}
				}
				return true
			})
		}
	}

	checked := 0
	for scope, sites := range built {
		for _, c := range sites {
			checked++
			if bound[scope][c.ident] {
				continue
			}
			t.Errorf("%s: evaluator %q is constructed but never passed to bindActorEnvelope / "+
				"bindNoCallerActorEnvelope in the same scope -- an unbound actor.* read renders as "+
				"its own path text and is TRUTHY, so a negated actor gate fails OPEN (memql#2801)",
				c.pos, c.ident)
		}
	}
	if checked == 0 {
		t.Fatal("no NewEvaluator() constructions found; the scan must not pass vacuously")
	}
	t.Logf("checked %d evaluator construction(s)", checked)
}

// No evaluator may be constructed outside this package, because the
// binders are unexported -- a construction elsewhere could neither be
// scanned by the invariant above nor use the helpers, and would have to
// hand-roll the envelope. Zero such call sites exist today; this keeps
// it that way rather than letting the guarantee quietly narrow.
func TestNoEvaluatorConstructionOutsideThisPackage(t *testing.T) {
	root := filepath.Join("..", "..")
	var outside []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		slash := filepath.ToSlash(path)
		if strings.Contains(slash, "/component/automations/") &&
			!strings.Contains(slash, "/component/automations/steps/") {
			return nil // the package the invariant above covers
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		if strings.Contains(string(src), "automations.NewEvaluator(") {
			outside = append(outside, slash)
		}
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
