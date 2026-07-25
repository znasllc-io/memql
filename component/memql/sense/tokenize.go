package sense

import (
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/language/parser"
)

// Tokenize returns semantic tokens for syntax highlighting.
func (s *Service) Tokenize(source string) []Token {
	if source == "" {
		return nil
	}

	// Phase 1: Extract comments (lexer skips them).
	comments := extractComments(source)

	// Phase 2: Tokenize with the MemQL lexer.
	lexer := parser.NewLexer(source)
	parserTokens, err := lexer.Tokenize()

	var tokens []Token

	if err != nil {
		// On lexer error, return whatever tokens we have plus comments.
		tokens = append(tokens, comments...)
		return tokens
	}

	// Phase 3: Map parser tokens to semantic types. Construct-keyword
	// coloring is position-aware, so the classification pass runs first.
	keywordPos, conceptPos := classifyConstructPositions(parserTokens)
	for i, pt := range parserTokens {
		if pt.Type == parser.TokenEOF {
			continue
		}
		st := mapTokenType(pt, keywordPos[i], conceptPos[i])
		tokens = append(tokens, st)
	}

	// Phase 4: Merge in comments, sorted by position.
	tokens = mergeTokens(tokens, comments)

	return tokens
}

// classifyConstructPositions decides which construct-keyword-spelled
// identifiers actually sit in a keyword position, and which identifier a
// declaration signature binds as a concept.
//
// The lowercase construct keywords (shape / mutate / action / capability /
// ...) lex as plain identifiers, so membership in constructKeywords alone
// cannot tell a declaration header from a payload field that happens to
// share the spelling -- `capability` is both a construct keyword and a
// field of v1:actions:action. The enclosing block plus "leads its line"
// separates them without a parser: a declaration header is the first
// token on its line at top level, an invocation step leads with a
// construct kind anywhere, and inside a field-list body a leading
// identifier names data rather than introducing anything.
func classifyConstructPositions(tokens []parser.Token) (keywordPos, conceptPos map[int]bool) {
	keywordPos = make(map[int]bool)
	conceptPos = make(map[int]bool)

	var stack []string
	for i, t := range tokens {
		switch t.Type {
		case parser.TokenBraceOpen:
			stack = append(stack, blockLabelBefore(tokens, i))
			continue
		case parser.TokenBraceClose:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			continue
		}
		if t.Type != parser.TokenIdentifier {
			continue
		}
		if !constructKeywords[t.Literal] && !invocationKindKeywords[t.Literal] {
			continue
		}
		// An invocation names a construct kind then a callee then `(`
		// -- `mutation createFoo(...)`, `capability script(...)`, and
		// the mid-line `existing := query existingCluster()`. It is a
		// keyword wherever it appears, so this test precedes the
		// line-leading one.
		if i+2 < len(tokens) &&
			tokens[i+1].Type == parser.TokenIdentifier &&
			tokens[i+2].Type == parser.TokenParenOpen {
			keywordPos[i] = true
			continue
		}
		// The automation arrow names its target by kind:
		// `automation x @trigger(...) => logic handleX`.
		if i > 0 && tokens[i-1].Type == parser.TokenOperator && tokens[i-1].Literal == "=>" {
			keywordPos[i] = true
			continue
		}
		// Past that, only a line-leading token can introduce anything.
		// A word sharing its line with a preceding token is an ordinary
		// identifier -- the bound concept of a signature, an annotation
		// argument key, or a field type.
		if i > 0 && tokens[i-1].Line == t.Line {
			continue
		}
		if len(stack) == 0 {
			keywordPos[i] = true
			markSignatureConcept(tokens, i, conceptPos)
			continue
		}
		// Otherwise the enclosing block decides. A field-list body holds
		// data names (`capability` projected from v1:actions:action); any
		// other construct body holds clauses, where a construct-keyword
		// spelling really is one -- notably the `shape` clause of a query.
		if !fieldListBlocks[innermostLabel(stack)] {
			keywordPos[i] = true
		}
	}
	return keywordPos, conceptPos
}

// fieldListBlocks are the bodies whose contents are FIELDS, projections
// or data keys rather than clauses. A bare identifier inside one names
// data and never introduces a construct, so a field that happens to be
// spelled like a construct keyword must not color as one. Everything not
// listed here (query / mutate / logic / automation bodies) holds clauses
// and keeps keyword coloring -- `shape participantFull` inside a query is
// a genuine clause keyword.
var fieldListBlocks = map[string]bool{
	// Constructs whose body IS the schema.
	"concept": true, "shape": true, "tool": true,
	"prompt": true, "builtin": true, "provider": true,
	// Blocks that declare or populate fields inside any construct.
	"args": true, "params": true, "accept": true, "stamp": true,
	"insert": true, "update": true, "data": true, "auth": true,
}

// innermostLabel returns the label of the innermost open block, or "" at
// top level.
func innermostLabel(stack []string) string {
	if len(stack) == 0 {
		return ""
	}
	return stack[len(stack)-1]
}

