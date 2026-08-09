package sense

// runnable.go answers one question for an editor client: which constructs in
// this buffer can be RUN, what arguments does each take, and where does its
// signature sit? It is the language-server half of the VS Code runtime panel's
// run loop (memql#3309).
//
// The load-bearing rule is that the LSP owns ALL .memql parsing. The extension
// must never parse the DSL itself, because two parsers means the generated
// argument form can silently disagree with the compiler about what a construct
// accepts. Everything below therefore goes through the real lexer + parser --
// the same lowering chain Diagnose runs -- and never through a bespoke text
// scanner.
//
// Two passes, because the two things the caller needs live in two different
// places:
//
//  1. POSITIONS come from a lexer pass over the AUTHORED source. The AST
//     carries no line/column at all (component/language/ast), and the lowering
//     that makes struct-form constructs parseable rewrites the text into a
//     different line count, so an AST-derived position would be meaningless to
//     the editor anyway.
//  2. SEMANTICS (arg names, types, enums, doc comments, trigger kwargs) come
//     from parsing each construct's own authored text through
//     applyRewriteChain + parser.Parse.
//
// Pass 2 runs PER CONSTRUCT rather than once over the whole file on purpose.
// The editor asks this question on every keystroke, so at any moment some other
// construct in the buffer is usually half-typed; a whole-file parse would make
// one broken construct erase the run affordances of every healthy one. Slicing
// each construct out by the byte span pass 1 already computed contains the
// blast radius to the construct actually being edited.
//
// A construct whose own text does not parse is OMITTED rather than emitted with
// an empty arg list. An arg form generated from a failed parse would be wrong
// in the one way that matters -- it would let the developer run the construct
// with arguments the compiler never agreed to -- and a missing lens is a far
// cheaper failure than a lying one.

import (
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/language/parser"
)

// RunnableConstruct is one construct the editor can offer a Run affordance for.
// The wire projection of this type is a FIXED contract shared with the VS Code
// extension (memql#3309); field semantics must not drift without changing both
// sides together.
type RunnableConstruct struct {
	// Kind is the authored construct keyword: query, mutate, logic, tool, or
	// automation. These five are the whole runnable set -- spec / trait /
	// prompt / seed / concept / shape / provider / builtin each need an
	// execution semantic decided (which row does a spec evaluate against; who
	// pays for a prompt's provider call) that the runtime-panel design
	// explicitly defers, so they are never returned.
	Kind string
	// Name is the construct's declared name.
	Name string
	// Concept is the signature-bound concept short name for a concept-binding
	// signature (`query <Concept> <name>`), empty otherwise.
	Concept string
	// SignatureRange spans the signature itself -- from the construct keyword
	// through the end of the declared name, NOT including the opening brace or
	// the body. It is what the editor anchors a CodeLens to.
	SignatureRange Range
	// Args is the construct's declared input schema, in authored order. Always
	// non-nil; a construct with no inputs carries an empty slice.
	Args []RunnableArg
	// Trigger is populated for automations only, from the @trigger annotation.
	Trigger *RunnableTrigger
	// Disabled reports the construct's @disabled lifecycle flag: the loader
	// skips it, so it is NOT active on a cluster right now and a run of it can
	// only be refused.
	//
	// Reported so a client can render the state rather than discover it as a
	// FAILED_PRECONDITION after the click -- which, at that point, is
	// indistinguishable from a @filter miss (memql#3333). Per CLAUDE.md
	// @disabled is a reversible on/off switch and NOT deprecation, so this is
	// state to show, not a reason to drop the construct from the result.
	Disabled bool
}

