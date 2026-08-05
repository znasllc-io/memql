package memql

import (
	"fmt"
	"slices"
	"strings"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/dslfs"
)

// ConceptResolver resolves symbolic concept references in a parsed .memql file
// to their canonical concept ID strings (e.g., "v1:cognition:participant").
//
// Resolution happens after parsing and before execution/compilation, so the
// runtime sees the same canonical strings as before. Old string-literal syntax
// continues to work unchanged.
type ConceptResolver struct {
	registry memoryNodes.Registry
}

// NewConceptResolver creates a resolver backed by the given concept registry.
func NewConceptResolver(registry memoryNodes.Registry) *ConceptResolver {
	return &ConceptResolver{registry: registry}
}

// ResolveFile resolves all use declarations and symbolic references in a parsed file.
// versionContext is derived from the file's filesystem path (e.g., "v1" for files in v1/).
// The file's AST is modified in place.
func (r *ConceptResolver) ResolveFile(file *languageParser.File, versionContext string) error {
	return r.ResolveFileWithSignatureConcepts(file, versionContext, nil)
}

// ResolveFileWithSignatureConcepts is the canonical entry point post-
// PR-48: it builds the symbol table from the file-top `use ...`
// declarations and the concept names captured from the construct
// SIGNATURE (`mutation <Concept> <name> { ... }`). The caller passes
// the bare-name list it pulled from the pre-NormaliseAll source via
// extractAllSignatureConceptNames.
func (r *ConceptResolver) ResolveFileWithSignatureConcepts(file *languageParser.File, versionContext string, signatureConcepts []string) error {
	return r.ResolveFileWithSignatureConceptsInDomain(file, versionContext, signatureConcepts, "")
}

// ResolveFileWithSignatureConceptsInDomain is ResolveFileWithSignatureConcepts
// with the #2617 ambient-domain rule: `domain` is the file's own domain
// directory, and it becomes the namespace hint for any signature concept no
// file-top import names -- same-domain constructs are in scope without a
// `use` line. An explicit import always wins (namespaceHintForName is
// consulted first), and the resolver's ambiguity error is unchanged when
// the hint cannot disambiguate.
func (r *ConceptResolver) ResolveFileWithSignatureConceptsInDomain(file *languageParser.File, versionContext string, signatureConcepts []string, domain string) error {
	if file == nil {
		return nil
	}

	// Build the symbol table from `use <ns>.<concept>` declarations.
	symbols, err := r.resolveUseDeclarations(file.Uses, versionContext)
	if err != nil {
		return fmt.Errorf("resolving use declarations: %w", err)
	}

	// Augment the symbol table with concepts named in construct
	// signatures (`<kind> <Concept> <name> { ... }`).
	for _, bareName := range signatureConcepts {
		if _, dup := symbols[bareName]; dup {
			continue
		}
		nsHint := namespaceHintForName(file.Uses, bareName)
		if nsHint == "" {
			nsHint = domain // ambient same-domain scope (#2617)
		}
		resolvedId, err := r.resolveBareConceptNameWithNamespace(bareName, nsHint)
		if err != nil {
			return fmt.Errorf("signature concept %q: %w", bareName, err)
		}
		symbols[bareName] = &symbolEntry{
			leafName:   bareName,
			resolvedId: resolvedId,
			fullPath:   "signature(" + bareName + ")",
		}
	}

	if len(symbols) == 0 {
		return nil
	}

	// Walk the AST and replace symbolic references
	for _, def := range file.Definitions {
		if err := r.resolveDefinition(def, symbols); err != nil {
			return err
		}
	}

	return nil
}

// resolveBareConceptName looks up a bare concept name (e.g. "space")
// in the registry and returns the canonical id (e.g.
// "v1:cognition:space"). Match rule: the canonical id's trailing
// segment (after the last ':') equals the bare name. When the
// trailing segment is ambiguous across namespaces it is a genuine
// collision the caller must resolve -- use
// resolveBareConceptNameWithNamespace to pass the disambiguating
// namespace hint from the importing `use` path.
func (r *ConceptResolver) resolveBareConceptName(name string) (string, error) {
	return r.resolveBareConceptNameWithNamespace(name, "")
}

