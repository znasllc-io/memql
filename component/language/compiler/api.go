package compiler

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/visionarys-io/memql/component/language/parser"
)

// CompileFile reads a .memql file and compiles it to the appropriate output format.
func CompileFile(inputPath string) (*CompileResult, error) {
	content, err := os.ReadFile(inputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read file %q: %w", inputPath, err)
	}

	return CompileSource(string(content))
}

// CompileSource compiles MemQL source code to the appropriate output format.
//
// Note: spec + trait sources are no longer handled here -- they have
// a dedicated parser in component/memql/spec_parser.go that's invoked
// from the unified spec loader at engine startup. CompileSource is
// for queries / mutations / logic / automations.
func CompileSource(source string) (*CompileResult, error) {
	// Apply every struct-form rewriter in sequence: queries,
	// mutations, logic blocks, automations, file-top args. Each
	// stage is a no-op when its detector doesn't match.
	rewritten, err := parser.NormaliseAll(source)
	if err != nil {
		return nil, err
	}
	source = rewritten

	// Tokenize
	lexer := parser.NewLexer(source)
	tokens, err := lexer.Tokenize()
	if err != nil {
		return nil, fmt.Errorf("lexer error: %w", err)
	}

	// Parse
	p := parser.NewParser(tokens)
	ast, err := p.Parse()
	if err != nil {
		return nil, fmt.Errorf("parser error: %w", err)
	}

	// Compile
	compiler := NewDefault()

	switch node := ast.(type) {
	case *parser.File:
		return compiler.CompileFile(node)

	case *parser.FunctionDef:
		// Single function definition
		result := &CompileResult{
			Warnings: LintFile(&parser.File{
				Definitions: []parser.Node{node},
			}),
		}
		switch node.Type {
		case parser.FunctionTypeAutomation:
			automation, err := compiler.compileAutomation(node)
			if err != nil {
				return nil, err
			}
			result.Automations = append(result.Automations, *automation)
		default:
			fn, err := compiler.compileFunction(node)
			if err != nil {
				return nil, err
			}
			result.Functions = append(result.Functions, *fn)
		}
		return result, nil

	default:
		return nil, fmt.Errorf("unexpected AST node type: %T", ast)
	}
}

// CompileToDirectory compiles a .memql file and writes outputs to a directory.
func CompileToDirectory(inputPath, outputDir string) error {
	result, err := CompileFile(inputPath)
	if err != nil {
		return err
	}

	// Ensure output directory exists
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	// Write outputs
	outputs, err := result.ToJSON(true)
	if err != nil {
		return err
	}

	for name, data := range outputs {
		outPath := filepath.Join(outputDir, name)

		// Create subdirectories if needed
		if dir := filepath.Dir(outPath); dir != "." {
			if err := os.MkdirAll(filepath.Join(outputDir, dir), 0755); err != nil {
				return fmt.Errorf("failed to create directory for %q: %w", name, err)
			}
		}

		if err := os.WriteFile(outPath, data, 0644); err != nil {
			return fmt.Errorf("failed to write %q: %w", name, err)
		}
	}

	return nil
}

// TranspileAutomation takes a .memql automation definition and returns JSON.
func TranspileAutomation(source string) (string, error) {
	result, err := CompileSource(source)
	if err != nil {
		return "", err
	}

	if len(result.Automations) == 0 {
		return "", fmt.Errorf("no automation definition found in source")
	}

	data, err := json.MarshalIndent(result.Automations[0].JSON, "", "  ")
	if err != nil {
		return "", err
	}

	return string(data), nil
}

// DetectFileType examines MemQL source and returns what type of content it contains.
func DetectFileType(source string) (FileType, error) {
	lexer := parser.NewLexer(source)
	tokens, err := lexer.Tokenize()
	if err != nil {
		return FileTypeUnknown, err
	}

	// Look at the first meaningful token
	for _, tok := range tokens {
		switch tok.Type {
		case parser.TokenKeywordAutomation:
			return FileTypeAutomation, nil
		case parser.TokenKeywordQuery:
			return FileTypeQuery, nil
		case parser.TokenKeywordMutation:
			return FileTypeMutation, nil
		case parser.TokenIdentifier:
			// Could be a query expression or a named function
			// Check if followed by (args) { ... }
			return FileTypeQuery, nil
		case parser.TokenEOF:
			return FileTypeUnknown, nil
		}
	}

	return FileTypeQuery, nil
}

// FileType represents the type of content in a .memql file.
type FileType int

const (
	FileTypeUnknown FileType = iota
	FileTypeQuery
	FileTypeMutation
	FileTypeAutomation
	FileTypeModule // Multiple definitions
)

// String returns the string representation of the file type.
func (t FileType) String() string {
	switch t {
	case FileTypeQuery:
		return "query"
	case FileTypeMutation:
		return "mutation"
	case FileTypeAutomation:
		return "automation"
	case FileTypeModule:
		return "module"
	default:
		return "unknown"
	}
}

