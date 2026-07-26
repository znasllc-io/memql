// Package dslclause owns ONE answer to "which keywords terminate a filter
// clause in a struct-form body", so the gates and checkers that walk `.memql`
// text line by line cannot drift about it (memql#2815).
//
// The drift this exists to stop was measured, not hypothetical: the
// conformance walker's list omitted `sort` / `paginate` / `asOf` / `count`,
// so 22 directive lines in the shipped corpus were emitted to the gates as
// pseudo-predicates. That direction happened to be harmless -- the gates saw
// extra non-predicates rather than missing real ones -- but the safety was
// luck rather than design: a gate that REJECTS rather than classifies would
// have started firing on `sort "createdAt", "desc"`.
//
// SCOPE. This package owns clause EXTRACTION (which lines bound a clause),
// not predicate DECOMPOSITION (how a clause splits into predicates). Those
// consumers legitimately differ and are deliberately left alone:
//
//   - the conformance walker needs predicate BOUNDARIES, because its gates
//     classify a predicate by its head;
//   - component/memql/sense needs a TEXT scan that works on the unparseable
//     in-progress buffers an editor holds;
//   - component/memql/dslimports walks the AST, which is the most precise and
//     immune to boolean-structure holes -- and unusable by the other two.
//
// "Just use one strategy" is therefore wrong. The tractable, measured win is
// sharing the layer where they had actually drifted.
package dslclause

import "strings"

// StructQueryDirectives is the set parseStructQueryBody recognises inside a
// struct-form query body (component/language/parser/rewriter.go). Anything
// else on a body line is an "unknown struct-query field" error, so this is
// the authoritative list -- TestStructQueryDirectivesMatchTheParser pins it
// against that switch so a new directive cannot be added to one and forgotten
// here.
//
// `concept` is included because the parser still recognises the line in order
// to reject it with a migration hint; it terminates a filter clause either
// way.
var StructQueryDirectives = []string{
	"concept",
	"filter",
	"count",
	"shape",
	"sort",
	"paginate",
	"asOf",
}

// BodyKeywords additionally covers the construct kinds a struct-query body
// does not have. A walker that scans every `.memql` line, rather than only
// query bodies, meets these too:
//
//	insert / update   mutation bodies
//	return            spec, trait and logic bodies
//	args / use        the argument block and file-top imports
var BodyKeywords = append(append([]string{}, StructQueryDirectives...),
	"insert",
	"update",
	"return",
	"args",
	"use",
)

// StartsWith reports whether a TRIMMED line opens the given keyword's clause:
// the keyword alone, or followed by whitespace.
//
// The boundary matters. The parser itself uses a bare HasPrefix, so it would
// read `sortOrder ...` as a `sort` directive -- a quirk worth not copying
// into the gates, where it would silently reclassify a payload field named
// after a directive.
func StartsWith(trimmed, keyword string) bool {
	rest, ok := strings.CutPrefix(trimmed, keyword)
	return ok && (rest == "" || rest[0] == ' ' || rest[0] == '\t')
}

// StartsAnyOf reports whether a trimmed line opens any of the keywords.
func StartsAnyOf(trimmed string, keywords []string) bool {
	for _, kw := range keywords {
		if StartsWith(trimmed, kw) {
			return true
		}
	}
	return false
}

// TerminatesFilterClause reports whether a trimmed line ends a filter clause
// that began on an earlier line -- any body keyword, or the structural close
// of the construct.
func TerminatesFilterClause(trimmed string) bool {
	return trimmed == "" ||
		strings.HasPrefix(trimmed, "}") ||
		strings.HasPrefix(trimmed, ")") ||
		strings.HasPrefix(trimmed, "@") ||
		StartsAnyOf(trimmed, BodyKeywords)
}
