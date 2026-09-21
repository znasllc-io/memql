package dslspec

// The generated vocabulary (memql#5388, D24 of the DSL v1 freeze record).
//
// The grammar says what may be WRITTEN. This says what each written thing
// MEANS, which is the half a grammar structurally cannot carry: `<expr-5>` is
// where `??` binds, and nothing in a production says that `??` falls through
// on a blank string as well as on an absent one.
//
// It is the second artifact a model is given, and it is generated for the same
// reason the grammar is: a hand-maintained glossary of a language that has
// moved three times in two months is a glossary that is confidently wrong, and
// a confidently wrong description is worse than none -- the model believes it.
//
// # Where every description comes from
//
// Nothing here is prose. Each entry's description is the DOC the table that
// owns the name already carries: Construct.Doc, annotations.Docs (or the
// receiver-specific Placement.Doc), Keyword.Doc, functions.Operator.Doc plus
// its Absence rule, functions.Function.Doc, FieldType.Doc, Builtin.Doc. Where
// a table's doc was too short to teach anything, the fix was to lengthen it IN
// THE TABLE -- TestVocabularyDescriptionsAreSentences is what found them, and
// the editor's hover card reads the same field, so every one of those fixes
// landed in Sense at the same time.
//
// # What it deliberately does NOT carry
//
// A per-name "since" version. No table in this tree records when a construct
// or an annotation was introduced, so the field would be empty on every entry
// or filled by hand -- and a hand-filled column on a generated page is the
// thing this task removes. The page names the edition and the grammar version
// once, which is the version fact the tree actually holds.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/language/annotations"
	"github.com/znasllc-io/memql/component/language/functions"
	"github.com/znasllc-io/memql/component/language/parser"
)

// VocabularyPath is where the generated vocabulary page is committed, relative
// to the repository root.
const VocabularyPath = "docs/public/language/vocabulary.md"

// The kinds a vocabulary entry can be. They are the sections of the page and
// the values the memqlVocabulary builtin's `kind` argument narrows on.
const (
	VocabularyConstruct  = "construct"
	VocabularyAnnotation = "annotation"
	VocabularyKeyword    = "keyword"
	VocabularyOperator   = "operator"
	VocabularyFieldType  = "fieldType"
	VocabularyFunction   = "function"
	VocabularyBuiltin    = "builtin"
)

// VocabularyKinds returns the kinds in page order. It is the closed set the
// builtin validates its `kind` argument against, so a caller that asks for a
// kind that does not exist is told which do.
func VocabularyKinds() []string {
	return []string{
		VocabularyConstruct, VocabularyAnnotation, VocabularyKeyword,
		VocabularyOperator, VocabularyFieldType, VocabularyFunction,
		VocabularyBuiltin,
	}
}

// VocabularyEntry is one named thing in the language: what kind of thing it
// is, how it is written, and what it means.
type VocabularyEntry struct {
	// Kind is one of the Vocabulary* constants above.
	Kind string `json:"kind"`
	// Name is the one spelling: `query`, `@cache`, `list.any`, `??`'s name
	// `coalesce`. An annotation carries its leading `@`, so a reader never has
	// to know which kind they are looking at to know what to type.
	Name string `json:"name"`
	// Signature is how the thing is WRITTEN -- the construct's declaration
	// form, the annotation's canonical example, the function's call signature,
	// the operator's form. A vocabulary of bare names is a list a model cannot
	// act on.
	Signature string `json:"signature"`
	// Description is the owning table's doc. For an operator it is the doc
	// followed by the absence rule, because "what does this do with a missing
	// field" is the question the record spends a whole table answering and the
	// one an author gets wrong.
	Description string `json:"description"`
	// Tier is a catalog function's tier: "P" when the call lowers to SQL, "M"
	// when it runs in process. Empty for every other kind.
	Tier string `json:"tier,omitempty"`
}

