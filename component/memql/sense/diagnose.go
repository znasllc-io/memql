package sense

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/language/parser"
)

// Diagnose returns errors and warnings for a MemQL source document.
//
// It lowers the authored source through the SAME rewriter chain the runtime
// loaders / memqllint / compiler.ParseFileSource run
// (parser.StripNonProceduralBlocks + parser.NormaliseAll) BEFORE lexing and
// parsing. Without that step the generic parser sees the contextual
// struct-form keywords it cannot dispatch (query / mutation / logic /
// automation are lowered, not native -- see parser.topLevelDeclParsers) and
// false-errors `unexpected token "query"` on every valid function-form file,
// which broke the cockpit editor's diagnostics for the four most common
// construct kinds (memql#2359).
//
// Positions the lexer/parser report are in LOWERED coordinates; a lineMap
// (built from the authored + rewritten pair) maps them back to the authored
// lines the user is editing.
func (s *Service) Diagnose(source string, filePath string) []Diagnostic {
	if strings.TrimSpace(source) == "" {
		return nil
	}

	// Lower struct-form constructs to the procedural form the parser
	// understands. A failure here is a LOWERING error (e.g. a logic without
	// its mandatory `body { }` block, an unbalanced brace) -- these carry no
	// line/column, so anchor the diagnostic on the named construct in the
	// authored source.
	rewritten, rewriteErr := applyRewriteChain(source)
	if rewriteErr != nil {
		return []Diagnostic{rewriteErrorDiagnostic(rewriteErr, source)}
	}

	lm := newLineMap(source, rewritten)

	var diagnostics []Diagnostic

	// Phase 1: Lexer errors (lowered coords -> authored).
	lexer := parser.NewLexer(rewritten)
	tokens, lexErr := lexer.Tokenize()
	if lexErr != nil {
		diagnostics = append(diagnostics, lm.remap(lexerDiagnostic(lexErr)))
		return diagnostics // Can't continue without valid tokens.
	}

	// Phase 2: Parser errors (lowered coords -> authored).
	//
	// A file whose only constructs are non-procedural (shape / builtin /
	// prompt / seed / spec / trait) is stripped to bare comments by
	// applyRewriteChain, so the generic parser sees no definitions and
	// returns the ErrEmptyInput sentinel. That is NOT a diagnostic -- those
	// constructs load via their own dedicated loaders. The real load
	// pipeline swallows it (dslimports.Load via errors.Is), so match that.
	p := parser.NewParser(tokens)
	ast, parseErr := p.Parse()
	if parseErr != nil && !errors.Is(parseErr, parser.ErrEmptyInput) {
		for _, d := range parserDiagnostics(parseErr) {
			diagnostics = append(diagnostics, lm.remap(d))
		}
		// Continue with semantic analysis on a partial AST if one survives.
	}

	// Phase 3: Semantic analysis (requires registries and a REAL *File).
	// NOTE: on a parse error p.Parse() returns a TYPED-NIL *parser.File
	// wrapped in a non-nil parser.Node interface, so a bare `ast != nil`
	// guard is not enough -- dereferencing the nil *File panics. Assert and
	// nil-check the pointer explicitly.
	if s.registries != nil {
		if file, ok := ast.(*parser.File); ok && file != nil {
			diagnostics = append(diagnostics, s.semanticDiagnostics(file, source)...)
		}
	}

	return diagnostics
}

// applyRewriteChain lowers struct-form constructs to the procedural func
// form the generic parser understands, mirroring compiler.ParseFileSource
// (applyFullRewriteChain) and the runtime loaders exactly: non-procedural
// blocks (shape / builtin / prompt / seed / spec / trait) are stripped
// FIRST so the struct-form rewriters and the bare parser don't choke on
// them, then NormaliseAll lowers query / mutation / logic / automation +
// the file-top args block. Those stripped kinds still load via their own
// dedicated loaders; for diagnostics we only need the file to parse.
func applyRewriteChain(source string) (string, error) {
	if parser.LooksLikeNonProcedural(source) {
		source = parser.StripNonProceduralBlocks(source)
	}
	return parser.NormaliseAll(source)
}

// rewriteConstructNameRe pulls the first double-quoted identifier out of a
// NormaliseAll lowering error. Those messages lead with the offending
// construct name, e.g.
//
//	logic rewrite: struct-form logic "doThing": logic "doThing" must wrap ...
var rewriteConstructNameRe = regexp.MustCompile(`"([A-Za-z_][A-Za-z0-9_]*)"`)

