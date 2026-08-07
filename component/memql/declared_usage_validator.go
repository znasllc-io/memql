package memql

import (
	"fmt"
	"regexp"
	"strings"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// validateDeclaredUsage walks a function definition's `@use*(...)`
// annotations and args-block fields and asserts each one is exercised
// somewhere in the body. Returns a load-time error naming the first
// stale declaration found. Mirrors Go's "imported and not used" /
// "declared but not used" discipline. (Phase G.3.g pt 4.)
//
// The validator runs on the RAW source (pre-path-translation) so
// `@use*` targets and args fields pass through unchanged.
//
// Inert cases:
//
//   - Unused-trait shapes / traits (concept-agnostic predicates) are
//     out of scope -- their body forms differ from function bodies
//     and they're already enforced by the kind validator.
//   - `@row` / `@caller` kind annotations on shapes are not checked
//     here (the shape kind validator already verifies the body
//     contains at least one matching path).
func validateDeclaredUsage(rawSource string, funcDef *languageParser.FunctionDef) error {
	if funcDef == nil {
		return nil
	}
	// Body slice from the raw source: everything inside the outermost
	// `{ ... }` of the construct. The receiver-style header is what
	// the rewriters emit (`func (Query) name(_ any) { ... }` / etc.),
	// so the outermost `{` is reliable.
	body := extractFunctionBody(rawSource)
	if body == "" {
		// No body to validate against (parsed AST without body text);
		// skip rather than guess.
		return nil
	}

	// Skip every annotation whose target appears INSIDE the
	// annotation's own argument list -- we want body references, not
	// the declaration itself. The annotation lines are stripped here
	// before the body-reference scan.
	bodyNoAnnotations := stripAttrLines(body)

	for _, attr := range funcDef.Attributes {
		if attr == nil {
			continue
		}
		switch attr.Name {
		case languageParser.AttrUseQuery,
			languageParser.AttrUseMutation,
			languageParser.AttrUseAutomation,
			languageParser.AttrUseBuiltin:
			// Call-style use: `name(...)` somewhere in the body.
			for _, name := range attr.UseTargets() {
				if !referencedAsCall(bodyNoAnnotations, name) {
					return fmt.Errorf("function %q: @%s(%s) declared but %s is never called in the body", funcDef.Name, attr.Name, name, name)
				}
			}
		case languageParser.AttrUseSpec,
			languageParser.AttrUseTrait,
			languageParser.AttrUseTool,
			languageParser.AttrUsePrompt,
			languageParser.AttrUseProvider,
			languageParser.AttrUseLogic:
			// Bare-name reference: `name` appears as a whole word
			// somewhere in the body.
			for _, name := range attr.UseTargets() {
				if !referencedAsBareName(bodyNoAnnotations, name) {
					return fmt.Errorf("function %q: @%s(%s) declared but %s is never referenced in the body", funcDef.Name, attr.Name, name, name)
				}
			}
		}
	}

	// args fields: each declared field must be referenced as `args.X`
	// or `ctx.X` (the legacy envelope alias the rewriter emits).
	//
	// Logic functions are exempt: they're called from automation steps
	// that always pass the triggering `event` payload (`logic name {
	// event: event }`). A cron-fired automation has nothing meaningful
	// in event, so the corresponding logic body legitimately ignores
	// it. Reflecting that as a validation pass keeps the convention
	// uniform across event-driven and cron-driven logics.
	if funcDef.ArgsSchema != nil && funcDef.Type != languageParser.FunctionTypeLogic {
		for _, field := range funcDef.ArgsSchema.Fields {
			if field == nil || field.Name == "" {
				continue
			}
			if !referencedAsArgsField(bodyNoAnnotations, field.Name) {
				return fmt.Errorf("function %q: args field %q declared but never referenced as args.%s in the body", funcDef.Name, field.Name, field.Name)
			}
		}
	}

	return nil
}

// extractFunctionBody returns the contents between the first
// `func (Receiver) name(...) {` opening brace and its matching closing
// brace. Returns the empty string when no body is found.
//
// There is exactly ONE header form to find, the parser-emitted
// `func (Query|Mutation|Automation|Logic) name(...) { ... }`. The
// snapshot every caller feeds this (`rawSourceForUsage` in
// function_loader.go) is taken AFTER NormaliseAll, so the
// author-facing struct-form headers (`query NAME {` / `mutate NAME {`
// / `logic NAME {` / `automation NAME {`) have already been rewritten
// into that shape and cannot reach here -- the same reasoning
// precededByBodyOpener records below, corrected here to match
// (memql#3194 fixed the neighbour; this doc still named the struct
// forms). The body lives inside the first top-level `{ ... }`.
//
// # String state is TRACKED, not inferred from the preceding byte
//
// This scan used to decide whether a `"` closed the literal with a
// ONE-BYTE LOOKBACK (`source[i-1] != '\\'`), which cannot tell an
// ESCAPED quote (`\"`) from a quote that follows a COMPLETED `\\`
// escape (memql#3190). A body containing a literal ending in a
// backslash pair therefore never left string state: the braces after
// it stopped being counted, depth never returned to zero, and this
// returned "" -- which every caller (declared-usage, actor-binding,
// logic event-field and event-binding validators) reads as "no body"
// and skips its check on. One mis-scanned literal, four validators
// failing open at once.
//
// # Why this is local and does not delegate
//
// It returns a slice of the ORIGINAL source, so the caller's offsets
// have to be offsets into the original. The shared scrubbers
// (baseparser.BlankComments, parser.BlankCommentsAndStrings) return a
// BLANKED COPY: scanning one would find the right brace positions,
// but the body would then have to be spliced back out of the original
// anyway, and the body-opener heuristic below inspects the real
// preceding text, not a blanked view of it. Same conclusion, for the
// same reason, as memql#3102's scanSegments.
//
// To be explicit about the state of the tree rather than only about
// the correct implementations in it: correct scanners existed here
// before this change AND so did nine buggy ones, this being one of
// them. memql#3190 enumerated and converted all nine; the gate in
// core/baseparser (TestNoOneByteLookbackQuoteScan) is what stops a
// tenth.
func extractFunctionBody(source string) string {
	open := -1
	depth := 0
	inString := false
	escaped := false
	for i := 0; i < len(source); i++ {
		c := source[i]
		if inString {
			switch {
			case escaped:
				// This byte was escaped by the backslash before it; the
				// escape is now spent. A `\\` pair therefore leaves
				// escaped false, so the NEXT quote closes the literal.
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
			escaped = false
		case '{':
			if open < 0 {
				// Heuristic: only count `{` AFTER a `func ` / `query ` /
				// `mutation ` / `automation ` keyword to avoid the
				// annotation-call form `@enum("a", "b")` and the
				// args-block `args { ... }`. Easier: pick the LAST
				// `{` that's preceded by a `)` (the function-arg-
				// list close) or by the construct's body-opener.
				if precededByBodyOpener(source[:i]) {
					open = i
					depth = 1
				}
			} else {
				depth++
			}
		case '}':
			if open < 0 {
				continue
			}
			depth--
			if depth == 0 {
				return source[open+1 : i]
			}
		}
	}
	return ""
}

// precededByBodyOpener reports whether the text immediately preceding
// position p is the header line of a function definition.
//
// There is exactly ONE recognised header -- the parser-emitted form
// after the struct rewriters run:
//
//   - `func (Receiver) name(<args>) <returns> {`
//
// The author-facing struct-form headers (`query NAME {` /
// `mutate NAME {` / `logic NAME {` / `automation NAME {` --
// parser.StructFormKeywords) cannot reach here: the snapshot every
// caller of extractFunctionBody is fed (`rawSourceForUsage` in
// function_loader.go) is taken AFTER NormaliseAll, which has already
// rewritten each of those keywords into the `func (Receiver)` shape
// above. The native keywords the rewriter leaves alone (`spec` /
// `trait` / `action`) parse to SpecDef / ActionDef, never a
// *FunctionDef, so they never reach this validator either. A
// struct-form keyword arm here would be dead by construction, and a
// stale one silently invited a fail-open (a header miss makes
// extractFunctionBody return "" and every caller skip its check), so
// it is deliberately absent -- memql#3194.
//
// The check looks at the last logical line ending at `prefix`; the
// header keyword must open that line (the rest of the header, up to
// and including `<retType>`, may follow it).
func precededByBodyOpener(prefix string) bool {
	trimmed := strings.TrimRight(prefix, " \t\r\n")
	if trimmed == "" {
		return false
	}
	nl := strings.LastIndexByte(trimmed, '\n')
	if nl < 0 {
		nl = 0
	} else {
		nl++
	}
	line := strings.TrimSpace(trimmed[nl:])
	return strings.HasPrefix(line, "func ") || strings.HasPrefix(line, "func\t")
}

// stripAttrLines removes lines that contain `@<word>(...)` annotation
// declarations so the body-reference scan doesn't count the
// declaration itself as a use.
//
// Conservative: only drops lines whose first non-whitespace character
// is `@`. Multi-line annotations are rare in MemQL DSL and not used
// in the @use* family.
func stripAttrLines(body string) string {
	var b strings.Builder
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, "@") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func referencedAsCall(body, name string) bool {
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\s*\(`)
	return re.MatchString(body)
}

func referencedAsBareName(body, name string) bool {
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`)
	return re.MatchString(body)
}

func referencedAsArgsField(body, fieldName string) bool {
	// Authors write `args.<field>`; the struct-form rewriter emits
	// the legacy `ctx.<field>` shape but also leaves any directly-
	// authored `args.<field>` references intact. Accept either.
	argsRe := regexp.MustCompile(`\bargs\.` + regexp.QuoteMeta(fieldName) + `\b`)
	if argsRe.MatchString(body) {
		return true
	}
	ctxRe := regexp.MustCompile(`\bctx\.` + regexp.QuoteMeta(fieldName) + `\b`)
	return ctxRe.MatchString(body)
}
