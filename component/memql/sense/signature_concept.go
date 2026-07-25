package sense

import (
	"fmt"
	"strings"

	parser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/dslimports"
)

// signature_concept.go flags a construct whose signature binds a concept that
// resolves to nothing -- `mutate/query/shape/seed <Concept> <name>` where
// <Concept> exists nowhere the workspace can see. This is the user's symptom 5
// (`mutate full ...` with no concept `full`). Error severity: a signature
// concept with no registry match is a hard boot failure (the strict-boot gate
// CrashLoops the node), so the editor mirrors a real load error, not a hint.
//
// The concept binding is recovered with dslimports.SignatureConceptRefs -- the
// boot-pinned extractor -- and resolved against the workspace graph with the
// same conservatism the load side uses (dslimports.resolveConceptForFile +
// missingIsProvable): an imported name is left to the import checks (#2730); a
// name is flagged only when its absence is PROVABLE. Everything else stays
// silent, so a legitimate external/global concept never false-squiggles.
func (s *Service) signatureConceptDiagnostics(file *parser.File, source string) []Diagnostic {
	refs := dslimports.SignatureConceptRefs(source)
	if len(refs) == 0 {
		return nil
	}
	imported := importedNames(file)
	local := localConceptNames(file)
	provable := s.signatureMissingIsProvable(file)

	var diagnostics []Diagnostic
	for _, ref := range refs {
		if imported[ref.Name] || local[ref.Name] || s.registryResolvesConcept(ref.Name) {
			// Imported: the import checks (#2730) own its validity, and an
			// external import is legitimately supplied from outside the tree.
			// Local: declared in this same buffer -> resolves.
			// Registry: present in the global vocabulary boot actually resolves
			// against (engine + product) -- boot binds it by trailing segment
			// with NO import. Skipping it here is what stops an unimported engine
			// concept (e.g. `mutate user ...` over a product bundle) from being
			// flagged as missing when it boots clean.
			continue
		}
		if !provable {
			// The buffer imports an external namespace, authors no Form-B
			// imports, or the tree has imports-only files -- the concept may
			// live somewhere invisible. Cannot prove absence; stay silent.
			continue
		}
		if s.workspace.ConceptExists(ref.Name, "") == ResolvedNo {
			pos := positionFromOffset(source, ref.Offset)
			diagnostics = append(diagnostics, Diagnostic{
				Range:    spanAt(pos, len(ref.Name)),
				Severity: SeverityError,
				Message:  fmt.Sprintf("signature concept %q is not declared or imported", ref.Name),
				Code:     "unknown-signature-concept",
			})
		}
	}
	return diagnostics
}

// registryResolvesConcept reports whether the engine registry -- the GLOBAL
// vocabulary boot resolves a signature concept against (engine + product) --
// carries a concept whose canonical id (`v1:namespace:name`) matches `name` by
// trailing segment, mirroring boot's resolveConceptByTrailingSegment. It returns
// false when there is no registry, so the registry-less fallback keeps its
// graph-only conservatism (the workspace graph sees product namespaces only, and
// without the registry an unimported engine concept is indistinguishable from a
// typo). The workspace graph alone cannot answer this -- it is a product-scoped
// view -- which is why the check consults the registry the LSP service holds.
func (s *Service) registryResolvesConcept(name string) bool {
	if s.registries == nil {
		return false
	}
	suffix := ":" + name
	for _, id := range s.registries.ConceptNames() {
		if id == name || strings.HasSuffix(id, suffix) {
			return true
		}
	}
	return false
}

// signatureMissingIsProvable mirrors dslimports.missingIsProvable for the open
// buffer: a "does not exist" conclusion about an unimported signature concept is
// safe only when the buffer authors Form-B imports, imports no external
// (unmounted) namespace, and the workspace tree has no imports-only files. Any
// of those failing means an unresolved name could live somewhere the buffer's
// view cannot see, so the editor must not assert it missing.
func (s *Service) signatureMissingIsProvable(file *parser.File) bool {
	hasFormB := false
	for _, u := range file.Uses {
		if u == nil || len(u.Names) == 0 {
			continue
		}
		hasFormB = true
		if len(u.Parts) > 0 && !s.workspace.HasNamespace(u.Parts[0]) {
			return false // external-namespace import -> absence not provable
		}
	}
	return hasFormB && !s.workspace.HasImportsOnlyFiles()
}

// importedNames is the set of every Form-B imported id across the buffer's use
// declarations.
func importedNames(file *parser.File) map[string]bool {
	out := make(map[string]bool)
	for _, u := range file.Uses {
		if u == nil {
			continue
		}
		for _, n := range u.Names {
			out[n] = true
		}
	}
	return out
}

// localConceptNames is the set of concept names declared in the buffer itself
// (a concept and a construct bound to it in the same file both resolve locally).
func localConceptNames(file *parser.File) map[string]bool {
	out := make(map[string]bool)
	for _, def := range file.Definitions {
		if c, ok := def.(*parser.ConceptDecl); ok && c.Name != "" {
			out[c.Name] = true
		}
	}
	return out
}

// signatureKindDiagnostics flags a signature that binds a name which IS
// declared -- just not as a concept (memql#2762).
//
// `shape todos displayTodo` where `todos` is a query is the motivating case:
// `shape` binds a CONCEPT, and todos is declared at
// dsl/todos/queries.memql. The sibling rule above only asks "does this name
// exist anywhere", so a name that exists as the WRONG KIND sailed through and
// the author got no squiggle at all -- the failure surfaced at boot instead.
//
// Scope follows the extractor: SignatureConceptRefs is pinned to
// `query|mutate|shape|seed`, every one of which binds a concept and nothing
// else. `spec` is deliberately outside it, which is what keeps the
// shape-XOR-concept binding legal -- flagging a spec that binds a shape would
// be wrong, and there are live ones in the tree.
//
// Conservatism, matching the rest of this file: a name the workspace has
// never seen is left to the missing-concept rule (it may be delivered at
// runtime via MEMQL_DSL_PATH); only a name the tree can SEE, declared solely
// as something other than a concept, is provably wrong. Measured over dsl/:
// 680 signature bindings, zero flagged.
func (s *Service) signatureKindDiagnostics(file *parser.File, source string) []Diagnostic {
	refs := dslimports.SignatureConceptRefs(source)
	if len(refs) == 0 {
		return nil
	}
	local := localConceptNames(file)

	var diagnostics []Diagnostic
	for _, ref := range refs {
		if local[ref.Name] || s.registryResolvesConcept(ref.Name) {
			continue
		}
		sites := s.workspace.DeclarationSites(ref.Name)
		if len(sites) == 0 {
			// Never seen here -- the missing-concept rule owns this case, and
			// a runtime-delivered concept must not be flagged.
			continue
		}
		var kinds []string
		seen := map[string]bool{}
		for _, site := range sites {
			if site.Kind == "concept" {
				kinds = nil
				break
			}
			if !seen[site.Kind] {
				seen[site.Kind] = true
				kinds = append(kinds, site.Kind)
			}
		}
		if len(kinds) == 0 {
			continue
		}
		pos := positionFromOffset(source, ref.Offset)
		diagnostics = append(diagnostics, Diagnostic{
			Range:    spanAt(pos, len(ref.Name)),
			Severity: SeverityError,
			Message: fmt.Sprintf("signature binds %q, which is declared as %s -- this construct binds a concept",
				ref.Name, strings.Join(kinds, "/")),
			Code: "signature-binds-wrong-kind",
		})
	}
	return diagnostics
}
