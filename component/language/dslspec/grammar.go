package dslspec

// The generated grammar (memql#5388, D24 of the DSL v1 freeze record).
//
// # Why a BNF ships with the engine
//
// Two consumers, and neither is a human reading it top to bottom. A model given
// the grammar in its prompt emits fewer forms the parser refuses; a decoder
// constrained to the grammar emits none. Both break the same way if the
// grammar is hand-maintained: it drifts, and a drifted grammar does not fail
// loudly -- it teaches a model a form that does not exist, and the failure
// surfaces as "the model is bad at MemQL".
//
// So it is GENERATED, from the same tables the parser itself reads: the
// construct set and its body clauses (parser.ConstructKeywords,
// parser.BodyClauses, parser.IsLineClause, parser.IsNamedBlock), the
// annotation registry (component/language/annotations), and the function
// catalog and operator table (component/language/functions). Nothing here is
// authored twice. The page it writes is gated the way the attribute matrix is:
// `make docs-grammar-check` fails when the committed page is not what the
// tables render.
//
// # The three facts the tables gained for it
//
// A production needs three things the parser's tables could not state, and
// each was added to the TABLE rather than written out here: Construct.BodyForm
// (what the braces hold -- clauses, statements, fields, paths),
// Construct.Signature (the EBNF between the keyword and the body) and
// Keyword.Grammar (a clause's or a statement's own right-hand side). That is
// the rule this file is written to: where a table carries no description of
// something, the description goes in the table. A list of language forms in a
// renderer is the hand-maintained page this task removes.
//
// # What it deliberately does NOT describe
//
// The internal query form -- the string an SDK sends to Execute -- keeps its
// own grammar and is not here. A reader given both would have no way to tell
// which one their file is written in.
//
// Per-annotation argument legality is not here either. `<annotation-args>` is
// one generic production; which annotation is legal on which construct, and in
// which argument form, is the generated attribute matrix's whole subject
// (AttributeMatrixPath), and a syntax-only grammar cannot carry it without one
// production per annotation per receiver.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/language/annotations"
	"github.com/znasllc-io/memql/component/language/functions"
	"github.com/znasllc-io/memql/component/language/parser"
)

// GrammarPath is where the generated grammar page is committed, relative to
// the repository root.
const GrammarPath = "docs/public/language/grammar.md"

// productionColumn is the column the `::=` of every production lines up on, so
// the emitted grammar reads as a table rather than as ragged text.
const productionColumn = 22

// BNF renders the grammar with no page furniture: the productions alone, which
// is the form a prompt or a constrained decoder takes.
func BNF() string {
	var b strings.Builder
	spec := Build()

	fmt.Fprintf(&b, "(* MemQL authoring grammar. Edition %s, grammar version %s.\n"+
		"   GENERATED from the parser, the annotation registry and the function\n"+
		"   catalog -- do not edit. Run `make docs-grammar` to refresh it.\n\n"+
		"   Notation: <name> is a production, \"x\" a terminal, [ x ] optional,\n"+
		"   { x } zero or more, x* zero or more, x+ one or more, ( a | b ) a\n"+
		"   choice, \"a\"..\"z\" a character range, and 'x' a terminal holding a\n"+
		"   double quote. A right-hand side written in English is lexical: the\n"+
		"   lexer decides it. *)\n\n",
		parser.Edition, parser.GrammarVersion)

	b.WriteString("(* ---- A file ---- *)\n")
	b.WriteString(production("file", `<use>* <declaration>*`))
	b.WriteString(production("use", constructSignature(spec, "use")))
	b.WriteString(production("dotted-path", `<name> { "." <name> } "."`))
	b.WriteString(production("declaration", joinAlternatives(declarationAlternatives(spec))))
	b.WriteString("\n")

	b.WriteString("(* ---- Declarations ---- *)\n")
	for _, c := range sortedConstructs(spec) {
		b.WriteString(constructProduction(spec, c))
	}

	b.WriteString(clauseProduction(spec))
	b.WriteString(statementProduction(spec))
	b.WriteString(annotationProduction(spec))
	b.WriteString(fieldProduction(spec))
	b.WriteString(expressionProduction(spec))
	b.WriteString(operatorProduction(functions.Operators()))
	b.WriteString(functionProduction(functions.Catalog()))
	b.WriteString(literalProduction(spec))
	return b.String()
}

