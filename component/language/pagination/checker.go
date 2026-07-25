// Package pagination is the single source of truth for memQL's
// pagination authoring rule (epic 5, issue 5.1 / memql#1965).
//
// The rule: every LIST-RETURNING query must declare how it is bounded.
// A list query is one whose shape projects a row set without a
// unique-key equality filter. Such a query MUST carry one of:
//
//   - a `paginate` directive (an explicit window), or
//   - a `sort` directive (an explicit ordering -- "give me the latest
//     N" is bounded by the page size the consumer applies), or
//   - a `count` clause (it returns an aggregate, not a row set), or
//   - an `@unbounded("reason")` annotation (the author has deliberately
//     marked it a legitimate full-set read).
//
// A query that filters by a unique-key equality (`id == <expr>`, an
// equality on the row's primary intrinsic) reads at most one row and is
// EXEMPT -- it is a single-row read, not a list.
//
// This package contains the pure, deterministic classifier. It operates
// on raw `.memql` source text using line structure only (no full
// parser), the same lightweight approach the dsl/conformance_test.go
// gates use, so it can run identically over the embedded tree (the
// conformance test) and the on-disk tree (the audit report) without an
// engine. Both consumers derive from this one definition so the rule can
// never drift between the checker and the audit.
//
// NOTE on sequencing (memql#1965 owner decision): this package SHIPS the
// classifier + the runtime backstop. The repo-wide hard-fail assertion
// over the existing 208-query tree is COUPLED to the issue 5.3 backfill
// (it cannot go green until every existing offender is marked) and lands
// in that PR. Until then the conformance test asserts only that the
// classifier DETECTS a freshly-authored unmarked list query; the
// tree-wide enforcement is report-only.
package pagination

import (
	"regexp"
	"sort"
	"strings"
)

// Classification is the bucket a single query falls into.
type Classification int

const (
	// SingleRow: the query filters by a unique-key equality (`id ==`)
	// and reads at most one row. Exempt from the pagination rule.
	SingleRow Classification = iota
	// Aggregate: the query carries a `count` clause and returns a
	// numeric aggregate, not a row set. Exempt.
	Aggregate
	// BoundedList: a list query that declares an explicit window via
	// `paginate` or `sort`. Compliant.
	BoundedList
	// UnboundedMarked: a list query explicitly marked
	// `@unbounded("reason")`. Compliant (and auditable).
	UnboundedMarked
	// UnmarkedList: a list query with none of the above -- the
	// violation the pagination rule targets.
	UnmarkedList
)

func (c Classification) String() string {
	switch c {
	case SingleRow:
		return "single-row"
	case Aggregate:
		return "aggregate"
	case BoundedList:
		return "bounded-list"
	case UnboundedMarked:
		return "unbounded-marked"
	case UnmarkedList:
		return "unmarked-list"
	default:
		return "unknown"
	}
}

// QueryFinding describes one query construct and how the classifier
// bucketed it.
type QueryFinding struct {
	File            string
	Line            int // 1-based line of the `query <Concept> <name> {` header
	Name            string
	Concept         string
	Class           Classification
	UnboundedReason string // populated when Class == UnboundedMarked
}

// IsUnmarkedList reports whether this finding is the violation the
// pagination authoring rule flags.
func (f QueryFinding) IsUnmarkedList() bool { return f.Class == UnmarkedList }

var (
	// queryHeaderRe matches the canonical struct-form query header:
	// `query <Concept> <name> {`. Group 1 = concept, group 2 = name.
	queryHeaderRe = regexp.MustCompile(`(?m)^[ \t]*query[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)

	// uniqueKeyFilterRe matches a unique-key equality on the row's
	// primary intrinsic: `row.id == <expr>` (single-row read). A
	// guarded `when(args.x) { row.id == ... }` is intentionally NOT
	// matched here -- the id filter is conditional on the arg being
	// present, so the query can still return the full set when the arg
	// is omitted.
	//
	// The `row.` namespace makes this unambiguous (memql#2779). Before it,
	// the pattern had to match a BARE `id ==` while excluding payload
	// sub-fields that merely end in "id" (`payload.threadId`) -- that is
	// what the leading negative class did, and it is kept so a property
	// named `myrow` cannot masquerade as the namespace.
	uniqueKeyFilterRe = regexp.MustCompile(`(^|[^A-Za-z0-9_.])row\.id[ \t]*==`)

	// unboundedRe matches `@unbounded("reason")` and captures the
	// reason.
	unboundedRe = regexp.MustCompile(`@unbounded\s*\(\s*"((?:[^"\\]|\\.)*)"\s*\)`)
)

// ScanSource classifies every struct-form query in a single `.memql`
// source file. file is the logical path used for reporting.
func ScanSource(file, src string) []QueryFinding {
	var findings []QueryFinding
	matches := queryHeaderRe.FindAllStringSubmatchIndex(src, -1)
	for _, m := range matches {
		concept := src[m[2]:m[3]]
		name := src[m[4]:m[5]]
		line := strings.Count(src[:m[0]], "\n") + 1

		openIdx := m[1] - 1
		closeIdx := matchingCloseBrace(src, openIdx)
		if closeIdx < 0 {
			continue
		}
		body := src[m[1]:closeIdx]
		preamble := precedingAnnotations(src, m[0])

		findings = append(findings, classifyQuery(file, line, name, concept, preamble, body))
	}
	return findings
}