// resolveBareConceptNameWithNamespace resolves a bare concept name to
// its canonical id by trailing-segment match, disambiguating an
// ambiguous trailing segment with nsHint -- the namespace segment of
// the importing `use` path (e.g. "planner" from
// `use planner.concepts.{ plan }`). This is what lets two concepts
// that share a trailing segment across namespaces coexist:
// v1:planner:plan and v1:harness:plan both end in ":plan", and a
// `query plan ...` in dsl/planner/ binds to the planner one because
// its file imports `planner.concepts.{ plan }`. An empty nsHint keeps
// the strict behaviour: an ambiguous trailing segment is an error.
func (r *ConceptResolver) resolveBareConceptNameWithNamespace(name, nsHint string) (string, error) {
	if r.registry == nil {
		return "", fmt.Errorf("concept registry not available")
	}
	all := r.registry.List()
	var matches []string
	for _, c := range all {
		if c == nil {
			continue
		}
		idx := strings.LastIndex(c.Name, ":")
		if idx < 0 {
			continue
		}
		if c.Name[idx+1:] == name {
			matches = append(matches, c.Name)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no registered concept has trailing segment %q", name)
	case 1:
		return matches[0], nil
	}
	// Ambiguous trailing segment. Disambiguate by the namespace hint
	// from the importing `use` path when one is available: keep only
	// matches whose NAMESPACE is the hint, or a colon-scoped extension
	// of it ("cognition" keeps v1:cognition:client:tool:request).
	//
	// Anchored at the namespace, not a `:planner:` substring search of
	// the whole id (memql#3026 landing review). The substring form
	// matched an INTERIOR segment, so a hint of "tool" kept
	// v1:cognition:client:tool:request alongside a bundle's own
	// v1:tool:request and the filter reduced two candidates to two --
	// leaving the name ambiguous, and the caller then demanding the
	// same-domain import TestNoSameDomainUse bans. That is the #2976
	// deadlock, and it defeated the anchoring in canonicalId's ambient
	// gate, which never got to run because this returned an error first.
	if nsHint != "" {
		var nsMatches []string
		for _, m := range matches {
			if ns := idNamespace(m); ns == nsHint || strings.HasPrefix(ns, nsHint+":") {
				nsMatches = append(nsMatches, m)
			}
		}
		if len(nsMatches) == 1 {
			return nsMatches[0], nil
		}
	}
	return "", fmt.Errorf("ambiguous concept name %q matches %d concepts: %s", name, len(matches), strings.Join(matches, ", "))
}

// namespaceHintForName returns the namespace segment of the file-top
// `use` import that brings <name> into scope (e.g. "planner" for
// `use planner.concepts.{ plan }`), or "" when no Form B import names
// it. Used to disambiguate a signature-bound concept whose bare
// trailing segment collides across namespaces.
func namespaceHintForName(uses []*languageParser.UseDeclaration, name string) string {
	for _, u := range uses {
		if u == nil || len(u.Parts) == 0 {
			continue
		}
		if slices.Contains(u.Names, name) {
			return u.Parts[0]
		}
	}
	return ""
}

// symbolEntry maps a short name to a resolved concept ID.
type symbolEntry struct {
	leafName   string // Short name used in code: "participant", "cognitionSession"
	resolvedId string // Canonical ID: "v1:cognition:participant"
	fullPath   string // Original dotted path: "cognition.participant"
}

// resolveUseDeclarations resolves each use declaration to a canonical concept ID
// and returns a symbol table keyed by leaf name.
//
// Form B (`use foo.bar.{ name1, name2 }`) resolves each Name via
// trailing-segment match against the registered concept list. The
// path itself is just a module hint and doesn't have to align with
// the canonical concept id. The kind of construct an imported name
// references (concept / shape / trait / spec / mutation / query /
// logic / builtin / prompt / provider / tool) is determined by the
// concept registry's trailing-segment lookup -- non-concept names
// are tolerated and pass through without a registered concept entry.
//
// Form A (`use cognition.space`) is retired but the legacy path is
// still here for one-off callers that haven't migrated; it treats
// the full dotted path as the concept id.
func (r *ConceptResolver) resolveUseDeclarations(uses []*languageParser.UseDeclaration, version string) (map[string]*symbolEntry, error) {
	symbols := make(map[string]*symbolEntry, len(uses))

	for _, u := range uses {
		if len(u.Names) > 0 {
			// Form B: resolve each Name via trailing-segment match.
			// The path is a module hint -- not a concept id -- but its
			// leading segment (e.g. "planner" in "planner.concepts")
			// disambiguates a trailing segment that collides across
			// namespaces (v1:planner:plan vs v1:harness:plan).
			nsHint := ""
			if len(u.Parts) > 0 {
				nsHint = u.Parts[0]
			}
			for _, name := range u.Names {
				if _, dup := symbols[name]; dup {
					continue
				}
				resolvedId, err := r.resolveBareConceptNameWithNamespace(name, nsHint)
				if err != nil {
					// Many Form B imports name non-concept constructs
					// (shapes / traits / specs / mutations / queries
					// / logic / builtins). Those don't resolve to a
					// concept id and that's fine -- they're symbols
					// the per-file translator handles separately.
					// Register a name-only entry so the symbol table
					// can still be consulted for collision-detection.
					symbols[name] = &symbolEntry{
						leafName: name,
						fullPath: u.Path + "." + name,
					}
					continue
				}
				symbols[name] = &symbolEntry{
					leafName:   name,
					resolvedId: resolvedId,
					fullPath:   u.Path + "." + name,
				}
			}
			continue
		}

		// Form A (legacy): the path IS the concept id.
		parts := u.Parts
		resolvedVersion := version
		startIdx := 0

		if len(parts) > 0 && len(parts[0]) >= 2 && parts[0][0] == 'v' && parts[0][1] >= '0' && parts[0][1] <= '9' {
			resolvedVersion = parts[0]
			startIdx = 1
		}

		if len(parts)-startIdx < 2 {
			return nil, fmt.Errorf("use declaration %q requires at least domain.entity", u.Path)
		}

		conceptParts := parts[startIdx:]
		conceptId := resolvedVersion + ":" + strings.Join(conceptParts, ":")

		if _, err := r.registry.Get(conceptId); err != nil {
			return nil, fmt.Errorf("use %s: concept %q not found in registry", u.Path, conceptId)
		}

		u.ResolvedId = conceptId

		leafName := u.LeafName()
		if existing, ok := symbols[leafName]; ok {
			return nil, fmt.Errorf("ambiguous reference %q: matches %s and %s (use 'as' alias to disambiguate)",
				leafName, existing.fullPath, u.Path)
		}

		symbols[leafName] = &symbolEntry{
			leafName:   leafName,
			resolvedId: conceptId,
			fullPath:   u.Path,
		}
	}

	return symbols, nil
}

// resolveDefinition resolves concept references within a single definition node.
func (r *ConceptResolver) resolveDefinition(node languageParser.Node, symbols map[string]*symbolEntry) error {
	switch def := node.(type) {
	case *languageParser.FunctionDef:
		return r.resolveFunctionDef(def, symbols)
	default:
		return nil
	}
}

// resolveFunctionDef resolves concept references in a function definition.
func (r *ConceptResolver) resolveFunctionDef(def *languageParser.FunctionDef, symbols map[string]*symbolEntry) error {
	// Resolve trigger on= references in attributes
	for _, attr := range def.Attributes {
		if err := r.resolveAttribute(attr, symbols); err != nil {
			return fmt.Errorf("function %q: %w", def.Name, err)
		}
	}

	// Resolve the function body
	if def.Body != nil {
		if err := r.resolveNode(def.Body, symbols); err != nil {
			return fmt.Errorf("function %q: %w", def.Name, err)
		}
	}

	return nil
}

// resolveAttribute resolves concept references in an attribute.
// Handles @trigger(on=participant.created) -> @trigger(event="graph.node.created.v1:cognition:participant").
func (r *ConceptResolver) resolveAttribute(attr *languageParser.Attribute, symbols map[string]*symbolEntry) error {
	if attr.Name != languageParser.AttrTrigger {
		return nil
	}

	onVal, hasOn := attr.Args["on"]
	if !hasOn {
		return nil
	}

	// on=participant.created -> split into concept ref + event type
	onStr, ok := onVal.(string)
	if !ok {
		return fmt.Errorf("@trigger on= value must be a dotted identifier, got %T", onVal)
	}

	// Split: "participant.created" -> conceptRef="participant", eventType="created"
	lastDot := strings.LastIndex(onStr, ".")
	if lastDot < 0 {
		return fmt.Errorf("@trigger on= value %q must be <concept>.<eventType> (e.g., participant.created)", onStr)
	}

	conceptRef := onStr[:lastDot]
	eventType := onStr[lastDot+1:]

	// Validate event type
	var eventAction string
	switch eventType {
	case "created":
		eventAction = "created"
	case "updated":
		eventAction = "updated"
	case "deleted":
		eventAction = "deleted"
	default:
		return fmt.Errorf("@trigger on= unknown event type %q (expected created, updated, or deleted)", eventType)
	}

	// Resolve the concept reference
	conceptId, err := r.resolveConceptRef(conceptRef, symbols)
	if err != nil {
		return fmt.Errorf("@trigger on=%s: %w", onStr, err)
	}

	// Replace on= with event= using the canonical topic format
	// graph.node.{action}.{concept}.
	eventTopic := fmt.Sprintf("graph.node.%s.%s", eventAction, conceptId)
	delete(attr.Args, "on")
	attr.Args["event"] = eventTopic

	return nil
}

// resolveNode walks an AST node and resolves concept references.
func (r *ConceptResolver) resolveNode(node languageParser.Node, symbols map[string]*symbolEntry) error {
	switch n := node.(type) {
	case *languageParser.MutationStmt:
		return r.resolveMutationStmt(n, symbols)
	case *languageParser.ComparisonExpr:
		return r.resolveComparisonExpr(n, symbols)
	case *languageParser.LogicalExpr:
		if err := r.resolveNode(n.Left, symbols); err != nil {
			return err
		}
		return r.resolveNode(n.Right, symbols)
	case *languageParser.RelationshipExpr:
		return r.resolveNode(n.Target, symbols)
	case *languageParser.SortExpr:
		return r.resolveNode(n.Target, symbols)
	case *languageParser.PaginateExpr:
		return r.resolveNode(n.Target, symbols)
	case *languageParser.SelectExpr:
		return r.resolveNode(n.Target, symbols)
	case *languageParser.TimestampExpr:
		return r.resolveNode(n.Target, symbols)
	case *languageParser.DepthExpr:
		return r.resolveNode(n.Target, symbols)
	case *languageParser.CountExpr:
		return r.resolveNode(n.Target, symbols)
	case *languageParser.ShapeExpr:
		return r.resolveNode(n.Target, symbols)
	case *languageParser.ConditionalFilterExpr:
		return r.resolveNode(n.Filter, symbols)
	case *languageParser.ReturnStmt:
		for _, result := range n.Results {
			if err := r.resolveNode(result, symbols); err != nil {
				return err
			}
		}
	case *languageParser.AssignStmt:
		if n.Value != nil {
			return r.resolveNode(n.Value, symbols)
		}
	case *languageParser.IfStmt:
		if n.Condition != nil {
			if err := r.resolveNode(n.Condition, symbols); err != nil {
				return err
			}
		}
		for _, stmt := range n.Then {
			if err := r.resolveNode(stmt, symbols); err != nil {
				return err
			}
		}
		for _, stmt := range n.Else {
			if err := r.resolveNode(stmt, symbols); err != nil {
				return err
			}
		}
	case *languageParser.ForRangeStmt:
		if n.Collection != nil {
			if err := r.resolveNode(n.Collection, symbols); err != nil {
				return err
			}
		}
		for _, stmt := range n.Body {
			if err := r.resolveNode(stmt, symbols); err != nil {
				return err
			}
		}
	case *languageParser.SwitchStmt:
		if n.Expr != nil {
			if err := r.resolveNode(n.Expr, symbols); err != nil {
				return err
			}
		}
		for _, c := range n.Cases {
			for _, stmt := range c.Body {
				if err := r.resolveNode(stmt, symbols); err != nil {
					return err
				}
			}
		}
		for _, stmt := range n.Default {
			if err := r.resolveNode(stmt, symbols); err != nil {
				return err
			}
		}
	}
	// Unrecognized node types are left as-is (no concept references to resolve)
	return nil
}

// resolveMutationStmt resolves the concept reference in an insert() statement.
// If the concept is a symbolic reference (not containing ':'), resolve it.
//
// An empty stmt.Concept means the `insert(...)` call omitted the concept
// argument entirely and relies on the implicit binding from the file's
// single `use` declaration. That binding is applied downstream in the
// function loader (see `if mutationConcept == "" && boundConcept != ""`)
// after this resolver runs, so we leave empty concepts alone here instead
// of erroring on them.
func (r *ConceptResolver) resolveMutationStmt(stmt *languageParser.MutationStmt, symbols map[string]*symbolEntry) error {
	// Skip if concept is already a canonical ID (contains ':')
	if strings.Contains(stmt.Concept, ":") {
		return nil
	}

	// Skip argumentless insert() calls; BoundConcept fill runs later.
	if strings.TrimSpace(stmt.Concept) == "" {
		return nil
	}

	resolved, err := r.resolveConceptRef(stmt.Concept, symbols)
	if err != nil {
		return fmt.Errorf("insert(%s): %w", stmt.Concept, err)
	}
	stmt.Concept = resolved
	return nil
}

// resolveComparisonExpr resolves concept== comparisons.
// If the field is "concept" and the value is a symbolic reference, resolve it.
func (r *ConceptResolver) resolveComparisonExpr(expr *languageParser.ComparisonExpr, symbols map[string]*symbolEntry) error {
	if expr.Field.Raw != "concept" || expr.Operator != languageParser.OpEq {
		return nil
	}

	val, ok := expr.Value.(string)
	if !ok {
		return nil
	}

	// Skip if already a canonical ID (contains ':')
	if strings.Contains(val, ":") {
		return nil
	}

	resolved, err := r.resolveConceptRef(val, symbols)
	if err != nil {
		return fmt.Errorf("concept==%s: %w", val, err)
	}
	expr.Value = resolved
	return nil
}

// resolveConceptRef resolves a symbolic concept reference to a canonical ID.
// Supports:
//   - Simple leaf name: "participant" -> looks up in symbols
//   - Qualified name: "cognition.participant" -> looks up by full path in symbols
func (r *ConceptResolver) resolveConceptRef(ref string, symbols map[string]*symbolEntry) (string, error) {
	// First try exact leaf name match
	if entry, ok := symbols[ref]; ok {
		return entry.resolvedId, nil
	}

	// Try qualified path match (e.g., "cognition.participant")
	for _, entry := range symbols {
		if entry.fullPath == ref {
			return entry.resolvedId, nil
		}
	}

	return "", fmt.Errorf("unresolved concept reference %q (not declared in any 'use' statement)", ref)
}

// VersionFromFilePath and DomainFromFilePath are thin re-exports of the
// canonical implementations in component/memql/dslfs (memql#2852).
//
// They moved there because dslimports needs the SAME answer and cannot import
// this package -- component/memql imports component/memql/dslimports, so the
// dependency only runs one way. Two copies is exactly what #2852 reports: boot
// walked from the last directory segment, the lint took the first, and
// agents/tools/askSpecialist.memql resolved to "tools" in one and "agents" in
// the other.
//
// Kept as wrappers rather than updating every call site, because the names are
// used across this package and the indirection costs nothing.
func VersionFromFilePath(path string) string { return dslfs.VersionFromFilePath(path) }

func DomainFromFilePath(path string) string { return dslfs.DomainFromFilePath(path) }

func RootDomainFromFilePath(path string) string { return dslfs.RootDomainFromFilePath(path) }