// RenderGrammar wraps BNF in the committed page, with the front matter docs/
// requires and the regeneration instruction.
func RenderGrammar() string {
	var b strings.Builder
	b.WriteString(generatedFrontMatter("MemQL Grammar", "docs-grammar"))
	b.WriteString("# MemQL grammar\n\n")
	b.WriteString("This is the authoring grammar for `.memql` files, in EBNF. It is generated from the parser's own construct and clause tables, the annotation registry in `component/language/annotations`, and the function catalog and operator table in `component/language/functions`, so it cannot describe a language the cluster does not accept.\n\n")
	b.WriteString("It exists to be given to a model -- as grammar-in-prompt, or as the grammar a constrained decoder is held to. A hand-maintained grammar drifts silently: it does not fail, it teaches a form the parser refuses, and the failure reads as \"the model is bad at MemQL\".\n\n")
	b.WriteString("Two things are deliberately absent. The internal query form -- the string an SDK sends to `Execute` -- has its own grammar; a reader given both would have no way to tell which one their file is written in. And which annotation is legal on which construct, in which argument form, is the [attribute matrix](attribute-matrix.md), not a syntax rule. What each name MEANS is the [vocabulary](vocabulary.md).\n\n")
	fmt.Fprintf(&b, "Edition `%s`, grammar version `%s`.\n\n", parser.Edition, parser.GrammarVersion)
	b.WriteString("```ebnf\n")
	b.WriteString(BNF())
	b.WriteString("```\n")
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// generatedFrontMatter is the six-key front-matter block docs/ requires
// (DOCS_STANDARD.md section 2, gated by docs_front_matter_test.go) plus the
// DO-NOT-EDIT banner, for a page this package generates.
func generatedFrontMatter(title, target string) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "title: %s\n", title)
	b.WriteString("audience: public\n")
	b.WriteString("status: stable\n")
	b.WriteString("area: language\n")
	b.WriteString("sinceVersion: 0.22.8\n")
	b.WriteString("owner: znas\n")
	b.WriteString("---\n\n")
	fmt.Fprintf(&b, "<!-- GENERATED by `make %s` (cmd/dslgrammar) from component/language/{parser,annotations,functions}. DO NOT EDIT: change the tables and run `make %s`. -->\n\n", target, target)
	return b.String()
}

// production renders one `<name> ::= rhs` line, padded so the `::=` of a run
// of productions lines up.
func production(name, rhs string) string {
	head := "<" + name + ">"
	for len(head) < productionColumn {
		head += " "
	}
	if !strings.HasSuffix(head, " ") {
		head += " "
	}
	return head + "::= " + strings.TrimSpace(rhs) + "\n"
}

// joinAlternatives renders an alternation, wrapping onto continuation lines
// that open with the bar so a long one stays readable.
func joinAlternatives(alts []string) string {
	const width = 60
	indent := "\n" + strings.Repeat(" ", productionColumn) + "  | "
	var (
		lines []string
		cur   string
	)
	for i, alt := range alts {
		switch {
		case i == 0:
			cur = alt
		case len(cur)+3+len(alt) > width:
			lines = append(lines, cur)
			cur = alt
		default:
			cur += " | " + alt
		}
	}
	return strings.Join(append(lines, cur), indent)
}

// sortedConstructs returns every construct but `use`, by keyword. `use` is the
// file-top import statement and is emitted with the file production above; it
// declares nothing, so it is not an alternative of <declaration>.
func sortedConstructs(spec *Spec) []Construct {
	out := make([]Construct, 0, len(spec.Constructs))
	for _, c := range spec.Constructs {
		if c.Keyword == useKeyword {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Keyword < out[j].Keyword })
	return out
}

// declarationAlternatives names every construct a top-level declaration may
// be, as productions, sorted.
func declarationAlternatives(spec *Spec) []string {
	var out []string
	for _, c := range sortedConstructs(spec) {
		out = append(out, "<"+c.Keyword+">")
	}
	return out
}

// constructSignature returns the hand-authored signature EBNF of one keyword.
func constructSignature(spec *Spec, keyword string) string {
	if c := spec.ConstructByKeyword(keyword); c != nil {
		return `"` + keyword + `" ` + c.Signature
	}
	return `"` + keyword + `"`
}

// constructProduction renders one construct: its leading annotations, its
// signature and its body. Every part is derived -- the annotation set from the
// registry receiver the construct declares, the signature from the construct
// table, and the body from BodyForm plus parser.BodyClauses.
func constructProduction(spec *Spec, c Construct) string {
	var b strings.Builder
	head := ""
	if len(annotations.ByReceiver[c.AnnotationReceiver]) > 0 {
		head = "<" + c.Keyword + "-annotation>* "
	}
	b.WriteString(production(c.Keyword, head+constructSignature(spec, c.Keyword)+" "+constructBodyRef(c)))
	if body := constructBodyProduction(c); body != "" {
		b.WriteString(body)
	}
	return b.String()
}

