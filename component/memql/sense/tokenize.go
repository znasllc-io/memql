package sense

import (
	"sort"
	"strings"

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

	// Phase 3: Map parser tokens to semantic types.
	for _, pt := range parserTokens {
		if pt.Type == parser.TokenEOF {
			continue
		}
		st := mapTokenType(pt)
		tokens = append(tokens, st)
	}

	// Phase 4: Merge in comments, sorted by position.
	tokens = mergeTokens(tokens, comments)

	return tokens
}

// mapTokenType maps a parser token to a semantic token. Keyword coloring is
// SoT-driven: reserved words (use, in, if, return, ...) color via the lexer's
// TokenKeyword* set, and lowercase construct keywords color via constructKeywords
// (projected from dslspec). The retired-operator audit item ("import"/"has"
// coloring) no longer applies -- imports are `use` (colored) and `has` was
// removed with the single `in` membership operator.
func mapTokenType(pt parser.Token) Token {
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
		case constructKeywords[pt.Literal] || invocationKindKeywords[pt.Literal]:
			// Lowercase construct keywords (query / mutate / logic / action /
			// ...) AND invocation kind prefixes (notably `mutation`, whose
			// declaration keyword is `mutate` -- E6, memql#2392) lex as
			// identifiers.
			// capability / shape / concept / ...) lex as identifiers -- the core
			// lexer only keywords the capitalized receiver forms (Query /
			// Mutation / ...). Color them at the Sense layer, sourced from
			// dslspec (constructKeywords) so a future grammar epic inherits
			// coloring. Accepted trade-off: an identifier (e.g. a field name)
			// that happens to share a construct keyword's spelling over-colors;
			// scoping by position would need a parser inside the tokenizer.
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

// positionFromOffset converts a byte offset to a line/column position.
func positionFromOffset(source string, offset int) Position {
	line := 1
	col := 1
	for i := 0; i < offset && i < len(source); i++ {
		if source[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
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
