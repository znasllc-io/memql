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
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
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
// function names (listPartitions, userById) are widely used in
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

// redundantEnabledRule surfaces the #2610 soft deprecation: enabled is the
// default on every construct kind (#2604-#2608), so a construct-attached
// @enabled is an accepted no-op. It stays legal to parse forever (legacy
// DSL keeps loading), but the editor nudges authors to delete the line.
func redundantEnabledRule(source string) []Diagnostic {
	var diagnostics []Diagnostic
	lines := strings.Split(source, "\n")
	for lineIdx, line := range lines {
		content := stripStringsAndComments(line)
		for _, m := range enabledAnnotationRE.FindAllStringSubmatchIndex(content, -1) {
			// Anchor on the @enabled token (submatch 1), not the
			// whitespace-swallowing full match.
			pos := Position{Line: lineIdx + 1, Column: m[2] + 1}
			diagnostics = append(diagnostics, Diagnostic{
				Range: Range{
					Start: pos,
					End:   Position{Line: pos.Line, Column: m[3] + 1},
				},
				Severity: SeverityHint,
				Message:  "`@enabled` is an accepted no-op: definitions are enabled by default (#2604-#2608). Delete the line; `@disabled` is the off-switch.",
				Code:     "redundant-enabled",
			})
		}
	}
	return diagnostics
}

// enabledAnnotationRE matches a construct-attached `@enabled` annotation
// at line start (the gate's form in dsl/no_redundant_enabled_test.go, so
// the editor hints every shape CI fails: trailing comments, arg forms,
// CRLF endings) -- not the word inside prose or a longer annotation name
// like @enabledFoo.
var enabledAnnotationRE = regexp.MustCompile(`^[ \t]*(@enabled)\b`)

// redundantVersionRule surfaces the #2613 soft deprecation: absent @version
// means 1.0.0, so the literal @version("1.0.0") is ceremony. An explicit
// non-default version never hints.
func redundantVersionRule(source string) []Diagnostic {
	var diagnostics []Diagnostic
	lines := strings.Split(source, "\n")
	for lineIdx, line := range lines {
		// The RAW line, deliberately: stripStringsAndComments blanks the
		// annotation's argument, destroying the "1.0.0"-vs-non-default
		// distinction this rule exists to make. Line-anchoring makes a
		// `// @version("1.0.0")` prose mention safe; a mention inside a
		// /* */ block or a multiline string still hints -- the same
		// latent exposure the sibling rules accept.
		for _, m := range versionDefaultRE.FindAllStringSubmatchIndex(line, -1) {
			pos := Position{Line: lineIdx + 1, Column: m[2] + 1}
			diagnostics = append(diagnostics, Diagnostic{
				Range: Range{
					Start: pos,
					End:   Position{Line: pos.Line, Column: m[3] + 1},
				},
				Severity: SeverityHint,
				Message:  "`@version(\"1.0.0\")` is the default: absent means 1.0.0 (#2613). Delete the line; keep @version only for genuine non-defaults.",
				Code:     "redundant-version",
			})
		}
	}
	return diagnostics
}

// versionDefaultRE matches the redundant default-version annotation line in
// every shape the dsl/ gate fails: inner spacing, a trailing // comment, and
// CRLF endings -- so the editor hints the shapes CI rejects. (Inside /* */
// the editor additionally hints where the gate stays silent; see the rule
// body's exposure note.)
var versionDefaultRE = regexp.MustCompile(`^[ \t]*(@version\([ \t]*"1\.0\.0"[ \t]*\))[ \t]*(//[^\r]*)?\r?$`)

// discardedArgsDescriptionRule surfaces the #2615 corpus rule via
// DiscardedArgsDescriptions (shared with the dsl/ tree gate).
func discardedArgsDescriptionRule(source string) []Diagnostic {
	return DiscardedArgsDescriptions(source)
}

