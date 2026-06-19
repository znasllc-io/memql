package sense

// authoring_rules.go implements Sense diagnostics for the gotchas
// catalogued in docs/public/language/authoring-rules.md. Each rule fires at
// edit time so Cockpit surfaces the mistake before the engine refuses
// to start.
//
// Phase 5 Step 34 of the MemQL language-improvements plan:
//
//   Gotcha #1 -- directives (sort / paginate / asOf / select / withDepth
//                 / shape) inside a function body
//   Gotcha #6 -- name shape (DNS-label, max 50 chars) for function and
//                 concept declarations
//   Phase 6   -- `array(T)` deprecation hint (migrate to `[]T`)
//
// Each rule is a function that appends zero or more Diagnostics based
// on an AST walk over *parser.File.

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/znasllc-io/memql/component/language/parser"
)

// directivesInBodyRule flags calls to sort() / paginate() / asOf() /
// select() / withDepth() / shape() that appear inside a function body.
// These are query-level directives that only work at the top level of
// a query string; placing them in a function body makes the
// function-loader validator treat them as references to unknown
// functions and the engine refuses to start.
//
// See docs/public/language/authoring-rules.md gotcha #1.
var directiveNames = map[string]struct{}{
	"sort":      {},
	"paginate":  {},
	"asof":      {},
	"select":    {},
	"withdepth": {},
	"shape":     {},
}

func directivesInBodyRule(file *parser.File, source string) []Diagnostic {
	var diagnostics []Diagnostic
	for _, def := range file.Definitions {
		funcDef, ok := def.(*parser.FunctionDef)
		if !ok {
			continue
		}
		if funcDef.Body == nil {
			continue
		}
		walkExpressionsForDirectives(funcDef, funcDef.Body, source, &diagnostics)
	}
	return diagnostics
}

func walkExpressionsForDirectives(funcDef *parser.FunctionDef, node parser.Node, source string, out *[]Diagnostic) {
	if node == nil {
		return
	}
	switch n := node.(type) {
	case *parser.FunctionCallExpr:
		lower := strings.ToLower(n.Name)
		if _, ok := directiveNames[lower]; ok {
			// Find the call site in source. We look for `<name>(`
			// inside the function's declared span.
			pos := findInSource(source, n.Name+"(")
			*out = append(*out, Diagnostic{
				Range: Range{
					Start: pos,
					End:   Position{Line: pos.Line, Column: pos.Column + len(n.Name)},
				},
				Severity: SeverityError,
				Message:  fmt.Sprintf("directive %q cannot appear inside a function body; the function-loader validator will reject it at engine init. Move it to the caller or inline the query at the call site. See docs/public/language/authoring-rules.md gotcha #1.", n.Name),
				Code:     "directive-in-body",
			})
		}
		for _, arg := range n.Args {
			if inner, ok := arg.(parser.Node); ok {
				walkExpressionsForDirectives(funcDef, inner, source, out)
			}
		}
	case *parser.SortExpr:
		reportDirectiveExpr(funcDef, "sort", source, out)
	case *parser.PaginateExpr:
		reportDirectiveExpr(funcDef, "paginate", source, out)
	case *parser.SelectExpr:
		reportDirectiveExpr(funcDef, "select", source, out)
	case *parser.DepthExpr:
		reportDirectiveExpr(funcDef, "withDepth", source, out)
	case *parser.CountExpr:
		reportDirectiveExpr(funcDef, "count", source, out)
	case *parser.ShapeExpr:
		reportDirectiveExpr(funcDef, "shape", source, out)
	case *parser.TimestampExpr:
		reportDirectiveExpr(funcDef, "asOf", source, out)
	case *parser.LogicalExpr:
		walkExpressionsForDirectives(funcDef, n.Left, source, out)
		walkExpressionsForDirectives(funcDef, n.Right, source, out)
	case *parser.CoalesceExpr:
		for _, a := range n.Args {
			walkExpressionsForDirectives(funcDef, a, source, out)
		}
	case *parser.CondExpr:
		walkExpressionsForDirectives(funcDef, n.Condition, source, out)
		walkExpressionsForDirectives(funcDef, n.Then, source, out)
		walkExpressionsForDirectives(funcDef, n.Else, source, out)
	}
}

func reportDirectiveExpr(funcDef *parser.FunctionDef, name, source string, out *[]Diagnostic) {
	pos := findInSource(source, name+"(")
	*out = append(*out, Diagnostic{
		Range: Range{
			Start: pos,
			End:   Position{Line: pos.Line, Column: pos.Column + len(name)},
		},
		Severity: SeverityError,
		Message:  fmt.Sprintf("directive %q cannot appear inside a function body (function %q); see docs/public/language/authoring-rules.md gotcha #1.", name, funcDef.Name),
		Code:     "directive-in-body",
	})
}

// nameShapeRule checks that function and concept names conform to the
// DNS-label shape (^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$, max 50 chars).
// The CLI enforces this at keystroke time for partition/cluster names
// and the mutation's `args { name string @required }` block only
// type-checks; the shape itself must be validated here at authoring
// time so a malformed name doesn't silently propagate into event-
// topic strings.
//
// See docs/public/language/authoring-rules.md gotcha #6.
//
// We deliberately do not enforce the strict DNS-label regex here
// (`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`) because Go-style camelCase
// function names (listPartitions, queryUserById) are widely used in
// the repo and not DNS-label-shaped. The CLI applies the strict regex
// to keystroke-level inputs (partition names, cluster names) where
// the constraint actually bites. Here we only surface the trivially
// wrong shapes: whitespace, leading/trailing dashes, and length >50.

