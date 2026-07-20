package parser

import (
	"regexp"
	"sort"
	"strings"
)

// RewriteAcceptStamp collapses arg-mirror runs inside struct-mutation
// write blocks into the accept/stamp form (#2616, form shipped by
// #2593):
//
//	insert {                      insert {
//	  args.slug                     accept { slug, name }
//	  args.name             ==>     stamp {
//	  status: "open"                  status: "open"
//	  id: makeId(args.slug)           id: makeId(args.slug)
//	}                               }
//	                              }
//
// Bare `args.X` mirrors (rule 15) and verbose `x: args.x` mirrors fold
// into accept; every other field moves verbatim into stamp -- the
// engine rejects loose fields beside a nested accept/stamp, so the
// rewrite is all-or-nothing per block. A block is only rewritten when
// the transformation is provably safe:
//
//   - at least two mirror fields fold (below that the sugar saves
//     nothing), every folded name is a declared arg (the load check
//     accept enforces), no key collides across accept and stamp,
//   - every field is single-line and brace-free (nested object values
//     and comments are preserved by NOT rewriting their block), and
//   - the rewritten mutation re-emits IDENTICALLY through
//     NormaliseMutationSource (payload compared order-insensitively --
//     accept hoists mirrors ahead of stamp fields, and the payload is
//     an object literal, so order carries no meaning).
//
// Blocks already using accept/stamp, and anything that fails a check,
// pass through byte-identical -- the rewrite is idempotent and the
// dsl/ conformance gate runs THIS function, so gate and codemod cannot
// disagree.
func RewriteAcceptStamp(src []byte) ([]byte, error) {
	runes := []rune(string(src))
	tokens, err := NewLexer(string(src)).Tokenize()
	if err != nil {
		return src, nil
	}
	if n := len(tokens); n > 0 && tokens[n-1].Type == TokenEOF {
		tokens = tokens[:n-1]
	}

	type edit struct {
		start, end  int // rune range of a write-block inner
		replacement string
	}
	var edits []edit

	depth := 0
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		switch tok.Type {
		case TokenBraceOpen:
			depth++
		case TokenBraceClose:
			depth--
		case TokenIdentifier:
			if depth != 0 || tok.Literal != "mutate" {
				continue
			}
			openIdx, closeIdx := constructBraceSpan(tokens, i)
			if openIdx < 0 {
				continue
			}
			bodyStart, bodyEnd := tokens[openIdx].EndPos, tokens[closeIdx].Pos
			if e, ok := acceptStampEditForMutation(runes, tokens[i:closeIdx+1], openIdx-i, bodyStart, bodyEnd); ok {
				edits = append(edits, edit{e.start, e.end, e.replacement})
			}
			// Skip past the whole construct (depth stays 0 after it).
			i = closeIdx
		}
	}

	if len(edits) == 0 {
		return src, nil
	}
	out := make([]rune, 0, len(runes))
	prev := 0
	for _, e := range edits {
		out = append(out, runes[prev:e.start]...)
		out = append(out, []rune(e.replacement)...)
		prev = e.end
	}
	out = append(out, runes[prev:]...)
	return []byte(string(out)), nil
}

// constructBraceSpan returns the token indexes of the construct's
// opening brace and its matching close, scanning from the keyword at
// kwIdx. Returns (-1, -1) when the braces never balance.
func constructBraceSpan(tokens []Token, kwIdx int) (openIdx, closeIdx int) {
	openIdx = -1
	for j := kwIdx + 1; j < len(tokens); j++ {
		if tokens[j].Type == TokenBraceOpen {
			openIdx = j
			break
		}
		if tokens[j].Type != TokenIdentifier {
			return -1, -1
		}
	}
	if openIdx < 0 {
		return -1, -1
	}
	d := 0
	for j := openIdx; j < len(tokens); j++ {
		switch tokens[j].Type {
		case TokenBraceOpen:
			d++
		case TokenBraceClose:
			d--
			if d == 0 {
				return openIdx, j
			}
		}
	}
	return -1, -1
}

type acceptStampEdit struct {
	start, end  int
	replacement string
}

