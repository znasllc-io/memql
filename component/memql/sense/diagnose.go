package sense

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/znasllc-io/memql/component/language/annotations"
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
// The lowering is lexed with the author's positions carried in it
// (parser.PositionLowering), so the lexer and parser report positions in the
// source the user is editing; a lineMap (built from the authored + rewritten
// pair) maps positions back only for a lowering left unmarked.
func (s *Service) Diagnose(source string, filePath string) []Diagnostic {
	if strings.TrimSpace(source) == "" {
		return nil
	}

	// Lower struct-form constructs to the procedural form the parser
	// understands. A failure here is a LOWERING error (e.g. an unbalanced
	// brace, `refine` without `paginate`); it names the author's text it
	// refuses, placed in the authored source (parser.PositionRewriteError),
	// and falls back to the named construct.
	rewritten, rewriteErr := applyRewriteChain(source)
	if rewriteErr != nil {
		return []Diagnostic{rewriteErrorDiagnostic(parser.PositionRewriteError(source, rewriteErr), source)}
	}

	lm := newLineMap(source, rewritten)

	// Lex the lowering with the author's positions carried in it
	// (parser.PositionLowering, memql#5364). Every position the lexer and
	// parser report is then the author's own -- down to the column of a token
	// inside a filter the lowering moved into its synthesized return, which no
	// line map can recover -- and the line map is only the fallback for a
	// lowering PositionLowering left unmarked.
	marked := parser.PositionLowering(source, rewritten)
	positioned := marked != rewritten

	var diagnostics []Diagnostic

	// Phase 1: Lexer errors. A marked text's lexer reports the author's
	// position already; an unmarked one's is mapped back.
	lexer := parser.NewLexer(marked)
	tokens, lexErr := lexer.Tokenize()
	if lexErr != nil {
		d := lexerDiagnostic(lexErr)
		if !positioned {
			d = lm.remap(d)
		}
		diagnostics = append(diagnostics, d)
		return diagnostics // Can't continue without valid tokens.
	}

	// Phase 2: Parser errors, positioned as the lexer's are. A ParseError
	// names its failing token's extent, which becomes the range.
	//
	// A file whose only constructs are non-procedural (shape / builtin /
	// prompt / seed / spec / trait) is stripped to bare comments by
	// applyRewriteChain, so the generic parser sees no definitions and
	// returns the ErrEmptyInput sentinel. That is NOT a diagnostic -- those
	// constructs load via their own dedicated loaders. The real load
	// pipeline swallows it (dslimports.Load via errors.Is), so match that.
	p := parser.NewParser(tokens)
	p.SetDocComments(lexer.DocComments())
	ast, parseErr := p.Parse()
	if parseErr != nil && !errors.Is(parseErr, parser.ErrEmptyInput) {
		for _, d := range parserDiagnostics(parseErr) {
			if r, ok := failingTokenRange(parseErr, rewritten == source); ok {
				d.Range = r
			} else if !positioned {
				d = lm.remap(d)
			}
			if d.Code == "invalid-annotation" {
				d.Range = annotationSpan(source, d.Range)
			}
			diagnostics = append(diagnostics, d)
		}
		// Continue with semantic analysis on a partial AST if one survives.
	}

	// Phase 3: Semantic analysis (requires registries and a REAL *File).
	// NOTE: on a parse error p.Parse() returns a TYPED-NIL *parser.File
	// wrapped in a non-nil parser.Node interface, so a bare `ast != nil`
	// guard is not enough -- dereferencing the nil *File panics. Assert and
	// nil-check the pointer explicitly.
	if file, ok := ast.(*parser.File); ok && file != nil {
		// Registry-backed semantic diagnostics (annotations, provider refs,
		// authoring rules) need the vocabulary; skip them when it is absent.
		if s.registries != nil {
			diagnostics = append(diagnostics, s.semanticDiagnostics(file, source)...)
		}
		// Import + signature-concept diagnostics need only the workspace graph
		// (always non-nil, a no-op that answers Unknown when absent), so they run
		// regardless -- a broken-reference workspace is exactly the registry-less
		// case.
		diagnostics = append(diagnostics, s.importDiagnostics(file, source)...)
		diagnostics = append(diagnostics, s.signatureConceptDiagnostics(file, source)...)
		diagnostics = append(diagnostics, s.signatureKindDiagnostics(file, source)...)
		// The @relationship axes need no vocabulary at all -- the structural
		// type set and the `as` label form are both static -- so they run in
		// the registry-less case too. An author whose workspace has not
		// loaded still gets told their relationship type refuses boot
		// (memql#3661).
		diagnostics = append(diagnostics, relationshipAxesRule(source)...)
		// A body written in statements is held to the scope rules the loader
		// refuses it for (compiler.CheckBody, epic memql#5370), which need no
		// vocabulary either. The parse's positions are the author's when the
		// lowering was marked, and the lowering's otherwise.
		lexed, place := source, func(d Diagnostic) Diagnostic { return d }
		if !positioned {
			lexed, place = rewritten, lm.remap
		}
		diagnostics = append(diagnostics, bodyScopeDiagnostics(file, lexed, place)...)
	}

	// Uses of a deprecated form still inside its window (memql#5390). Lexical
	// and vocabulary-free, so outside both guards above: a registry-less
	// service, a file whose only constructs the lowering strips (a builtin, a
	// prompt) and a file broken elsewhere all still say where they spell one.
	diagnostics = append(diagnostics, deprecatedFormsRule(source)...)

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
	rng := Range{Start: pos, End: Position{Line: pos.Line, Column: pos.Column + 1}}
	if r, ok := failingTokenRange(err, true); ok {
		rng = r // the refused clause, keyword or field the author wrote
	}
	return Diagnostic{
		Range:    rng,
		Severity: SeverityError,
		Message:  msg,
		Code:     errorCode(err, "rewrite-error"),
	}
}

