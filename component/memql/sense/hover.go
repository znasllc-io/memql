package sense

import (
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/language/parser"
)

// Hover returns information about the symbol at a position.
func (s *Service) Hover(source string, line, col int) *HoverResult {
	if source == "" || s.registries == nil {
		return nil
	}

	// Find the token at the cursor position.
	token, tokenRange := tokenAtPosition(source, line, col)
	if token == "" {
		return nil
	}

	// Check in priority order.

	// 1. Keyword.
	if doc, ok := KeywordDocs[token]; ok {
		return &HoverResult{
			Contents: fmt.Sprintf("**%s** (keyword)\n\n%s", token, doc),
			Range:    tokenRange,
		}
	}

	// 2. Builtin function.
	if def, ok := BuiltinFunctions[token]; ok {
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("**%s** (builtin)\n\n```\n%s\n```\n\n%s", token, def.Signature, def.Doc))
		if len(def.Parameters) > 0 {
			sb.WriteString("\n\n**Parameters:**\n")
			for _, p := range def.Parameters {
				sb.WriteString(fmt.Sprintf("- `%s` -- %s\n", p.Label, p.Documentation))
			}
		}
		return &HoverResult{Contents: sb.String(), Range: tokenRange}
	}

	// 3. Annotation (token after @, or token starting with @).
	annotName := strings.TrimPrefix(token, "@")
	if doc, ok := AnnotationDocs[annotName]; ok {
		return &HoverResult{
			Contents: fmt.Sprintf("**@%s** (annotation)\n\n%s", annotName, doc),
			Range:    tokenRange,
		}
	}

	// 4. Receiver type.
	for _, rt := range ReceiverTypes {
		if token == rt {
			return &HoverResult{
				Contents: fmt.Sprintf("**%s** (receiver type)\n\nDeclare a %s function: `func (%s) name(args) { ... }`", rt, strings.ToLower(rt), rt),
				Range:    tokenRange,
			}
		}
	}

	// 5. Concept name (contains colons).
	if strings.Contains(token, ":") {
		if c, ok := s.registries.ConceptGet(token); ok {
			return &HoverResult{
				Contents: formatConceptHover(c),
				Range:    tokenRange,
			}
		}
	}

	// 6. Function name (from registry).
	if fn, ok := s.registries.FunctionGet(token); ok {
		return &HoverResult{
			Contents: formatFunctionHover(fn),
			Range:    tokenRange,
		}
	}

	// 7. Provider name.
	if p, ok := s.registries.ProviderGet(token); ok {
		return &HoverResult{
			Contents: fmt.Sprintf("**%s** (provider)\n\n- Type: %s\n- Model: `%s`\n- Modality: %s", p.Name, p.Type, p.Model, p.Modality),
			Range:    tokenRange,
		}
	}

	// 8. Shape name.
	if sh, ok := s.registries.ShapeGet(token); ok {
		return &HoverResult{
			Contents: fmt.Sprintf("**%s** (shape)\n\n%s", sh.Name, sh.Description),
			Range:    tokenRange,
		}
	}

	return nil
}

// tokenAtPosition finds the token (word or identifier) at a cursor position.
func tokenAtPosition(source string, line, col int) (string, Range) {
	// Tokenize the source.
	lexer := parser.NewLexer(source)
	tokens, err := lexer.Tokenize()
	if err != nil {
		return "", Range{}
	}

	for _, t := range tokens {
		if t.Type == parser.TokenEOF {
			continue
		}
		endCol := t.Column + len(t.Literal)
		if t.Line == line && col >= t.Column && col <= endCol {
			return t.Literal, Range{
				Start: Position{Line: t.Line, Column: t.Column},
				End:   Position{Line: t.Line, Column: endCol},
			}
		}
	}

	return "", Range{}
}

// formatConceptHover formats hover content for a concept.
func formatConceptHover(c *ConceptInfo) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("**%s** (concept)\n\n%s\n", c.Name, c.Description))

	if len(c.Fields) > 0 {
		sb.WriteString("\n**Fields:**\n\n| Name | Type | Required | Description |\n|------|------|----------|-------------|\n")
		for _, f := range c.Fields {
			req := ""
			if f.Required {
				req = "yes"
			}
			fieldType := f.Type
			if len(f.Enum) > 0 {
				fieldType = fmt.Sprintf("enum(%s)", strings.Join(f.Enum, ", "))
			}
			sb.WriteString(fmt.Sprintf("| %s | %s | %s | %s |\n", f.Name, fieldType, req, f.Description))
		}
	}

	return sb.String()
}

// formatFunctionHover formats hover content for a function.
func formatFunctionHover(fn *FunctionInfo) string {
	var sb strings.Builder
	status := ""
	if !fn.Enabled {
		status = " [disabled]"
	}
	if fn.Deprecated != "" {
		status = fmt.Sprintf(" [DEPRECATED: %s]", fn.Deprecated)
	}
	sb.WriteString(fmt.Sprintf("**%s** (%s)%s\n\n%s", fn.Name, fn.Kind, status, fn.Description))
	if fn.ArgsDoc != "" {
		sb.WriteString(fmt.Sprintf("\n\n**Arguments:** %s", fn.ArgsDoc))
	}
	return sb.String()
}