// RunnableArg is one declared input of a runnable construct, projected for
// generating an argument form.
type RunnableArg struct {
	Name string
	// Type is one of string / number / boolean / object / array / any. A DSL
	// type with no form-level equivalent degrades to "any" rather than
	// erroring, so an arg the editor cannot type still gets a JSON entry box.
	Type     string
	Required bool
	// Enum is the closed value set from @enum("a", "b") or the first-class
	// enum("a", "b") type, empty when unconstrained.
	Enum []string
	// Description is the arg's documentation.
	//
	// Sourcing note: for query / mutate / logic args this is the `///` doc
	// comment above the field, which is the ONLY per-arg documentation channel
	// the parser retains -- an args-field @description(...) is accepted and
	// then DISCARDED with no AST slot (memql#2615), and Sense already flags it
	// via DiscardedArgsDescriptions. For tool fields it is the field's
	// @description(...) value, which tools do keep.
	Description string
	// AutoInjected reports a tool field's @autoInjected flag: the engine stamps
	// the value server-side and DROPS whatever the caller sent for it.
	//
	// Reported per-field so a client can mark the one or two fields it applies
	// to, instead of disclaiming the whole form (memql#3333). Only a `tool`
	// field can carry it -- query / mutate / logic args have no such annotation
	// -- so it is always false for the other kinds.
	//
	// The field stays in Args rather than being filtered out here: dropping it
	// client-side would hide a field the engine's own schema declares, and
	// silently diverge from what dispatch actually does.
	AutoInjected bool
}

// RunnableTrigger is an automation's @trigger annotation, projected so the
// editor can build a synthetic event. An automation binds its triggering event
// as `args` and reads args.payload.<field> freely, so there is no declared
// payload schema to generate a form from; the trigger's concept is what lets
// the extension offer a row picker instead.
type RunnableTrigger struct {
	Concept  string
	Event    string
	Schedule string
}

// runnableKeywords is the closed set of runnable construct keywords. See
// RunnableConstruct.Kind for why it is exactly these five.
var runnableKeywords = map[string]bool{
	"query":      true,
	"mutate":     true,
	"logic":      true,
	"tool":       true,
	"automation": true,
}

// RunnableConstructs returns every runnable construct declared in source, in
// declaration order. It never returns an error: a buffer that does not lex, or
// whose constructs do not parse, yields an empty result, because the editor
// asks this on every keystroke and a mid-edit buffer is the normal case rather
// than an exceptional one.
//
// The returned slice is always non-nil.
func (s *Service) RunnableConstructs(source string) []RunnableConstruct {
	out := []RunnableConstruct{}
	if strings.TrimSpace(source) == "" {
		return out
	}
	// The lexer scans a []rune, so every Token.Pos / EndPos is a RUNE offset.
	// Slicing the source by those offsets therefore has to happen over runes
	// too: a single multi-byte character anywhere earlier in the buffer (a
	// non-ASCII @description, an accented string literal) would otherwise
	// shift every later fragment slice left by its extra byte count and cut
	// constructs off mid-token.
	runes := []rune(source)
	for _, span := range scanTopLevelConstructs(source) {
		if !runnableKeywords[span.keyword] {
			continue
		}
		rc, ok := runnableFromSpan(runes, span)
		if !ok {
			continue
		}
		out = append(out, rc)
	}
	return out
}

// constructSpan is one top-level construct located in the authored source: its
// header identifiers, the authored range of its signature, and the byte span of
// its full declaration text (annotation preamble through closing brace).
type constructSpan struct {
	keyword   string
	concept   string
	name      string
	signature Range
	start     int // RUNE offset of the first token of the declaration
	end       int // RUNE offset one past the closing brace
}

