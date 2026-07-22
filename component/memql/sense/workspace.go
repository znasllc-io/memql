package sense

// workspace.go defines the cross-reference resolution seam for the language
// service. Where RegistryProvider is a FLAT vocabulary (concept/function/shape
// names, no structure), WorkspaceGraph answers STRUCTURED questions about the
// .memql workspace -- does <ns>.<kind> resolve to a module, is an id declared
// there, does a concept exist -- plus the enumerations completion needs
// (namespaces, kinds, module symbols). It is the source of truth for import and
// reference diagnostics (which construct names another symbol) and for
// segment-aware completion on the `use` line.
//
// The graph is built from the dslimports Tree -- the same file/tree resolution
// `memqllint` uses -- and handed to the Service alongside the RegistryProvider.

// Resolved is the tri-state outcome of a workspace reference query. The third
// state -- ResolvedUnknown -- is load-bearing: a language server sees a single,
// possibly-stale buffer and a workspace that need not mount every namespace a
// file imports (a product bundle imports engine namespaces it does not carry).
// When a fact cannot be PROVEN from the workspace, the answer is Unknown and the
// caller must stay silent, never emit a false diagnostic. This mirrors the
// load-side verifier's own conservatism (dslimports missingIsProvable /
// external-namespace skip).
type Resolved int

const (
	// ResolvedUnknown -- the workspace cannot decide (namespace not mounted
	// here, target not visible, or no workspace graph at all). Inconclusive.
	ResolvedUnknown Resolved = iota
	// ResolvedYes -- proven to exist / resolve in the workspace.
	ResolvedYes
	// ResolvedNo -- proven absent in a namespace the workspace fully owns.
	ResolvedNo
)

// WorkspaceGraph answers cross-reference questions about the .memql workspace as
// a whole. A nil graph degrades to a no-op that answers Unknown/empty
// everywhere (see noWorkspace), so consumers never need a nil check.
type WorkspaceGraph interface {
	// ModuleResolves reports whether <ns>.<kind> (e.g. "fylo","concepts")
	// resolves to a workspace module file. ResolvedNo means the namespace is
	// owned by the workspace but the kind segment names no module.
	ModuleResolves(ns, kind string) Resolved
	// SymbolDeclared reports whether id is declared in module <ns>.<kind>.
	SymbolDeclared(ns, kind, id string) Resolved
	// ConceptExists reports whether a concept named `name` exists; nsHint, when
	// non-empty, scopes the match to that namespace.
	ConceptExists(name, nsHint string) Resolved
	// Namespaces returns the workspace's top-level namespaces (for completion).
	Namespaces() []string
	// Kinds returns the module kind segments under ns (for completion).
	Kinds(ns string) []string
	// SymbolsInModule returns the ids importable from <ns>.<kind> (for
	// completion).
	SymbolsInModule(ns, kind string) []string
}

// noWorkspace is the nil-object WorkspaceGraph: it knows nothing, so every query
// is inconclusive/empty. Stored whenever a Service is built without a graph (the
// registry-less fallback, or every existing New(rp) caller), letting consumers
// treat s.workspace as always non-nil.
type noWorkspace struct{}

func (noWorkspace) ModuleResolves(string, string) Resolved         { return ResolvedUnknown }
func (noWorkspace) SymbolDeclared(string, string, string) Resolved { return ResolvedUnknown }
func (noWorkspace) ConceptExists(string, string) Resolved          { return ResolvedUnknown }
func (noWorkspace) Namespaces() []string                           { return nil }
func (noWorkspace) Kinds(string) []string                          { return nil }
func (noWorkspace) SymbolsInModule(string, string) []string        { return nil }