// constructBodyRef is what follows a construct's signature: braces around a
// body production, `= <lambda>` for a predicate, or nothing.
func constructBodyRef(c Construct) string {
	switch c.BodyForm {
	case BodyFormLambda:
		return `"=" <lambda>`
	case BodyFormEmpty:
		return `"{" "}"`
	case BodyFormImport:
		return ""
	default:
		return `"{" <` + c.Keyword + `-body> "}"`
	}
}

// constructBodyProduction renders the body production a construct's braces
// hold, from its BodyForm and -- for the clause forms -- the parser's own
// clause table.
func constructBodyProduction(c Construct) string {
	name := c.Keyword + "-body"
	switch c.BodyForm {
	case BodyFormClauses:
		return production(name, "( "+joinAlternatives(clauseRefs(c))+" )*")
	case BodyFormStatements:
		return production(name, "( "+joinAlternatives(clauseRefs(c))+" )* <statement>*")
	case BodyFormCapabilityCall:
		return production(name, "( "+joinAlternatives(clauseRefs(c))+" )* <capability-call>")
	case BodyFormFields:
		// A concept's fields are their own production: only a concept
		// property may be a nested object block or carry a @variant body
		// (parser.parsePropertyDecl); an args, tool, prompt or builtin field
		// reads a type word and stops.
		if c.Keyword == "concept" {
			if extra := annotations.ByReceiver[string(annotations.ConceptBody)]; len(extra) > 0 {
				return production(name, "( <concept-field> | <"+c.Keyword+"-body-annotation> )*")
			}
			return production(name, "<concept-field>*")
		}
		return production(name, "<field>*")
	case BodyFormPaths:
		return production(name, "<path>*")
	case BodyFormAssignments:
		// A seed body is NOT a map literal: its entries carry a literal value
		// rather than an expression, and a nested object is written as a
		// BLOCK with no colon (`providerConfig { llm { provider: "..." } }`,
		// parser.parseSeedBlock). Spelling it `<map-entry>*` said neither.
		return production(name, "<seed-entry>*")
	default:
		return ""
	}
}

// clauseRefs names the production of every clause a construct's body accepts,
// in the parser's authoring order. A line clause and a block are told apart by
// parser.IsLineClause, which is what decides the production's suffix.
func clauseRefs(c Construct) []string {
	out := make([]string, 0, len(c.BodyBlocks))
	for _, clause := range c.BodyBlocks {
		out = append(out, "<"+clauseProductionName(clause)+">")
	}
	if len(out) == 0 {
		return []string{"<statement>"}
	}
	return out
}

// clauseProductionName is a clause's production name: `<filter-clause>` for a
// line clause, `<args-block>` for a block. The suffix is what keeps a clause
// that shares a word with a construct (`shape`, `count`) from colliding with
// it.
func clauseProductionName(clause string) string {
	if parser.IsLineClause(clause) {
		return clause + "-clause"
	}
	return clause + "-block"
}

// clauseProduction renders every body clause any construct accepts, each from
// the Grammar its keyword table entry carries.
func clauseProduction(spec *Spec) string {
	var b strings.Builder
	b.WriteString("\n(* ---- Body clauses ---- *)\n")
	for _, k := range spec.Keywords {
		if k.Kind != "clause" {
			continue
		}
		b.WriteString(production(clauseProductionName(k.Name), k.Grammar))
	}
	b.WriteString(production("capability-call", `"capability" <dotted-name> "(" [ <argument> { "," <argument> } ] ")"`))
	// The bare-mirror shorthand (authoring rule 15): a write-block entry that
	// is only `args.name` means `name: args.name`. It is write-block SYNTAX
	// rather than an expression -- the struct-form rewriter expands it while
	// the block is still lines (expandBareMirror), because the edition-2026
	// map literal has no key-less entry -- so the grammar has to spell it, or
	// it describes a write block this engine's own mutations do not write.
	// One segment only: `args.user.id` is refused by name.
	b.WriteString(production("write-entry", `<name> ":" <expression> | "args" "." <name>`+
		"\n"+strings.Repeat(" ", productionColumn)+`  | <accept-block> | <stamp-block>`))
	b.WriteString(production("map-entry", `<name> ":" <expression>`))
	// The three entry forms that are NOT map entries, each because the
	// construct's own parser reads a literal where a map entry reads an
	// expression: a provider's params and auth (parseProviderParamsBlock,
	// parseProviderAuthBlock) and a seed's body (parseSeedBlock). All three
	// were spelled <map-entry> and none of them is one -- a provider entry
	// carries no colon at all.
	b.WriteString(production("param-entry", `<name> ( <string> | <number> | "true" | "false" )`))
	b.WriteString(production("auth-entry", `<name> ( <string> | "env" "(" <string> ")" )`))
	// A precondition's keys are the closed set the loader's regex names, and
	// `check:` is required -- which is a LOAD rule, not a syntax one, so the
	// production offers the three keys and the doc carries the requirement.
	// Each value runs to the end of its line and quotes around it are
	// optional, so <expression> is what an author writes there rather than
	// what the regex would take.
	b.WriteString(production("precondition-entry", `<precondition-key> ":" <expression>`))
	b.WriteString(production("precondition-key", `"check" | "literal" | "description"`))
	b.WriteString(production("seed-entry", `<name> ":" <seed-value> | <name> "{" <seed-entry>* "}"`))
	b.WriteString(production("seed-value", `<string> | <number> | "true" | "false" | "[" ( <string> [ "," ] )* "]"`))
	b.WriteString(production("path", `<name> { "." <name> }`))
	return b.String()
}

