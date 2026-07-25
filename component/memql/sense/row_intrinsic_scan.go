package sense

// row_intrinsic_scan.go is the SINGLE detector for filter predicates that
// name a row intrinsic without the `row.` namespace (memql#2779). Both gates
// consume it -- the edit-time Sense rule (bareRowIntrinsicRule) and the
// tree-wide CI gate (dsl.TestFilterIntrinsicsUseRowNamespace) -- so the two
// cannot drift into disagreeing about what counts as a violation.
//
// The first cut kept two independent detectors and they disagreed
// immediately: the CI gate split filter clauses on `&&` only, so a bare
// intrinsic joined by `||` or wrapped in parens
// (`filter (row.id==args.a || id==args.b)`) sailed through green while the
// editor flagged it. This scanner reads clause TEXT rather than parsed
// predicate structure, so boolean shape is irrelevant to it.

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// BareRowIntrinsic is one filter predicate naming a row intrinsic bare.
type BareRowIntrinsic struct {
	Line   int    // 1-based line in the authored source
	Column int    // 1-based RUNE column of the offending token (the Sense contract)
	Text   string // the token exactly as authored, including any leaf
	Name   string // the replacement to suggest, rendered as `row.<Name>`
}

// bareRowIntrinsicRE matches a row intrinsic in comparison-LHS position with
// no namespace.
//
//   - group 1 rejects a dotted or identifier context, so `args.id`, `row.id`
//     and a payload property like `threadId` never match;
//   - group 2 is a SCALAR intrinsic, matched case-insensitively because
//     resolveIntrinsicField is (the engine accepts `createdat`);
//   - group 3 is `provenance` WITH its mandatory leaf. Provenance is the one
//     object-valued intrinsic: bare `provenance ==` has no SQL push-down, and
//     `row.provenance` is rejected outright by the parser -- so suggesting
//     `row.provenance` for a leafless `provenance` would name a spelling
//     nothing accepts. Requiring the leaf here keeps every suggestion this
//     scanner makes a form the engine actually compiles;
//   - group 4 requires a comparison or membership operator, so a bare word in
//     a spec call does not match. `in` is included: `filter id in args.ids`
//     is a real predicate shape.
var bareRowIntrinsicRE = regexp.MustCompile(
	`(^|[^A-Za-z0-9_.])(?:((?i:id|concept|type|createdAt|createdBy))|((?i:provenance)\.[A-Za-z_][A-Za-z0-9_]*))[ \t]*(==|!=|<=|>=|<|>|\bin\b)`)

// canonicalScalarIntrinsic maps a lower-cased scalar intrinsic to the
// spelling the suggestion should use. Mirrors
// component/memql/intrinsic_fields.go, which lives in a package this one must
// not import -- sense is consumed by the editor AND by the dsl conformance
// suite, and either would pull the engine in.
var canonicalScalarIntrinsic = map[string]string{
	"id":        "id",
	"concept":   "concept",
	"type":      "type",
	"createdat": "createdAt",
	"createdby": "createdBy",
}

// ScanBareRowIntrinsics returns every filter predicate in source that names a
// row intrinsic bare.
//
// Line comments, block comments, and string-literal contents are excluded, so
// `filter name == "a id==b"` does not fire, a `//` inside a URL literal does
// not truncate the rest of the line, and commented-out code in a `/* */`
// block is not authored code.
//
// Only a line that OPENS a filter clause is scanned. A filter clause cannot
// span lines: parseStructQueryBody (component/language/parser/rewriter.go)
// walks a struct-query body line by line and hard-errors on any line that is
// not a clause keyword, so a continuation line never reaches the engine
// anyway. An earlier version carried multi-line tracking for that shape; it
// protected a grammar the parser rejects while giving a block comment a way
// to escape the single-line bound.
//
// KNOWN, ACCEPTED GAP: BlankBlockComments ends a string literal at a newline
// rather than letting it span lines, so the second line of a multi-line string
// is treated as code -- if it happens to begin `filter ` and contain a bare
// intrinsic, this reports a false positive. That is the deliberate trade-off
// the helper already makes for DiscardedArgsDescriptions, and the alternative
// is worse for a gate: a string that spans newlines means one unbalanced quote
// blanks the rest of the file, silently disabling detection everywhere after
// it. A false positive fails CI at a precise file:line and is obvious; a
// silent false negative is the failure mode this whole change exists to kill.
func ScanBareRowIntrinsics(source string) []BareRowIntrinsic {
	var found []BareRowIntrinsic

	for i, line := range strings.Split(BlankBlockComments(source), "\n") {
		code := blankLineCommentsAndStrings(line)
		if !isFilterClauseOpener(strings.TrimSpace(code)) {
			continue
		}
		for _, m := range bareRowIntrinsicRE.FindAllStringSubmatchIndex(code, -1) {
			start, text, name := -1, "", ""
			switch {
			case m[4] >= 0: // scalar intrinsic
				start, text = m[4], code[m[4]:m[5]]
				name = canonicalScalarIntrinsic[strings.ToLower(text)]
			case m[6] >= 0: // provenance.<leaf>
				start, text = m[6], code[m[6]:m[7]]
				_, leaf, _ := strings.Cut(text, ".")
				name = "provenance." + leaf
			}
			if name == "" {
				continue
			}
			found = append(found, BareRowIntrinsic{
				Line:   i + 1,
				Column: utf8.RuneCountInString(line[:start]) + 1,
				Text:   text,
				Name:   name,
			})
		}
	}
	return found
}

// isFilterClauseOpener reports whether a trimmed line opens a `filter` clause.
// `@filter(...)` on an automation is a different surface with a different
// evaluator and is deliberately excluded -- it does not start with `filter`.
func isFilterClauseOpener(trimmed string) bool {
	rest, ok := strings.CutPrefix(trimmed, "filter")
	return ok && (rest == "" || rest[0] == ' ' || rest[0] == '\t')
}

// blankLineCommentsAndStrings replaces string-literal contents and any
// trailing `//` comment with spaces, preserving byte offsets so the caller can
// still index the ORIGINAL line. Quote-awareness is the point: a naive
// strings.Index(line, "//") truncates at the `//` inside a URL literal and
// blinds the scanner to everything after it.
//
// Block comments are handled by the caller via BlankBlockComments, which is
// whole-source (they span lines) and already used by DiscardedArgsDescriptions.
func blankLineCommentsAndStrings(line string) string {
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
