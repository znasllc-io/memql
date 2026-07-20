package sense

import (
	"regexp"
	"strings"

	"github.com/znasllc-io/memql/component/language/parser"
)

// ContextKind describes the syntactic context at a cursor position.
type ContextKind int

const (
	ContextTopLevel         ContextKind = iota // Outside any definition
	ContextAnnotation                          // After @
	ContextAnnotationArgs                      // Inside @trigger(...)
	ContextFuncBody                            // Inside func body { ... }
	ContextFuncCallArgs                        // Inside someFunc(...)
	ContextConceptFilter                       // After concept==
	ContextFieldAccess                         // After node.payload. or event.payload.
	ContextReceiver                            // Inside func (...) receiver declaration
	ContextUseDeclaration                      // After "use "
	ContextConceptDef                          // Inside concept { ... }
	ContextConstructConcept                    // After a concept-binding construct keyword (mutation/query/seed/shape <Concept>)
)

// CursorContext describes the syntactic context at a cursor position.
type CursorContext struct {
	Kind           ContextKind
	Prefix         string // partial identifier before cursor
	ParentFunc     string // function being called (for signature help)
	ArgIndex       int    // argument position (0-indexed)
	ReceiverType   string // if inside a func definition with a known receiver
	AnnotationName string // if inside annotation args
	ConceptName    string // if in field access, the relevant concept
	ConstructKey   string // if after a concept-binding construct keyword, the keyword (mutation/query/seed/shape)
	Source         string // full source text (so completers can scan file-scope imports)
	FilePath       string // the document's path (so completers can derive the ambient domain, #2617)
	AccessorRoot   string // for ContextFieldAccess: the dotted path before the final dot ("actor", "event.actor", ...)
}

// analyzeCursorContext determines the syntactic context at a cursor position.
func analyzeCursorContext(source string, line, col int) CursorContext {
	ctx := analyzeCursorContextInner(source, line, col)
	// Thread the full source through so completers can scan file-scope `use`
	// imports (the construct-concept import-suggestion path needs it).
	ctx.Source = source
	return ctx
}

// analyzeCursorContextInner does the actual context classification; the public
// wrapper stamps Source on whatever it returns.
func analyzeCursorContextInner(source string, line, col int) CursorContext {
	// Get text up to cursor position.
	textBefore := textBeforeCursor(source, line, col)
	if textBefore == "" {
		return CursorContext{Kind: ContextTopLevel}
	}

	// Tokenize the text before the cursor.
	lexer := parser.NewLexer(textBefore)
	tokens, _ := lexer.Tokenize()

	// Remove EOF token.
	if len(tokens) > 0 && tokens[len(tokens)-1].Type == parser.TokenEOF {
		tokens = tokens[:len(tokens)-1]
	}

	if len(tokens) == 0 {
		return CursorContext{Kind: ContextTopLevel}
	}

	// Extract the prefix (partial identifier at cursor).
	prefix := extractPrefix(textBefore)

	// Check contexts in priority order.

	// 1. After @: annotation context
	if ctx, ok := checkAnnotationContext(tokens, prefix); ok {
		return ctx
	}

	// 2. Inside func (...) receiver
	if ctx, ok := checkReceiverContext(tokens, prefix); ok {
		return ctx
	}

	// 3. After "use ": use declaration
	if ctx, ok := checkUseContext(tokens, prefix); ok {
		return ctx
	}

	// 4. After concept==: concept filter
	if ctx, ok := checkConceptFilterContext(tokens, prefix); ok {
		return ctx
	}

	// 4.5 After a concept-binding construct keyword: mutation/query/seed/shape <Concept>
	if ctx, ok := checkConstructConceptContext(tokens, prefix); ok {
		return ctx
	}

	// 4.7 After a dotted accessor: member completion (#2624). Must sit
	// ABOVE the func-call check so `cond(actor.` completes members, and
	// above the body fallback so a dot context never offers the
	// everything list.
	if ctx, ok := checkFieldAccessContext(textBefore); ok {
		return ctx
	}

	// 5. Inside function call args: funcName(...)
	if ctx, ok := checkFuncCallContext(tokens, prefix, textBefore); ok {
		return ctx
	}

	// 6. Inside concept definition body
	if ctx, ok := checkConceptDefContext(tokens, prefix); ok {
		return ctx
	}

	// 7. Inside func body (between braces)
	if isInsideFuncBody(tokens) {
		return CursorContext{Kind: ContextFuncBody, Prefix: prefix}
	}

	return CursorContext{Kind: ContextTopLevel, Prefix: prefix}
}

