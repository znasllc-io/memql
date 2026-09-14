package memql

import (
	"errors"
	"strings"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// authoring_lower.go -- a session-authored query, spec or trait is lowered
// exactly as the tree's are (epic memql#5363, task memql#5366).
//
// After the edition-2026 flip every construct an author writes at runtime is
// v1, and a v1 construct is not callable until it is LOWERED: a spec or trait
// body has no Expr until Lower builds one, and a query's filter lambda is
// checked against the specs it applies only once a spec registry is in hand.
// The Init pass does that for the tree. This file does it, with the same
// function (lowerPushdownSet), on every path that makes an authored construct
// callable -- so no authored construct can skip the lowering or run unlowered:
//
//   - GATE 1, behind validate, define and stage (compileBundleWith): the
//     bundle's queries, specs and traits are compiled into the forms that get
//     registered and lowered as one set, and a refusal becomes that
//     construct's diagnostic -- the three-part message, positioned on the
//     author's line and column -- at define time, not at the first call;
//   - REGISTRATION (lowerAuthoredForRegistry), inside the promote and the
//     stage every route funnels through -- MCP and gRPC promote, the durable
//     bundle promote, training a staged construct, and the boot and
//     cross-node re-hydration of stored v1:authoring:construct rows -- against
//     the registries the construct is about to enter;
//   - after the BOOT walk (checkRehydratedPushdown), the check Init makes
//     after every spec is loaded: stored rows are replayed one at a time, so
//     their predicate checks wait until every row is back.
//
// Where an engine is behind the call, the scope is its registries with the
// authored layers on top. Where none is -- the package-level ValidateBundle and
// AuthorSessionBundle, which take no engine -- the scope is the bundle alone,
// and what needs the engine's registries is deferred to the registration
// lowering and the execution overlay (buildAuthoredSpecOverlay), which always
// have one. Every production caller of define and validate goes through the
// engine (DefineSessionBundle, ValidateAuthoredBundle), so its refusals arrive
// at define time.

// authoringLowering is where an authoring path lowers: the engine whose
// registries are the base of the scope (nil: an engine-free validation), and
// the owner's authored layers above the engine's, lowest precedence first.
type authoringLowering struct {
	engine *MemQLEngine
	owner  string
	layers []*AuthoredRuntimeRegistry
}

// engineFreeLowering is the lowering of a call with no engine behind it.
var engineFreeLowering = &authoringLowering{}

// scope builds the pushdown scope one set of authored constructs is lowered
// in: concepts (the bundle's overlay, or the engine's), the engine's shapes
// with the bundle's layered on, and predicates resolved CORE FIRST -- an
// authored spec never shadows a core one, as in every authored overlay --
// then among the set being defined, then down the owner's layers from the
// highest (the session) to the lowest (staged).
func (al *authoringLowering) scope(concepts memoryNodes.Registry, bundleShapes []*ShapeDefinition, set []*Spec) pushdownScope {
	s := pushdownScope{concepts: concepts}
	if al == nil || al.engine == nil {
		reg := newShapeRegistry()
		for _, sh := range bundleShapes {
			_ = reg.Upsert(sh)
		}
		expandDefaultShapeProjections(nil, reg, concepts)
		s.shapes = reg
		s.engineFree = true
		return s
	}
	e := al.engine
	s.shapes = e.Shapes()
	if len(bundleShapes) > 0 {
		reg := newShapeRegistry()
		if core := e.Shapes(); core != nil {
			for _, sh := range core.List() {
				_ = reg.Upsert(sh)
			}
		}
		fresh := newShapeRegistry()
		for _, sh := range bundleShapes {
			if _, taken := reg.Get(sh.Name); taken {
				continue // core first, for shapes as for specs
			}
			_ = reg.Upsert(sh)
			_ = fresh.Upsert(sh)
		}
		expandDefaultShapeProjections(nil, fresh, concepts)
		for _, sh := range fresh.List() {
			_ = reg.Upsert(sh)
		}
		s.shapes = reg
	}
	bySetName := make(map[string]*Spec, len(set))
	for _, spec := range set {
		if spec != nil {
			bySetName[spec.Name] = spec
		}
	}
	core := e.predicateLookup()
	owner, layers := strings.TrimSpace(al.owner), al.layers
	s.predicate = func(name string) (*Spec, bool) {
		if spec, ok := core(name); ok {
			return spec, true
		}
		if spec := bySetName[name]; spec != nil {
			return spec, true
		}
		if owner == "" {
			return nil, false
		}
		for i := len(layers) - 1; i >= 0; i-- {
			if spec := authoredSpecIn(layers[i], owner, name); spec != nil {
				return spec, true
			}
		}
		return nil, false
	}
	return s
}

// authoredSpecIn is the owner's active spec or trait named name in one
// authored layer, or nil.
func authoredSpecIn(layer *AuthoredRuntimeRegistry, owner, name string) *Spec {
	if layer == nil {
		return nil
	}
	for _, kind := range []string{"spec", "trait"} {
		if c, ok := layer.Resolve(owner, kind, name); ok {
			if spec, isSpec := c.Compiled.(*Spec); isSpec && spec != nil {
				return spec
			}
		}
	}
	return nil
}

// compileBundleWith is compileBundle -- the Gate-1 per-construct compile --
// followed by the lowering of the bundle's queries, specs and traits as one
// set. A construct that does not lower fails with the lowering's diagnostic
// (the three-part message, on the author's line and column), and the bundle
// with it. The returned map holds the compiled, lowered forms by kind/name --
// what a define registers -- for every query, spec and trait that compiled.
func compileBundleWith(constructs []SandboxConstruct, al *authoringLowering) (SandboxReport, *memoryNodes.MemoryRegistry, map[string]any) {
	rep, concepts := compileBundle(constructs)
	compiled := al.lowerBundle(constructs, &rep, concepts)
	return rep, concepts, compiled
}

// lowerBundle compiles every query, spec and trait of a bundle that passed the
// per-construct pass into the form a define registers, lowers them as one set
// in this lowering's scope, and turns each refusal into its construct's
// diagnostic.
func (al *authoringLowering) lowerBundle(constructs []SandboxConstruct, rep *SandboxReport, concepts *memoryNodes.MemoryRegistry) map[string]any {
	diagAt := map[string]int{}
	for i, d := range rep.Diagnostics {
		key := d.Kind + "/" + d.Name
		if _, seen := diagAt[key]; !seen {
			diagAt[key] = i
		}
	}
	passed := func(c SandboxConstruct) bool {
		i, ok := diagAt[c.Kind+"/"+c.Name]
		return ok && rep.Diagnostics[i].OK
	}

	compiled := map[string]any{}
	var specs []*Spec
	var queries []*Function
	var shapes []*ShapeDefinition
	owner := map[any]SandboxConstruct{}
	for _, c := range constructs {
		if !passed(c) {
			continue
		}
		switch c.Kind {
		case "query":
			// Compile failures are left to the define that registers the
			// construct, which compiles it the same way and reports them as it
			// always has: this pass adds the lowering's refusals, nothing else.
			if fn, err := compileAuthoredFunction(c, concepts); err == nil && fn != nil {
				compiled[c.Kind+"/"+c.Name] = fn
				queries = append(queries, fn)
				owner[fn] = c
			}
		case "spec", "trait":
			if spec, err := compileAuthoredSpec(c); err == nil && spec != nil {
				compiled[c.Kind+"/"+c.Name] = spec
				specs = append(specs, spec)
				owner[spec] = c
			}
		case "shape":
			if decl, err := languageParser.ParseShapeDecl(stripUseDeclarations(c.Source)); err == nil {
				if sh, err := shapeDeclToShapeDefinition(decl, c.sandboxOrigin()); err == nil && sh != nil {
					shapes = append(shapes, sh)
				}
			}
		}
	}
	if len(specs) == 0 && len(queries) == 0 {
		return compiled
	}

	for _, f := range lowerPushdownSet(specs, queries, al.scope(concepts, shapes, specs)) {
		var key any = f.fn
		if f.spec != nil {
			key = f.spec
		}
		c, ok := owner[key]
		if !ok {
			continue
		}
		delete(compiled, c.Kind+"/"+c.Name)
		i, ok := diagAt[c.Kind+"/"+c.Name]
		if !ok {
			continue
		}
		rep.Diagnostics[i] = attachPos(fail(rep.Diagnostics[i], lowerDiagnosticMessage(c, f.err)), c, f.err)
		rep.OK = false
	}
	return compiled
}

// lowerDiagnosticMessage is a lowering refusal as a Gate-1 diagnostic reads:
// the construct's origin, then the refusal itself. A LowerError is printed on
// its own -- its sentence already names the node, the position and the fix --
// rather than behind the converter's chain of "convert ... target" prefixes,
// which describe the engine's walk, not the author's source.
func lowerDiagnosticMessage(c SandboxConstruct, err error) string {
	var lerr *LowerError
	if errors.As(err, &lerr) {
		return c.sandboxOrigin() + ": " + lerr.Error()
	}
	return c.sandboxOrigin() + ": " + err.Error()
}

// lowerAuthoredForRegistry lowers the compiled form of an authored query,
// spec or trait against the registries it is about to enter, and returns the
// form to register. owner and layers name the owner-scoped registries that
// resolve beside the shared ones (the staged tier's own; none for a shared
// promote, which may depend on nothing another session cannot see).
//
// A spec is re-bound and re-lowered on a CLONE, never in place: the compiled
// form it came from may be a session registry's entry, lowered in that
// session's scope, and a promote must not rewrite what the session resolves.
// Re-binding is the point -- a spec the session bound to a shape its own
// bundle declared cannot be promoted over that shape, which the shared
// registry does not hold.
//
// replay marks the boot walk and the cross-node propagation re-applying a
// promote somebody already decided. Rows come back one at a time, so a spec
// another row applies may not be back yet: the predicate checks are deferred
// (checkRehydratedPushdown runs them at boot, once every row is back; the
// originating node ran them for a propagation), while everything that reads
// the construct alone -- fields, bindings, tiers, the dry compile -- refuses
// here.
func (e *MemQLEngine) lowerAuthoredForRegistry(c *AuthoredConstruct, owner string, layers []*AuthoredRuntimeRegistry, replay bool) (any, error) {
	if c == nil {
		return nil, nil
	}
	var specs []*Spec
	var queries []*Function
	out := c.Compiled
	switch compiled := c.Compiled.(type) {
	case *Spec:
		if compiled == nil || compiled.Lambda == nil {
			return c.Compiled, nil
		}
		spec := compiled.clone()
		spec.Kind, spec.Expr, spec.ExprSource = "", nil, ""
		specs, out = []*Spec{spec}, spec
	case *Function:
		if compiled == nil || (compiled.V1Filter == nil && refineIn(compiled.Expr) == nil) {
			return c.Compiled, nil
		}
		queries = []*Function{compiled}
	default:
		return c.Compiled, nil
	}
	al := &authoringLowering{engine: e, owner: owner, layers: layers}
	scope := al.scope(e.concepts, nil, specs)
	if replay {
		scope.predicate = nil
	}
	if failures := lowerPushdownSet(specs, queries, scope); len(failures) > 0 {
		return nil, failures[0].err
	}
	return out, nil
}

// checkRehydratedPushdown is the boot walk's second moment -- the check the
// Init pass makes once every spec is loaded. The walk registered each stored
// row with its predicate checks deferred (lowerAuthoredForRegistry, replay);
// here every authored v1 query, spec and trait now registered is checked
// against the full registries, and one that fails is quarantined and taken
// back out, as a row whose source no longer compiles is. Returns the
// quarantined constructs as kind:name, the form RehydrateResult.Failed reads.
func (e *MemQLEngine) checkRehydratedPushdown() []string {
	if e == nil {
		return nil
	}
	var quarantined []string
	quarantine := func(kind, name, owner string, err error) {
		e.quarantineRehydratedConstruct(AuthoringConstructRow{Kind: kind, Name: name, OwnerUserId: owner}, err)
		quarantined = append(quarantined, kind+":"+name)
	}

	// The shared tier: every construct a promote registered.
	var specs []*Spec
	var queries []*Function
	e.promotedAuthored.Range(func(k, _ any) bool {
		key, _ := k.(string)
		switch {
		case strings.HasPrefix(key, "spec:") && e.specs != nil:
			if spec, err := e.specs.Get(strings.TrimPrefix(key, "spec:")); err == nil && spec != nil && spec.Lambda != nil {
				specs = append(specs, spec)
			}
		case strings.HasPrefix(key, "function:") && e.functions != nil:
			if fn, err := e.functions.Get(strings.TrimPrefix(key, "function:")); err == nil && fn != nil && fn.V1Filter != nil {
				queries = append(queries, fn)
			}
		}
		return true
	})
	scope := pushdownScope{concepts: e.concepts, shapes: e.Shapes(), predicate: e.predicateLookup(), bindingsResolved: true}
	for _, f := range lowerPushdownSet(specs, queries, scope) {
		if f.spec != nil {
			e.specs.Remove(f.spec.Name)
			e.promotedAuthored.Delete("spec:" + f.spec.Name)
			quarantine(specKeyword(f.spec), f.spec.Name, "", f.err)
		} else if f.fn != nil {
			e.functions.Remove(f.fn.Name)
			e.promotedAuthored.Delete("function:" + f.fn.Name)
			quarantine("query", f.fn.Name, "", f.err)
		}
	}

	// The staged tier: each owner's constructs, in that owner's scope.
	staged := e.stagedAuthored
	if staged == nil {
		return quarantined
	}
	byOwner := map[string][]*AuthoredConstruct{}
	for _, c := range staged.ListAll() {
		byOwner[c.OwnerUserId] = append(byOwner[c.OwnerUserId], c)
	}
	for owner, entries := range byOwner {
		var ospecs []*Spec
		var oqueries []*Function
		entryOf := map[any]*AuthoredConstruct{}
		for _, c := range entries {
			switch compiled := c.Compiled.(type) {
			case *Spec:
				if compiled != nil && compiled.Lambda != nil {
					clone := compiled.clone()
					ospecs = append(ospecs, clone)
					entryOf[clone] = c
				}
			case *Function:
				if compiled != nil && compiled.V1Filter != nil {
					oqueries = append(oqueries, compiled)
					entryOf[compiled] = c
				}
			}
		}
		al := &authoringLowering{engine: e, owner: owner, layers: []*AuthoredRuntimeRegistry{staged}}
		oscope := al.scope(e.concepts, nil, nil)
		oscope.bindingsResolved = true
		for _, f := range lowerPushdownSet(ospecs, oqueries, oscope) {
			var key any = f.fn
			if f.spec != nil {
				key = f.spec
			}
			if c := entryOf[key]; c != nil {
				_ = staged.Retire(owner, c.Kind, c.Name)
				quarantine(c.Kind, c.Name, owner, f.err)
			}
		}
	}
	return quarantined
}

// DefineSessionBundle is the engine's session define: AuthorSessionBundle with
// this engine's registries in the lowering scope, so a query, spec or trait
// that does not lower is refused HERE, with its diagnostic, rather than at its
// first call. The owner's staged constructs and the session's own resolve
// beside the core ones, at the precedence execution resolves them at. Every
// production define goes through it: the MCP define tool, the gRPC define,
// work templates, and the durable stage and promote of a bundle.
func (e *MemQLEngine) DefineSessionBundle(reg *AuthoredRuntimeRegistry, owner, bundleSource, origin string) (SessionDefineResult, error) {
	if e == nil {
		return AuthorSessionBundle(reg, owner, bundleSource, origin)
	}
	return authorSessionBundle(&authoringLowering{engine: e, owner: owner, layers: []*AuthoredRuntimeRegistry{e.stagedAuthored, reg}},
		reg, owner, bundleSource, origin)
}

// ValidateAuthoredBundle is ValidateBundle with this engine's registries in the
// lowering scope: the Gate-1 report a validate returns, including every
// refusal the lowering makes. Read-only, like ValidateBundle.
func (e *MemQLEngine) ValidateAuthoredBundle(bundleSource, origin string) SandboxReport {
	constructs := WithOrigin(SplitBundleSource(bundleSource), origin)
	if e == nil || len(constructs) == 0 {
		return ValidateBundle(bundleSource, origin)
	}
	rep, _, _ := compileBundleWith(constructs, &authoringLowering{engine: e})
	return rep
}

// promoteOrder is a bundle's constructs in the order their registration
// depends on: concepts (every binding names one), then specs and traits
// (queries apply them, and a promote checks each query against the specs
// already registered), then everything else, each group in source order.
func promoteOrder(constructs []SandboxConstruct) []SandboxConstruct {
	rank := func(kind string) int {
		switch kind {
		case "concept":
			return 0
		case "spec", "trait":
			return 1
		}
		return 2
	}
	out := make([]SandboxConstruct, 0, len(constructs))
	for r := 0; r <= 2; r++ {
		for _, c := range constructs {
			if rank(c.Kind) == r {
				out = append(out, c)
			}
		}
	}
	return out
}