var (
	bareMirrorRe    = regexp.MustCompile(`^args\.([A-Za-z_][A-Za-z0-9_]*)$`)
	verboseMirrorRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)[ \t]*:[ \t]*args\.([A-Za-z_][A-Za-z0-9_]*)$`)
	stampKeyRe      = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)[ \t]*:`)
)

// acceptStampEditForMutation inspects one `mutate` construct's tokens
// (mtoks[0] is the mutate keyword; braceIdx indexes its opening brace
// within mtoks) and returns the write-block inner replacement if the
// block is eligible.
func acceptStampEditForMutation(runes []rune, mtoks []Token, braceIdx, bodyStart, bodyEnd int) (acceptStampEdit, bool) {
	var none acceptStampEdit

	// Locate the write block at construct depth 1; bail on any existing
	// accept/stamp (already migrated) or a second write block.
	d := 0
	writeOpen, writeClose := -1, -1
	for j := braceIdx; j < len(mtoks); j++ {
		switch mtoks[j].Type {
		case TokenBraceOpen:
			d++
		case TokenBraceClose:
			d--
		case TokenIdentifier:
			if d != 1 {
				continue
			}
			switch mtoks[j].Literal {
			case "accept", "stamp":
				return none, false
			case "insert", "update":
				if j+1 < len(mtoks) && mtoks[j+1].Type == TokenBraceOpen {
					if writeOpen >= 0 {
						return none, false
					}
					o, c := constructBraceSpan(mtoks, j)
					if o < 0 {
						return none, false
					}
					writeOpen, writeClose = o, c
				}
			}
		}
	}
	if writeOpen < 0 {
		return none, false
	}

	innerStart, innerEnd := mtoks[writeOpen].EndPos, mtoks[writeClose].Pos
	inner := string(runes[innerStart:innerEnd])
	if strings.Contains(inner, "//") || strings.Contains(inner, "/*") {
		return none, false // comments are preserved by not rewriting
	}
	if hasMultilineField(inner) {
		// splitInsertFields glues depth>0 newlines to spaces, so a
		// paren-continued multi-line expression would reflow into one
		// line with the original indentation frozen as space runs
		// (#2660 review). Formatting is preserved by not rewriting.
		return none, false
	}

	fields, err := splitInsertFields(inner)
	if err != nil {
		return none, false
	}

	var acceptNames []string
	var stampFields []string
	seen := map[string]bool{}
	for _, f := range fields {
		if strings.ContainsAny(f, "{[") {
			return none, false // nested-object / array values stay longhand
		}
		if m := bareMirrorRe.FindStringSubmatch(f); m != nil {
			if seen[m[1]] {
				return none, false
			}
			seen[m[1]] = true
			acceptNames = append(acceptNames, m[1])
			continue
		}
		if m := verboseMirrorRe.FindStringSubmatch(f); m != nil && m[1] == m[2] {
			if seen[m[1]] {
				return none, false
			}
			seen[m[1]] = true
			acceptNames = append(acceptNames, m[1])
			continue
		}
		if m := stampKeyRe.FindStringSubmatch(f); m != nil {
			if seen[m[1]] {
				return none, false
			}
			seen[m[1]] = true
		} else {
			return none, false // unclassifiable field shape
		}
		stampFields = append(stampFields, f)
	}
	if len(acceptNames) < 2 {
		return none, false
	}

	// Every accepted name must be a declared arg, or the rewritten
	// mutation fails the accept load check.
	body := string(runes[bodyStart:bodyEnd])
	argsText, err := extractArgsBlock(body)
	if err != nil {
		return none, false
	}
	declared := argNamesFromArgsText(argsText)
	for _, nm := range acceptNames {
		if !declared[nm] {
			return none, false
		}
	}

	ind := innerIndent(inner)
	closeInd := blockCloseIndent(runes, innerEnd)
	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(ind + "accept { " + strings.Join(acceptNames, ", ") + " }\n")
	if len(stampFields) > 0 {
		b.WriteString(ind + "stamp {\n")
		for _, f := range stampFields {
			b.WriteString(ind + "  " + f + "\n")
		}
		b.WriteString(ind + "}\n")
	}
	b.WriteString(closeInd)

	// The proof: both spellings must emit the same procedural text
	// through the engine's own rewriter, modulo payload field order.
	oldConstruct := string(runes[mtoks[0].Pos:mtoks[len(mtoks)-1].EndPos])
	newConstruct := oldConstruct[:innerStart-mtoks[0].Pos] + b.String() + oldConstruct[innerEnd-mtoks[0].Pos:]
	if !emitEquivalent(oldConstruct, newConstruct) {
		return none, false
	}

	return acceptStampEdit{start: innerStart, end: innerEnd, replacement: b.String()}, true
}