// DiscardedArgsDescriptions flags every @description annotation on an
// args{} field: the parser consumes the annotation and throws it away
// (args_block_parser.go, "silently accepted (no AST slot)", #2615), so
// the prose is dead weight the runtime never sees. Declaration-level
// and concept-field @description are load-bearing and never flag.
// Per-field documentation returns with /// doc comments (#2601).
//
// EXPORTED deliberately: the dsl/ tree gate (no_args_field_description
// _test.go) runs this exact function, so the editor hint and the CI
// gate cannot drift -- parity by construction (#2658 review round).
//
// The scan blanks /* */ spans (BlankBlockComments), strips strings and
// // tails per line, then walks characters so brace transitions are
// position-aware: an annotation on the `args {` opener line is inside
// the block, and a `} shape {` line exits args at the closing brace --
// the sibling block's fields never flag (both were #2658 findings).
func DiscardedArgsDescriptions(source string) []Diagnostic {
	var diagnostics []Diagnostic
	depth := 0
	inArgs := false
	argsDepth := 0
	for lineIdx, line := range strings.Split(BlankBlockComments(source), "\n") {
		code := stripStringsAndComments(line)
		openBraces := map[int]bool{}
		for _, m := range argsBlockOpenRE.FindAllStringSubmatchIndex(code, -1) {
			openBraces[m[1]-1] = true // the opener's brace position
		}
		for i := 0; i < len(code); i++ {
			switch code[i] {
			case '{':
				depth++
				if !inArgs && openBraces[i] {
					inArgs = true
					argsDepth = depth
				}
			case '}':
				depth--
				if inArgs && depth < argsDepth {
					inArgs = false
				}
			case '@':
				if !inArgs || !strings.HasPrefix(code[i:], "@description") {
					continue
				}
				if end := i + len("@description"); end < len(code) && isWordByte(code[end]) {
					continue // @descriptionX -- a different annotation
				}
				pos := Position{Line: lineIdx + 1, Column: i + 1}
				diagnostics = append(diagnostics, Diagnostic{
					Range: Range{
						Start: pos,
						End:   Position{Line: pos.Line, Column: i + 1 + len("@description")},
					},
					Severity: SeverityHint,
					Message:  "`@description` on an args field is parsed and discarded (no AST slot, #2615). Delete it -- per-field docs arrive with /// doc comments (#2601).",
					Code:     "discarded-args-description",
				})
			}
		}
	}
	return diagnostics
}

func isWordByte(c byte) bool {
	return c == '_' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9')
}

// BlankBlockComments blanks real /* */ spans (newline-preserving, so
// line numbers stay stable) while leaving line comments and string
// literals intact -- a `/*` inside a // comment or a string must NOT
// open a span (#2615: dsl/deployment/actions.memql's "scripts/*" prose
// combined with a later real span would phantom-blank live code).
// String handling matches stripStringsAndComments: a bare `"` toggles,
// no escape processing. Shared by the sense rules and the dsl/ gates.
func BlankBlockComments(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	const (
		stNormal = iota
		stString
		stLineComment
		stBlockComment
	)
	state := stNormal
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch state {
		case stNormal:
			switch {
			case c == '"':
				state = stString
				b.WriteByte(c)
			case c == '/' && i+1 < len(src) && src[i+1] == '/':
				state = stLineComment
				b.WriteByte(c)
			case c == '/' && i+1 < len(src) && src[i+1] == '*':
				state = stBlockComment
				b.WriteByte(' ')
				b.WriteByte(' ')
				i++
			default:
				b.WriteByte(c)
			}
		case stString:
			if c == '"' || c == '\n' {
				state = stNormal
			}
			b.WriteByte(c)
		case stLineComment:
			if c == '\n' {
				state = stNormal
			}
			b.WriteByte(c)
		case stBlockComment:
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				state = stNormal
				b.WriteByte(' ')
				b.WriteByte(' ')
				i++
			} else if c == '\n' {
				b.WriteByte('\n')
			} else {
				b.WriteByte(' ')
			}
		}
	}
	return b.String()
}

// argsBlockOpenRE matches an `args {` block opener outside strings and
// comments (the rule scans stripped lines).
var argsBlockOpenRE = regexp.MustCompile(`(^|[^A-Za-z0-9_])args[ \t]*\{`)

