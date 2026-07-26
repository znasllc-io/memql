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
	"unicode"
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
// One shape that DOES reach the engine is excluded by a gate rather than by
// the grammar (memql#2817): the language accepts a clause on the same line as
// its opening brace -- `query w q { filter id == args.x }` normalises cleanly
// -- and a line-oriented scanner cannot see it, so a bare intrinsic written
// that way passed both gates in silence.
// dsl.TestClauseDoesNotShareTheOpeningBraceLine pins that shape out of
// authored .memql. Do not relax it without first giving these scanners a way
// to see the shape it excludes.
//
// That gate closes the BRACE-LINE shape, not every spelling this opener
// misses. isFilterClauseOpener still requires the keyword be followed by a
// space or tab at the start of a line, so `filter(id == args.x)` and
// `filter<NBSP>id == args.x` on their OWN line both score zero hits and no
// gate covers them today (memql#2863). Recorded because it would be easy to
// read the gate above as making the opener airtight; it does not.
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

	// Columns are counted against the ORIGINAL line, not the blanked one.
	// BlankBlockComments substitutes one space per BYTE, so a multi-byte rune
	// inside a same-line block comment becomes several runes and shifts every
	// rune column after it. Byte offsets are preserved by that substitution,
	// so a byte index from the blanked line indexes the original correctly.
	original := strings.Split(source, "\n")

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
			prefix := line[:start]
			if i < len(original) && start <= len(original[i]) {
				prefix = original[i][:start]
			}
			found = append(found, BareRowIntrinsic{
				Line:   i + 1,
				Column: utf8.RuneCountInString(prefix) + 1,
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
// The separator set is ANY UNICODE SPACE, plus `(`. It was `' '` or `'\t'`,
// which let four spellings the engine accepts score zero hits and walk past
// both row-intrinsic gates (memql#2863 / #2779 / #2786).
//
// Measured through NormaliseQuerySource -> ParseFile, which is the path a
// struct query actually takes. ParseFile ALONE rejects all four and would have
// argued the hole was already closed:
//
//	sep     normalise  parse  hits BEFORE
//	SPACE   ok         ok     1
//	TAB     ok         ok     1
//	NBSP    ok         ok     0   <- U+00A0
//	EMSP    ok         ok     0   <- U+2003
//	IDSP    ok         ok     0   <- U+3000, not named in the issue
//	PAREN   ok         ok     0
//
// Testing `rest[0]` was also a BYTE comparison against a UTF-8 string, so it
// could never have matched a multi-byte separator however the set was spelled.
//
// `(` is included because the engine genuinely accepts `filter(...)`. The
// sibling question was settled the other way by the same probe: `sort(...)` is
// REJECTED -- "sort field must be a string literal ... (got \"(\")" -- so
// isSortClauseOpener deliberately does NOT accept `(`. It already handles
// unicode space, via its TrimLeftFunc(unicode.IsSpace).
//
// The lesson these gates keep re-teaching: the lexer admits unicode.IsLetter /
// IsDigit and separators are not reliably ASCII, so a guard written to an ASCII
// set is narrower than the grammar it polices.
func isFilterClauseOpener(trimmed string) bool {
	rest, ok := strings.CutPrefix(trimmed, "filter")
	if !ok {
		return false
	}
	if rest == "" {
		return true
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return unicode.IsSpace(r) || r == '('
}

// isSortClauseOpener reports whether a trimmed line opens a `sort` clause.
//
// It requires the STRING LITERAL the grammar demands (parseSortFunction
// rejects a sort whose first argument is not a string), not merely the word
// `sort` followed by a boundary. That is stricter than isFilterClauseOpener
// on purpose, and it closes two holes a boundary check leaves open:
//
//   - FALSE POSITIVE. Unlike the filter scanner, this one READS literal
//     contents rather than blanking them, so a construct FIELD named `sort`
//     would otherwise have its annotation literals read as sort keys --
//     `sort string @enum("createdAt", "title")` in an args block, or a
//     `@default("createdAt")` on a concept field, would fail CI pointing
//     inside the annotation. A field named `sort` (letting a caller pick an
//     ordering) is an ordinary shape, so this had to be a scanner-side fix
//     rather than a tree convention. NOTE this covers the `name type
//     @annotation` field shape, NOT the `key "value"` block shape that
//     providers use (`voice "alloy"` in a `params` block): a `params` entry
//     spelled `sort "createdAt"` is byte-identical to a directionless sort
//     clause, so no line-local rule can separate them. Distinguishing it needs
//     enclosing-construct state, which this scanner deliberately does not
//     carry -- the filter sibling records why multi-line tracking was removed.
//     Reaching it requires a `params` key named exactly `sort` whose value is
//     exactly one of the five intrinsic names; the tree has none.
//   - FALSE NEGATIVE. The rewriter opens the clause with a bare
//     strings.HasPrefix(line, "sort") and no boundary at all
//     (component/language/parser/rewriter.go), so `sort"createdAt"` with no
//     space IS a sort clause to the engine. Skipping optional whitespace
//     before the quote keeps the scanner and the rewriter agreeing.
//
// The whitespace skip is UNICODE-aware for the same reason: the rewriter
// separates the keyword from the arguments with strings.TrimSpace, which is
// unicode.IsSpace-based, so `sort<NBSP>"createdAt"` rewrites byte-identically
// to the ASCII-space form and is a live clause. An ASCII-only TrimLeft here
// would let it bypass the gate -- and NBSP is exactly the character the
// rune-column fix in bareRowIntrinsicSortKeyRule exists to handle, so treating
// it as unreachable one column to the left would contradict that.
//
// `sortOrder "x"` is still excluded -- after the `sort` prefix comes `O`,
// which is neither whitespace nor a quote.
func isSortClauseOpener(trimmed string) bool {
	rest, ok := strings.CutPrefix(trimmed, "sort")
	if !ok {
		return false
	}
	return strings.HasPrefix(strings.TrimLeftFunc(rest, unicode.IsSpace), `"`)
}

// ScanBareRowIntrinsicSortKeys returns every sort KEY in source that names a
// row intrinsic without the `row.` namespace (memql#2786) -- the ordering half
// of the filter rule above.
//
// It is a sibling of ScanBareRowIntrinsics rather than a branch inside it
// because the two read opposite halves of a line. A filter names fields as
// CODE, so that scanner blanks string literals before matching; a sort names
// them as STRING LITERALS (`sort "createdAt", "desc"`), so this one must read
// literal contents and ignore the code around them. Folding both into one
// regex pass would mean a scanner that both blanks and reads the same bytes.
//
// The ambiguity is identical to the filter case and so is the fix: in
// `sort "id"` the key can be the row id or a payload property named `id`, and
// the two compile to completely different ORDER BY expressions -- a table
// column vs `payload #>> '{id}'` (component/memql compileSortField).
//
// Key-vs-direction classification mirrors parseSortFunction
// (component/language/parser/parser.go) exactly: literals alternate
// field [, direction] [, field [, direction]] ..., and a literal counts as a
// direction only when it directly follows a field AND spells asc/desc. A first
// literal spelled "desc" is therefore a FIELD here, as it is to the parser.
//
// Scope is the five SCALAR intrinsics compileSortField resolves under `row.`
// (id / concept / type / createdAt / createdBy). `provenance` is deliberately
// absent: it is object-valued with no ordering, and `row.provenance` is
// rejected outright by compileSortField (locked by
// TestRowNamespaceSortRejectsNonIntrinsic), so suggesting it would name a
// spelling nothing accepts -- the same reasoning that gives provenance its
// mandatory-leaf arm in the filter scanner.
//
// Comment and block-comment handling matches the filter scanner, including its
// documented multi-line-string trade-off.
//
// KNOWN, ACCEPTED GAP: the scanner reads a literal's RAW text, while the lexer
// resolves escapes -- so `sort "\u0063reatedAt"` is a bare intrinsic to the
// engine and invisible here. Unescaping would mean reimplementing the lexer's
// escape handling in a package that must not import it, to close a spelling
// no author produces by accident. The gate's job is catching the ordinary
// bare key, and it does that exactly.
func ScanBareRowIntrinsicSortKeys(source string) []BareRowIntrinsic {
	var found []BareRowIntrinsic

	// Columns count against the ORIGINAL line for the same reason as the
	// filter scanner: BlankBlockComments preserves byte offsets but not rune
	// counts.
	original := strings.Split(source, "\n")

	for i, line := range strings.Split(BlankBlockComments(source), "\n") {
		code := blankLineComments(line)
		if !isSortClauseOpener(strings.TrimSpace(code)) {
			continue
		}
		literals := stringLiterals(code)
		for j := 0; j < len(literals); j++ {
			key := literals[j]
			// Consume an optional trailing direction so the NEXT iteration
			// lands on a field, never on "asc"/"desc".
			if j+1 < len(literals) && isSortDirectionLiteral(literals[j+1].value) {
				j++
			}
			name, ok := canonicalScalarIntrinsic[strings.ToLower(strings.TrimSpace(key.value))]
			if !ok {
				continue
			}
			prefix := line[:key.start]
			if i < len(original) && key.start <= len(original[i]) {
				prefix = original[i][:key.start]
			}
			found = append(found, BareRowIntrinsic{
				Line:   i + 1,
				Column: utf8.RuneCountInString(prefix) + 1,
				Text:   key.value,
				Name:   name,
			})
		}
	}
	return found
}

// isSortDirectionLiteral mirrors the parser predicate of the same name
// (component/language/parser/parser.go). Duplicated rather than imported for
// the reason canonicalScalarIntrinsic is: sense is consumed by the editor AND
// by the dsl conformance suite, and importing the parser from here would pull
// the language package into both.
func isSortDirectionLiteral(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "asc", "desc":
		return true
	}
	return false
}

// sortLiteral is one string literal on a clause line: its contents and the
// byte offset of the first CONTENT byte (past the opening quote), so a
// diagnostic underlines the key itself rather than the quote.
type sortLiteral struct {
	value string
	start int
}

// stringLiterals returns the double-quoted literals on a line in source order.
// Escape pairs are skipped so `"a\"b"` is one literal, matching the lexer.
// An unterminated literal is ignored rather than run to end-of-line: a sort
// clause with an unbalanced quote does not parse, so there is no authored key
// to report.
func stringLiterals(line string) []sortLiteral {
	var out []sortLiteral
	for i := 0; i < len(line); i++ {
		if line[i] != '"' {
			continue
		}
		start := i + 1
		j := start
		closed := false
		for ; j < len(line); j++ {
			if line[j] == '\\' && j+1 < len(line) {
				j++
				continue
			}
			if line[j] == '"' {
				closed = true
				break
			}
		}
		if !closed {
			break
		}
		out = append(out, sortLiteral{value: line[start:j], start: start})
		i = j
	}
	return out
}

// blankLineComments replaces a trailing `//` comment with spaces while
// PRESERVING string-literal contents -- the inverse of
// blankLineCommentsAndStrings, which the filter scanner needs and this one
// must not use. Quote-awareness still matters: a `//` inside a literal is
// content, not a comment.
func blankLineComments(line string) string {
	out := []byte(line)
	inStr := false
	for i := 0; i < len(out); i++ {
		switch {
		case inStr && out[i] == '\\' && i+1 < len(out):
			i++
		case out[i] == '"':
			inStr = !inStr
		case inStr:
			// literal content -- preserved
		case out[i] == '/' && i+1 < len(out) && out[i+1] == '/':
			for j := i; j < len(out); j++ {
				out[j] = ' '
			}
			return string(out)
		}
	}
	return string(out)
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
