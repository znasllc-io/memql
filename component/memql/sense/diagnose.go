package sense

import (
	"fmt"
	"strings"

	"github.com/visionarys-io/memql/component/language/parser"
)

// Diagnose returns errors and warnings for a MemQL source document.
func (s *Service) Diagnose(source string, filePath string) []Diagnostic {
	if source == "" {
		return nil
	}

	var diagnostics []Diagnostic

	// Phase 1: Lexer errors.
	lexer := parser.NewLexer(source)
	tokens, lexErr := lexer.Tokenize()
	if lexErr != nil {
		diagnostics = append(diagnostics, lexerDiagnostic(lexErr))
		return diagnostics // Can't continue without valid tokens.
	}

	// Phase 2: Parser errors.
	p := parser.NewParser(tokens)
	ast, parseErr := p.Parse()
	if parseErr != nil {
		diagnostics = append(diagnostics, parserDiagnostics(parseErr)...)
		// Continue with semantic analysis on partial AST if available.
	}

	// Phase 3: Semantic analysis (requires registries and a valid AST).
	if ast != nil && s.registries != nil {
		diagnostics = append(diagnostics, s.semanticDiagnostics(ast, source)...)
	}

	return diagnostics
}

// lexerDiagnostic converts a lexer error to a diagnostic.
func lexerDiagnostic(err error) Diagnostic {
	pos := Position{Line: 1, Column: 1}
	msg := err.Error()

	// Try to extract position from error message.
	if line, col, found := extractLineCol(msg); found {
		pos.Line = line
		pos.Column = col
	}

	return Diagnostic{
		Range: Range{
			Start: pos,
			End:   Position{Line: pos.Line, Column: pos.Column + 1},
		},
		Severity: SeverityError,
		Message:  msg,
		Code:     "lex-error",
	}
}

// parserDiagnostics converts parser errors to diagnostics.
func parserDiagnostics(err error) []Diagnostic {
	if err == nil {
		return nil
	}

	msg := err.Error()
	pos := Position{Line: 1, Column: 1}

	// Extract line/column from error message if present.
	if line, col, found := extractLineCol(msg); found {
		pos.Line = line
		pos.Column = col
	}

	return []Diagnostic{{
		Range: Range{
			Start: pos,
			End:   Position{Line: pos.Line, Column: pos.Column + 10},
		},
		Severity: SeverityError,
		Message:  msg,
		Code:     "parse-error",
	}}
}

// semanticDiagnostics walks the AST to find semantic issues.
func (s *Service) semanticDiagnostics(ast parser.Node, source string) []Diagnostic {
	var diagnostics []Diagnostic

	file, ok := ast.(*parser.File)
	if !ok {
		return nil
	}

	// Authoring-rule diagnostics (Phase 5 Step 34).
	diagnostics = append(diagnostics, directivesInBodyRule(file, source)...)
	diagnostics = append(diagnostics, nameShapeRule(file, source)...)
	diagnostics = append(diagnostics, arraySyntaxRule(source)...)

	// Check function definitions.
	for _, def := range file.Definitions {
		funcDef, ok := def.(*parser.FunctionDef)
		if !ok {
			continue
		}

		// Validate annotations for receiver type.
		if funcDef.Receiver != nil {
			receiverType := string(funcDef.Receiver.Type)
			validAnnotations := AnnotationsByReceiver[receiverType]
			for _, attr := range funcDef.Attributes {
				if !containsStr(validAnnotations, attr.Name) {
					// Find position of @attrName in source.
					pos := findInSource(source, "@"+attr.Name)
					diagnostics = append(diagnostics, Diagnostic{
						Range: Range{
							Start: pos,
							End:   Position{Line: pos.Line, Column: pos.Column + len(attr.Name) + 1},
						},
						Severity: SeverityError,
						Message:  fmt.Sprintf("annotation @%s is not valid for receiver type %s", attr.Name, receiverType),
						Code:     "invalid-annotation",
					})
				}
			}

			// Check @defaultProvider references for Prompt.
			if receiverType == "Prompt" {
				for _, attr := range funcDef.Attributes {
					if attr.Name == "defaultProvider" {
						value, _ := attr.Value.(string)
						if value != "" {
							if _, ok := s.registries.ProviderGet(value); !ok {
								pos := findInSource(source, value)
								diagnostics = append(diagnostics, Diagnostic{
									Range: Range{
										Start: pos,
										End:   Position{Line: pos.Line, Column: pos.Column + len(value)},
									},
									Severity: SeverityWarning,
									Message:  fmt.Sprintf("unknown provider \"%s\" in @defaultProvider", value),
									Code:     "unknown-provider",
								})
							}
						}
					}
				}
			}
		}
	}

	// Check use declarations.
	for _, u := range file.Uses {
		conceptPath := u.Path
		if conceptPath != "" {
			if _, ok := s.registries.ConceptGet(conceptPath); !ok {
				pos := findInSource(source, conceptPath)
				diagnostics = append(diagnostics, Diagnostic{
					Range: Range{
						Start: pos,
						End:   Position{Line: pos.Line, Column: pos.Column + len(conceptPath)},
					},
					Severity: SeverityWarning,
					Message:  fmt.Sprintf("unknown concept \"%s\"", conceptPath),
					Code:     "unknown-concept",
				})
			}
		}
	}

	return diagnostics
}

// findInSource finds the first occurrence of text in source and returns its position.
func findInSource(source, text string) Position {
	idx := strings.Index(source, text)
	if idx < 0 {
		return Position{Line: 1, Column: 1}
	}
	return positionFromOffset(source, idx)
}

// extractLineCol extracts line and column from a parser error message.
func extractLineCol(msg string) (int, int, bool) {
	// Format: "parse error at line X, column Y: ..."
	idx := strings.Index(msg, "at line ")
	if idx < 0 {
		return 0, 0, false
	}
	rest := msg[idx+8:]
	var line, col int
	n, err := fmt.Sscanf(rest, "%d, column %d", &line, &col)
	if err != nil || n != 2 {
		return 0, 0, false
	}
	return line, col, true
}

// containsStr checks if a string slice contains a value.
func containsStr(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}