// statementProduction renders the statement language a logic's and an
// automation's body is written in: one alternative per control keyword that
// heads a production, each spelled by that keyword's Grammar.
func statementProduction(spec *Spec) string {
	var (
		b        strings.Builder
		heads    []string
		trailing []string
	)
	b.WriteString("\n(* ---- Statements (a logic's and an automation's body) ---- *)\n")
	for _, k := range spec.Keywords {
		switch k.Heads {
		case "statement":
			heads = append(heads, "<"+k.Name+"-statement>")
		case "trailing":
			trailing = append(trailing, k.Grammar)
		}
	}
	// The three forms written with no keyword of their own, in the order
	// parser.BodyStatementForms() names them: assign, fieldWrite, call.
	// statementFormsHaveAProduction holds this list to that one.
	b.WriteString(production("statement", joinAlternatives(append([]string{"<binding>", "<field-write>", "<construct-call>"}, heads...))))
	// A binding's value is a construct call OR an expression
	// (parser.parseV1Assign falls through to parseV1BodyExpression), which is
	// what `decide := row.submitterRole == "owner" ? ... : ...` is.
	b.WriteString(production("binding", `<name> ":=" ( <construct-call> | <expression> )`))
	// The before-write field write (ast.FieldWriteStatement): one declared
	// payload field of the row being written, in an automation whose trigger
	// carries `before=`.
	b.WriteString(production("field-write", `"row" "." <name> "=" <expression>`))
	// The invocation is split from the statement because an EXPRESSION may
	// hold one too: `query lbTickets().count()` is what a logic body and a
	// step argument write (parseV1Name dispatches to parseV1ConstructCall).
	// Only the statement takes trailing clauses, so <primary> references the
	// invocation and <statement> the call.
	b.WriteString(production("construct-call", `<construct-invocation> <trailing-clause>*`))
	b.WriteString(production("construct-invocation", `<construct-kind> <name> "(" [ <argument> { "," <argument> } ] ")"`))
	b.WriteString(production("construct-kind", joinAlternatives(callableConstructKinds(spec))))
	b.WriteString(production("argument", `<name> ":" <expression>`))
	if len(trailing) > 0 {
		b.WriteString(production("trailing-clause", joinAlternatives(trailing)))
	}
	for _, k := range spec.Keywords {
		if k.Heads == "statement" {
			b.WriteString(production(k.Name+"-statement", k.Grammar))
		}
	}
	return b.String()
}

// callableConstructKinds are the words a statement names a call with. A call
// is spelled with the callee's own construct keyword (D13: one word declares
// and calls), so the set is the construct set narrowed to the kinds a body may
// invoke -- the function categories plus the declarative constructs a
// statement dispatches (action, capability). A concept, a shape or a provider
// is never called.
func callableConstructKinds(spec *Spec) []string {
	var out []string
	for _, c := range sortedConstructs(spec) {
		switch c.Category {
		case CategoryFunction:
			out = append(out, `"`+c.Keyword+`"`)
		case CategoryDeclarative:
			if c.BodyForm == BodyFormCapabilityCall || c.Keyword == "builtin" {
				out = append(out, `"`+c.Keyword+`"`)
			}
		}
	}
	return out
}

