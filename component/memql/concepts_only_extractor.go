package memql

// concepts_only_extractor.go provides a parse path that pulls
// concept declarations out of mixed-content .memql files without
// running the struct-form rewriters.
//
// Why this exists: the new domain-first tree consolidates queries,
// mutations, shapes, specs, automations, and concepts into single
// files per entity. The struct-form rewriters
// (mutation_rewrite, query_rewrite, etc.) currently assume one
// concept per file and pick the wrong concept binding when they
// see multiple. ParseFileSource therefore errors out on most
// consolidated files, and the unified loader's ConceptDecl walk
// comes up empty.
//
// This extractor solves the immediate need (concept registration
// from the new tree) by scanning the source text for concept block
// boundaries, slicing each block + its preamble, and parsing each
// slice through the bare parser (which handles concept syntax
// natively and doesn't run any rewriters).

import (
	"strings"

	languageAst "github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// ExtractConceptDecls scans `source` for `concept <name> { ... }`
// blocks (with their preceding @-attribute preamble) and returns
// each parsed ConceptDecl. Everything else in the file (queries,
// mutations, shapes, specs, etc.) is ignored.
//
// The function is tolerant: parse failures on individual slices
// are skipped with no error returned, so callers get whatever
// loaded cleanly. Use this for transitional-state registration
// only; full multi-construct loading must wait for the struct-form
// rewriters to gain multi-concept awareness.
func ExtractConceptDecls(source string) []*languageAst.ConceptDecl {
	var out []*languageAst.ConceptDecl
	for _, slice := range conceptSlices(source) {
		file, err := languageParser.ParseFile(slice)
		if err != nil {
			continue
		}
		for _, def := range file.Definitions {
			if decl, ok := def.(*languageAst.ConceptDecl); ok {
				out = append(out, decl)
			}
		}
	}
	return out
}

// conceptSlices walks `source` line-by-line and returns each
// concept declaration's preamble + body as a self-contained text
// slice ready for parser.ParseFile.
//
// A "concept block" starts at the first `@`-attribute line in a
// run that culminates in a `concept <name> {` line, and ends at
// the matching closing brace at indent 0. Brace counting handles
// nested object bodies inside the concept's property declarations.
func conceptSlices(source string) []string {
	lines := strings.Split(source, "\n")

	// Declaration detection and brace balancing run on a comment-BLANKED view;
	// the preamble walk and the emitted slice are cut from the ORIGINAL
	// (memql#2868).
	//
	// Concepts do NOT go through ExtractKeywordSlices -- they have this
	// dedicated line-oriented slicer, which #2868's diagnosis missed. Fixing
	// the keyword slicer alone left a block-commented concept REGISTERING; the
	// end-to-end test in keyword_slices_e2e_test.go is what caught it. A
	// commented-out schema is live: it validates writes and appears in the
	// concepts API.
	//
	// BlankComments preserves byte offsets, so splitting the blanked source
	// yields the same number of lines at the same indices as `lines` --
	// scanLines[j] and lines[j] are the same line, one blanked and one not.
	scanLines := strings.Split(languageParser.BlankComments(source), "\n")
	if len(scanLines) != len(lines) {
		// BlankComments stopped being length-preserving. Falling back to the
		// raw view would silently restore the #2868 defect, so refuse to slice
		// rather than mis-slice: the caller then reports zero concepts and the
		// strict-boot gate surfaces it loudly instead of quietly loading a
		// commented-out schema.
		return nil
	}

	var slices []string

	i := 0
	for i < len(lines) {
		// Find the next `concept <name> {` declaration at column 0.
		conceptLine := -1
		for j := i; j < len(lines); j++ {
			line := scanLines[j]
			if isConceptDeclStart(line) {
				conceptLine = j
				break
			}
		}
		if conceptLine == -1 {
			break
		}

		// Walk backwards from the concept line, gathering the
		// preamble: any contiguous run of @-attribute lines (and
		// adjacent comment lines) immediately above the concept.
		preambleStart := conceptLine
		for k := conceptLine - 1; k >= i; k-- {
			line := strings.TrimSpace(lines[k])
			if strings.HasPrefix(line, "@") {
				preambleStart = k
				continue
			}
			if strings.HasPrefix(line, "//") {
				// Comment lines stay attached to the preamble only
				// if they're immediately adjacent. Stop when we
				// hit a non-comment, non-@ line.
				preambleStart = k
				continue
			}
			if line == "" {
				// Blank line ends the preamble run -- comments and
				// attributes BELOW the blank stay attached; above
				// don't.
				break
			}
			break
		}

		// Walk forward from the concept line, balancing braces to
		// find the closing `}` at indent 0.
		depth := 0
		endLine := -1
		for k := conceptLine; k < len(scanLines); k++ {
			// Blanked view: a `{` or `}` inside a comment must not move the
			// depth, or the slice closes early and emits a truncated concept.
			depth += strings.Count(scanLines[k], "{")
			depth -= strings.Count(scanLines[k], "}")
			if depth == 0 {
				endLine = k
				break
			}
		}
		if endLine == -1 {
			// Unterminated -- skip this slice.
			i = conceptLine + 1
			continue
		}

		slice := strings.Join(lines[preambleStart:endLine+1], "\n")
		slices = append(slices, slice)
		i = endLine + 1
	}

	return slices
}

// isConceptDeclStart returns true if `line` starts with the
// `concept <name>` declaration at column 0 (i.e. the top-level
// concept block opener, not a property reference inside a body).
func isConceptDeclStart(line string) bool {
	if !strings.HasPrefix(line, "concept ") && !strings.HasPrefix(line, "concept\t") {
		return false
	}
	// Must contain a `{` somewhere on the same line or be a
	// multi-line form. For simplicity require `{` on the same line
	// (the parser handles same-line opener; cross-line is rare).
	return strings.Contains(line, "{")
}