// actorUndeclaredRule mirrors the engine's actor-binding load rule
// (#2621) as an edit-time Error (#2622): a query/mutate/logic/
// automation body reading `actor.*` without `@actor` in its preamble.
// The detection is parser.ActorUndeclaredRefOffsets -- the SAME
// header/preamble/reference internals the loader validator and the
// memqlmigrate codemod consume, so squiggle and boot error cannot
// drift, and every position is computed from the AUTHORED source (the
// findInSource first-occurrence trap and the lowered-AST scaffolding
// trap never apply: the rule reads no AST at all). Spec/trait bodies
// (inverse rule) and shapes (@actor kind marker) are excluded by the
// shared header regex; declared-but-unused never flags.
func actorUndeclaredRule(source string) []Diagnostic {
	offsets := parser.ActorUndeclaredRefOffsets(source)
	if len(offsets) == 0 {
		return nil
	}
	var diagnostics []Diagnostic
	line, col, prev := 1, 1, 0
	for _, off := range offsets {
		for i := prev; i < off && i < len(source); i++ {
			if source[i] == '\n' {
				line++
				col = 1
			} else {
				col++
			}
		}
		prev = off
		diagnostics = append(diagnostics, Diagnostic{
			Range: Range{
				Start: Position{Line: line, Column: col},
				End:   Position{Line: line, Column: col + len("actor.")},
			},
			Severity: SeverityError,
			Message:  "this body reads the auth envelope (actor.*) but the construct does not declare `@actor`; reading the actor is a declared capability (#2621) -- add a bare `@actor` annotation line above the declaration",
			Code:     "actor-undeclared",
		})
	}
	return diagnostics
}

// actorUnknownPropertyRule mirrors the engine's closed-set member
// validation (#2625) as an edit-time Error: `actor.displayName` was
// memqllint-green and failed only at runtime -- or resolved to silent
// nil on the logic/spec surfaces. Both layers consume the SAME two
// sources -- parser.ActorMemberRefs for what counts as a read,
// auth.ActorEnvelopeCanonicalName for what is valid -- so no second
// hand-maintained property list can creep in (pinned by a conformance
// test). Positions are computed from the authored source; comments and
// strings are stripped by the shared extractor, so the corpus's
// prose-only `actor.rank` and the `event.actor.id` event stamp never
// flag.
func actorUnknownPropertyRule(source string) []Diagnostic {
	refs := parser.ActorMemberRefs(source)
	if len(refs) == 0 {
		return nil
	}
	var diagnostics []Diagnostic
	line, col, prev := 1, 1, 0
	for _, ref := range refs {
		for i := prev; i < ref.Offset && i < len(source); i++ {
			if source[i] == '\n' {
				line++
				col = 1
			} else {
				col++
			}
		}
		prev = ref.Offset
		if _, ok := auth.ActorEnvelopeCanonicalName(ref.Name); ok {
			continue
		}
		diagnostics = append(diagnostics, Diagnostic{
			Range: Range{
				Start: Position{Line: line, Column: col},
				End:   Position{Line: line, Column: col + len(ref.Name)},
			},
			Severity: SeverityError,
			Message:  "unknown actor member `actor." + ref.Name + "`; the auth envelope is a closed set (#2623) -- valid: " + auth.ActorEnvelopeValidNames(),
			Code:     "actor-unknown-property",
		})
	}
	return diagnostics
}

// ActorUnknownPropertyDiagnostics exposes the closed-set member rule to
// the engine-side conformance test that pins the loader rule and this
// rule to the same tables (#2625).
func ActorUnknownPropertyDiagnostics(source string) []Diagnostic {
	return actorUnknownPropertyRule(source)
}