// annotationProduction renders one annotation production per receiver, each
// listing exactly the names that receiver accepts, plus the one generic
// argument form. The name sets are the registry's, so an annotation the
// registry gains is offered the day it lands and one it retires disappears
// with it.
func annotationProduction(spec *Spec) string {
	var b strings.Builder
	b.WriteString("\n(* ---- Annotations ---- *)\n")
	b.WriteString("(* Which annotation is legal where is this production set; which ARGUMENT\n" +
		"   form each takes there is the attribute matrix, not a syntax rule. *)\n")
	for _, c := range sortedConstructs(spec) {
		names := annotations.ByReceiver[c.AnnotationReceiver]
		if len(names) == 0 {
			continue
		}
		b.WriteString(production(c.Keyword+"-annotation", annotationAlternation(names)))
	}
	if names := annotations.ByReceiver[string(annotations.ConceptBody)]; len(names) > 0 {
		b.WriteString(production("concept-body-annotation", annotationAlternation(names)))
	}
	for _, r := range fieldReceiverNames() {
		if names := annotations.ByReceiver[r.receiver]; len(names) > 0 {
			b.WriteString(production(r.production, annotationAlternation(names)))
		}
	}
	b.WriteString(production("annotation-args", `"(" [ <annotation-arg> { "," <annotation-arg> } ] ")"`))
	b.WriteString(production("annotation-arg", joinAlternatives([]string{
		"<string>", "<number>", `"true"`, `"false"`, "<object>", "<lambda>",
		`<name> [ "=" <annotation-value> ]`, `"!" <string>`,
	})))
	// A keyword argument's value may be a LAMBDA: `@trigger(filter=row => ...)`
	// and `@loop(until=row => ...)` both take one (parseAttributeArgValue),
	// and neither derived while this listed scalars only.
	b.WriteString(production("annotation-value", `<string> | <number> | "true" | "false" | <name> | <lambda>`))
	return b.String()
}

// annotationAlternation renders `"@name" [ <annotation-args> ] | ...` over a
// receiver's accepted names, sorted.
func annotationAlternation(names []string) string {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	alts := make([]string, 0, len(sorted))
	for _, n := range sorted {
		alts = append(alts, `"@`+n+`"`)
	}
	return `( ` + joinAlternatives(alts) + ` ) [ <annotation-args> ]`
}

// fieldReceiver pairs a registry field receiver with the production name the
// grammar gives its annotation set.
type fieldReceiver struct {
	receiver   string
	production string
}

// fieldReceiverNames lists the registry's field receivers and the production
// each becomes. The receivers are named as registry constants, not as a list
// of annotation names, so the set a receiver accepts stays the registry's.
func fieldReceiverNames() []fieldReceiver {
	return []fieldReceiver{
		{string(annotations.ConceptField), "concept-field-annotation"},
		{string(annotations.ArgsField), "args-field-annotation"},
		{string(annotations.ToolField), "tool-field-annotation"},
		{string(annotations.PromptField), "prompt-field-annotation"},
		{string(annotations.BuiltinField), "builtin-field-annotation"},
	}
}

// fieldProduction renders a field: its name, its type, the `!` required sigil
// and its annotations.
//
// `!` is the CANONICAL required spelling (#2618) -- the codemod rewrites
// `@required` into it and every field receiver's parser reads it -- so a
// grammar without it cannot derive most of the tree, and a model held to one
// would be taught the long form the codemod removes.
//
// <field-type> ENUMERATES the field-type table, DEPRECATED SPELLINGS
// INCLUDED. A field type is the most frequent token in a field declaration and
// the one a constrained decoder most needs told, so `<name>` there would be
// guidance thrown away -- and it would newly admit `title return`, which the
// parser refuses. The table is what makes the enumeration safe: every word the
// engine reads is in it, and a word that is not is now a LOUD failure naming
// the file, which is what this whole change buys. A deprecated spelling stays
// derivable (the tree writes `array` 43 times) and the vocabulary is where it
// is flagged, with its replacement.
func fieldProduction(spec *Spec) string {
	var b strings.Builder
	b.WriteString("\n(* ---- Fields ---- *)\n")
	b.WriteString(production("field", `<doc-comment>* <name> <field-type> [ "!" ] <field-annotation>*`))
	// A concept property has two more forms, and neither is available to an
	// args, tool, prompt or builtin field: a nested object written as a BLOCK
	// with no type word (`capabilities { ... }`), and a trailing block after
	// the annotations, which is the @variant body or an @open object's fields.
	// The two trailing roles are one token shape -- a variant branch IS a
	// name and a block -- and which one the parser reads is decided by the
	// annotation, which is the attribute matrix's subject, not a syntax rule.
	b.WriteString(production("concept-field", `<doc-comment>* <name> ( "{" <concept-field>* "}"`+
		"\n"+strings.Repeat(" ", productionColumn)+`  | <field-type> [ "!" ] <concept-field-annotation>* [ "{" <concept-field>* "}" ] )`))
	b.WriteString(production("field-annotation",
		joinAlternatives([]string{
			"<concept-field-annotation>", "<args-field-annotation>",
			"<tool-field-annotation>", "<prompt-field-annotation>", "<builtin-field-annotation>",
		})))
	if flagged := deprecatedFieldTypes(spec); flagged != "" {
		fmt.Fprintf(&b, "(* %s. It still derives --\n"+
			"   the tree writes it -- and the vocabulary is where a reader is told what\n"+
			"   to write instead. *)\n", flagged)
	}
	var types []string
	for _, ft := range spec.FieldTypes {
		if ft.Name == "enum" {
			types = append(types, `"enum" "(" <string> { "," <string> } ")"`)
			continue
		}
		types = append(types, `"`+ft.Name+`"`)
	}
	types = append(types, `"[]" <field-type>`)
	b.WriteString(production("field-type", joinAlternatives(types)))
	b.WriteString(production("doc-comment", `"///" <text>`))
	return b.String()
}