// blockLabelBefore returns the label that opens the brace at index i --
// the first identifier of the run preceding it, so `shape action
// actionFull {` labels the block "shape" and `args {` labels it "args".
// A preceding `@annotation` is dropped, mirroring resolveEnclosingConstruct.
func blockLabelBefore(tokens []parser.Token, i int) string {
	var idents []string
	j := i - 1
	for ; j >= 0 && len(idents) < 3; j-- {
		if tokens[j].Type == parser.TokenIdentifier {
			idents = append([]string{tokens[j].Literal}, idents...)
			continue
		}
		break
	}
	if j >= 0 && tokens[j].Type == parser.TokenAt && len(idents) > 0 {
		idents = idents[1:]
	}
	if len(idents) == 0 {
		return ""
	}
	return idents[0]
}

// markSignatureConcept records the concept a declaration signature binds.
// The two-identifier signature (`shape <Concept> <name>`, `mutate
// <Concept> <name>`, `spec <Bound> <name>`) carries it in the middle
// slot; `concept <Name>` declares one outright. Every other header
// (`trait isActiveRecord`, `provider chat54Mini`) names only itself.
func markSignatureConcept(tokens []parser.Token, kw int, conceptPos map[int]bool) {
	var run []int
	for j := kw + 1; j < len(tokens) && tokens[j].Type == parser.TokenIdentifier; j++ {
		run = append(run, j)
	}
	switch {
	case len(run) >= 2:
		conceptPos[run[0]] = true
	case len(run) == 1 && tokens[kw].Literal == "concept":
		conceptPos[run[0]] = true
	}
}

// mapTokenType maps a parser token to a semantic token. Keyword coloring is
// SoT-driven: reserved words (use, in, if, return, ...) color via the lexer's
// TokenKeyword* set, and lowercase construct keywords color via constructKeywords
// (projected from dslspec) -- but only in the keyword POSITIONS resolved by
// classifyConstructPositions. The retired-operator audit item ("import"/"has"
// coloring) no longer applies -- imports are `use` (colored) and `has` was
// removed with the single `in` membership operator.
func mapTokenType(pt parser.Token, isKeywordPos, isConceptPos bool) Token {
	var tokenType string
	switch pt.Type {
	case parser.TokenKeywordFunc,
		parser.TokenKeywordFor, parser.TokenKeywordRange,
		parser.TokenKeywordIf, parser.TokenKeywordElse,
		parser.TokenKeywordSwitch, parser.TokenKeywordCase, parser.TokenKeywordDefault,
		parser.TokenKeywordContinue, parser.TokenKeywordBreak, parser.TokenKeywordReturn,
		parser.TokenKeywordNil, parser.TokenKeywordRetry, parser.TokenKeywordWhen,
		parser.TokenKeywordAs, parser.TokenKeywordWhere, parser.TokenKeywordUse,
		parser.TokenKeywordConcept,
		parser.TokenKeywordIn, parser.TokenKeywordNot,
		parser.TokenKeywordQuery, parser.TokenKeywordMutation,
		parser.TokenKeywordAutomation, parser.TokenKeywordSpec,
		parser.TokenKeywordTool, parser.TokenKeywordBuiltin:
		tokenType = "keyword"

	case parser.TokenString:
		tokenType = "string"

	case parser.TokenNumber:
		tokenType = "number"

	case parser.TokenOperator,
		parser.TokenDefine, parser.TokenAmpAmp, parser.TokenBang,
		parser.TokenQuestion, parser.TokenQuestionDot, parser.TokenQuestionQuestion:
		tokenType = "operator"

	case parser.TokenAt:
		tokenType = "annotation"

	case parser.TokenParenOpen, parser.TokenParenClose,
		parser.TokenBraceOpen, parser.TokenBraceClose,
		parser.TokenBracketOpen, parser.TokenBracketClose,
		parser.TokenColon, parser.TokenSemicolon, parser.TokenComma:
		tokenType = "punctuation"

	case parser.TokenIdentifier:
		switch {
		case strings.Contains(pt.Literal, ":"):
			tokenType = "concept"
		case isConceptPos:
			// The concept a signature binds (`shape <Concept> <name>`) or
			// declares (`concept <Name>`). Colored as a type rather than a
			// keyword so the binding reads distinctly from the construct
			// keyword introducing it.
			tokenType = "concept"
		case isKeywordPos:
			// Lowercase construct keywords (query / mutate / logic / action /
			// capability / shape / concept / ...) AND invocation kind prefixes
			// (notably `mutation`, whose declaration keyword is `mutate` --
			// E6, memql#2392) lex as identifiers -- the core lexer only
			// keywords the capitalized receiver forms (Query / Mutation /
			// ...). Color them at the Sense layer, sourced from dslspec
			// (constructKeywords) so a future grammar epic inherits coloring.
			tokenType = "keyword"
		default:
			tokenType = "identifier"
		}

	default:
		tokenType = "identifier"
	}

	// Use the lexer-stamped end positions instead of computing them
	// from len(Literal). For TokenString the Literal has quotes
	// stripped and escapes decoded, so length-based math undershoots
	// the source span by at least 2 (the quote chars) -- and more
	// when escape sequences are present. The bug surfaced in the
	// cockpit's viewer as the trailing 1-2 cells of every string
	// rendering unstyled (memql-cockpit#114).
	return Token{
		Type:    tokenType,
		Literal: pt.Literal,
		Range: Range{
			Start: Position{Line: pt.Line, Column: pt.Column},
			End:   Position{Line: pt.EndLine, Column: pt.EndCol},
		},
	}
}