// innerIndent returns the leading whitespace of the first non-empty
// line of a block inner, defaulting to four spaces.
func innerIndent(inner string) string {
	for _, line := range strings.Split(inner, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		return line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	}
	return "    "
}

// blockCloseIndent returns the whitespace immediately preceding the
// write block's closing brace, so the rebuilt inner reproduces the
// original close-line indentation.
func blockCloseIndent(runes []rune, innerEnd int) string {
	j := innerEnd
	for j > 0 && (runes[j-1] == ' ' || runes[j-1] == '\t') {
		j--
	}
	return string(runes[j:innerEnd])
}

// emitEquivalent reports whether two spellings of one mutation emit
// the same procedural text, comparing the payload object literal
// order-insensitively (it lowers to a map; accept hoists mirrors ahead
// of stamp fields).
func emitEquivalent(oldSrc, newSrc string) bool {
	oldEmit, err := NormaliseMutationSource(oldSrc)
	if err != nil {
		return false
	}
	newEmit, err := NormaliseMutationSource(newSrc)
	if err != nil {
		return false
	}
	return canonicalEmit(oldEmit) == canonicalEmit(newEmit)
}

// payloadRe finds the emitted payload object literal in both spellings
// of the write call: `..., payload={...})` when an id expression is
// hoisted, and the bare `insert(cid, {...})` form when there is none.
// Greedy .* reaches the LAST `})` -- migrated blocks carry no nested
// braces (fields containing `{` are skipped), so the span is exact.
var payloadRe = regexp.MustCompile(`\{(.*)\}\)`)

func canonicalEmit(emit string) string {
	m := payloadRe.FindStringSubmatchIndex(emit)
	if m == nil {
		return emit
	}
	fields, err := splitInsertFields(emit[m[2]:m[3]])
	if err != nil {
		return emit
	}
	for i, f := range fields {
		fields[i] = canonicalPayloadField(f)
	}
	sort.Strings(fields)
	return emit[:m[2]] + strings.Join(fields, ", ") + emit[m[3]:]
}

// canonicalPayloadField normalises one emitted payload field so the
// two spellings compare equal: the rule-15 bare mirror (`args.x`,
// which passes through the legacy write body verbatim and is resolved
// by the object-literal parser downstream) expands to its explicit
// `x: args.x` form, and `key : value` spacing collapses.
func canonicalPayloadField(f string) string {
	f = strings.TrimSpace(f)
	if m := bareMirrorRe.FindStringSubmatch(f); m != nil {
		return m[1] + ": args." + m[1]
	}
	if idx := strings.Index(f, ":"); idx >= 0 {
		return strings.TrimSpace(f[:idx]) + ": " + strings.TrimSpace(f[idx+1:])
	}
	return f
}

// hasMultilineField reports whether any field in a write-block inner
// continues across lines (a newline at bracket depth > 0, outside
// strings). splitInsertFields glues those newlines to spaces, so the
// check must run on the RAW inner text.
func hasMultilineField(inner string) bool {
	depth := 0
	inString := false
	var quote byte
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		if inString {
			// Escape-aware, matching skipStringLiteral: an escaped quote
			// must not close the string early and desync the depth
			// (#2660 delta review).
			if c == '\\' && quote == '"' && i+1 < len(inner) {
				i++
				continue
			}
			if c == quote {
				inString = false
			}
			continue
		}
		switch c {
		case '"', '`':
			inString = true
			quote = c
		case '{', '[', '(':
			depth++
		case '}', ']', ')':
			depth--
		case '\n':
			if depth > 0 {
				return true
			}
		}
	}
	return false
}