// deprecatedFieldTypes names the field types the table flags, with each one's
// replacement, for the note beside <field-type>. Empty when none is flagged,
// so the note appears only while there is something to say.
func deprecatedFieldTypes(spec *Spec) string {
	var out []string
	for _, ft := range spec.FieldTypes {
		if !ft.Deprecated {
			continue
		}
		out = append(out, fmt.Sprintf("`%s` is a deprecated spelling of %s", ft.Name, ft.ReplacedBy))
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, "; ")
}

// expressionProduction renders the frame of the expression grammar: what an
// expression bottoms out in. The operator ladder above it is
// operatorProduction, and the callable names are functionProduction.
func expressionProduction(spec *Spec) string {
	var b strings.Builder
	b.WriteString("\n(* ---- Expressions ---- *)\n")
	b.WriteString(production("expression", "<"+expressionLevelName(loosestLevel(functions.Operators()))+">"))
	b.WriteString(production("lambda-params", `<name> | "(" <name> "," <name> ")"`))
	b.WriteString(production("primary", joinAlternatives([]string{
		"<literal>", "<reserved-root>", "<construct-invocation>", "<call>", "<list>", "<object>",
		`"(" <expression> ")"`, "<name>",
	})))
	b.WriteString(production("reserved-root", joinAlternatives(reservedRoots(spec))))
	// Both literals admit a TRAILING comma (parseV1List, parseV1Map), which
	// the shipped tree writes and the shape without it could not derive.
	b.WriteString(production("list", `"[" [ <expression> { "," <expression> } [ "," ] ] "]"`))
	b.WriteString(production("object", `"{" [ <map-entry> { "," <map-entry> } [ "," ] ] "}"`))
	return b.String()
}

// reservedRoots are the reserved engine identifiers an expression may open on,
// from the keyword table.
func reservedRoots(spec *Spec) []string {
	var out []string
	for _, k := range spec.Keywords {
		if k.Kind == "reserved" {
			out = append(out, `"`+k.Name+`"`)
		}
	}
	sort.Strings(out)
	return out
}

// loosestLevel is the highest precedence level in the operator table -- the
// level the expression production enters the ladder at.
func loosestLevel(ops []functions.Operator) int {
	loosest := 0
	for _, op := range ops {
		if op.Level > loosest {
			loosest = op.Level
		}
	}
	return loosest
}

// expressionLevelName is the production name of one precedence level.
func expressionLevelName(level int) string {
	return fmt.Sprintf("expr-%d", level)
}

// operatorProduction renders the precedence ladder from the operator table's
// Level column, loosest first, so the generated grammar parses an expression
// into the same tree the parser does. A ladder that disagrees with
// parser.V1PrecedenceTable is the one defect a syntax-only grammar can still
// cause: the text parses, and it means something else.
//
// The SHAPE of each level comes from its operators' Kind, which is the node
// kind the operator builds -- lambda, ternary, the two unary kinds and the
// two member kinds each have their own shape; everything else is a
// left-associative binary rung.
func operatorProduction(ops []functions.Operator) string {
	byLevel := map[int][]functions.Operator{}
	var levels []int
	for _, op := range ops {
		if _, seen := byLevel[op.Level]; !seen {
			levels = append(levels, op.Level)
		}
		byLevel[op.Level] = append(byLevel[op.Level], op)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(levels)))

	var b strings.Builder
	b.WriteString("\n(* ---- The precedence ladder, loosest binding first.\n" +
		"   Level numbers are the operator table's; level 1 binds tightest. ---- *)\n")
	for i, level := range levels {
		tighter := "<primary>"
		if i+1 < len(levels) {
			tighter = "<" + expressionLevelName(levels[i+1]) + ">"
		}
		b.WriteString(production(expressionLevelName(level), levelRHS(byLevel[level], tighter)))
		// The lambda rung names <lambda>, which the filter and refine clauses
		// reference by name; it is defined here, beside the rung it is, rather
		// than a second time in the expression frame.
		for _, op := range byLevel[level] {
			if op.Kind == "lambda" {
				b.WriteString(production("lambda", `<lambda-params> "`+op.Symbol+`" `+tighter))
			}
		}
	}
	return b.String()
}