// descriptionLengthTarget is the ruling-5 editorial target (#2601 design;
// #2703): a description longer than this gets a HINT -- never an error --
// nudging authors toward the terse agent-facing form. One constant,
// re-tunable.
//
// Raised 200 -> 500 in #2759. At 200 the hint fired on 701 of the corpus's
// 2607 descriptions (26%) -- frequent enough to read as background noise
// rather than an editorial nudge, which trains authors to tune the whole
// diagnostic class out. At 500 it flags 133, which is the genuine long
// tail. TestDescriptionLengthTargetIsConsistent pins the two author-facing
// doc strings to this constant so the advice cannot drift from the rule.
const descriptionLengthTarget = 500

// descriptionLengthRule hints when a resolved description (the /// doc
// comment when present, else @description -- ruling-3 precedence) exceeds
// the editorial target, across the full ruling-1 target set: every
// describable declaration kind plus args{} fields. The corpus's long tail
// is grandfathered by severity: a hint never fails the whole-tree sweep or
// any gate. Lengths are rune counts -- the target is defined in characters.
func descriptionLengthRule(file *parser.File, source string) []Diagnostic {
	if file == nil {
		return nil
	}
	var diagnostics []Diagnostic
	hint := func(pos Position, subject string, runes int) {
		diagnostics = append(diagnostics, Diagnostic{
			Range:    Range{Start: pos, End: Position{Line: pos.Line, Column: pos.Column + 1}},
			Severity: SeverityHint,
			Message:  fmt.Sprintf("%sdescription is %d characters; the editorial target is ~%d (#2601 ruling 5) -- consider moving elaboration into an ordinary comment above the /// block", subject, runes, descriptionLengthTarget),
			Code:     "description-length",
		})
	}
	report := func(name string, resolved string) {
		runes := utf8.RuneCountInString(resolved)
		if runes <= descriptionLengthTarget {
			return
		}
		// Anchor on the declaration header (exact-name match), never on
		// an earlier call site or a field that merely contains the name
		// as a substring.
		hint(declHeaderPos(source, name), "", runes)
	}
	reportArgs := func(owner string, schema *parser.ArgsSchema) {
		if schema == nil {
			return
		}
		ownerPos := declHeaderPos(source, owner)
		var walk func(fields []*parser.ArgsField)
		walk = func(fields []*parser.ArgsField) {
			for _, f := range fields {
				if f == nil {
					continue
				}
				// Args fields have a single description channel:
				// the /// doc comment (#2615 stripped the discarded
				// @description form).
				if runes := utf8.RuneCountInString(f.DocComment); runes > descriptionLengthTarget {
					hint(argsFieldAnchor(source, ownerPos, f.Name), fmt.Sprintf("args field %q ", f.Name), runes)
				}
				walk(f.Nested)
				if f.Items != nil {
					walk([]*parser.ArgsField{f.Items})
				}
			}
		}
		walk(schema.Fields)
	}
	attrDesc := func(attrs []*parser.Attribute) string {
		for _, a := range attrs {
			if a != nil && a.Name == "description" {
				if s, ok := a.Value.(string); ok {
					return s
				}
			}
		}
		return ""
	}
	for _, def := range file.Definitions {
		switch d := def.(type) {
		case *parser.FunctionDef:
			report(d.Name, parser.EffectiveDescription(d.DocComment, d.Description))
			reportArgs(d.Name, d.ArgsSchema)
		case *parser.ConceptDecl:
			report(d.Name, parser.EffectiveDescription(d.DocComment, attrDesc(d.Attributes)))
		case *parser.ShapeDecl:
			report(d.Name, parser.EffectiveDescription(d.DocComment, attrDesc(d.Attributes)))
		case *parser.ToolDecl:
			report(d.Name, parser.EffectiveDescription(d.DocComment, d.Description))
		case *parser.SpecDecl:
			report(d.Name, parser.EffectiveDescription(d.DocComment, attrDesc(d.Attributes)))
		case *parser.PromptDecl:
			report(d.Name, parser.EffectiveDescription(d.DocComment, attrDesc(d.Attributes)))
		case *parser.ProviderDecl:
			report(d.Name, parser.EffectiveDescription(d.DocComment, d.Description))
		case *parser.PolicyDecl:
			report(d.Name, parser.EffectiveDescription(d.DocComment, d.Description))
		case *parser.CapabilityDecl:
			report(d.Name, parser.EffectiveDescription(d.DocComment, attrDesc(d.Attributes)))
			reportArgs(d.Name, d.Args)
		case *parser.ActionDecl:
			report(d.Name, parser.EffectiveDescription(d.DocComment, attrDesc(d.Attributes)))
			reportArgs(d.Name, d.Args)
		}
	}
	return diagnostics
}

