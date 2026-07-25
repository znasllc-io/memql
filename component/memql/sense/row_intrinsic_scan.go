package sense

// row_intrinsic_scan.go is the SINGLE detector for filter predicates that
// name a row intrinsic without the `row.` namespace (memql#2779). Both gates
// consume it -- the edit-time Sense rule (bareRowIntrinsicRule) and the
// tree-wide CI gate (dsl.TestFilterIntrinsicsUseRowNamespace) -- so the two
// can never drift into disagreeing about what counts as a violation.
//
// The first cut of this rule kept two independent detectors, and they
// disagreed immediately: the CI gate split filter clauses on `&&` only, so a
// bare intrinsic joined by `||` or wrapped in parens
// (`filter (row.id==args.a || id==args.b)`) sailed through green while the
// editor flagged it. This scanner works on clause TEXT rather than on parsed
// predicate structure, so boolean shape is irrelevant to it.

import (
	"regexp"
	"strings"
)

// BareRowIntrinsic is one filter predicate naming a row intrinsic bare.
type BareRowIntrinsic struct {
	Line   int    // 1-based line in the authored source
	Column int    // 1-based column of the offending token
	Text   string // the token exactly as authored (may differ in case)
	Name   string // the canonical intrinsic name to suggest (`row.<Name>`)
}

// bareRowIntrinsicRE matches a row intrinsic in comparison-LHS position with
// no namespace.
//
//   - group 1 rejects a dotted or identifier context, so `args.id`, `row.id`
//     and a payload property like `threadId` never match;
//   - group 2 is the intrinsic, matched case-insensitively because
//     resolveIntrinsicField is (the engine accepts `createdat`);
//   - group 3 is the optional leaf `provenance` requires -- without it the
//     rule would claim to cover provenance while never firing on the only
//     form it can be written in;
//   - group 4 requires a comparison or membership operator, so a bare word in
//     a spec call or an identifier elsewhere on the line does not match.
//     `in` is included: `filter id in args.ids` is a real predicate shape.
var bareRowIntrinsicRE = regexp.MustCompile(
	`(^|[^A-Za-z0-9_.])((?i:id|concept|type|createdAt|createdBy|provenance))(\.[A-Za-z_][A-Za-z0-9_]*)?[ \t]*(==|!=|<=|>=|<|>|\bin\b)`)

// canonicalRowIntrinsic maps a lower-cased intrinsic token to the spelling the
// suggestion should use. Mirrors component/memql/intrinsic_fields.go, which is
// in a package this one must not import (sense is consumed by the editor and
// by the dsl conformance suite; both would pull the engine in).
var canonicalRowIntrinsic = map[string]string{
	"id":         "id",
	"concept":    "concept",
	"type":       "type",
	"createdat":  "createdAt",
	"createdby":  "createdBy",
	"provenance": "provenance",
}

// ScanBareRowIntrinsics returns every filter predicate in source that names a
// row intrinsic bare. Comments and string-literal contents are excluded, so
// `filter name == "a id==b"` does not fire and a `//` inside a string does not
// truncate the rest of the line.
//
// Multi-line filter clauses are covered: the clause continues until a line
// opens another clause (shape / sort / paginate / asOf / args / return), an
// annotation, a brace at column 0, or a blank line.
func ScanBareRowIntrinsics(source string) []BareRowIntrinsic {
	var found []BareRowIntrinsic
	inFilter := false

	for i, raw := range strings.Split(source, "\n") {
		code := blankCommentsAndStrings(raw)
		trimmed := strings.TrimSpace(code)

		if !inFilter {
			if !isFilterClauseOpener(trimmed) {
				continue
			}
			inFilter = true
		} else if trimmed == "" || endsFilterClause(trimmed) {
			inFilter = false
			// The terminator may itself open a new filter clause.
			if !isFilterClauseOpener(trimmed) {
				continue
			}
			inFilter = true
		}

		for _, m := range bareRowIntrinsicRE.FindAllStringSubmatchIndex(code, -1) {
			token := code[m[4]:m[5]]
			name, ok := canonicalRowIntrinsic[strings.ToLower(token)]
			if !ok {
				continue
			}
			found = append(found, BareRowIntrinsic{
				Line:   i + 1,
				Column: m[4] + 1,
				Text:   token,
				Name:   name,
			})
		}
	}
	return found
}

// filterClauseEnders are the tokens that close a filter clause: any other
// clause keyword, or a block delimiter.
var filterClauseEnders = map[string]bool{
	"shape": true, "sort": true, "paginate": true, "asOf": true,
	"args": true, "return": true, "insert": true, "update": true,
	"filter": true,
}

func isFilterClauseOpener(trimmed string) bool {
	rest, ok := strings.CutPrefix(trimmed, "filter")
	return ok && (rest == "" || rest[0] == ' ' || rest[0] == '\t')
}

func endsFilterClause(trimmed string) bool {
	if strings.HasPrefix(trimmed, "@") || strings.HasPrefix(trimmed, "}") || strings.HasPrefix(trimmed, "{") {
		return true
	}
	head := trimmed
	if idx := strings.IndexAny(head, " \t"); idx >= 0 {
		head = head[:idx]
	}
	return filterClauseEnders[head]
}

// blankCommentsAndStrings replaces string-literal contents and any trailing
// `//` comment with spaces, preserving byte offsets so reported columns still
// index the ORIGINAL line. Quote-awareness is the point: a naive
// strings.Index(line, "//") truncates at the `//` inside a URL literal and
// blinds the scanner to everything after it.
func blankCommentsAndStrings(line string) string {
	out := []byte(line)
	inStr := false
	for i := 0; i < len(out); i++ {
		switch {
		case inStr && out[i] == '\\' && i+1 < len(out):
			out[i], out[i+1] = ' ', ' '
			i++
		case out[i] == '"':
			inStr = !inStr
		case inStr:
			out[i] = ' '
		case out[i] == '/' && i+1 < len(out) && out[i+1] == '/':
			for j := i; j < len(out); j++ {
				out[j] = ' '
			}
			return string(out)
		}
	}
	return string(out)
}