// levelRHS renders one rung of the ladder over the operators that sit on it.
func levelRHS(ops []functions.Operator, tighter string) string {
	var symbols []string
	kinds := map[string]bool{}
	for _, op := range ops {
		kinds[op.Kind] = true
		symbols = append(symbols, `"`+op.Symbol+`"`)
	}
	sort.Strings(symbols)
	switch {
	case kinds["lambda"]:
		return "<lambda> | " + tighter
	case kinds["ternary"]:
		// Right-associative: the two branches re-enter this level, so
		// `p ? a : q ? b : c` nests to the right as the parser reads it.
		own := "<" + expressionLevelName(ops[0].Level) + ">"
		return tighter + ` [ "?" ` + own + ` ":" ` + own + ` ]`
	case kinds["not"] || kinds["negate"]:
		return "( " + strings.Join(symbols, " | ") + " ) <" + expressionLevelName(ops[0].Level) + "> | " + tighter
	case kinds["member"] || kinds["optionalMember"]:
		// The two member operators do NOT take the same right-hand side, so
		// one alternative cannot spell both. `.` reads a field OR calls a
		// method; `.?` reads a field only -- v1MemberChain refuses a method
		// call after it by name, because `.?` is about an absent OBJECT and a
		// method on an absent object has no answer. The earlier shape,
		// `( "." | ".?" ) <name> | <method-call>`, was wrong in both
		// directions at once: `x.count()` did not derive (the alternation
		// left the method call with no dot in front of it) and `x count()`
		// did.
		return tighter + " { " + memberTails(ops) + " }"
	case len(symbols) == 1:
		return tighter + " { " + symbols[0] + " " + tighter + " }"
	default:
		return tighter + " { ( " + strings.Join(symbols, " | ") + " ) " + tighter + " }"
	}
}

// memberTails renders the member rung's tails, one per member operator, from
// the operator's Kind: the plain member reads a field or calls a method, the
// optional one reads a field.
func memberTails(ops []functions.Operator) string {
	var tails []string
	for _, op := range ops {
		switch op.Kind {
		case "member":
			tails = append(tails, `"`+op.Symbol+`" ( <name> | <method-call> )`)
		case "optionalMember":
			tails = append(tails, `"`+op.Symbol+`" <name>`)
		}
	}
	sort.Strings(tails)
	return strings.Join(tails, " | ")
}

// RetiredCall is one retired spelling written as a call: the name it is
// written with, the spelling the parser's table carries and the edition-2026
// form that replaces it.
type RetiredCall struct {
	Name        string
	Spelling    string
	Replacement string
}

// OneArgumentRetiredCalls names the retired call spellings `<predicate-name>
// "(" <expression> ")"` admits, read from the parser's own retired-form table
// rather than listed here: a spelling of the shape `name(x)` -- one argument,
// no comma -- has the same tokens as a spec or a trait applied to its
// receiver, so no syntax rule can tell them apart. The set is exported for the
// test that records the looseness, so the note in the grammar and the test
// cannot name different sets.
func OneArgumentRetiredCalls() []RetiredCall {
	var out []RetiredCall
	for _, f := range parser.V1RetiredForms() {
		name, arg, ok := splitCallSpelling(f.Spelling)
		if !ok || arg == "" || strings.Contains(arg, ",") {
			continue
		}
		out = append(out, RetiredCall{Name: name, Spelling: f.Spelling, Replacement: f.Replacement})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// splitCallSpelling reads a retired form written as a call, `name(args)`,
// into its name and its argument text. Anything else -- an operator, a
// keyword, a method with a leading dot -- is not a call and answers false.
func splitCallSpelling(spelling string) (name, args string, ok bool) {
	open := strings.IndexByte(spelling, '(')
	if open <= 0 || !strings.HasSuffix(spelling, ")") {
		return "", "", false
	}
	name = spelling[:open]
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return "", "", false
		}
	}
	return name, strings.TrimSpace(spelling[open+1 : len(spelling)-1]), true
}