// classifyQuery applies the deterministic pagination rule to one query.
func classifyQuery(file string, line int, name, concept, preamble, body string) QueryFinding {
	f := QueryFinding{File: file, Line: line, Name: name, Concept: concept}

	// `@unbounded("reason")` -- explicit full-set opt-out (highest
	// precedence: an author who marks @unbounded means it, even if a
	// paginate/sort also appears; the rewriter rejects that combination
	// at load time, so here we just record the mark).
	if um := unboundedRe.FindStringSubmatch(preamble); um != nil {
		f.Class = UnboundedMarked
		f.UnboundedReason = strings.TrimSpace(um[1])
		return f
	}

	filter := filterClause(body)

	// Single-row read: a bare `id ==` equality on the primary intrinsic.
	// Strip `when(args.x) { ... }` guard blocks first -- a guarded id
	// equality is conditional on the arg being present, so it does NOT
	// guarantee a single-row read (the query returns the full set when
	// the arg is omitted) and must stay classified as a list.
	if uniqueKeyFilterRe.MatchString(stripWhenGuards(filter)) {
		f.Class = SingleRow
		return f
	}

	// count -> aggregate, not a row set.
	if hasDirective(body, "count") {
		f.Class = Aggregate
		return f
	}

	// Explicit window via paginate or sort -> bounded list.
	if hasDirective(body, "paginate") || hasDirective(body, "sort") {
		f.Class = BoundedList
		return f
	}

	// Everything else is an unmarked list read -- the violation.
	f.Class = UnmarkedList
	return f
}

// filterClause returns the text of the query body's `filter` line(s):
// the `filter` directive plus any indented continuation lines that
// belong to it (the same clause grammar the conformance test's
// walkFilterPredicates uses). Returns "" when the query has no filter.
func filterClause(body string) string {
	var sb strings.Builder
	inFilter := false
	for _, raw := range strings.Split(body, "\n") {
		line := stripComment(raw)
		trim := strings.TrimSpace(line)
		if !inFilter {
			if strings.HasPrefix(trim, "filter ") || strings.HasPrefix(trim, "filter\t") {
				inFilter = true
				sb.WriteString(" ")
				sb.WriteString(strings.TrimSpace(strings.TrimPrefix(trim, "filter")))
			}
			continue
		}
		// Continuation: stop at the next directive / annotation / blank.
		if trim == "" || startsWithDirective(trim) || strings.HasPrefix(trim, "@") {
			inFilter = false
			continue
		}
		sb.WriteString(" ")
		sb.WriteString(trim)
	}
	return sb.String()
}

// whenGuardRe matches a `when(...) { ... }` guard block so it can be
// removed before the unaffected (always-applied) predicates are tested
// for a unique-key equality.
var whenGuardRe = regexp.MustCompile(`when\s*\([^)]*\)\s*\{[^}]*\}`)

// stripWhenGuards removes every `when(...) { ... }` guard block from a
// filter clause, leaving only the predicates that always apply.
func stripWhenGuards(filter string) string {
	return whenGuardRe.ReplaceAllString(filter, " ")
}

// directiveKeywords are the struct-query body directives that terminate
// a multi-line filter continuation.
var directiveKeywords = []string{"shape", "sort", "paginate", "asOf", "count", "args", "use", "filter"}

func startsWithDirective(trim string) bool {
	for _, kw := range directiveKeywords {
		if trim == kw || strings.HasPrefix(trim, kw+" ") || strings.HasPrefix(trim, kw+"\t") {
			return true
		}
	}
	return false
}

// hasDirective reports whether the query body has a directive line
// beginning with kw (e.g. "sort", "paginate", "count"). A bare-keyword
// directive (`count` on its own line) is matched too.
func hasDirective(body, kw string) bool {
	for _, raw := range strings.Split(body, "\n") {
		trim := strings.TrimSpace(stripComment(raw))
		if trim == kw || strings.HasPrefix(trim, kw+" ") || strings.HasPrefix(trim, kw+"\t") {
			return true
		}
	}
	return false
}

// precedingAnnotations returns the contiguous run of `@...` / `//` lines
// immediately above headerStart -- the construct's annotation preamble.
func precedingAnnotations(src string, headerStart int) string {
	blockStart := headerStart
	for k := headerStart - 1; k >= 0; {
		lineStart := strings.LastIndexByte(src[:k], '\n') + 1
		line := strings.TrimSpace(strings.TrimRight(src[lineStart:k+1], "\r\n"))
		if strings.HasPrefix(line, "@") || strings.HasPrefix(line, "//") {
			blockStart = lineStart
			k = lineStart - 1
			continue
		}
		break
	}
	return src[blockStart:headerStart]
}

// matchingCloseBrace walks src from openIdx (a `{`) to its matching
// `}`, honouring string literals and line comments. Returns -1 when
// unbalanced.
func matchingCloseBrace(src string, openIdx int) int {
	if openIdx < 0 || openIdx >= len(src) || src[openIdx] != '{' {
		return -1
	}
	depth := 0
	inString := false
	for i := openIdx; i < len(src); i++ {
		c := src[i]
		if inString {
			if c == '\\' && i+1 < len(src) {
				i++
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '/':
			if i+1 < len(src) && src[i+1] == '/' {
				nl := strings.IndexByte(src[i:], '\n')
				if nl < 0 {
					return -1
				}
				i += nl
			}
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// stripComment trims a trailing `// ...` line comment, preserving any
// `//` inside a string literal.
func stripComment(line string) string {
	inStr := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == '"' && (i == 0 || line[i-1] != '\\') {
			inStr = !inStr
			continue
		}
		if !inStr && c == '/' && i+1 < len(line) && line[i+1] == '/' {
			return line[:i]
		}
	}
	return line
}

// SortFindings orders findings by file then line for stable reporting.
func SortFindings(findings []QueryFinding) {
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		return findings[i].Line < findings[j].Line
	})
}