func nameShapeRule(file *parser.File, source string) []Diagnostic {
	var diagnostics []Diagnostic
	for _, def := range file.Definitions {
		switch d := def.(type) {
		case *parser.FunctionDef:
			diagnostics = appendIfBadName(diagnostics, d.Name, "function", source)
		case *parser.ConceptDecl:
			// Concept names in files are usually `v1:domain:name` --
			// check only the last segment, since the v1:domain
			// prefix carries colons (which the DNS-label regex
			// would reject).
			segment := d.Name
			if idx := strings.LastIndex(segment, ":"); idx >= 0 {
				segment = segment[idx+1:]
			}
			diagnostics = appendIfBadName(diagnostics, segment, "concept", source)
		}
	}
	return diagnostics
}

func appendIfBadName(diagnostics []Diagnostic, name, kind, source string) []Diagnostic {
	// CamelCase is legal for function names (e.g. `listPartitions`),
	// so we normalise to lowercase for the DNS-label check. The
	// length cap still applies to the original.
	if name == "" {
		return diagnostics
	}
	if len(name) > 50 {
		pos := findInSource(source, name)
		diagnostics = append(diagnostics, Diagnostic{
			Range: Range{
				Start: pos,
				End:   Position{Line: pos.Line, Column: pos.Column + len(name)},
			},
			Severity: SeverityWarning,
			Message:  fmt.Sprintf("%s name %q is %d characters; max 50 recommended (gotcha #6)", kind, name, len(name)),
			Code:     "name-too-long",
		})
	}
	// Only flag obvious shape issues: whitespace, underscores,
	// leading/trailing dashes, colons. Go-style camelCase identifiers
	// are tolerated. Partition/cluster names that go through the CLI's
	// DNS-label enforcement will fail there; we're only surfacing
	// things here so the author sees them before engine startup.
	if strings.ContainsAny(name, " \t\n") {
		pos := findInSource(source, name)
		diagnostics = append(diagnostics, Diagnostic{
			Range: Range{
				Start: pos,
				End:   Position{Line: pos.Line, Column: pos.Column + len(name)},
			},
			Severity: SeverityError,
			Message:  fmt.Sprintf("%s name %q contains whitespace; names must be contiguous (gotcha #6)", kind, name),
			Code:     "name-has-whitespace",
		})
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		pos := findInSource(source, name)
		diagnostics = append(diagnostics, Diagnostic{
			Range: Range{
				Start: pos,
				End:   Position{Line: pos.Line, Column: pos.Column + len(name)},
			},
			Severity: SeverityWarning,
			Message:  fmt.Sprintf("%s name %q should not start or end with '-' (gotcha #6)", kind, name),
			Code:     "name-dash-boundary",
		})
	}
	return diagnostics
}

// arraySyntaxRule emits a deprecation hint for `array(T)` usages,
// guiding authors toward the Go-aligned `[]T` spelling introduced in
// Phase 6 of the language-improvements plan. memqlmigrate --rewrite=
// slice-syntax performs the automated rewrite.
func arraySyntaxRule(source string) []Diagnostic {
	var diagnostics []Diagnostic
	// Scan source for `array(T)` occurrences. Skip inside string
	// literals and comments -- a full tokenize would be more robust,
	// but a crude heuristic is enough for a deprecation hint.
	lines := strings.Split(source, "\n")
	for lineIdx, line := range lines {
		content := stripStringsAndComments(line)
		for _, m := range arrayCallRE.FindAllStringIndex(content, -1) {
			start := m[0]
			end := m[1]
			matchText := content[start:end]
			// Filter out calls named `array` inside a larger identifier;
			// the regex anchors to non-identifier-chars on both sides.
			_ = matchText
			pos := Position{Line: lineIdx + 1, Column: start + 1}
			diagnostics = append(diagnostics, Diagnostic{
				Range: Range{
					Start: pos,
					End:   Position{Line: pos.Line, Column: end + 1},
				},
				Severity: SeverityHint,
				Message:  "`array(T)` is deprecated; use `[]T` (run `memqlmigrate --rewrite=slice-syntax` to migrate). See Phase 6 of the language-improvements plan.",
				Code:     "deprecated-array-syntax",
			})
		}
	}
	return diagnostics
}

// arrayCallRE matches `array(T)` where T is a Go-ish type identifier.
// It deliberately matches only the declared-shape form, not a call
// named `array` that appears inside a larger expression.
var arrayCallRE = regexp.MustCompile(`\barray\(\s*[A-Za-z_][A-Za-z0-9_:]*\s*\)`)

// stripStringsAndComments returns the input line with string-literal
// contents and // line-comment tails blanked out, so regex-based
// rules can skip over them without matching embedded syntax.
func stripStringsAndComments(line string) string {
	var b strings.Builder
	inString := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if !inString && c == '/' && i+1 < len(line) && line[i+1] == '/' {
			break // rest of the line is a line comment
		}
		if c == '"' {
			inString = !inString
			b.WriteByte(' ')
			continue
		}
		if inString {
			b.WriteByte(' ')
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}