// textBeforeCursor extracts text from the source up to the cursor position.
func textBeforeCursor(source string, line, col int) string {
	lines := strings.Split(source, "\n")
	if line <= 0 || line > len(lines) {
		return source
	}

	var sb strings.Builder
	for i := 0; i < line-1 && i < len(lines); i++ {
		sb.WriteString(lines[i])
		sb.WriteByte('\n')
	}
	lineText := lines[line-1]
	endCol := col - 1
	if endCol > len(lineText) {
		endCol = len(lineText)
	}
	if endCol < 0 {
		endCol = 0
	}
	sb.WriteString(lineText[:endCol])
	return sb.String()
}

// extractPrefix gets the partial identifier at the cursor.
func extractPrefix(textBefore string) string {
	if len(textBefore) == 0 {
		return ""
	}
	// Walk backwards to find the start of the current identifier.
	i := len(textBefore) - 1
	for i >= 0 && isIdentChar(textBefore[i]) {
		i--
	}
	return textBefore[i+1:]
}

func isIdentChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == ':'
}

// checkAnnotationContext checks if cursor is after @ or inside @name(...).
func checkAnnotationContext(tokens []parser.Token, prefix string) (CursorContext, bool) {
	n := len(tokens)
	if n == 0 {
		return CursorContext{}, false
	}

	// After bare @
	if tokens[n-1].Type == parser.TokenAt {
		return CursorContext{Kind: ContextAnnotation, Prefix: ""}, true
	}

	// After @name (identifier immediately after @)
	if n >= 2 && tokens[n-2].Type == parser.TokenAt && tokens[n-1].Type == parser.TokenIdentifier {
		return CursorContext{Kind: ContextAnnotation, Prefix: tokens[n-1].Literal}, true
	}

	// Inside @name(...) -- find matching @ before open paren
	parenDepth := 0
	for i := n - 1; i >= 0; i-- {
		switch tokens[i].Type {
		case parser.TokenParenClose:
			parenDepth++
		case parser.TokenParenOpen:
			parenDepth--
			if parenDepth < 0 {
				// Found unmatched open paren. Check if preceded by @name.
				if i >= 2 && tokens[i-2].Type == parser.TokenAt && tokens[i-1].Type == parser.TokenIdentifier {
					return CursorContext{
						Kind:           ContextAnnotationArgs,
						Prefix:         prefix,
						AnnotationName: tokens[i-1].Literal,
					}, true
				}
			}
		}
	}

	return CursorContext{}, false
}

// checkReceiverContext checks if cursor is inside func (...) receiver.
func checkReceiverContext(tokens []parser.Token, prefix string) (CursorContext, bool) {
	n := len(tokens)
	// Pattern: func (prefix  OR  func (
	for i := n - 1; i >= 0; i-- {
		if tokens[i].Type == parser.TokenParenOpen {
			if i >= 1 && tokens[i-1].Type == parser.TokenKeywordFunc {
				return CursorContext{Kind: ContextReceiver, Prefix: prefix}, true
			}
			break
		}
		// Only allow identifiers between the open paren and cursor
		if tokens[i].Type != parser.TokenIdentifier {
			break
		}
	}
	return CursorContext{}, false
}

// checkUseContext checks if cursor is after "use ".
func checkUseContext(tokens []parser.Token, prefix string) (CursorContext, bool) {
	n := len(tokens)
	if n >= 1 && tokens[n-1].Type == parser.TokenKeywordUse {
		return CursorContext{Kind: ContextUseDeclaration, Prefix: prefix}, true
	}
	if n >= 2 && tokens[n-2].Type == parser.TokenKeywordUse && tokens[n-1].Type == parser.TokenIdentifier {
		return CursorContext{Kind: ContextUseDeclaration, Prefix: tokens[n-1].Literal}, true
	}
	return CursorContext{}, false
}

