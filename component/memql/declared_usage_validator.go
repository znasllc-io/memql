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
// `func (...) name(...) {` (or `query name {` / `mutate name {`)
// opening brace and its matching closing brace. Returns the empty
// string when no body is found.
//
// The raw source coming into the function loader is either the
// post-rewriter `func (Query|Mutation|Automation) name(...) { ... }`
// form (when the file was struct-form) or the author-written
// procedural form. Either way, the body lives inside the first
// top-level `{ ... }` of the file.
func extractFunctionBody(source string) string {
	open := -1
	depth := 0
	inString := false
	for i := 0; i < len(source); i++ {
		c := source[i]
		if inString {
			if c == '"' && (i == 0 || source[i-1] != '\\') {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
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
// position p is the header line of a function/struct definition.
//
// Recognised headers:
//
//   - `func (Receiver) name(<args>) <returns> {` -- the procedural form, and
//     in practice the ONLY one that ever reaches here.
//   - a struct-form opener (`query NAME {`, `mutate NAME {`, ...), kept as a
//     defensive arm. See below: today it matches nothing.
//
// # The struct-form arm is UNREACHABLE today (memql#3105)
//
// Every caller of extractFunctionBody is fed `rawSourceForUsage`, which
// function_loader.go assigns AFTER NormaliseAll runs -- so the snapshot is
// POST-rewrite text. The rewriter has already turned every struct-form
// construct into `func (Receiver) ...{`, and those keywords have no native
// top-level parser entry, so an un-rewritten one could not have parsed at all.
// `spec` / `trait` / `action` are native but produce SpecDef / ActionDef
// rather than a *FunctionDef, so they never reach this validator either.
//
// Measured on the real tree (81 construct files): 46 `func ` hits, ZERO
// keyword-arm hits.
//
// It is kept rather than deleted because the doc here once said BOTH "the
// parser-emitted forms after the struct rewriters run" AND that the struct
// forms were recognised "pre-rewrite" -- a contradiction that let this arm
// carry the RETIRED `mutation` keyword (renamed to `mutate` in memql#2041)
// and omit `logic` entirely, unnoticed. The keywords now come from the
// rewriter's own StructFormKeywords, so the set cannot drift from the
// rewriter again -- the single-sourcing memql#3094 applied to the call-graph
// checker.
//
// The trap that closes: `rawSourceForUsage` READS like authored text, and if
// anyone moves that snapshot above the rewriter this arm goes live. A header
// miss makes extractFunctionBody return "", and each caller then silently
// SKIPS validation -- disabling validateDeclaredUsage,
// validateLogicEventBinding, validateActorBinding and
// validateLogicEventFields at once. Correct keywords mean that refactor
// degrades safely instead of failing open.
//
// The check looks at the last logical line ending at `prefix`. The
// header keyword is allowed anywhere on that line (covers
// `func (...) name(...) <retType> {`).
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
	// Procedural form (`func (...) name(...) {`).
	if strings.HasPrefix(line, "func ") || strings.HasPrefix(line, "func\t") {
		return true
	}
	// Struct-form opener. Sourced from the rewriter's own keyword set
	// (memql#3105) rather than a hardcoded list, so it cannot drift from the
	// rewriter the way the retired `mutation` spelling did.
	for _, kw := range structFormBodyOpeners {
		if strings.HasPrefix(line, kw) {
			return true
		}
	}
	return false
}

// structFormBodyOpeners is StructFormKeywords with the trailing space each
// prefix match needs, computed once.
//
// `spec ` and `trait ` are deliberately NOT here, and dropping them is not a
// narrowing: neither is a struct-form keyword, both are native parser
// constructs producing SpecDef rather than a *FunctionDef, and neither can
// reach this validator. Listing them implied a coverage this function never
// had.
var structFormBodyOpeners = func() []string {
	kws := languageParser.StructFormKeywords
	out := make([]string, 0, len(kws))
	for _, kw := range kws {
		out = append(out, kw+" ")
	}
	return out
}()

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