// oneArgumentRetiredCallNote renders the comment that names them, so the
// published grammar says out loud what its predicate application admits.
func oneArgumentRetiredCallNote() string {
	calls := OneArgumentRetiredCalls()
	if len(calls) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("(* <predicate-name> is a spec or a trait applied to its receiver, so it is any\n" +
		"   name. It therefore also derives the retired ONE-ARGUMENT calls, which have\n" +
		"   the same tokens and which the PARSER refuses by name:\n")
	for _, c := range calls {
		fmt.Fprintf(&b, "     %-14s write %s\n", c.Spelling, c.Replacement)
	}
	b.WriteString("   Every other retired call takes a different number of arguments, and none of\n" +
		"   those derives here at all. *)\n")
	return b.String()
}

// functionProduction renders the callable names from the function catalog:
// the functions a call may name and the methods a value may answer to. A
// spelling the catalog retires has no entry, so it cannot be offered here.
func functionProduction(catalog []functions.Function) string {
	var fns, methods []string
	for _, f := range catalog {
		if f.Receiver == "" {
			fns = append(fns, `"`+f.Name+`"`)
			continue
		}
		methods = append(methods, `"`+f.Name+`"`)
	}
	sort.Strings(fns)
	sort.Strings(methods)
	methods = dedupe(methods)

	var b strings.Builder
	b.WriteString("\n(* ---- Calls ---- *)\n")
	// Two call forms, and the second is why the first stays closed. A catalog
	// function is called by name with its arguments. A spec or a trait is
	// APPLIED to its one receiver -- `isActiveRecord(row)`, `requiresOwner(
	// actor)` -- and its name is declared by a `.memql` file, so no table can
	// list it and no closed alternation can spell it.
	//
	// It is a ONE-argument form, which is the whole of the containment: every
	// retired call taking two or more arguments, and `now()` and `timestamp()`
	// taking none, still fail to derive. What it does admit is the retired
	// calls that take exactly one -- they are indistinguishable at the syntax
	// level from a predicate application, and BNF cannot say "any name except
	// these". That is a bounded, known looseness rather than a gap, so the
	// note below NAMES them from the parser's own retired-form table, and
	// TestPublishedGrammarAdmitsTheOneArgumentRetiredCalls records that they
	// derive. The parser refuses each by name with its replacement.
	b.WriteString(oneArgumentRetiredCallNote())
	b.WriteString(production("call", `<function-name> "(" [ <expression> { "," <expression> } ] ")"`+
		"\n"+strings.Repeat(" ", productionColumn)+`  | <predicate-name> "(" <expression> ")"`))
	b.WriteString(production("predicate-name", `<name>`))
	b.WriteString(production("method-call", `<method-name> "(" [ <expression> { "," <expression> } ] ")"`))
	b.WriteString(production("function-name", joinAlternatives(fns)))
	b.WriteString(production("method-name", joinAlternatives(methods)))
	return b.String()
}

// dedupe removes adjacent duplicates from a sorted slice: a method name
// declared on more than one receiver is one name in the grammar.
func dedupe(sorted []string) []string {
	out := sorted[:0]
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// literalProduction renders the terminals every other production bottoms out
// in. These are lexical, not grammatical: the lexer decides them, and the
// shapes below are what it accepts.
func literalProduction(spec *Spec) string {
	var b strings.Builder
	b.WriteString("\n(* ---- Terminals ---- *)\n")
	b.WriteString(production("literal", `<string> | <number> | "true" | "false" | "nil"`))
	// The hyphen is the lexer's (isIdentifierCharNoColon), not an oversight:
	// seed ids carry one by design (`seed skill go-backend-engineering`,
	// `seed capability cap-owner-read-principal`), and a <name> without it
	// cannot derive them. It may not OPEN a name, because the scanner reaches
	// an identifier on a letter or an underscore.
	b.WriteString(production("name", `( "a".."z" | "A".."Z" | "_" ) { "a".."z" | "A".."Z" | "0".."9" | "_" | "-" }`))
	b.WriteString(production("dotted-name", `<name> { "." <name> }`))
	b.WriteString(production("concept-name", `<name>`))
	b.WriteString(production("bound-name", `<name>`))
	b.WriteString(production("string", `'"' { <character> } '"'`))
	b.WriteString(production("number", `[ "-" ] <digit>+ [ "." <digit>+ ]`))
	b.WriteString(production("text", `{ <character> }`))
	b.WriteString(production("character", `any Unicode code point; "\\" escapes the next one`))
	b.WriteString(production("digit", `"0".."9"`))
	return b.String()
}
