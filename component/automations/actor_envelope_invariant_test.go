package automations

// Machine-checked invariants behind the memql#2801 actor-envelope fix.
//
// These are lint rules, not behaviour tests -- the behaviour lives in
// actor_envelope_binding_test.go. They exist because the fix drifted
// across FIVE evaluator construction sites, each inventing its own
// representation of "no actor" (an empty map, an unbound root, absent
// keys, the envelope), and review-by-review discovery did not converge.
//
// An unbound actor root is not neutral: the evaluator renders an
// unresolved dotted path as its own path TEXT, so `actor.isClusterOwner`
// is a non-empty -- therefore truthy -- string, and a negated admin gate
// reads TRUE with no auth context.
//
// The rule these enforce, in one sentence: every evaluator must get the
// canonical envelope UNCONDITIONALLY and before first use, and nothing
// else may write the `actor` root.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every NewEvaluator() must be the RHS of a plain assignment to a local,
// and that local must reach a binder as a TOP-LEVEL statement of the same
// block, after the construction and before any other use.
//
// Checking the property rather than a proxy matters here: earlier versions
// asked "does the word appear in this function", then "does the identifier
// appear as a binder argument", and both were satisfied by code that was
// still fail-open -- a decoy binding, a conditional one, a bind after
// first use, a nested closure. Nested in an if/switch/for is not
// top-level, so conditional binding fails with no dead-branch carve-out
// to respell around.
//
// `ev.SetX(...)` is configuration, not use: it stores a root and never
// resolves a path, which is why newEvaluatorForLogic can seed a dozen
// roots before binding.
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
					// `actor.*` happily -- fail-open.
					//
					// Exempt only the type's OWN constructors, which must
					// build it literally. A file-wide exemption left any
					// other helper in that file able to smuggle an
					// unchecked literal past both rules.
					if constructorFuncs[enclosingFuncName(file, node.Pos())] {
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
				// opposite of what the code did.
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

// constructorFuncs are the only functions permitted to build an
// Evaluator as a composite literal -- the type's own constructors.
var constructorFuncs = map[string]bool{"NewEvaluator": true, "Clone": true}

// enclosingFuncName returns the name of the FuncDecl containing pos, or
// "" when pos is not inside one.
func enclosingFuncName(file *ast.File, pos token.Pos) string {
	name := ""
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			return true
		}
		if pos >= fn.Body.Pos() && pos <= fn.Body.End() {
			name = fn.Name.Name
		}
		return true
	})
	return name
}

// evaluatorSetters is the closed set of Evaluator methods that only
// STORE a root and never resolve a path, so appearing before the binding
// is harmless. An allowlist rather than a `Set` name prefix: the prefix
// waved through anything called Setup() or SetAndEvaluate(), and this is
// the one place the invariant deliberately stops looking.
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
	// Must be the package-level binder, not a local of the same name:
	// `bindActorEnvelope := func(context.Context, *Evaluator) {}`
	// compiles, reads like a binding, and binds nothing.
	//
	// Keyed on Obj.KIND, not on Obj being nil. The parser resolves
	// package-level identifiers declared in the SAME file, so a nil check
	// rejected correct calls made from actor_envelope_binding.go itself
	// -- the very file where a bound-constructor helper would go. A local
	// shadow resolves to ast.Var; a func declaration to ast.Fun, and Go
	// has no nested func declarations.
	if fn.Obj != nil && fn.Obj.Kind != ast.Fun {
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
// file exists to stop repeating.
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
				//. The TYPE is deliberately not matched
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
