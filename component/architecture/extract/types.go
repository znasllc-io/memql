package extract

import (
	"fmt"
	"go/token"
	"go/types"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/packages"

	"github.com/visionarys-io/memql/component/architecture/model"
)

// ExtractTypes adds Type, Interface, Method, and Field nodes for every
// named type defined in a service's own packages, plus relationship
// edges:
//
//   - EdgeContains  : package -> type, package -> interface, type -> method
//   - EdgeHasField  : type -> field
//   - EdgeEmbeds    : type -> embedded type (and interface -> embedded
//     interface)
//   - EdgeImplements: type -> interface, computed via types.Implements
//     over every interface defined in any analyzed service
//
// The pass is heavier than ExtractPackages because it requires
// go/types output, so packages are reloaded with NeedTypes |
// NeedTypesInfo. We tolerate type-check errors the same way the
// package pass does -- a partial type universe is still useful for
// diagrams.
func ExtractTypes(m *model.Model, plans []ServicePlan, workspaceRoot string) error {
	// Phase 1: load every plan's packages with type info. We collect
	// interfaces globally across services so the Implements
	// computation can cross service boundaries (cockpit can satisfy
	// a memql-core interface, etc.).
	all := make([]loadedTypePass, 0, len(plans))
	var interfaces []*types.Named // every analyzed interface across the workspace

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
			return fmt.Errorf("load types for %s: %w", plan.Arch.Service, err)
		}
		var firstErr error
		packages.Visit(pkgs, nil, func(p *packages.Package) {
			if len(p.Errors) == 0 || firstErr != nil {
				return
			}
			firstErr = fmt.Errorf("%s: %s", p.PkgPath, p.Errors[0])
		})
		if firstErr != nil {
			fmt.Printf("memql-arch: warning (types): %v\n", firstErr)
		}

		all = append(all, loadedTypePass{plan: plan, pkgs: pkgs})
	}

	// Helper: is this package one of ours (any service)?
	isOurs := func(importPath string) bool {
		for _, plan := range plans {
			if importPath == plan.ModulePath || strings.HasPrefix(importPath, plan.ModulePath+"/") {
				return true
			}
		}
		return false
	}

	// Pre-pass: collect every interface from every analyzed service.
	for _, l := range all {
		packages.Visit(l.pkgs, nil, func(p *packages.Package) {
			if !isOurs(p.PkgPath) || p.Types == nil {
				return
			}
			scope := p.Types.Scope()
			for _, name := range scope.Names() {
				obj, ok := scope.Lookup(name).(*types.TypeName)
				if !ok {
					continue
				}
				named, ok := obj.Type().(*types.Named)
				if !ok {
					continue
				}
				if _, isIface := named.Underlying().(*types.Interface); isIface {
					interfaces = append(interfaces, named)
				}
			}
		})
	}

	// Phase 2: emit nodes + structural edges per service.
	seenNodes := make(map[model.ID]bool)
	emitNode := func(n model.Node) {
		if seenNodes[n.ID] {
			return
		}
		seenNodes[n.ID] = true
		m.Nodes = append(m.Nodes, n)
	}

	for _, l := range all {
		packages.Visit(l.pkgs, nil, func(p *packages.Package) {
			if !isOurs(p.PkgPath) || p.Types == nil {
				return
			}
			// Skip packages that the package pass already filtered.
			if model.PackageID(p.PkgPath) == "" {
				return
			}
			pkgID := model.PackageID(p.PkgPath)
			scope := p.Types.Scope()
			for _, name := range scope.Names() {
				if !ast_isExported(name) {
					// Unexported types are usually noise at the
					// diagram level; including them blows up the L4
					// view without adding insight. Renderers can
					// already infer their existence from method
					// references. Skip.
					continue
				}
				obj, ok := scope.Lookup(name).(*types.TypeName)
				if !ok {
					continue
				}
				named, ok := obj.Type().(*types.Named)
				if !ok {
					continue
				}
				underlying := named.Underlying()
				src := posToSourceRef(p, obj.Pos(), workspaceRoot)

				switch u := underlying.(type) {
				case *types.Interface:
					ifaceID := model.InterfaceID(p.PkgPath, name)
					emitNode(model.Node{
						ID:     ifaceID,
						Kind:   model.KindInterface,
						Name:   name,
						Parent: pkgID,
						Source: src,
						Attrs: map[string]string{
							"num_methods": fmt.Sprintf("%d", u.NumMethods()),
						},
					})
					m.Edges = append(m.Edges, model.Edge{
						From: pkgID,
						To:   ifaceID,
						Kind: model.EdgeContains,
					})
					// Interface embedding: NumEmbeddeds covers other
					// interfaces (and type constraints in generics).
					for i := 0; i < u.NumEmbeddeds(); i++ {
						if emb, ok := u.EmbeddedType(i).(*types.Named); ok {
							if eID, ok := namedID(emb); ok {
								m.Edges = append(m.Edges, model.Edge{
									From: ifaceID,
									To:   eID,
									Kind: model.EdgeEmbeds,
								})
							}
						}
					}
				case *types.Struct:
					typeID := model.TypeID(p.PkgPath, name)
					emitNode(model.Node{
						ID:     typeID,
						Kind:   model.KindType,
						Name:   name,
						Parent: pkgID,
						Source: src,
						Attrs: map[string]string{
							"num_fields": fmt.Sprintf("%d", u.NumFields()),
						},
					})
					m.Edges = append(m.Edges, model.Edge{
						From: pkgID,
						To:   typeID,
						Kind: model.EdgeContains,
					})
					// Fields + embedding.
					for i := 0; i < u.NumFields(); i++ {
						f := u.Field(i)
						fieldName := f.Name()
						fieldID := model.FieldID(p.PkgPath, name, fieldName)
						emitNode(model.Node{
							ID:     fieldID,
							Kind:   model.KindField,
							Name:   fieldName,
							Parent: typeID,
							Attrs: map[string]string{
								"type":     f.Type().String(),
								"exported": fmt.Sprintf("%t", f.Exported()),
							},
						})
						m.Edges = append(m.Edges, model.Edge{
							From: typeID,
							To:   fieldID,
							Kind: model.EdgeHasField,
						})
						if f.Embedded() {
							if embNamed := derefNamed(f.Type()); embNamed != nil {
								if eID, ok := namedID(embNamed); ok {
									m.Edges = append(m.Edges, model.Edge{
										From: typeID,
										To:   eID,
										Kind: model.EdgeEmbeds,
									})
								}
							}
						}
					}
					// Methods (declared on the type or via pointer
					// receiver). Use NewMethodSet on a pointer so we
					// catch both receiver styles in a single pass.
					mset := types.NewMethodSet(types.NewPointer(named))
					for i := 0; i < mset.Len(); i++ {
						sel := mset.At(i)
						fn, ok := sel.Obj().(*types.Func)
						if !ok {
							continue
						}
						methodID := model.MethodID(p.PkgPath, name, fn.Name())
						emitNode(model.Node{
							ID:     methodID,
							Kind:   model.KindMethod,
							Name:   fn.Name(),
							Parent: typeID,
							Source: posToSourceRef(p, fn.Pos(), workspaceRoot),
							Attrs: map[string]string{
								"signature": fn.Type().String(),
							},
						})
						m.Edges = append(m.Edges, model.Edge{
							From: typeID,
							To:   methodID,
							Kind: model.EdgeContains,
						})
					}
					// Implements: check this concrete type against
					// every known interface. Use the pointer type
					// because pointer receivers count toward the
					// method set -- the dominant Go pattern.
					ptr := types.NewPointer(named)
					for _, iface := range interfaces {
						ifaceUnder, ok := iface.Underlying().(*types.Interface)
						if !ok || ifaceUnder.NumMethods() == 0 {
							continue // empty interface matches everything; not useful
						}
						if types.Implements(ptr, ifaceUnder) {
							if iID, ok := namedID(iface); ok {
								m.Edges = append(m.Edges, model.Edge{
									From: typeID,
									To:   iID,
									Kind: model.EdgeImplements,
								})
							}
						}
					}
				default:
					// Other named kinds (basic aliases, slice types,
					// channel types) aren't part of the class
					// diagram yet. Skip silently; can be added when
					// renderers ask for them.
				}
			}
			// Package-level functions.
			for _, name := range scope.Names() {
				obj, ok := scope.Lookup(name).(*types.Func)
				if !ok || !obj.Exported() {
					continue
				}
				funcID := model.FuncID(p.PkgPath, name)
				emitNode(model.Node{
					ID:     funcID,
					Kind:   model.KindFunc,
					Name:   name,
					Parent: pkgID,
					Source: posToSourceRef(p, obj.Pos(), workspaceRoot),
					Attrs: map[string]string{
						"signature": obj.Type().String(),
					},
				})
				m.Edges = append(m.Edges, model.Edge{
					From: pkgID,
					To:   funcID,
					Kind: model.EdgeContains,
				})
			}
		})
	}

	// Phase 3: AST pass for //memql:observe markers and default
	// `observable` attrs. Runs after node emission so we patch onto
	// nodes that actually exist in the model.
	stampObserveMarkers(m, plans, all)

	return nil
}