// scanTopLevelConstructs walks the authored token stream and locates every
// top-level construct declaration. It tracks brace depth so nested blocks
// (args / filter / insert / step / params) are never mistaken for declarations,
// and it records EVERY construct kind -- not just the runnable ones -- because
// the annotation preamble is attributed to the construct that FOLLOWS it: a
// non-runnable construct that was skipped here would leave its own annotations
// dangling and they would be spliced onto the next runnable construct's
// fragment.
func scanTopLevelConstructs(source string) []constructSpan {
	lexer := parser.NewLexer(source)
	tokens, err := lexer.Tokenize()
	if err != nil {
		return nil
	}

	var out []constructSpan
	depth := 0
	// preambleTok indexes the `@` that opened the pending annotation block, or
	// -1 when none is pending. Everything at depth 0 between two declarations
	// is annotations, so the first `@` after a declaration closes is the start
	// of the next declaration's text.
	preambleTok := -1
	var pending *constructSpan

	for i := 0; i < len(tokens); i++ {
		t := tokens[i]
		if t.Type == parser.TokenEOF {
			break
		}
		if depth == 0 && pending == nil {
			if t.Type == parser.TokenAt {
				if preambleTok < 0 {
					preambleTok = i
				}
				continue
			}
			startTok := i
			if preambleTok >= 0 {
				startTok = preambleTok
			}
			if span, braceIdx, ok := matchConstructHeader(tokens, i); ok {
				span.start = tokens[startTok].Pos
				pending = &span
				depth = 1
				i = braceIdx // the loop's i++ steps past the opening brace
				continue
			}
			if span, lastTok, ok := matchTerseAutomation(tokens, i); ok {
				span.start = tokens[startTok].Pos
				span.end = tokens[lastTok].EndPos
				out = append(out, span)
				preambleTok = -1
				i = lastTok
				continue
			}
		}
		switch t.Type {
		case parser.TokenBraceOpen:
			depth++
		case parser.TokenBraceClose:
			if depth > 0 {
				depth--
			}
			if depth == 0 && pending != nil {
				pending.end = t.EndPos
				out = append(out, *pending)
				pending = nil
				preambleTok = -1
			}
		}
	}
	return out
}

// matchConstructHeader recognises a construct declaration header starting at
// tokens[i]: a construct keyword, one or two identifiers (the optional
// signature concept plus the declared name), then an opening brace. It returns
// the populated span (minus byte offsets, which the caller fills) and the index
// of that opening brace.
//
// The keyword set comes from dslspec (constructKeywords) rather than a hand
// list, so a construct a future grammar epic adds is located automatically. The
// unnamed `use` import is excluded: it is `use a.b.{ x, y }`, whose brace group
// would otherwise read as a declaration body.
//
// Lexer asymmetry this must respect: the lowercase construct keywords lex as
// plain IDENTIFIERS -- only `concept` and `use` get their own token types -- so
// the match compares literals, never token types.
func matchConstructHeader(tokens []parser.Token, i int) (constructSpan, int, bool) {
	t := tokens[i]
	if t.Type != parser.TokenIdentifier && t.Type != parser.TokenKeywordConcept {
		return constructSpan{}, 0, false
	}
	kw := t.Literal
	if kw == "use" || !constructKeywords[kw] {
		return constructSpan{}, 0, false
	}

	idents := make([]int, 0, 2)
	j := i + 1
	for ; j < len(tokens) && len(idents) < 2; j++ {
		if tokens[j].Type != parser.TokenIdentifier {
			break
		}
		idents = append(idents, j)
	}
	if len(idents) == 0 || j >= len(tokens) || tokens[j].Type != parser.TokenBraceOpen {
		return constructSpan{}, 0, false
	}

	nameTok := tokens[idents[len(idents)-1]]
	span := constructSpan{
		keyword: kw,
		name:    nameTok.Literal,
		signature: Range{
			Start: Position{Line: t.Line, Column: t.Column},
			End:   Position{Line: nameTok.EndLine, Column: nameTok.EndCol},
		},
	}
	if len(idents) == 2 {
		span.concept = tokens[idents[0]].Literal
	}
	return span, j, true
}

// matchTerseAutomation recognises the brace-less single-step automation form:
//
//	automation NAME @trigger(schedule="0 0 2 * * *") => logic targetLogic
//
// It has to be handled explicitly for two reasons. The obvious one is that it
// IS a runnable automation and would otherwise be invisible. The subtler one is
// that its inline `@trigger(...)` sits at depth 0 with no declaration body to
// close: left unrecognised it becomes a dangling annotation preamble, and the
// NEXT declaration's fragment gets sliced from there -- swallowing this whole
// line and every comment between the two, which is exactly how the two
// dsl/identity automations that surfaced this were lost.
//
// The form is one line by construction (parser.terseAutomationHeader is
// `(?m)^...$`), so the declaration ends with the last token on the header's
// line. Returns the span (start filled by the caller) and the index of that
// last token.
func matchTerseAutomation(tokens []parser.Token, i int) (constructSpan, int, bool) {
	t := tokens[i]
	if t.Type != parser.TokenIdentifier || t.Literal != "automation" {
		return constructSpan{}, 0, false
	}
	if i+2 >= len(tokens) {
		return constructSpan{}, 0, false
	}
	nameTok := tokens[i+1]
	// `automation NAME @...` -- the `@` is what distinguishes the terse form
	// from a block automation (whose next token is `{`).
	if nameTok.Type != parser.TokenIdentifier || tokens[i+2].Type != parser.TokenAt {
		return constructSpan{}, 0, false
	}

	last := i + 2
	for k := i + 2; k < len(tokens); k++ {
		if tokens[k].Type == parser.TokenEOF || tokens[k].Line != t.Line {
			break
		}
		last = k
	}

	return constructSpan{
		keyword: "automation",
		name:    nameTok.Literal,
		signature: Range{
			Start: Position{Line: t.Line, Column: t.Column},
			End:   Position{Line: nameTok.EndLine, Column: nameTok.EndCol},
		},
	}, last, true
}

