package parser

import "strings"

// doc_comments.go -- parser-side attachment of /// doc-comment blocks
// (memql#2633), implementing rulings 1-2 of
// docs/internal/design/doc-comments-description-source.md.
//
// The lexer captures /// blocks on a side channel (Lexer.DocComments); the
// parser attaches a block to the immediately following declaration by line
// adjacency: the block must end on the line directly above the definition's
// first token, walking transparently over ordinary-comment lines. A blank
// line breaks adjacency (it is neither a comment line nor a token line), so
// a detached block simply never matches -- ignored, never an error.
// Annotations are transparent by construction: attachment anchors on the
// definition's FIRST token, which is the first annotation when annotations
// are present. The struct-form rewriter's hoisted args{} block is made
// transparent explicitly (addTransparentSpan at the two args interception
// sites).

// SetDocComments hands the parser the lexer's captured /// blocks and
// ordinary-comment line set. Entry points that parse declarations call this
// between NewParser and the parse; expression-only consumers skip it and the
// parser behaves exactly as before.
func (p *Parser) SetDocComments(blocks []DocCommentBlock, commentLines map[int]bool) {
	p.docBlocks = blocks
	p.docConsumed = make([]bool, len(blocks))
	p.transparentLines = make(map[int]bool, len(commentLines))
	for line := range commentLines {
		p.transparentLines[line] = true
	}
}

// addTransparentSpan marks a line span transparent for attachment -- used
// for args{} blocks sitting between a /// block (or its annotations) and
// the definition the rewriter hoisted them above.
func (p *Parser) addTransparentSpan(from, to int) {
	if p.transparentLines == nil {
		p.transparentLines = make(map[int]bool)
	}
	for i := from; i <= to; i++ {
		p.transparentLines[i] = true
	}
}

// takeFieldDocFor is takeDocFor scoped to args-block FIELDS: only a block
// that STARTS after the enclosing args block opened may attach, so a
// single-line (or hoisted) args block cannot steal the declaration's own
// doc block for its first field (memql#2633 review).
func (p *Parser) takeFieldDocFor(firstLine int) string {
	if len(p.docBlocks) == 0 {
		return ""
	}
	for line := firstLine - 1; line > 0; line-- {
		for i := range p.docBlocks {
			if !p.docConsumed[i] && p.docBlocks[i].EndLine == line && p.docBlocks[i].StartLine > p.argsBlockOpenLine {
				p.docConsumed[i] = true
				return JoinDocComment(p.docBlocks[i].Lines)
			}
		}
		if !p.transparentLines[line] {
			break
		}
	}
	return ""
}

// takeDocFor returns the joined doc comment attached to a definition whose
// first token sits on firstLine, consuming the block so it can attach only
// once. Returns "" when no block is adjacent.
func (p *Parser) takeDocFor(firstLine int) string {
	if len(p.docBlocks) == 0 {
		return ""
	}
	// Check-then-walk: a block ending ON a transparent line (the
	// `/* x */ /// Doc.` comment-only mix) still matches before the walk
	// steps past it.
	for line := firstLine - 1; line > 0; line-- {
		for i := range p.docBlocks {
			if !p.docConsumed[i] && p.docBlocks[i].EndLine == line {
				p.docConsumed[i] = true
				return JoinDocComment(p.docBlocks[i].Lines)
			}
		}
		if !p.transparentLines[line] {
			break
		}
	}
	return ""
}

// JoinDocComment implements the ruling-2 multi-line join: strip exactly one
// leading space from each line's post-/// content, join consecutive lines
// with a single space, and turn a bare /// line (empty content) into a
// paragraph break (newline). Strip-one (not strip-all) preserves deliberate
// sub-indentation.
func JoinDocComment(lines []string) string {
	var b strings.Builder
	pendingBreak := false
	for _, raw := range lines {
		content := strings.TrimSuffix(raw, "\r")
		content = strings.TrimPrefix(content, " ")
		if strings.TrimSpace(content) == "" {
			if b.Len() > 0 {
				pendingBreak = true
			}
			continue
		}
		if pendingBreak {
			b.WriteString("\n")
			pendingBreak = false
		} else if b.Len() > 0 {
			b.WriteString(" ")
		}
		b.WriteString(strings.TrimRight(content, " \t"))
	}
	return b.String()
}

// attachDocComment stores the joined doc on the definition node's DocComment
// slot -- one attach site, one type switch, every describable kind from the
// design's ruling-1 target set. For kinds outside the set (seed, builtin)
// the block was already consumed by takeDocFor and is simply discarded --
// observably identical to the detached-block behavior: ignored.
func attachDocComment(def Node, doc string) {
	if doc == "" || def == nil {
		return
	}
	switch d := def.(type) {
	case *FunctionDef:
		d.DocComment = doc
		if auto, ok := d.Body.(*AutomationDef); ok && auto != nil {
			auto.DocComment = doc
		}
	case *AutomationDef:
		d.DocComment = doc
	case *ConceptDecl:
		d.DocComment = doc
	case *ShapeDecl:
		d.DocComment = doc
	case *ToolDecl:
		d.DocComment = doc
	case *PromptDecl:
		d.DocComment = doc
	case *ProviderDecl:
		d.DocComment = doc
	case *PolicyDecl:
		d.DocComment = doc
	case *SpecDecl:
		d.DocComment = doc
	case *CapabilityDecl:
		d.DocComment = doc
	case *ActionDecl:
		d.DocComment = doc
	}
}