// namedID builds the right ID for a *types.Named based on whether its
// underlying is an interface. Returns false when the named type's
// package is unknown (which happens for builtin error / comparable).
func namedID(n *types.Named) (model.ID, bool) {
	obj := n.Obj()
	if obj == nil || obj.Pkg() == nil {
		return "", false
	}
	pkgPath := obj.Pkg().Path()
	if _, isIface := n.Underlying().(*types.Interface); isIface {
		return model.InterfaceID(pkgPath, obj.Name()), true
	}
	return model.TypeID(pkgPath, obj.Name()), true
}

// derefNamed peels a single pointer indirection if present and returns
// the underlying *types.Named, or nil when the type isn't a named type
// (e.g. structural type, slice, map). Used for embedding edges where
// `struct { *Foo }` should still resolve to Foo.
func derefNamed(t types.Type) *types.Named {
	if ptr, ok := t.(*types.Pointer); ok {
		t = ptr.Elem()
	}
	n, _ := t.(*types.Named)
	return n
}

// posToSourceRef converts a token.Pos to a SourceRef whose file path
// is relative to the workspace root. Returns nil when the position is
// invalid or the package has no FileSet.
func posToSourceRef(p *packages.Package, pos token.Pos, workspaceRoot string) *model.SourceRef {
	if !pos.IsValid() || p.Fset == nil {
		return nil
	}
	tp := p.Fset.Position(pos)
	if tp.Filename == "" {
		return nil
	}
	rel, err := filepath.Rel(workspaceRoot, tp.Filename)
	if err != nil {
		rel = tp.Filename
	}
	return &model.SourceRef{File: rel, Line: tp.Line}
}

// ast_isExported mirrors token.IsExported without dragging the
// go/ast package in for one identifier check. The ASCII fast path
// covers every identifier in the workspace; if a non-ASCII
// uppercase identifier ever appears, fall back to token.IsExported.
func ast_isExported(name string) bool {
	if name == "" {
		return false
	}
	c := name[0]
	return c >= 'A' && c <= 'Z'
}