// runnableFromSpan parses one construct's authored text in isolation and
// projects it onto the wire contract. Reports false when the fragment does not
// parse, or when the parsed declaration disagrees with the header the scan
// found -- the latter can only mean the slice captured something other than the
// construct we located, and emitting it would attach one construct's arguments
// to another's signature.
func runnableFromSpan(source []rune, span constructSpan) (RunnableConstruct, bool) {
	if span.start < 0 || span.end > len(source) || span.start >= span.end {
		return RunnableConstruct{}, false
	}
	def, ok := parseConstructFragment(string(source[span.start:span.end]))
	if !ok {
		return RunnableConstruct{}, false
	}

	rc := RunnableConstruct{
		Kind:           span.keyword,
		Name:           span.name,
		Concept:        span.concept,
		SignatureRange: span.signature,
		Args:           []RunnableArg{},
	}

	switch d := def.(type) {
	case *parser.ToolDecl:
		if span.keyword != "tool" || d.Name != span.name {
			return RunnableConstruct{}, false
		}
		// A tool declaration's @disabled is a typed field on the decl rather
		// than a surviving attribute, because parseToolDecl folds the leading
		// attribute set into named fields.
		rc.Disabled = d.Disabled
		// A tool declaration's body field list IS its input schema; there is
		// no args block to read.
		for _, f := range d.Fields {
			rc.Args = append(rc.Args, RunnableArg{
				Name:         f.Name,
				Type:         normaliseArgType(f.Type),
				Required:     f.Required,
				Enum:         copyStrings(f.EnumValues),
				Description:  strings.TrimSpace(f.Description),
				AutoInjected: f.AutoInjected,
			})
		}
	case *parser.FunctionDef:
		if d.Name != span.name {
			return RunnableConstruct{}, false
		}
		// A procedural construct keeps its lifecycle annotation in the
		// surviving attribute list rather than in a typed field.
		rc.Disabled = hasDisabledAttribute(d.Attributes)
		if span.keyword == "automation" {
			// An automation declares no caller args: the triggering event is
			// bound as `args`, and an automation's own args block projects
			// fields OFF that event rather than declaring inputs a caller
			// supplies. The editor builds the event from the trigger instead
			// (pick a row of the trigger concept, or paste JSON), so the arg
			// list stays deliberately empty.
			rc.Trigger = triggerFromAttributes(d.Attributes)
			break
		}
		if d.ArgsSchema != nil {
			for _, f := range d.ArgsSchema.Fields {
				if f == nil {
					continue
				}
				rc.Args = append(rc.Args, RunnableArg{
					Name:        f.Name,
					Type:        normaliseArgType(f.Type),
					Required:    !f.Optional,
					Enum:        enumStrings(f.Enum),
					Description: strings.TrimSpace(f.DocComment),
				})
			}
		}
	default:
		return RunnableConstruct{}, false
	}
	return rc, true
}