// checkConceptFilterContext checks if cursor is after concept==.
func checkConceptFilterContext(tokens []parser.Token, prefix string) (CursorContext, bool) {
	n := len(tokens)
	// Pattern: concept == prefix
	if n >= 2 && tokens[n-2].Type == parser.TokenOperator && tokens[n-2].Literal == "==" {
		// Check if the token before == is "concept"
		if n >= 3 && tokens[n-3].Type == parser.TokenIdentifier && tokens[n-3].Literal == "concept" {
			return CursorContext{Kind: ContextConceptFilter, Prefix: prefix}, true
		}
	}
	// Pattern: concept==
	if n >= 1 && tokens[n-1].Type == parser.TokenOperator && tokens[n-1].Literal == "==" {
		if n >= 2 && tokens[n-2].Type == parser.TokenIdentifier && tokens[n-2].Literal == "concept" {
			return CursorContext{Kind: ContextConceptFilter, Prefix: ""}, true
		}
	}
	return CursorContext{}, false
}

// checkConstructConceptContext checks if the cursor sits where a concept-binding
// construct's signature expects a concept: the tokens before the cursor are
// `<constructKeyword> [partialIdent]` where constructKeyword is a dslspec
// construct with ConceptInSignature==true (mutation/query/seed/shape). The
// keyword set is derived from the spec so it cannot drift from the grammar.
//
// Author-facing construct keywords (mutation/query/seed/shape) lex as plain
// identifiers -- only the retired capitalized receiver forms (Query/Mutation)
// are keyword tokens -- so detection matches on the identifier literal against
// the spec's concept-signature keyword set.
func checkConstructConceptContext(tokens []parser.Token, prefix string) (CursorContext, bool) {
	n := len(tokens)
	if n == 0 {
		return CursorContext{}, false
	}

	// This is a top-level signature position; bail if we're inside any
	// unmatched brace / paren / bracket (a body, args block, or call args),
	// where these keywords mean something else (or nothing).
	depth := 0
	for _, t := range tokens {
		switch t.Type {
		case parser.TokenBraceOpen, parser.TokenParenOpen, parser.TokenBracketOpen:
			depth++
		case parser.TokenBraceClose, parser.TokenParenClose, parser.TokenBracketClose:
			depth--
		}
	}
	if depth > 0 {
		return CursorContext{}, false
	}

	conceptKeywords := specConceptSignatureKeywords()

	// Case A: `<keyword> ` with an empty partial -- the last token is the
	// construct keyword identifier and the cursor is just past it.
	if prefix == "" {
		last := tokens[n-1]
		if last.Type == parser.TokenIdentifier && conceptKeywords[last.Literal] {
			return CursorContext{Kind: ContextConstructConcept, Prefix: "", ConstructKey: last.Literal}, true
		}
		return CursorContext{}, false
	}

	// Case B: `<keyword> <partialIdent>` -- the last token is the partial
	// concept name; the token before it is the construct keyword.
	if n >= 2 && tokens[n-1].Type == parser.TokenIdentifier && tokens[n-1].Literal == prefix {
		kwTok := tokens[n-2]
		if kwTok.Type == parser.TokenIdentifier && conceptKeywords[kwTok.Literal] {
			return CursorContext{Kind: ContextConstructConcept, Prefix: prefix, ConstructKey: kwTok.Literal}, true
		}
	}

	return CursorContext{}, false
}

// checkFuncCallContext checks if cursor is inside a function call's arguments.
func checkFuncCallContext(tokens []parser.Token, prefix, textBefore string) (CursorContext, bool) {
	parenDepth := 0
	for i := len(tokens) - 1; i >= 0; i-- {
		switch tokens[i].Type {
		case parser.TokenParenClose:
			parenDepth++
		case parser.TokenParenOpen:
			parenDepth--
			if parenDepth < 0 {
				// Found unmatched open paren.
				if i >= 1 && tokens[i-1].Type == parser.TokenIdentifier {
					funcName := tokens[i-1].Literal
					// Skip receiver declarations: func (Type)
					if i >= 2 && tokens[i-2].Type == parser.TokenKeywordFunc {
						return CursorContext{}, false
					}
					// Count commas to determine arg index.
					argIdx := countCommasBetween(tokens, i+1, len(tokens)-1)
					return CursorContext{
						Kind:       ContextFuncCallArgs,
						Prefix:     prefix,
						ParentFunc: funcName,
						ArgIndex:   argIdx,
					}, true
				}
				return CursorContext{}, false
			}
		}
	}
	return CursorContext{}, false
}

