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
			// Escape state TRACKED, not inferred from the preceding byte
			// (memql#3120): `source[i-1] != '\\'` cannot tell an escaped
			// quote from one following a COMPLETED `\\` escape, so a literal
			// ending in a backslash pair never left string state and every
			// brace after it went uncounted -- silently returning the wrong
			// body, or "" (which makes every caller SKIP validation).
			if c == '\\' {
				i++
				continue
			}
			if c == '"' {
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
// Recognised headers (the parser-emitted forms after the struct
// rewriters run):
//
//   - `func (Receiver) name(<args>) <returns> {` (procedural form)
//   - `query NAME {` / `mutate NAME {` / `automation NAME {`
//     `spec NAME {` / `trait NAME {` (struct form, pre-rewrite)
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
	// Struct-form opener.
	for _, kw := range []string{"query ", "mutation ", "automation ", "spec ", "trait "} {
		if strings.HasPrefix(line, kw) {
			return true
		}
	}
	return false
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
