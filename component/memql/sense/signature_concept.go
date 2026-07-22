package sense

import (
	"fmt"

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
		if imported[ref.Name] || local[ref.Name] {
			// Imported: the import checks (#2730) own its validity, and an
			// external import is legitimately supplied from outside the tree.
			// Local: declared in this same buffer -> resolves.
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