// checkConceptDefContext checks if cursor is inside a concept { ... } definition.
func checkConceptDefContext(tokens []parser.Token, prefix string) (CursorContext, bool) {
	braceDepth := 0
	for i := len(tokens) - 1; i >= 0; i-- {
		switch tokens[i].Type {
		case parser.TokenBraceClose:
			braceDepth++
		case parser.TokenBraceOpen:
			braceDepth--
			if braceDepth < 0 {
				// Inside unmatched brace. Check if preceded by concept Name.
				if i >= 2 && tokens[i-2].Type == parser.TokenKeywordConcept {
					return CursorContext{Kind: ContextConceptDef, Prefix: prefix}, true
				}
			}
		}
	}
	return CursorContext{}, false
}

// isInsideFuncBody checks if we're inside a function body (between braces after func declaration).
func isInsideFuncBody(tokens []parser.Token) bool {
	braceDepth := 0
	for i := len(tokens) - 1; i >= 0; i-- {
		switch tokens[i].Type {
		case parser.TokenBraceClose:
			braceDepth++
		case parser.TokenBraceOpen:
			braceDepth--
			if braceDepth < 0 {
				return true
			}
		}
	}
	return false
}

// countCommasBetween counts comma tokens between two indices.
func countCommasBetween(tokens []parser.Token, start, end int) int {
	count := 0
	depth := 0
	for i := start; i <= end && i < len(tokens); i++ {
		switch tokens[i].Type {
		case parser.TokenParenOpen, parser.TokenBraceOpen, parser.TokenBracketOpen:
			depth++
		case parser.TokenParenClose, parser.TokenBraceClose, parser.TokenBracketClose:
			depth--
		case parser.TokenComma:
			if depth == 0 {
				count++
			}
		}
	}
	return count
}

// checkFieldAccessContext recognizes a dotted member access at the
// cursor (#2624). Both lexer shapes reduce to the same TEXT shape at
// the cursor -- a trailing dot (`actor.`, Identifier + TokenDot) and a
// mid-member position (`actor.us`, one dot-joined identifier with the
// prefix after the last dot) -- so the detector reads the raw text:
// walk back over the member prefix, require a '.', then walk back over
// the dotted root. The root must start with a letter/underscore so a
// float literal (`3.`) never fires.
func checkFieldAccessContext(textBefore string) (CursorContext, bool) {
	i := len(textBefore) - 1
	for i >= 0 && isIdentChar(textBefore[i]) {
		i--
	}
	if i < 0 || textBefore[i] != '.' {
		return CursorContext{}, false
	}
	memberPrefix := textBefore[i+1:]
	j := i - 1
	for j >= 0 && (isIdentChar(textBefore[j]) || textBefore[j] == '.') {
		j--
	}
	root := textBefore[j+1 : i]
	if root == "" || strings.HasSuffix(root, ".") || !isIdentStart(root[0]) {
		return CursorContext{}, false
	}
	return CursorContext{
		Kind:         ContextFieldAccess,
		Prefix:       memberPrefix,
		AccessorRoot: root,
		ConceptName:  enclosingSignatureConcept(textBefore),
	}, true
}

func isIdentStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}

// enclosingSignatureConceptRe captures the concept short-name of the
// nearest preceding concept-binding construct header.
var enclosingSignatureConceptRe = regexp.MustCompile(`(?m)^[ \t]*(?:query|mutate|shape|seed)[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]+[A-Za-z_][A-Za-z0-9_]*[ \t]*\{`)

// enclosingSignatureConcept returns the signature concept of the last
// construct header above the cursor, or "" (automations/logic carry
// none).
func enclosingSignatureConcept(textBefore string) string {
	ms := enclosingSignatureConceptRe.FindAllStringSubmatch(textBefore, -1)
	if len(ms) == 0 {
		return ""
	}
	return ms[len(ms)-1][1]
}
