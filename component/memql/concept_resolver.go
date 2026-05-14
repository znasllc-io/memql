package memql

import (
	"fmt"
	"strings"

	memoryNodes "github.com/visionarys-io/memql/component/database/memory-nodes"
	languageParser "github.com/visionarys-io/memql/component/language/parser"
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
	if file == nil {
		return nil
	}

	// Build the symbol table from `use <ns>.<concept>` declarations.
	symbols, err := r.resolveUseDeclarations(file.Uses, versionContext)
	if err != nil {
		return fmt.Errorf("resolving use declarations: %w", err)
	}

	// Augment the symbol table with `@useConcept(<bareName>)` annotations
	// on function definitions. The bare name is resolved by trailing-
	// segment match against the registered concept list. This is the
	// canonical concept-binding form going forward; the `use` directive
	// is on its way out (G.3.g).
	if err := r.collectUseConceptAnnotations(file, symbols); err != nil {
		return fmt.Errorf("resolving @useConcept annotations: %w", err)
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

// collectUseConceptAnnotations walks every function definition in the
// file, picks up `@useConcept(name1, name2, ...)` attribute targets,
// resolves each bare name against the concept registry by trailing-
// segment match, and adds the resulting symbol entries to `symbols`.
//
// Resolution rule: a bare name resolves to a concept whose canonical id
// ends with `:<name>` (case-insensitive comparison). Zero matches or
// more than one match is a load-time error — the caller should fix the
// annotation to disambiguate or add the concept first.
//
// Collisions with the file's `use` declarations are an error so authors
// don't accidentally bind the same short name twice.
func (r *ConceptResolver) collectUseConceptAnnotations(file *languageParser.File, symbols map[string]*symbolEntry) error {
	if file == nil || len(file.Definitions) == 0 {
		return nil
	}
	for _, def := range file.Definitions {
		fn, ok := def.(*languageParser.FunctionDef)
		if !ok {
			continue
		}
		for _, attr := range fn.Attributes {
			if attr == nil || attr.Name != languageParser.AttrUseConcept {
				continue
			}
			for _, bareName := range attr.UseTargets() {
				resolvedId, err := r.resolveBareConceptName(bareName)
				if err != nil {
					return fmt.Errorf("function %q: @useConcept(%s): %w", fn.Name, bareName, err)
				}
				if existing, dup := symbols[bareName]; dup {
					if existing.resolvedId != resolvedId {
						return fmt.Errorf("function %q: @useConcept(%s) conflicts with `use %s` (resolves to %q vs %q)", fn.Name, bareName, existing.fullPath, resolvedId, existing.resolvedId)
					}
					continue
				}
				symbols[bareName] = &symbolEntry{
					leafName:   bareName,
					resolvedId: resolvedId,
					fullPath:   "@useConcept(" + bareName + ")",
				}
			}
		}
	}
	return nil
}

// resolveBareConceptName looks up a bare concept name (e.g. "space")
// in the registry and returns the canonical id (e.g.
// "v1:cognition:space"). Match rule: the canonical id's trailing
// segment (after the last ':') equals the bare name. Concept names
// are globally unique by trailing segment in this codebase, so
// ambiguity is a genuine collision the caller must resolve by
// renaming or qualifying.
func (r *ConceptResolver) resolveBareConceptName(name string) (string, error) {
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
	default:
		return "", fmt.Errorf("ambiguous concept name %q matches %d concepts: %s", name, len(matches), strings.Join(matches, ", "))
	}
}

// symbolEntry maps a short name to a resolved concept ID.
type symbolEntry struct {
	leafName   string // Short name used in code: "participant", "cognitionSession"
	resolvedId string // Canonical ID: "v1:cognition:participant"
	fullPath   string // Original dotted path: "cognition.participant"
}

// resolveUseDeclarations resolves each use declaration to a canonical concept ID
// and returns a symbol table keyed by leaf name.
func (r *ConceptResolver) resolveUseDeclarations(uses []*languageParser.UseDeclaration, version string) (map[string]*symbolEntry, error) {
	symbols := make(map[string]*symbolEntry, len(uses))

	for _, u := range uses {
		// Determine if the path includes an explicit version
		parts := u.Parts
		resolvedVersion := version
		startIdx := 0

		// Check if first part looks like a version (v1, v2, etc.)
		if len(parts) > 0 && len(parts[0]) >= 2 && parts[0][0] == 'v' && parts[0][1] >= '0' && parts[0][1] <= '9' {
			resolvedVersion = parts[0]
			startIdx = 1
		}

		if len(parts)-startIdx < 2 {
			return nil, fmt.Errorf("use declaration %q requires at least domain.entity", u.Path)
		}

		// Build canonical concept ID: version:domain:entity[:sub...]
		// Dots map to colons: cognition.space.context -> v1:cognition:space:context
		conceptParts := parts[startIdx:]
		conceptId := resolvedVersion + ":" + strings.Join(conceptParts, ":")

		// Validate the concept exists in the registry
		if _, err := r.registry.Get(conceptId); err != nil {
			return nil, fmt.Errorf("use %s: concept %q not found in registry", u.Path, conceptId)
		}

		// Store the resolved ID back on the declaration
		u.ResolvedId = conceptId

		// Register in symbol table
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
// Handles @trigger(on=participant.created) -> @trigger(event="graph.node.created.*.v1:cognition:participant")
// The emitted pattern uses the current 5-segment partition-aware topic form
// (graph.node.{action}.{partition}.{concept}) with `*` as the partition
// wildcard so the trigger matches emitted CDC events across all partitions.
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

	// Replace on= with event= using the canonical 5-segment topic format.
	// The `*` in the partition position matches any partition (default, acme,
	// _system, etc.) and mirrors the convention used by .memql automation
	// triggers written with the explicit event= form. Emitting the 4-segment
	// form silently never matched because graph CDC events ship as
	// graph.node.{action}.{partition}.{concept}.
	eventTopic := fmt.Sprintf("graph.node.%s.*.%s", eventAction, conceptId)
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

// VersionFromFilePath extracts the version directory from a file path.
// Examples:
//
//	"v1/mutationJoinSpace.memql" -> "v1"
//	"mutations/v1/mutationJoinSpace.memql" -> "v1"
//	"automations/v1/cognition/autoJoinAI/automation.memql" -> "v1"
//
// Returns empty string if no version directory is found.
func VersionFromFilePath(path string) string {
	parts := strings.Split(path, "/")
	for _, part := range parts {
		if len(part) >= 2 && part[0] == 'v' && part[1] >= '0' && part[1] <= '9' {
			// Validate it's purely v<digits>
			allDigits := true
			for _, ch := range part[1:] {
				if ch < '0' || ch > '9' {
					allDigits = false
					break
				}
			}
			if allDigits {
				return part
			}
		}
	}
	return ""
}