// Vocabulary returns every named thing in the language, in page order: by
// kind, and within a kind by name. The result is a fresh slice.
func Vocabulary() []VocabularyEntry {
	spec := Build()
	var out []VocabularyEntry
	out = append(out, constructVocabulary(spec)...)
	out = append(out, annotationVocabulary()...)
	out = append(out, keywordVocabulary(spec)...)
	out = append(out, operatorVocabulary()...)
	out = append(out, fieldTypeVocabulary(spec)...)
	out = append(out, functionVocabulary()...)
	out = append(out, builtinVocabulary(spec)...)
	return out
}

// VocabularyOf returns the entries of one kind, or every entry when kind is
// empty. The second result is false when kind names no kind at all, so a
// caller can tell "no entries of this kind" from "there is no such kind".
func VocabularyOf(kind string) ([]VocabularyEntry, bool) {
	if kind == "" {
		return Vocabulary(), true
	}
	known := false
	for _, k := range VocabularyKinds() {
		if k == kind {
			known = true
		}
	}
	if !known {
		return nil, false
	}
	var out []VocabularyEntry
	for _, e := range Vocabulary() {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out, true
}

// constructVocabulary projects the construct table. The signature is the
// declaration as the generated grammar spells it, so the two pages cannot
// describe different declarations.
func constructVocabulary(spec *Spec) []VocabularyEntry {
	var out []VocabularyEntry
	for _, c := range spec.Constructs {
		signature := c.Keyword + " " + c.Signature
		switch c.BodyForm {
		case BodyFormLambda:
			signature += " = <lambda>"
		case BodyFormImport:
			// The signature already carries the whole statement.
		default:
			signature += " { ... }"
		}
		out = append(out, VocabularyEntry{
			Kind:        VocabularyConstruct,
			Name:        c.Keyword,
			Signature:   strings.Join(strings.Fields(signature), " "),
			Description: c.Doc,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// annotationVocabulary projects the annotation registry, one entry per NAME.
// A name placed on more than one receiver is one word an author types, so it
// is one entry; the receivers it is legal on are the grammar's and the
// attribute matrix's subject, and the entry says how many there are rather
// than restating the matrix.
func annotationVocabulary() []VocabularyEntry {
	byName := map[string]annotations.Placement{}
	receivers := map[string][]string{}
	for _, p := range annotations.Placements() {
		if _, seen := byName[p.Name]; !seen {
			byName[p.Name] = p
		}
		receivers[p.Name] = append(receivers[p.Name], ReceiverName(p.Receiver))
	}
	var out []VocabularyEntry
	for name, p := range byName {
		doc := annotations.Docs[name]
		if p.Doc != "" {
			doc = p.Doc
		}
		out = append(out, VocabularyEntry{
			Kind:        VocabularyAnnotation,
			Name:        "@" + name,
			Signature:   p.Example,
			Description: doc + " Written as " + p.Forms.String() + "; legal on " + joinReceiverNames(receivers[name]) + ".",
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// joinReceiverNames renders a receiver list for an annotation entry, deduped
// and capped: an annotation legal on sixteen receivers is "every construct",
// and listing them is a table the attribute matrix already draws.
func joinReceiverNames(names []string) string {
	seen := map[string]bool{}
	var uniq []string
	for _, n := range names {
		if seen[n] {
			continue
		}
		seen[n] = true
		uniq = append(uniq, n)
	}
	sort.Strings(uniq)
	if len(uniq) > 6 {
		return fmt.Sprintf("%d receivers (see the attribute matrix)", len(uniq))
	}
	switch len(uniq) {
	case 0:
		return "no receiver"
	case 1:
		return uniq[0]
	}
	return strings.Join(uniq[:len(uniq)-1], ", ") + " and " + uniq[len(uniq)-1]
}

// keywordVocabulary projects the keyword table -- the control words, the body
// clauses and the reserved engine identifiers. A keyword that heads a
// production signs itself with that production; one that does not (a reserved
// root, a continuation) signs itself with its own word.
func keywordVocabulary(spec *Spec) []VocabularyEntry {
	var out []VocabularyEntry
	at := map[string]int{}
	for _, k := range spec.Keywords {
		signature := k.Grammar
		if strings.TrimSpace(signature) == "" {
			signature = k.Name
		}
		description := k.Doc
		if len(k.Properties) > 0 {
			var members []string
			for _, p := range k.Properties {
				members = append(members, k.Name+"."+p.Name)
			}
			description += " Members: " + strings.Join(members, ", ") + "."
		}
		// One word, one entry. `args` is both a body clause and a reserved
		// root, and an author types the same four letters for both; two
		// entries under one heading would make the page answer its own
		// question twice and differently.
		if i, seen := at[k.Name]; seen {
			out[i].Signature += "  /  " + signature
			out[i].Description += " " + description
			continue
		}
		at[k.Name] = len(out)
		out = append(out, VocabularyEntry{
			Kind:        VocabularyKeyword,
			Name:        k.Name,
			Signature:   signature,
			Description: description,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// operatorVocabulary projects the operator table. The description is the doc
// followed by the absence rule, which is the half an author gets wrong: the
// record spends a table on what each operator does with a missing field, and
// an entry that omits it teaches the easy half only.
func operatorVocabulary() []VocabularyEntry {
	var out []VocabularyEntry
	for _, op := range functions.Operators() {
		description := op.Doc
		if op.Absence != "" {
			description += " Absence: " + op.Absence
		}
		out = append(out, VocabularyEntry{
			Kind:        VocabularyOperator,
			Name:        op.Name,
			Signature:   op.Form,
			Description: description + fmt.Sprintf(" Written `%s`, precedence level %d.", op.Symbol, op.Level),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// fieldTypeVocabulary projects the field-type table. A deprecated spelling
// stays listed and SAYS SO with its replacement: a reader who meets `array` in
// an old file needs to be told what it is and what to write instead.
//
// The GRAMMAR lists it too, and cannot say that -- a syntax rule has nowhere
// to put "deprecated", and a grammar that left the spelling out would refuse
// the 43 files that write it. That is the division of labour between the two
// generated pages: the grammar says what derives, the vocabulary says what to
// write.
func fieldTypeVocabulary(spec *Spec) []VocabularyEntry {
	var out []VocabularyEntry
	for _, ft := range spec.FieldTypes {
		description := ft.Doc
		signature := "<name> " + ft.Name
		if ft.Deprecated {
			description += " DEPRECATED: write " + ft.ReplacedBy + " instead."
		}
		out = append(out, VocabularyEntry{
			Kind:        VocabularyFieldType,
			Name:        ft.Name,
			Signature:   signature,
			Description: description,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// functionVocabulary projects the function catalog, keyed by the one spelling
// a call uses: the bare name for a function, receiver.name for a method.
func functionVocabulary() []VocabularyEntry {
	var out []VocabularyEntry
	for _, f := range functions.Catalog() {
		out = append(out, VocabularyEntry{
			Kind:        VocabularyFunction,
			Name:        f.Key(),
			Signature:   f.Signature(),
			Description: f.Doc,
			Tier:        string(f.Tier),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// builtinVocabulary projects the builtins the function catalog does NOT
// describe: the parser's context accessors and the runtime-registry builtins.
// A catalog function is already a function entry, and listing it twice would
// make the page disagree with itself about which spelling is the one.
func builtinVocabulary(spec *Spec) []VocabularyEntry {
	var out []VocabularyEntry
	for _, b := range spec.Builtins {
		if b.Category == CategoryBuiltinExpr {
			continue
		}
		out = append(out, VocabularyEntry{
			Kind:        VocabularyBuiltin,
			Name:        b.Name,
			Signature:   b.Signature,
			Description: b.Doc,
			Tier:        b.Tier,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// vocabularyKindHeadings is the page's heading and lead sentence for each
// kind: the page's FRAME, the way the attribute matrix's headings are. No name
// is described here.
var vocabularyKindHeadings = map[string][2]string{
	VocabularyConstruct: {"Constructs", "The words a top-level declaration opens with. One section of the [grammar](grammar.md) per entry."},
	VocabularyAnnotation: {"Annotations", "The `@name` directives a construct or a field carries. " +
		"WHERE each is legal, and in which argument form, is the [attribute matrix](attribute-matrix.md)."},
	VocabularyKeyword:   {"Keywords", "The reserved words that are not constructs: the statements of a body, the clauses of a construct, and the engine identifiers a body may read."},
	VocabularyOperator:  {"Operators", "Every operator of the expression grammar, with what it does to an ABSENT operand -- the half an author gets wrong -- and where it binds."},
	VocabularyFieldType: {"Field types", "The type words a concept field, an args field or a schema-body field is declared with."},
	VocabularyFunction:  {"Functions and methods", "Every call an expression may make, one spelling each. `P` lowers to SQL; `M` runs in process, and in a pushdown position only on an input that does not read the row."},
	VocabularyBuiltin:   {"Builtins", "The context accessors the parser recognises and the builtins resolved from the integration registry -- the callables the function catalog does not describe."},
}

// RenderVocabulary renders the committed vocabulary page.
func RenderVocabulary() string {
	var b strings.Builder
	b.WriteString(generatedFrontMatter("MemQL Vocabulary", "docs-grammar"))
	b.WriteString("# MemQL vocabulary\n\n")
	b.WriteString("Every named thing in the MemQL language -- construct, annotation, keyword, operator, field type, function and builtin -- with how it is written and what it means. It is generated from the tables that own those names, so a description here cannot disagree with the engine that reads them.\n\n")
	b.WriteString("It is the companion to the [grammar](grammar.md): the grammar says what may be written, this says what each written thing does. Together they are what a model is given before it is asked to write MemQL.\n\n")
	fmt.Fprintf(&b, "Edition `%s`, grammar version `%s`.\n\n", parser.Edition, parser.GrammarVersion)

	entries := Vocabulary()
	byKind := map[string][]VocabularyEntry{}
	for _, e := range entries {
		byKind[e.Kind] = append(byKind[e.Kind], e)
	}

	b.WriteString("| Section | Entries |\n|---|---:|\n")
	for _, kind := range VocabularyKinds() {
		heading := vocabularyKindHeadings[kind][0]
		fmt.Fprintf(&b, "| [%s](#%s) | %d |\n", heading, anchorFor(heading), len(byKind[kind]))
	}
	fmt.Fprintf(&b, "| **Total** | **%d** |\n\n", len(entries))

	for _, kind := range VocabularyKinds() {
		heading := vocabularyKindHeadings[kind]
		fmt.Fprintf(&b, "## %s\n\n%s\n\n", heading[0], heading[1])
		if kind == VocabularyFunction {
			b.WriteString("| Name | Written | Tier | What it means |\n|---|---|---|---|\n")
			for _, e := range byKind[kind] {
				fmt.Fprintf(&b, "| `%s` | `%s` | %s | %s |\n",
					e.Name, e.Signature, e.Tier, tableCell(e.Description))
			}
			b.WriteString("\n")
			continue
		}
		b.WriteString("| Name | Written | What it means |\n|---|---|---|\n")
		for _, e := range byKind[kind] {
			fmt.Fprintf(&b, "| `%s` | `%s` | %s |\n", e.Name, e.Signature, tableCell(e.Description))
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// tableCell escapes what GFM splits a row on, and flattens a doc's newlines:
// a pipe inside a description would silently add a column, and a newline would
// end the row.
func tableCell(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return strings.ReplaceAll(s, "|", "\\|")
}

// anchorFor is GitHub's heading anchor: lowercased, spaces to hyphens, and
// everything else dropped.
func anchorFor(heading string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(heading) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	return b.String()
}