// parseConstructFragment lowers and parses a single construct's authored text,
// returning its declaration node. It runs the SAME rewrite chain Diagnose and
// the runtime loaders run, so what parses here is exactly what the engine
// accepts -- the whole point of routing this through the compiler's parser
// instead of a bespoke scanner.
func parseConstructFragment(fragment string) (parser.Node, bool) {
	rewritten, err := applyRewriteChain(fragment)
	if err != nil {
		return nil, false
	}
	lexer := parser.NewLexer(rewritten)
	tokens, lexErr := lexer.Tokenize()
	if lexErr != nil {
		return nil, false
	}
	p := parser.NewParser(tokens)
	// Doc comments are the only per-arg documentation channel the AST keeps,
	// so they have to be handed to the parser explicitly (as Diagnose does).
	p.SetDocComments(lexer.DocComments())
	node, parseErr := p.Parse()
	if parseErr != nil {
		return nil, false
	}
	// NOTE: a parse error returns a TYPED-NIL *parser.File inside a non-nil
	// Node interface, so the pointer needs its own nil check even on the
	// success path.
	file, ok := node.(*parser.File)
	if !ok || file == nil || len(file.Definitions) == 0 {
		return nil, false
	}
	return file.Definitions[0], true
}

// hasDisabledAttribute reports whether a procedural construct's surviving
// attribute list carries @disabled.
//
// @enabled is deliberately not consulted: per the lifecycle ruling it is the
// explicit-on default and a no-op, so ENABLED is the absence of @disabled
// rather than the presence of @enabled. Reading both would make an unannotated
// construct -- the overwhelming majority -- look like neither.
func hasDisabledAttribute(attrs []*parser.Attribute) bool {
	for _, a := range attrs {
		if a != nil && a.Name == string(parser.AttrDisabled) {
			return true
		}
	}
	return false
}

// triggerFromAttributes projects an automation's @trigger annotation. Returns
// nil when the automation carries no trigger kwargs at all, so the wire form
// omits the field rather than shipping an empty object.
//
// The `partition="*"` kwarg is deliberately not projected: it is the residual
// of the retired partition dimension (#56 phase 8) and says nothing about how
// to synthesize the event.
func triggerFromAttributes(attrs []*parser.Attribute) *RunnableTrigger {
	for _, a := range attrs {
		if a == nil || a.Name != string(parser.AttrTrigger) {
			continue
		}
		tr := &RunnableTrigger{
			Event:    attrArgString(a.Args, "event"),
			Concept:  attrArgString(a.Args, "concept"),
			Schedule: attrArgString(a.Args, "schedule"),
		}
		if tr.Event == "" && tr.Concept == "" && tr.Schedule == "" {
			return nil
		}
		return tr
	}
	return nil
}

// attrArgString reads one annotation kwarg as a string. A structured trigger's
// concept= can arrive as a dotted symbol reference rather than a string
// literal, so a non-string value is stringified rather than dropped (mirroring
// ast.ParseStructuredTriggerArgs).
func attrArgString(args map[string]any, key string) string {
	v, ok := args[key]
	if !ok || v == nil {
		return ""
	}
	if s, isStr := v.(string); isStr {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// normaliseArgType maps a DSL argument type onto the closed set the argument
// form renders: string / number / boolean / object / array / any.
//
// `datetime` maps to string rather than any: the engine takes an RFC3339
// string for it, so a text field is the honest control. Anything unrecognised
// degrades to "any" (a free JSON entry box) instead of erroring -- an arg the
// editor cannot type is still an arg the developer can fill in.
func normaliseArgType(t string) string {
	t = strings.TrimSpace(t)
	if strings.HasPrefix(t, "[]") {
		return "array"
	}
	switch strings.ToLower(t) {
	case "string", "text", "enum", "datetime", "timestamp", "date", "time":
		return "string"
	case "number", "int", "integer", "float", "double", "decimal":
		return "number"
	case "bool", "boolean":
		return "boolean"
	case "object", "map", "json":
		return "object"
	case "array", "list":
		return "array"
	default:
		return "any"
	}
}

// enumStrings projects an args-field enum value set onto strings. Non-string
// members are stringified so a numeric enum still renders as a closed choice
// list rather than silently becoming a free-text field.
func enumStrings(values []any) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, v := range values {
		if s, ok := v.(string); ok {
			out = append(out, s)
			continue
		}
		out = append(out, fmt.Sprintf("%v", v))
	}
	return out
}

func copyStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	return append([]string(nil), in...)
}
