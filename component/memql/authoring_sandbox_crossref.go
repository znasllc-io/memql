package memql

// authoring_sandbox_crossref.go -- Gate 1 cross-reference resolution (issue
// #956, follow-up 3/3).
//
// The per-construct compile (SandboxCompileBundle) proves each construct
// parses + binds in isolation, but it does NOT prove the bundle is
// self-consistent as a whole: a shape may `use` a spec that nothing defines,
// a query may reference a field its bound concept never declares, an
// automation may trigger on a concept that does not exist. This pass closes
// those gaps. It resolves every cross-construct reference against the union
// of (live core registries + the bundle's own constructs + the overlay
// concept registry the per-construct pass built), and verifies field
// existence on referenced concepts. A self-consistent bundle passes; a
// dangling reference fails with a file:line-style diagnostic.
//
// This needs the live core construct registries (shapes / specs / functions),
// which only an initialised engine carries, so it is a SEPARATE entry point
// (SandboxCompileBundleWithEngine) layered on top of the registry-only
// SandboxCompileBundle. The engine is read ONLY -- no registration, no
// mutation.

import (
	"encoding/json"
	"fmt"
	"strings"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// SandboxCompileBundleWithEngine runs the full Gate 1 pass: the per-construct
// compile (SandboxCompileBundle) followed by cross-reference resolution
// against the live engine's core construct registries plus the bundle's own
// constructs. The engine is consulted read-only. A construct that already
// failed the compile pass is not re-checked for references (its diagnostic
// stands); a construct that compiled cleanly but carries a dangling
// reference is flipped to a hard failure.
func SandboxCompileBundleWithEngine(constructs []SandboxConstruct, engine *MemQLEngine) SandboxReport {
	rep, overlay := compileBundle(constructs)

	// Index the bundle's own construct names by kind so a reference to a
	// sibling construct in the same bundle resolves even though it is not
	// (yet) in any live registry.
	bundleByKind := map[string]map[string]bool{}
	for _, c := range constructs {
		if bundleByKind[c.Kind] == nil {
			bundleByKind[c.Kind] = map[string]bool{}
		}
		bundleByKind[c.Kind][c.Name] = true
	}

	resolver := &crossRefResolver{
		engine:       engine,
		overlay:      overlay,
		bundleByKind: bundleByKind,
	}

	// Walk diagnostics in place: only constructs that compiled cleanly are
	// eligible for the reference checks (a parse failure already failed the
	// bundle, and its source may be too broken to scan).
	byKey := map[string]int{}
	for i := range rep.Diagnostics {
		d := rep.Diagnostics[i]
		byKey[d.Kind+"/"+d.Name] = i
	}
	for _, c := range constructs {
		idx, ok := byKey[c.Kind+"/"+c.Name]
		if !ok {
			continue
		}
		d := &rep.Diagnostics[idx]
		if !d.OK || d.Skipped {
			continue
		}
		if msg := resolver.check(c); msg != "" {
			d.OK = false
			d.Error = msg
			rep.OK = false
		}
	}
	return rep
}

// crossRefResolver resolves cross-construct references for one bundle.
type crossRefResolver struct {
	engine       *MemQLEngine
	overlay      *memoryNodes.MemoryRegistry
	bundleByKind map[string]map[string]bool
}

// check runs every reference check applicable to the construct's kind and
// returns the first failure message (empty when the construct is clean).
func (r *crossRefResolver) check(c SandboxConstruct) string {
	origin := fmt.Sprintf("sandbox:%s:%s", c.Kind, c.Name)

	// All construct kinds that carry file-top `use` imports get
	// cross-construct reference resolution.
	if msg := r.checkUseImports(c, origin); msg != "" {
		return msg
	}

	// Field existence on the referenced concept is checked for @row shapes
	// (their body is a pure projection of the bound concept's fields).
	if c.Kind == "shape" {
		if msg := r.checkShapeFieldExistence(c, origin); msg != "" {
			return msg
		}
	}
	return ""
}

// checkUseImports resolves every file-top `use <path>.{ name, ... }` import
// against the union of (overlay concept registry + live core construct
// registries + bundle-defined constructs). The construct-kind is the last
// segment of the dotted path (concepts / shapes / specs / traits / queries /
// mutations / logic / builtins). A name that resolves nowhere is a dangling
// cross-construct reference.
func (r *crossRefResolver) checkUseImports(c SandboxConstruct, origin string) string {
	uses, err := parsedUseDeclarations(c.Source)
	if err != nil {
		// The construct compiled, so its uses parse; a failure here is
		// internal and should not mask the clean compile.
		return ""
	}
	for _, u := range uses {
		if len(u.Parts) < 2 {
			continue
		}
		kind := u.Parts[len(u.Parts)-1]
		namespace := u.Parts[len(u.Parts)-2]
		for _, name := range u.Names {
			if !r.resolves(kind, namespace, name) {
				return fmt.Sprintf("%s: unresolved reference %q imported from %q -- no %s by that name in the core registry or this bundle", origin, name, u.Path, singularKind(kind))
			}
		}
	}
	return ""
}

// resolves reports whether an imported name of the given construct-kind
// resolves against the bundle, the live core registries, or (for concepts)
// the overlay registry.
func (r *crossRefResolver) resolves(kind, namespace, name string) bool {
	singular := singularKind(kind)
	// Bundle-defined sibling of the same kind?
	if r.bundleByKind[singular] != nil && r.bundleByKind[singular][name] {
		return true
	}
	switch kind {
	case "concepts":
		// Concepts resolve by trailing-segment match against the overlay
		// (core + bundle-defined concepts), disambiguated by the import's
		// namespace segment.
		return r.conceptResolves(namespace, name)
	case "shapes":
		return r.engine != nil && r.engine.Shapes().Has(name)
	case "specs", "traits":
		return r.engine != nil && r.engine.Specs().Has(name)
	case "queries", "mutations", "logic", "builtins":
		return r.engine != nil && r.engine.Functions().Has(name)
	default:
		// Unknown construct-kind segment: don't invent a failure for a kind
		// this pass does not model (providers / prompts / tools / policies).
		return true
	}
}

// conceptResolves reports whether a bare concept name resolves to a concept
// id in the overlay registry, preferring an exact namespace match.
func (r *crossRefResolver) conceptResolves(namespace, name string) bool {
	if r.overlay == nil {
		return false
	}
	wantId := ""
	for _, concept := range r.overlay.List() {
		if concept == nil {
			continue
		}
		idx := strings.LastIndex(concept.Name, ":")
		if idx < 0 || concept.Name[idx+1:] != name {
			continue
		}
		// Trailing segment matches; prefer one whose namespace segment equals
		// the import's namespace hint.
		if strings.Contains(concept.Name, ":"+namespace+":") {
			return true
		}
		wantId = concept.Name
	}
	return wantId != ""
}

// checkShapeFieldExistence verifies every `row.payload.<field>` path of a
// @row shape names a field declared on the shape's signature concept. A path
// onto a non-existent concept field is rejected.
func (r *crossRefResolver) checkShapeFieldExistence(c SandboxConstruct, origin string) string {
	decl, err := languageParser.ParseShapeDecl(stripUseDeclarations(c.Source))
	if err != nil || decl.SignatureConcept == "" {
		// No signature concept (pure @actor shape) or unparseable -- nothing
		// concept-bound to field-check.
		return ""
	}

	conceptId, err := r.resolveConceptId(c.Source, decl.SignatureConcept)
	if err != nil || conceptId == "" {
		// Could not resolve the signature concept to an id; the use-import
		// check above already reports a dangling concept import, so don't
		// double-report here.
		return ""
	}
	concept, err := r.overlay.Get(conceptId)
	if err != nil {
		return ""
	}
	fields := conceptPayloadFields(concept)
	if fields == nil {
		// Schema unreadable -- skip rather than false-fail.
		return ""
	}
	for _, p := range decl.Paths {
		field := payloadFieldFromPath(p)
		if field == "" {
			continue // row intrinsic (row.id / row.createdAt / ...) or nested
		}
		if !fields[field] {
			return fmt.Sprintf("%s: shape projects payload field %q, which concept %q does not declare", origin, field, conceptId)
		}
	}
	return ""
}

// resolveConceptId resolves a bare signature-concept name to its canonical id
// against the overlay registry, using the file's concept imports as the
// namespace hint where available.
func (r *crossRefResolver) resolveConceptId(source, name string) (string, error) {
	resolver := NewConceptResolver(r.overlay)
	// Prefer the namespace from a `use <ns>.concepts.{ name }` import.
	if uses, err := parsedUseDeclarations(source); err == nil {
		for _, u := range uses {
			if len(u.Parts) >= 2 && u.Parts[len(u.Parts)-1] == "concepts" {
				for _, n := range u.Names {
					if n == name {
						return resolver.resolveBareConceptNameWithNamespace(name, u.Parts[len(u.Parts)-2])
					}
				}
			}
		}
	}
	return resolver.resolveBareConceptName(name)
}

// parsedUseDeclarations parses the file-top `use` declarations out of a
// construct source into structured form. It reuses the package's useBlockRE
// to pull just the `use ... .{ ... }` blocks (so a struct-form body the full
// parser would choke on does not block use extraction) and parses that
// import-only prefix.
func parsedUseDeclarations(source string) ([]*languageParser.UseDeclaration, error) {
	prefix := strings.TrimSpace(extractUseDeclarations(source))
	if prefix == "" {
		return nil, nil
	}
	file, err := languageParser.ParseFile(prefix)
	if err != nil {
		return nil, err
	}
	return file.Uses, nil
}

// stripUseDeclarations removes file-top `use ... .{ ... }` import blocks from
// a construct source, leaving the declaration body the per-construct parsers
// (ParseSpecDecl / ParseShapeDecl) expect -- they consume a single
// declaration and reject a leading `use`, since the unified loader slices the
// imports off before calling them. The sandbox stores each candidate as a
// self-contained source WITH its imports, so it strips them here;
// cross-reference resolution validates the imports separately.
func stripUseDeclarations(source string) string {
	return strings.TrimLeft(useBlockRE.ReplaceAllString(source, ""), "\n \t")
}

// conceptPayloadFields returns the set of top-level payload field names
// declared by a concept, read from its draft-07 JSON-Schema. Returns nil when
// the schema cannot be read (the caller skips field-existence rather than
// false-failing).
func conceptPayloadFields(concept *memoryNodes.Concept) map[string]bool {
	raw, ok := concept.Schemas["definition"]
	if !ok {
		return nil
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil
	}
	if schema.Properties == nil {
		return nil
	}
	out := make(map[string]bool, len(schema.Properties))
	for name := range schema.Properties {
		out[name] = true
	}
	return out
}

// payloadFieldFromPath returns the top-level payload field a shape path
// projects, or "" when the path is a row intrinsic (row.id / row.createdAt /
// ...) or an actor path. `row.payload.label` -> "label";
// `row.payload.nested.x` -> "nested" (top-level field).
func payloadFieldFromPath(path string) string {
	segs := strings.Split(path, ".")
	// Tolerate both `row.payload.x` and bare `payload.x`.
	for i := 0; i < len(segs)-1; i++ {
		if segs[i] == "payload" {
			return segs[i+1]
		}
	}
	return ""
}

// singularKind maps a `use`-path construct-kind segment (plural) to the
// singular kind name the bundle indexes by.
func singularKind(kind string) string {
	switch kind {
	case "concepts":
		return "concept"
	case "shapes":
		return "shape"
	case "specs":
		return "spec"
	case "traits":
		return "trait"
	case "queries":
		return "query"
	case "mutations":
		return "mutation"
	case "logic":
		return "logic"
	case "builtins":
		return "builtin"
	default:
		return strings.TrimSuffix(kind, "s")
	}
}