// ParseFileSource parses MemQL source after running the full
// rewriter chain (every struct-form normalization the runtime
// loader applies). Returns a *parser.File on success so callers
// don't have to type-assert through parser.Node.
//
// This is the entry point new tooling (dslimports.Load, the
// `memql-cockpit lint` CLI, the engine's Validate(target) API)
// should call. It supersedes the partial chain in ParseMemQL,
// which only normalised a subset of constructs and is kept around
// for backwards-compatibility with existing single-expression
// call sites.
func ParseFileSource(source string) (*parser.File, error) {
	rewritten, err := applyFullRewriteChain(source)
	if err != nil {
		return nil, err
	}

	lexer := parser.NewLexer(rewritten)
	tokens, err := lexer.Tokenize()
	if err != nil {
		return nil, fmt.Errorf("lexer error: %w", err)
	}

	p := parser.NewParser(tokens)
	node, err := p.Parse()
	if err != nil {
		return nil, fmt.Errorf("parser error: %w", err)
	}

	switch n := node.(type) {
	case *parser.File:
		return n, nil
	case *parser.FunctionDef:
		// Single-definition file -- wrap in a File so the caller
		// gets a uniform shape.
		return &parser.File{Definitions: []parser.Node{n}}, nil
	default:
		return nil, fmt.Errorf("unexpected top-level node type %T", node)
	}
}

// applyFullRewriteChain runs every struct-form rewriter in the
// canonical order. Shared between ParseFileSource, CompileSource,
// and ValidateMemQL so the three entry points stay byte-identical
// in their parse-time behavior.
//
// Non-procedural construct stripping runs FIRST so subsequent
// rewriters and the bare parser don't choke on shape / provider /
// builtin / prompt / tool / policy struct-form declarations.
// These constructs continue to load via their dedicated loaders
// (shape_loader, provider_loader, ...) reading the same source
// files in parallel.
func applyFullRewriteChain(source string) (string, error) {
	if parser.LooksLikeNonProcedural(source) {
		source = parser.StripNonProceduralBlocks(source)
	}
	return parser.NormaliseAll(source)
}

// ParseMemQL is a convenience function that parses MemQL source and returns the AST.
// Runs the same rewriter chain as CompileSource so callers get the
// canonical struct / args / spec forms transparently. Per-stage
// errors are swallowed (the eventual parse failure is the signal).
func ParseMemQL(source string) (parser.Node, error) {
	if rewritten, err := parser.NormaliseAll(source); err == nil {
		source = rewritten
	}

	lexer := parser.NewLexer(source)
	tokens, err := lexer.Tokenize()
	if err != nil {
		return nil, fmt.Errorf("lexer error: %w", err)
	}

	p := parser.NewParser(tokens)
	return p.Parse()
}

// IsAutomationFile checks if the source contains an automation definition.
func IsAutomationFile(source string) bool {
	t, err := DetectFileType(source)
	if err != nil {
		return false
	}
	return t == FileTypeAutomation
}

// GetAutomationName extracts the automation name from source.
func GetAutomationName(source string) (string, error) {
	// Quick parse to find automation name
	lexer := parser.NewLexer(source)
	tokens, err := lexer.Tokenize()
	if err != nil {
		return "", err
	}

	for i, tok := range tokens {
		// New Go-style syntax: func (Automation) name()
		if tok.Type == parser.TokenKeywordFunc {
			// Look for (Automation) pattern
			if i+3 < len(tokens) &&
				tokens[i+1].Type == parser.TokenParenOpen &&
				tokens[i+2].Literal == "Automation" &&
				tokens[i+3].Type == parser.TokenParenClose {
				// Next token after ) should be the function name
				if i+4 < len(tokens) && tokens[i+4].Type == parser.TokenIdentifier {
					return tokens[i+4].Literal, nil
				}
			}
		}

		// Legacy syntax: automation name()
		if tok.Type == parser.TokenKeywordAutomation {
			// Next identifier is the name
			if i+1 < len(tokens) && tokens[i+1].Type == parser.TokenIdentifier {
				return tokens[i+1].Literal, nil
			}
		}
	}

	return "", fmt.Errorf("no automation name found")
}

// ValidateMemQL validates MemQL source without compiling.
// It checks both syntax and file composition rules.
//
// Note: spec + trait sources use their own dedicated parser
// (component/memql/spec_parser.go) and don't flow through this
// path. ValidateMemQL is for queries / mutations / logic /
// automations.
func ValidateMemQL(source string) error {
	rewritten, err := parser.NormaliseAll(source)
	if err != nil {
		return err
	}
	source = rewritten

	lexer := parser.NewLexer(source)
	tokens, err := lexer.Tokenize()
	if err != nil {
		return fmt.Errorf("lexer error: %w", err)
	}

	p := parser.NewParser(tokens)
	ast, err := p.Parse()
	if err != nil {
		return fmt.Errorf("parser error: %w", err)
	}

	// Validate file composition (CQS rules)
	if file, ok := ast.(*parser.File); ok {
		if err := ValidateFileComposition(file); err != nil {
			return fmt.Errorf("composition error: %w", err)
		}
	}

	return nil
}

// FormatMemQL formats MemQL source code (placeholder for future implementation).
func FormatMemQL(source string) (string, error) {
	// For now, just validate and return as-is
	if err := ValidateMemQL(source); err != nil {
		return "", err
	}
	return strings.TrimSpace(source), nil
}
