package sense

import (
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/language/parser"
)

// Hover returns information about the symbol at a position. filePath is
// the document's path, used to derive the ambient domain when a bare
// construct name is ambiguous across namespaces; "" is accepted and only
// costs that disambiguation.
func (s *Service) Hover(source string, line, col int, filePath string) *HoverResult {
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

	// 9. Tool name.
	if tl, ok := s.registries.ToolGet(token); ok {
		return &HoverResult{
			Contents: fmt.Sprintf("**%s** (tool)\n\n%s", tl.Name, tl.Description),
			Range:    tokenRange,
		}
	}

	// 10. Prompt name.
	if pr, ok := s.registries.PromptGet(token); ok {
		return &HoverResult{
			Contents: fmt.Sprintf("**%s** (prompt)\n\n%s", pr.Name, pr.Description),
			Range:    tokenRange,
		}
	}

	// 11. Bare concept short name. Same-domain constructs are ambient --
	// referenced with no `use` import (authoring rule 25, #2617) -- so the
	// signature binding in `shape candidate candidateFull` names a concept
	// the registry only knows by its canonical id. Resolve the short name
	// last, after every exact-name lookup above has had its turn.
	if canonical, ok := s.resolveBareConcept(token, filePath); ok {
		if c, ok := s.registries.ConceptGet(canonical); ok {
			return &HoverResult{
				Contents: formatConceptHover(c),
				Range:    tokenRange,
			}
		}
	}

	return nil
}

// resolveBareConcept maps a bare short name to a canonical concept id by
// trailing segment, mirroring the loader's own resolution
// (component/memql/concept_resolver.go:119-158). A single match wins
// outright. An ambiguous one -- `plan` is both v1:planner:plan and
// v1:harness:plan, and 46 bare names collide tree-wide -- is settled only
// by the file's own domain, and otherwise stays unresolved: hover says
// nothing rather than naming the wrong concept.
func (s *Service) resolveBareConcept(name, filePath string) (string, bool) {
	if s.registries == nil || name == "" || strings.Contains(name, ":") {
		return "", false
	}
	var matches []string
	for _, canonical := range s.registries.ConceptNames() {
		if idx := strings.LastIndex(canonical, ":"); idx >= 0 && canonical[idx+1:] == name {
			matches = append(matches, canonical)
		}
	}
	switch len(matches) {
	case 0:
		return "", false
	case 1:
		return matches[0], true
	}
	domain := fileDomain(filePath)
	if domain == "" {
		return "", false
	}
	needle := ":" + domain + ":"
	var scoped []string
	for _, m := range matches {
		if strings.Contains(m, needle) {
			scoped = append(scoped, m)
		}
	}
	if len(scoped) == 1 {
		return scoped[0], true
	}
	return "", false
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