// constructHeaderRe locates a top-level construct header for a given name so
// a lowering error can point at the authored declaration instead of line 1.
// The concept-binding identifier (query <Concept> <name>) is optional.
//
// The keyword alternation is sourced from dslspec (constructKeywords) rather
// than hand-listed, so a construct a future grammar epic adds is anchored
// automatically. `use` is excluded: it is an unnamed file-top import, not a
// named construct declaration this regex looks for.
var constructHeaderRe = buildConstructHeaderRe()

func buildConstructHeaderRe() *regexp.Regexp {
	kws := make([]string, 0, len(constructKeywords))
	for kw := range constructKeywords {
		if kw == "use" {
			continue
		}
		kws = append(kws, regexp.QuoteMeta(kw))
	}
	sort.Strings(kws) // deterministic alternation
	return regexp.MustCompile(
		`(?m)^[ \t]*(?:` + strings.Join(kws, "|") + `)[ \t]+(?:[A-Za-z_][A-Za-z0-9_]*[ \t]+)?`,
	)
}

// rewriteErrorDiagnostic turns a lowering failure (which has no position)
// into a diagnostic anchored on the named construct in the authored source.
func rewriteErrorDiagnostic(err error, source string) Diagnostic {
	msg := err.Error()
	pos := Position{Line: 1, Column: 1}
	if m := rewriteConstructNameRe.FindStringSubmatch(msg); m != nil {
		if p, ok := findConstructHeader(source, m[1]); ok {
			pos = p
		}
	}
	return Diagnostic{
		Range: Range{
			Start: pos,
			End:   Position{Line: pos.Line, Column: pos.Column + 1},
		},
		Severity: SeverityError,
		Message:  msg,
		Code:     "rewrite-error",
	}
}

// findConstructHeader returns the position of `name` where it appears as a
// top-level construct declaration name. Falls back to the first bare
// occurrence, then to (nothing) so the caller can default to line 1.
func findConstructHeader(source, name string) (Position, bool) {
	for _, loc := range constructHeaderRe.FindAllStringIndex(source, -1) {
		rest := source[loc[1]:]
		if strings.HasPrefix(rest, name) {
			// Ensure a word boundary after the name (`{`, whitespace, EOL).
			after := loc[1] + len(name)
			if after >= len(source) || !isIdentByte(source[after]) {
				return positionFromOffset(source, loc[1]), true
			}
		}
	}
	// No header match; fall back to the first standalone occurrence.
	if idx := strings.Index(source, name); idx >= 0 {
		return positionFromOffset(source, idx), true
	}
	return Position{}, false
}

func isIdentByte(b byte) bool {
	return b == '_' ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
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

// semanticDiagnostics walks the (lowered) AST to find semantic issues. The
// AST here is the rewriter's output -- struct-form constructs already
// lowered to procedural func form -- while `source` is the AUTHORED text the
// source-scanning rules and findInSource positions read from.
func (s *Service) semanticDiagnostics(file *parser.File, source string) []Diagnostic {
	var diagnostics []Diagnostic

	// Authoring-rule diagnostics (Phase 5 Step 34).
	//
	// directivesInBodyRule is deliberately NOT run here: it flags
	// sort()/paginate()/asOf()/... calls inside a function body, but the
	// rewriter now LEGITIMATELY lowers struct-form list clauses (`sort`,
	// `paginate`, `asOf latest`, `@unbounded`) into exactly those calls, so
	// running it on the lowered AST only ever flags the rewriter's own
	// output -- a false positive on every list query. The authored
	// procedural form it originally guarded is retired + rejected at parse,
	// so the rule can no longer fire on real authored input. The rule (and
	// its unit test) are kept for reference.
	diagnostics = append(diagnostics, nameShapeRule(file, source)...)
	diagnostics = append(diagnostics, arraySyntaxRule(source)...)
	diagnostics = append(diagnostics, redundantEnabledRule(source)...)

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
	//
	// Only concept-id-shaped paths are checked. The modern Form-B import
	// (`use cluster.concepts.{ node, ... }`) carries a dotted MODULE path in
	// u.Path (e.g. "cluster.concepts"), not a concept id, so passing it to
	// ConceptGet would miss and warn on every import. Concept ids are
	// colon-shaped (`v1:cluster:node`); gate on that so module-path imports
	// don't produce a spurious "unknown concept" warning on every file.
	for _, u := range file.Uses {
		conceptPath := u.Path
		if strings.Contains(conceptPath, ":") {
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
