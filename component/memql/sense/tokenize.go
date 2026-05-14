package sense

import (
	"strings"

	"github.com/visionarys-io/memql/component/language/parser"
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

// mapTokenType maps a parser token to a semantic token.
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
		parser.TokenKeywordIn, parser.TokenKeywordHas, parser.TokenKeywordNot,
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
		if strings.Contains(pt.Literal, ":") {
			tokenType = "concept"
		} else {
			tokenType = "identifier"
		}

	default:
		tokenType = "identifier"
	}

	endCol := pt.Column + len(pt.Literal)
	return Token{
		Type:    tokenType,
		Literal: pt.Literal,
		Range: Range{
			Start: Position{Line: pt.Line, Column: pt.Column},
			End:   Position{Line: pt.Line, Column: endCol},
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