// declHeaderPos anchors a length hint on the line declaring `name`: a line
// whose first word is a construct keyword and where `name` appears as one of
// the following whole words (allowing for an optional concept binder, e.g.
// `query <Concept> <name>`). Exact-word matching -- an earlier call site or
// a field containing the name as a substring never wins. Falls back to line
// 1 when no header matches (the hint is advisory).
func declHeaderPos(source, name string) Position {
	for i, l := range strings.Split(source, "\n") {
		fields := strings.Fields(l)
		if len(fields) < 2 || !constructKeywords[fields[0]] {
			continue
		}
		limit := len(fields)
		if limit > 4 {
			limit = 4
		}
		for _, f := range fields[1:limit] {
			if f == name || strings.Trim(f, "\"") == name {
				return Position{Line: i + 1, Column: 1}
			}
		}
	}
	return Position{Line: 1, Column: 1}
}

// argsFieldAnchor anchors an args-field hint on the field's own line: the
// first line at or after the owning construct's header whose first word is
// the field name. Falls back to the construct header when the field line is
// not found (the hint is advisory; a header anchor beats line 1).
func argsFieldAnchor(source string, ownerPos Position, fieldName string) Position {
	lines := strings.Split(source, "\n")
	for i := ownerPos.Line - 1; i >= 0 && i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if strings.HasPrefix(trimmed, fieldName) {
			rest := trimmed[len(fieldName):]
			if rest == "" || !isIdentByte(rest[0]) {
				return Position{Line: i + 1, Column: 1}
			}
		}
	}
	return ownerPos
}

// bareRowIntrinsicRule flags a filter predicate that names a row intrinsic
// BARE -- `filter id == args.x` instead of `filter row.id == args.x`
// (memql#2779). It is the edit-time half of
// dsl.TestFilterIntrinsicsUseRowNamespace, so an author sees the rule in
// Cockpit instead of discovering it when CI rejects the branch. Both halves
// call ScanBareRowIntrinsics, so they cannot drift.
//
// Why it matters beyond style: a filter mixes two field surfaces under one
// syntax. Payload properties are bare (epic #2292), so a bare `id` is
// indistinguishable from a payload property by shape alone, and the two
// compile to completely different SQL.
//
// Severity is Warning, not Error: the ENGINE still resolves bare intrinsics
// correctly -- only the authoring gates retired the spelling -- so this never
// breaks boot, and the sense.md convention reserves Error for what does. A
// product bundle mounted via MEMQL_DSL_PATH and authored before the rule
// should read as "migrate this", not as "your workspace is broken".
//
// Scope is deliberately narrow -- filter clauses only. A spec/trait body
// reads its signature-bound fields bare and REJECTS `row.*` outright (epic
// #2281), so flagging a `return` predicate would teach the exact opposite of
// what that surface accepts.
func bareRowIntrinsicRule(source string) []Diagnostic {
	var diagnostics []Diagnostic
	for _, hit := range ScanBareRowIntrinsics(source) {
		diagnostics = append(diagnostics, Diagnostic{
			Range: Range{
				Start: Position{Line: hit.Line, Column: hit.Column},
				End:   Position{Line: hit.Line, Column: hit.Column + len(hit.Text)},
			},
			Severity: SeverityWarning,
			Message: fmt.Sprintf(
				"filter names the row intrinsic %q bare -- write `row.%s`. In a filter, payload properties are BARE (the concept is bound by the signature) and the row envelope is addressed through `row.`, so the two surfaces stay distinguishable (memql#2779)",
				hit.Text, hit.Name),
			Code: "bare-row-intrinsic",
		})
	}
	return diagnostics
}
