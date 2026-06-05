package memql

// authoring_resolver.go -- the core-first, owner-scoped construct resolver
// (epic memql#954, issue #959).
//
// At execution time a construct name resolves against TWO layers:
//
//  1. CORE -- the sealed DSL loaded at engine Init from the embedded tree.
//     Immutable, authoritative, shared by every owner.
//  2. AUTHORED -- the per-owner mutable layer (AuthoredRuntimeRegistry).
//
// Precedence is CORE FIRST, ALWAYS. An authored construct can never shadow a
// core one: if core defines `name`, the authored layer is not even consulted.
// This is the one-way guarantee that keeps a malformed or malicious authored
// construct from overriding sealed engine behavior -- it can only ADD
// owner-private capability, never redefine the platform.
//
// The resolver is deliberately decoupled from the concrete engine registries:
// it takes a `coreHas` predicate so it can be unit-tested in isolation and so
// the authored layer never has to know the shape of the core function / spec
// / shape registries. EngineCoreHas (below) is the production predicate built
// from a live *MemQLEngine.

// ConstructResolution is the outcome of resolving a construct name for an
// owner: which layer answered, and (for an authored hit) the live entry.
type ConstructResolution struct {
	// Source is "core", "authored", or "" (unresolved).
	Source string
	// Authored is the live authored entry when Source=="authored"; nil
	// otherwise. A core hit carries no payload here -- core resolution flows
	// through the existing engine registries, unchanged.
	Authored *AuthoredConstruct
}

// CoreHasFunc reports whether the SEALED core layer defines a construct of the
// given kind+name. It must consult only the immutable core registries (never
// the authored layer). EngineCoreHas builds one from a live engine.
type CoreHasFunc func(kind, name string) bool

// ResolveConstruct applies the core-first precedence for one owner. It returns
// a core resolution the instant core defines the name (the authored layer is
// not consulted), then an authored resolution if the owner has an ACTIVE
// authored construct, then an unresolved result.
//
// owner must be the authenticated actor's userId -- the authored layer is
// owner-scoped, so passing a different owner can never reach another owner's
// constructs (the registry key includes the owner).
func ResolveConstruct(coreHas CoreHasFunc, authored *AuthoredRuntimeRegistry, owner, kind, name string) ConstructResolution {
	// Core first, always. A core hit short-circuits -- the authored layer is
	// never consulted, so it cannot shadow sealed engine behavior.
	if coreHas != nil && coreHas(kind, name) {
		return ConstructResolution{Source: "core"}
	}

	// Core missed: fall through to the owner's authored layer.
	if authored != nil && owner != "" {
		if entry, ok := authored.Resolve(owner, kind, name); ok {
			return ConstructResolution{Source: "authored", Authored: entry}
		}
	}

	return ConstructResolution{}
}

// EngineCoreHas builds the production core-membership predicate from a live
// engine. It reports membership against the sealed core registries the engine
// loaded at Init -- functions (query / mutation / logic / builtin) and specs.
// It NEVER consults the authored layer, preserving the one-way precedence.
//
// Kinds map onto the engine's core registries:
//   - query / mutation / logic / builtin -> the function registry
//   - spec / trait                       -> the spec registry
//
// Any other kind reports false (core does not own it through these
// registries), so the authored layer is free to define it. As the authored
// runtime grows to cover more kinds, extend this mapping alongside.
func EngineCoreHas(e *MemQLEngine) CoreHasFunc {
	return func(kind, name string) bool {
		if e == nil {
			return false
		}
		switch kind {
		case "query", "mutation", "logic", "builtin":
			if fns := e.Functions(); fns != nil {
				if fn, err := fns.Get(name); err == nil && fn != nil {
					return true
				}
			}
			return false
		case "spec", "trait":
			if specs := e.Specs(); specs != nil {
				if _, ok := specs.Lookup(name); ok {
					return true
				}
			}
			return false
		default:
			return false
		}
	}
}
