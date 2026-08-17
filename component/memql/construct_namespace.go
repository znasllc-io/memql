package memql

// Namespacing the twelve FLAT construct kinds (memql#3897).
//
// `concept` has been namespaced since forever -- its identity is
// `v1:<ns>:<name>`, so two namespaces may declare a `plan` and each resolves to
// its own. The other twelve -- query, mutation, logic, spec, trait, shape, tool,
// prompt, provider, builtin, policy, seed -- shared ONE flat registry keyed by
// the bare authored name, and the S5 uniqueness gate (memql#2360) refused a
// duplicate at load. So the model was implemented for one kind out of thirteen.
//
// WHAT THAT COST, CONCRETELY. A product DSL bundle mounted at MEMQL_DSL_PATH
// could not declare a flat-kind construct whose name a core construct already
// used. Every product shared one namespace with the engine, and the collision
// was a load-time error the product could not resolve except by renaming its own
// construct. Under platform consolidation (memql#2472) a product IS a DSL bundle
// plus a client, so the primary delivery path was hitting a constraint it had no
// way around. And aliasing could not help: memql#3802 shipped
// `use x.y.{ n as m }`, but it was INERT for these twelve, because two
// same-named flat constructs could not coexist in the first place. There was
// nothing to alias between. Namespacing is what makes aliasing mean anything for
// them, which is the real answer to "why can't I alias a shape".
//
// # The key is (namespace, name), spelled `<namespace>.<name>`
//
// A dot, not a colon, and deliberately: a colon is the CONCEPT id separator, and
// a flat construct is not a concept. `cognition.spaceParticipants` reads as what
// it is -- the module path an author already writes in a `use` line.
//
// The namespace comes from the construct's ORIGIN through
// dslfs.NamespaceFromFilePath, which is the same function ambient concept
// resolution and canonical-id assembly use (memql#3898). One rule, three
// consumers: a construct's namespace, a concept's namespace, and the ambient
// scope are the same answer to the same question, so they cannot disagree.
//
// # Resolution is Go's, and so is the failure mode
//
// A reference resolves in this order, and stops at the first hit:
//
//	1. the referencing file's OWN namespace     -- same-package, no import
//	2. an explicit `use` import                 -- including an alias
//
// and nothing else. There is no global fallback, because a global fallback is
// exactly the flat namespace this replaces. An unresolvable bare name is an
// error that NAMES THE IMPORT that would fix it -- the same shape the concept
// path already uses, so an author meets one vocabulary rather than two.
//
// Go again: every reference is either same-package or qualified through an
// import, so there is no unqualified cross-package lookup to be ambiguous. That
// is why the engine's own call sites pass a QUALIFIED name (see
// EngineQualified): once two namespaces may both hold `cognitionReply`, a bare
// lookup has no answer, and picking one would be the silent-capture defect
// memql#3802 fixed for concepts.

import (
	"fmt"
	"sort"
	"strings"

	languageAst "github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/core/dslfs"
)

// constructKeySeparator joins a namespace to a construct name.
//
// A DOT rather than a colon: colon is the concept-id separator and reusing it
// would make `cognition:spaceParticipants` look like a malformed concept id in
// every log line and error message. The dot is what an author already types in
// `use cognition.queries.{ spaceParticipants }`.
const constructKeySeparator = "."

// QualifyConstruct is the registry key for a construct.
//
// An EMPTY namespace yields the bare name unchanged, and that is load-bearing
// rather than a convenience: engine-internal constructs with no file origin
// (test fixtures, the authoring sandbox's synthetic candidates, Go-registered
// builtins that predate an origin) must stay reachable. They occupy the
// unnamespaced key space, which nothing else can collide with because every
// file-loaded construct has a namespace.
func QualifyConstruct(namespace, name string) string {
	namespace = strings.TrimSpace(namespace)
	name = strings.TrimSpace(name)
	if namespace == "" {
		return name
	}
	return namespace + constructKeySeparator + name
}

// SplitConstructKey is QualifyConstruct's inverse.
//
// Splits at the LAST separator, because a namespace is a PATH and may itself
// contain dots in the `use` spelling of a nested one. The construct name never
// contains a dot -- the parser refuses it -- so the last separator is
// unambiguously the join.
func SplitConstructKey(key string) (namespace, name string) {
	key = strings.TrimSpace(key)
	if i := strings.LastIndex(key, constructKeySeparator); i >= 0 {
		return key[:i], key[i+1:]
	}
	return "", key
}