// errorCode is the diagnostic code for a lowering or parse failure: the rule
// id of a retired edition-2026 form (parser.RetiredFormError), so an editor
// can key a quick fix on it and a person sees which rule fired, or fallback
// for every other failure.
func errorCode(err error, fallback string) string {
	var retired *parser.RetiredFormError
	if errors.As(err, &retired) && retired.Form.Rule != "" {
		return retired.Form.Rule
	}
	// A refusal of the statement parser carries its stable code the same way
	// (epic memql#5370): body_empty, body_step_retired, ...
	var body *parser.BodyRefusal
	if errors.As(err, &body) && body.Code != "" {
		return body.Code
	}
	return fallback
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

// failingTokenRange is the range of the token a parse error is about, in the
// author's coordinates, so the squiggle covers that token: the authored extent
// a marked lowering gives it, or -- lexedIsAuthored, nothing was lowered --
// the lexed one. ok is false when the error carries no token extent.
func failingTokenRange(err error, lexedIsAuthored bool) (Range, bool) {
	var pe *parser.ParseError
	if !errors.As(err, &pe) || (pe.AuthoredLine == 0 && !(lexedIsAuthored && pe.Line > 0)) {
		return Range{}, false
	}
	line, col := pe.Position()
	endLine, endCol := pe.EndPosition()
	r := Range{Start: Position{Line: line, Column: col}, End: Position{Line: endLine, Column: endCol}}
	if r.End.Line < r.Start.Line || (r.End.Line == r.Start.Line && r.End.Column <= r.Start.Column) {
		r.End = Position{Line: line, Column: col + 1}
	}
	return r, true
}

// parserDiagnostics converts parser errors to diagnostics.
// annotationSpan narrows a refused annotation's diagnostic to the annotation
// it refuses: the parser reports a registry refusal at the annotation's `@`
// (memql#5359), so the range runs from there over `@name`. A start that does
// not land on an `@` in the authored source keeps its range.
func annotationSpan(source string, r Range) Range {
	lines := strings.Split(source, "\n")
	if r.Start.Line < 1 || r.Start.Line > len(lines) {
		return r
	}
	line := []rune(lines[r.Start.Line-1])
	at := r.Start.Column - 1
	if at < 0 || at >= len(line) || line[at] != '@' {
		return r
	}
	end := at + 1
	for end < len(line) && (line[end] == '_' || unicode.IsLetter(line[end]) || unicode.IsDigit(line[end])) {
		end++
	}
	r.End = Position{Line: r.Start.Line, Column: end + 1}
	return r
}

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

	// An annotation the registry refused is the parser's to report, and
	// the editor keeps the code it has always shown for one: the message is
	// the registry's own (it names the receiver, the annotation, what to
	// write, and ends with the stable refusal code), so the squiggle and the
	// load gate say the same thing.
	code := "parse-error"
	var refusal *annotations.Refusal
	if errors.As(err, &refusal) {
		code = "invalid-annotation"
	}

	return []Diagnostic{{
		Range: Range{
			Start: pos,
			End:   Position{Line: pos.Line, Column: pos.Column + 10},
		},
		Severity: SeverityError,
		Message:  msg,
		Code:     errorCode(err, code),
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
	// deprecatedFormsRule runs in Diagnose itself, outside the vocabulary guard.
	diagnostics = append(diagnostics, redundantEnabledRule(source)...)
	diagnostics = append(diagnostics, redundantVersionRule(source)...)
	diagnostics = append(diagnostics, bareRowIntrinsicRule(source)...)
	diagnostics = append(diagnostics, bareRowIntrinsicSortKeyRule(source)...)
	diagnostics = append(diagnostics, actorUndeclaredRule(source)...)
	diagnostics = append(diagnostics, actorUnknownPropertyRule(source)...)
	diagnostics = append(diagnostics, descriptionLengthRule(file, source)...)

	// Check function definitions.
	for _, def := range file.Definitions {
		funcDef, ok := def.(*parser.FunctionDef)
		if !ok {
			continue
		}

		// A function construct's annotations are held to its receiver by
		// the parser itself (the annotation registry, memql#5359), so an
		// invalid one never reaches this pass: it is a parse error, which
		// parserDiagnostics reports as "invalid-annotation".
		if funcDef.Receiver != nil {
			receiverType := string(funcDef.Receiver.Type)

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

	// Form-B import resolution (`use <ns>.<kind>.{ id, ... }`) lives in
	// importDiagnostics, called from Diagnose OUTSIDE this registry gate: it
	// needs only the workspace graph, so it runs even in the registry-less
	// fallback where the engine tripped strict boot -- a broken-reference
	// workspace is exactly when an author needs those warnings.

	return diagnostics
}

// importDiagnostics resolves every Form-B import against the workspace graph and
// warns on a PROVABLE failure: a kind segment that names no module in a
// workspace-owned namespace (the user's `use fylo.concept.{...}` typo), or an id
// not declared in the resolved module (`use fylo.concepts.{ oder }`). It needs
// only s.workspace -- not the registry -- so Diagnose runs it in the registry-
// less fallback too.
//
// Every "missing" conclusion is gated on the graph's tri-state. A namespace the
// workspace does not own (an external engine namespace a product bundle imports,
// e.g. `platform`/`common`/`identity`) resolves Unknown and is left silent --
// never a false squiggle -- mirroring the load side's own missingIsProvable
// conservatism. Warnings, not errors: there is no Error-severity reference-check
// precedent, and a stale edit-path registry must not hard-fail the buffer.
func (s *Service) importDiagnostics(file *parser.File, source string) []Diagnostic {
	var diagnostics []Diagnostic
	// cursor advances past each processed `use` so a module path that appears on
	// more than one line anchors each diagnostic on its OWN line rather than the
	// first occurrence (file.Uses is in source order).
	cursor := 0
	for _, u := range file.Uses {
		// Resolve only exactly-two-segment module paths (`<ns>.<kind>`). Fewer
		// segments are Form A / legacy / malformed (not a Form-B module import).
		// MORE segments are consolidated-capability paths
		// (`capabilities.integration.github.{...}`), which map to a consolidated
		// file with a dotted construct prefix and cannot be faithfully resolved
		// through the graph's (ns, kind) API -- skip them rather than risk a
		// false squiggle on a valid capability verb.
		if u == nil || len(u.Names) == 0 || len(u.Parts) != 2 {
			continue
		}
		useStart := indexFrom(source, u.Path, cursor)
		if useStart >= 0 {
			cursor = useStart + len(u.Path)
		}
		ns, kind := u.Parts[0], u.Parts[1]
		switch s.workspace.ModuleResolves(ns, kind) {
		case ResolvedNo:
			// Namespace is owned by the workspace but names no such module. The
			// kind segment lives inside the dotted path, so anchor from useStart.
			pos := segmentPos(source, useStart, kind)
			diagnostics = append(diagnostics, Diagnostic{
				Range:    spanAt(pos, len(kind)),
				Severity: SeverityWarning,
				Message:  fmt.Sprintf("unknown import module %q: namespace %q has no %q module", ns+"."+kind, ns, kind),
				Code:     "unknown-import-module",
			})
		case ResolvedYes:
			// Ids live in the brace list AFTER the path; search from past the
			// path so an id that also appears in the ns/kind (`order.concepts.{
			// order }`) anchors on the braced id. Dedup a repeated id so a
			// `{ oder, oder }` list flags once, matching the loader.
			idsFrom := -1
			if useStart >= 0 {
				idsFrom = useStart + len(u.Path)
			}
			flagged := make(map[string]bool, len(u.Names))
			for _, name := range u.Names {
				if flagged[name] || s.workspace.SymbolDeclared(ns, kind, name) != ResolvedNo {
					continue
				}
				flagged[name] = true
				pos := segmentPos(source, idsFrom, name)
				diagnostics = append(diagnostics, Diagnostic{
					Range:    spanAt(pos, len(name)),
					Severity: SeverityWarning,
					Message:  fmt.Sprintf("%q is not declared in %s.%s", name, ns, kind),
					Code:     "unknown-import-symbol",
				})
			}
		case ResolvedUnknown:
			// External / unmounted namespace, or no workspace graph -- inconclusive.
		}
	}
	return diagnostics
}

// spanAt returns a single-line Range of n columns starting at start.
func spanAt(start Position, n int) Range {
	return Range{Start: start, End: Position{Line: start.Line, Column: start.Column + n}}
}

// indexFrom returns the byte offset of the first occurrence of substr at or
// after `from` (clamped into range), or -1.
func indexFrom(source, substr string, from int) int {
	if from < 0 {
		from = 0
	}
	if from > len(source) {
		return -1
	}
	if i := strings.Index(source[from:], substr); i >= 0 {
		return from + i
	}
	return -1
}

// segmentPos returns the Position of `segment` searching from byte offset `from`
// (an anchor within a single `use` statement), falling back to a whole-source
// search when `from` is out of range or the segment is not found past it.
func segmentPos(source string, from int, segment string) Position {
	if from >= 0 && from <= len(source) {
		if rel := strings.Index(source[from:], segment); rel >= 0 {
			return positionFromOffset(source, from+rel)
		}
	}
	return findInSource(source, segment)
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