// extractComments scans the source for line and block comments.
func extractComments(source string) []Token {
	var tokens []Token
	lines := strings.Split(source, "\n")

	for lineNum, line := range lines {
		lineIdx := lineNum + 1 // 1-indexed

		// Line comments: //
		if idx := strings.Index(line, "//"); idx >= 0 {
			// Check not inside a string (simple heuristic: count quotes before)
			if !isInsideString(line, idx) {
				tokens = append(tokens, Token{
					Type:    "comment",
					Literal: line[idx:],
					Range: Range{
						Start: Position{Line: lineIdx, Column: idx + 1},
						End:   Position{Line: lineIdx, Column: len(line) + 1},
					},
				})
			}
		}
	}

	// Block comments: /* ... */
	for i := 0; i < len(source)-1; i++ {
		if source[i] == '/' && source[i+1] == '*' {
			startPos := positionFromOffset(source, i)
			// Skip a `/*` that sits inside a string literal (e.g. the value
			// "/* not */") -- it belongs to the lexer's string token, not a
			// real comment. Emitting one here would mint a spurious comment
			// token overlapping the string token, which LSP disallows. Reuse
			// the line-based isInsideString heuristic (as the // scan above
			// already does): startPos.Column-1 is the 0-indexed offset of `/*`
			// within its line.
			if l := startPos.Line - 1; l >= 0 && l < len(lines) &&
				isInsideString(lines[l], startPos.Column-1) {
				continue
			}
			end := strings.Index(source[i+2:], "*/")
			if end < 0 {
				end = len(source) - i - 2
			}
			endIdx := i + 2 + end + 2
			if endIdx > len(source) {
				endIdx = len(source)
			}
			endPos := positionFromOffset(source, endIdx)
			tokens = append(tokens, Token{
				Type:    "comment",
				Literal: source[i:endIdx],
				Range: Range{
					Start: startPos,
					End:   endPos,
				},
			})
			i = endIdx - 1 // skip past the comment
		}
	}

	// mergeTokens (Tokenize Phase 4) merges two sorted lists, but line
	// comments are collected in line order and block comments afterwards in
	// source order, so the combined slice is not globally sorted by
	// (line, col). Sort here so every consumer -- the LSP delta-encoder (which
	// silently drops a token on a backwards delta), the Cockpit gRPC path, and
	// the lexer-error fallback -- receives a monotonic, mergeable stream.
	sort.SliceStable(tokens, func(a, b int) bool {
		return tokenBefore(tokens[a], tokens[b])
	})

	return tokens
}

// isInsideString checks if a position is inside a quoted string (simple heuristic).
func isInsideString(line string, pos int) bool {
	inString := false
	for i := 0; i < pos; i++ {
		if line[i] == '"' && (i == 0 || line[i-1] != '\\') {
			inString = !inString
		}
	}
	return inString
}

// positionFromOffset converts a byte offset to a line / RUNE-column position.
//
// Columns count runes, not bytes (memql#2788): cmd/memql-lsp/internal/position
// states the contract -- the lexer scans a []rune and does one column++ per
// rune -- so a byte-counting walk shifts every position right by one per extra
// UTF-8 byte earlier on the line. This is the shared conversion behind
// findInSource, so fixing it here fixes every rule anchoring through it instead
// of each re-deriving the arithmetic.
//
// Decoding rather than testing utf8.RuneStart also keeps invalid UTF-8
// consistent with []rune and utf8.RuneCountInString: DecodeRuneInString yields
// (RuneError, 1) per bad byte, so it contributes exactly one column. Counting
// only rune-start bytes would contribute zero for a stray continuation byte and
// place the squiggle LEFT of where the author sees it.
func positionFromOffset(source string, offset int) Position {
	line := 1
	col := 1
	for i := 0; i < offset && i < len(source); {
		r, size := utf8.DecodeRuneInString(source[i:])
		if size == 0 {
			break
		}
		if r == '\n' {
			line++
			col = 1
		} else {
			col++
		}
		i += size
	}
	return Position{Line: line, Column: col}
}

// mergeTokens merges two sorted token lists by position.
func mergeTokens(a, b []Token) []Token {
	if len(b) == 0 {
		return a
	}
	result := make([]Token, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if tokenBefore(a[i], b[j]) {
			result = append(result, a[i])
			i++
		} else {
			result = append(result, b[j])
			j++
		}
	}
	result = append(result, a[i:]...)
	result = append(result, b[j:]...)
	return result
}

// tokenBefore returns true if token a comes before token b.
func tokenBefore(a, b Token) bool {
	if a.Range.Start.Line != b.Range.Start.Line {
		return a.Range.Start.Line < b.Range.Start.Line
	}
	return a.Range.Start.Column < b.Range.Start.Column
}