// ConstructNamespaceForOrigin is the namespace a construct loaded from `origin`
// belongs to.
//
// Delegates to the SAME function ambient concept resolution and canonical-id
// assembly use (memql#3898). A second definition of "which namespace is this
// file in" is the drift core/dslfs exists to prevent, and it has already been
// the subject of three issues.
func ConstructNamespaceForOrigin(origin string) string {
	return dslfs.NamespaceFromFilePath(origin)
}

// EngineQualified names a construct the ENGINE'S OWN Go code refers to.
//
// Every such reference must be qualified (memql#3897). Once two namespaces may
// both declare `cognitionReply`, `Prompts().Get("cognitionReply")` has no
// answer, and resolving it to whichever one happened to load is the silent
// capture memql#3802 fixed for concepts -- arriving on the path that renders an
// agent's reply.
//
// It exists as a named function rather than string concatenation at each call
// site so the ~17 engine references are greppable as one set, and so a reader
// meeting `EngineQualified("cognition", "cognitionReply")` sees a deliberate
// namespace rather than a string somebody happened to spell with a dot.
//
// Consistent with the MEMQL_DSL_PATH rule that a product bundle may not take
// over a core namespace: the engine naming its own namespace explicitly is what
// makes a product's same-named construct coexist rather than shadow.
func EngineQualified(namespace, name string) string {
	return QualifyConstruct(namespace, name)
}

// ConstructScope is one file's view of the flat-kind name space: its own
// namespace, plus whatever its `use` lines brought in.
//
// Built once per file at load and carried on each construct, so RESOLUTION
// HAPPENS AT LOAD TIME. That placement is the whole design. Construct
// references inside a compiled body used to resolve at EXECUTION time against a
// global flat snapshot -- `newFunctionValidator(fns.Snapshot(), nil)` in
// engine.go, with no file context anywhere near it -- so there was no moment at
// which "which namespace is this reference in" could be asked. Resolving at load
// and storing the qualified name makes the runtime lookup context-free, exactly
// as baking canonical concept ids in at load already does.
type ConstructScope struct {
	// Namespace is the referencing file's own -- searched first, with no import.
	Namespace string
	// Imports maps a LOCAL name (the alias when there is one) to what it binds.
	Imports map[string]ImportBinding
}

// ImportBinding is what one imported local name resolves to.
//
// BOTH HALVES ARE NEEDED, and forgetting the second is a real bug rather than a
// tidiness point: `use tools.shapes.{ widgetFull as toolsWidget }` binds the
// LOCAL name `toolsWidget` to the construct `tools.widgetFull`. Carrying only
// the namespace would compose `tools.toolsWidget` -- a key nothing registered --
// so every aliased reference would silently fail to resolve, which is precisely
// the mechanism memql#3802 shipped to make aliasing work at all.
type ImportBinding struct {
	// Namespace the construct was imported from.
	Namespace string
	// SourceName is what the DECLARING namespace calls it, which is the half an
	// alias renames away from.
	SourceName string
}

// NewConstructScope builds a scope from a file's origin and its `use` lines.
//
// ALIASES ARE RESOLVED TO THEIR LOCAL NAME, which is what makes memql#3802's
// aliasing finally mean something for these twelve kinds: `use tools.shapes.{
// widget as toolsWidget }` binds `toolsWidget`, leaving a bare `widget` to mean
// this file's own. Before namespacing there was nothing to alias between,
// because two same-named flat constructs could not coexist.
func NewConstructScope(origin string, uses []*languageAst.UseDeclaration) ConstructScope {
	scope := ConstructScope{
		Namespace: ConstructNamespaceForOrigin(origin),
		Imports:   map[string]ImportBinding{},
	}
	for _, use := range uses {
		if use == nil {
			continue
		}
		ns := namespaceOfUsePath(use.Parts)
		if ns == "" {
			continue
		}
		for _, sourceName := range use.Names {
			local := sourceName
			if alias, ok := use.Aliases[sourceName]; ok && strings.TrimSpace(alias) != "" {
				local = alias
			}
			scope.Imports[local] = ImportBinding{Namespace: ns, SourceName: sourceName}
		}
	}
	return scope
}

// namespaceOfUsePath turns a dotted module path into a namespace.
//
// The LAST part is the construct FILE (`concepts`, `traits`, `queries`, ...) and
// everything before it is the namespace, slash-joined -- so `common.traits`
// yields "common" and `agents.tools.trainerTools` yields "agents/tools". That
// mirrors the on-disk layout the path is a spelling of, which is what keeps a
// nested namespace importable at all.
func namespaceOfUsePath(parts []string) string {
	if len(parts) < 2 {
		return ""
	}
	segments := make([]string, 0, len(parts)-1)
	for _, part := range parts[:len(parts)-1] {
		if strings.TrimSpace(part) == "" {
			continue
		}
		segments = append(segments, part)
	}
	return strings.Join(segments, "/")
}

// Resolve turns a bare reference into a registry key.
//
// `exists` is supplied by the caller rather than the registry being reached
// directly, because the twelve kinds live in eight registries and the rule must
// be identical across all of them -- one function, eight callers, rather than
// eight copies that drift.
//
// The search order is the whole contract: OWN NAMESPACE, then imports. A name
// already carrying a separator is taken as fully qualified and checked as-is,
// so an author (or the engine) may always be explicit.
func (s ConstructScope) Resolve(bare string, exists func(key string) bool) (string, bool) {
	bare = strings.TrimSpace(bare)
	if bare == "" {
		return "", false
	}
	// Already qualified: verify rather than re-resolve.
	if strings.Contains(bare, constructKeySeparator) {
		return bare, exists(bare)
	}
	if s.Namespace != "" {
		if key := QualifyConstruct(s.Namespace, bare); exists(key) {
			return key, true
		}
	}
	if binding, ok := s.Imports[bare]; ok {
		// The SOURCE name under the imported namespace, not the local one: an
		// alias renames the reference, never the construct.
		if key := QualifyConstruct(binding.Namespace, binding.SourceName); exists(key) {
			return key, true
		}
	}
	// The unnamespaced space: engine-internal constructs with no file origin.
	if exists(bare) {
		return bare, true
	}
	return "", false
}

// UnresolvedConstructError is the refusal an author reads, and it names the
// import that fixes it.
//
// A refusal that only says "not found" is the failure mode memql#3800 and
// memql#2976 both produced: the author knows a name did not bind and has no
// route to making it bind. The two candidate spellings below are the only two
// that can ever be right, so stating them IS the fix rather than a hint.
func UnresolvedConstructError(kind, bare string, scope ConstructScope, known []string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %q is not in scope", kind, bare)
	if scope.Namespace != "" {
		fmt.Fprintf(&b, " for namespace %q", scope.Namespace)
	}
	b.WriteString(" -- declare it in this namespace, or add a file-top ")
	if ns := namespaceHoldingName(bare, known); ns != "" {
		fmt.Fprintf(&b, "`use %s.<construct>.{ %s }` import (it is declared in %q)",
			strings.ReplaceAll(ns, "/", "."), bare, ns)
	} else {
		fmt.Fprintf(&b, "`use <namespace>.<construct>.{ %s }` import", bare)
	}
	return fmt.Errorf("%s", b.String())
}

// namespaceHoldingName finds where a bare name IS declared, so the refusal can
// name the namespace rather than leaving the author to search for it.
//
// Returns "" when the name is declared in more than one -- naming one of several
// would be a guess, and a guess in an error message is worse than silence
// because it reads as authoritative.
func namespaceHoldingName(bare string, known []string) string {
	var found string
	for _, key := range known {
		ns, name := SplitConstructKey(key)
		if name != bare || ns == "" {
			continue
		}
		if found != "" && found != ns {
			return ""
		}
		found = ns
	}
	return found
}

// NamespacesDeclaring lists every namespace declaring a bare name, sorted.
// Used by the collision diagnostics and by the S5 per-namespace gate.
func NamespacesDeclaring(bare string, known []string) []string {
	seen := map[string]struct{}{}
	for _, key := range known {
		ns, name := SplitConstructKey(key)
		if name == bare {
			seen[ns] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for ns := range seen {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}
