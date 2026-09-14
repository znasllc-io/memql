package dslspec

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/language/annotations"
)

// matrix.go renders docs/public/language/attribute-matrix.md from the
// annotation registry (memql#5360). The page answers two questions -- "can I
// write @X here, and how?" and "what does @X do?" -- and every fact on it comes
// from component/language/annotations: the placements, their argument forms,
// keys, examples and docs, the retirements and the refusal codes. What is
// written here is only the page's frame: its headings, the family grouping of
// the constructs, a short word for each argument form, and the sentences that
// say how to read the tables. No annotation is described here.
//
// The root package's TestAttributeMatrixIsGenerated holds the committed page to
// this renderer byte for byte, and TestAttributeMatrixListsWhatTheParserAccepts
// holds it to the registry, so the page can neither go stale nor list an
// annotation the parser refuses. `make docs-matrix` writes it.

// AttributeMatrixPath is where the generated attribute matrix is committed,
// relative to the repository root.
const AttributeMatrixPath = "docs/public/language/attribute-matrix.md"

// matrixColumn is one construct or kind of field: a column in its family's
// table and a section under "By construct". The concept's holds two
// receivers -- the annotations written before a concept, and @relationship,
// written inside its body.
type matrixColumn []annotations.Receiver

// matrixFamily is one table under "At a glance".
type matrixFamily struct {
	title   string
	columns []matrixColumn
}

// matrixFamilies is the page's grouping of the receivers, in page order. Every
// receiver sits in exactly one column (TestMatrixFamiliesCoverEveryReceiver),
// so a receiver the registry gains cannot be left off the page silently.
var matrixFamilies = []matrixFamily{
	{title: "Functions", columns: []matrixColumn{
		{annotations.Query}, {annotations.Mutation}, {annotations.Logic}, {annotations.Automation},
	}},
	{title: "Data", columns: []matrixColumn{
		{annotations.Concept, annotations.ConceptBody}, {annotations.Shape}, {annotations.Spec}, {annotations.Seed},
	}},
	{title: "Model access", columns: []matrixColumn{
		{annotations.Prompt}, {annotations.Provider}, {annotations.Policy}, {annotations.Rule}, {annotations.Tool},
	}},
	{title: "Capabilities", columns: []matrixColumn{
		{annotations.Builtin}, {annotations.Action}, {annotations.Capability},
	}},
	{title: "Fields", columns: []matrixColumn{
		{annotations.ConceptField}, {annotations.ArgsField}, {annotations.ToolField}, {annotations.PromptField}, {annotations.BuiltinField},
	}},
}

// formWords is the short word a matrix cell uses for each argument form, in
// the order a cell lists them. The long words (a refusal's, and the "Written
// as" column's) are the registry's Form.String.
var formWords = []struct {
	form annotations.Form
	word string
}{
	{annotations.FormFlag, "flag"},
	{annotations.FormEmpty, "empty"},
	{annotations.FormString, "string"},
	{annotations.FormStrings, "strings"},
	{annotations.FormNumber, "number"},
	{annotations.FormKeywords, "keywords"},
	{annotations.FormObject, "object"},
	{annotations.FormExpression, "expression"},
	{annotations.FormExclude, "exclusion"},
	{annotations.FormBool, "bool"},
}

// ReceiverName is the name the attribute matrix gives a receiver: the
// construct keyword for a construct receiver ("query", "mutate"), both
// keywords where two constructs share one ("spec and trait"), "concept" for
// the concept body, and the kind of field for a field receiver ("args field").
func ReceiverName(r annotations.Receiver) string {
	if r == annotations.ConceptBody {
		r = annotations.Concept
	}
	var keywords []string
	for _, c := range constructs() {
		if c.AnnotationReceiver == string(r) {
			keywords = append(keywords, c.Keyword)
		}
	}
	if len(keywords) > 0 {
		return joinWords(keywords, "and")
	}
	phrase := r.Phrase()
	return strings.TrimPrefix(strings.TrimPrefix(phrase, "an "), "a ")
}

// AttributeMatrix renders the attribute matrix page. It is pure and
// deterministic: the same registry always renders the same bytes.
func AttributeMatrix() string {
	// Two passes over one writer: the first learns the anchor of every
	// heading in page order -- GitHub suffixes a repeated heading's anchor
	// with -1, and `### policy` (the construct) comes before `### @policy`
	// (the rule's annotation) -- and the second writes the page with every
	// link resolved.
	w := &matrixWriter{anchors: map[string]string{}}
	w.page()
	w.b.Reset()
	w.slugs = slugger{}
	w.resolved = true
	w.page()
	return strings.TrimRight(w.b.String(), "\n") + "\n"
}

// matrixWriter writes the page.
type matrixWriter struct {
	b        strings.Builder
	slugs    slugger
	anchors  map[string]string // heading key -> anchor
	resolved bool              // second pass: every link must resolve
}

// Heading keys, for the headings a link points at.
const (
	keyAtAGlance   = "at a glance"
	keyAnnotations = "annotations"
	keyRefusals    = "refusals"
)

func sectionKey(r annotations.Receiver) string {
	if r == annotations.ConceptBody {
		r = annotations.Concept
	}
	return "section:" + string(r)
}

func entryKey(name string) string { return "entry:" + name }

func (w *matrixWriter) page() {
	w.b.WriteString("---\n" +
		"title: MemQL Attribute Matrix\n" +
		"audience: public\n" +
		"status: stable\n" +
		"area: language\n" +
		"sinceVersion: 0.9.0\n" +
		"owner: znas\n" +
		"---\n\n" +
		"<!-- GENERATED by `make docs-matrix` (cmd/attributematrix) from the annotation registry in " +
		"component/language/annotations. DO NOT EDIT: change the registry and run `make docs-matrix`. -->\n\n")
	w.heading(1, "MemQL attribute matrix", "")
	w.para("This page lists every annotation the language accepts, the constructs and fields that accept it, and the " +
		"form its arguments take there. It is generated from the annotation registry in " +
		"`component/language/annotations`, which the construct parsers and the concept loader check every annotation " +
		"against, and `make docs-matrix` regenerates it.")
	w.howToRead()
	w.atAGlance()
	w.byConstruct()
	w.entries()
	w.retired()
	w.refusals()
}

func (w *matrixWriter) howToRead() {
	w.heading(2, "How to read it", "")
	w.para("Under " + w.link("At a glance", keyAtAGlance) + ", a row is an annotation and a column is a construct or a " +
		"kind of field: a filled cell says how the annotation is written there, and an empty cell means it is refused " +
		"there, with one of the codes under " + w.link("Refusals", keyRefusals) + ". " + formLegend() + " Each " +
		"annotation links to its entry under " + w.link("Annotations", keyAnnotations) + ", which shows it written on " +
		"every construct that accepts it, lists its keys and says what it does.")
}

func (w *matrixWriter) atAGlance() {
	w.heading(2, "At a glance", keyAtAGlance)
	w.para("One table per family of constructs, and one for the fields a construct declares.")
	for _, fam := range matrixFamilies {
		w.heading(3, fam.title, "")
		header := []string{"Annotation"}
		for _, col := range fam.columns {
			header = append(header, ReceiverName(col[0]))
		}
		var rows [][]string
		for _, name := range familyNames(fam) {
			row := []string{w.link(code("@"+name), entryKey(name))}
			for _, col := range fam.columns {
				row = append(row, glanceCell(col, name))
			}
			rows = append(rows, row)
		}
		w.table(header, rows)
	}
}

func (w *matrixWriter) byConstruct() {
	w.heading(2, "By construct", "")
	w.para("Every construct and every kind of field, with each annotation it accepts, how the annotation is written " +
		"there, and an example.")
	for _, fam := range matrixFamilies {
		for _, col := range fam.columns {
			w.heading(3, ReceiverName(col[0]), sectionKey(col[0]))
			if lead := fieldLead(col[0]); lead != "" {
				w.para(lead)
			}
			var rows [][]string
			for _, p := range columnPlacements(col) {
				rows = append(rows, []string{w.link(code("@"+p.Name), entryKey(p.Name)), writtenAs(p), code(p.Example)})
			}
			w.table([]string{"Annotation", "Written as", "Example"}, rows)
			if pointer := w.fieldPointer(col[0]); pointer != "" {
				w.para(pointer)
			}
		}
	}
}

func (w *matrixWriter) entries() {
	w.heading(2, "Annotations", keyAnnotations)
	w.para("One entry per annotation, in alphabetical order: where it is accepted and how it is written there, its " +
		"keys, and what it does. An annotation marked repeatable may be written more than once on one declaration; any " +
		"other is refused the second time with " + code(annotations.CodeRepeated) + ".")
	for _, name := range acceptedNames() {
		w.heading(3, "@"+name, entryKey(name))
		placements := namePlacements(name)

		// Where it is accepted, and how it is written there: one row per
		// distinct way of writing it, naming every construct that shares it.
		var rows [][]string
		for _, g := range groupPlacements(placements, func(p annotations.Placement) string {
			return fmt.Sprintf("%d|%t|%t|%s", p.Forms, p.Repeatable, p.Receiver == annotations.ConceptBody, p.Example)
		}) {
			links := make([]string, 0, len(g))
			for _, p := range g {
				links = append(links, w.receiverLink(p.Receiver))
			}
			rows = append(rows, []string{strings.Join(links, ", "), writtenAs(g[0]), code(g[0].Example)})
		}
		w.table([]string{"On", "Written as", "Example"}, rows)

		// Its keys, per distinct key set.
		keyed := make([]annotations.Placement, 0, len(placements))
		for _, p := range placements {
			if len(p.Keys) > 0 {
				keyed = append(keyed, p)
			}
		}
		for _, g := range groupPlacements(keyed, keySignature) {
			w.para(keysLead(name, g, len(g) == len(placements)))
			var keyRows [][]string
			for _, k := range g[0].Keys {
				keyRows = append(keyRows, []string{code(k.Name), k.Type, k.Doc})
			}
			w.table([]string{"Key", "Type", "Meaning"}, keyRows)
		}

		// What it does: the one doc, or each receiver's where they differ.
		docGroups := groupPlacements(placements, placementDoc)
		if len(docGroups) == 1 {
			w.para(placementDoc(docGroups[0][0]))
			continue
		}
		var list strings.Builder
		for _, g := range docGroups {
			phrases := make([]string, 0, len(g))
			for _, p := range g {
				phrases = append(phrases, p.Receiver.Phrase())
			}
			item := registryText("On "+joinWords(phrases, "or")+": "+placementDoc(g[0]), false)
			list.WriteString("- " + indentContinuation(item) + "\n")
		}
		w.b.WriteString(list.String() + "\n")
	}
}

func (w *matrixWriter) retired() {
	w.heading(2, "Retired", "")
	w.para("A retired name is refused where the table says, with " + code(annotations.CodeRetired) +
		" and a hint that says what to write instead.")
	retirements := annotations.Retirements()
	// Stable, so the entries of one name keep the registry's order:
	// everywhere first, then by receiver.
	sort.SliceStable(retirements, func(i, j int) bool { return lessName(retirements[i].Name, retirements[j].Name) })
	var rows [][]string
	for _, ret := range retirements {
		rows = append(rows, []string{w.retiredName(ret), w.retiredWhere(ret), sentence(ret.Hint)})
	}
	w.table([]string{"Annotation", "Where", "Write instead"}, rows)
}

func (w *matrixWriter) refusals() {
	w.heading(2, "Refusals", keyRefusals)
	w.para("Every refusal names the construct and the annotation, says what to write instead, and ends with its code " +
		"in square brackets, as in " + code("["+annotations.CodeUnknown+"]") + ".")
	var rows [][]string
	for _, c := range annotations.RefusalCodes() {
		rows = append(rows, []string{code(c.Code), c.Meaning})
	}
	w.table([]string{"Code", "Meaning"}, rows)
}

// heading writes a heading and records its anchor under key.
func (w *matrixWriter) heading(level int, text, key string) {
	anchor := w.slugs.slug(text)
	if key != "" {
		w.anchors[key] = anchor
	}
	w.b.WriteString(strings.Repeat("#", level) + " " + text + "\n\n")
}

// link renders an in-page link to the heading recorded under key.
func (w *matrixWriter) link(text, key string) string {
	anchor, ok := w.anchors[key]
	if !ok && w.resolved {
		panic(fmt.Sprintf("dslspec: the attribute matrix links to %q, which no heading carries", key))
	}
	return "[" + text + "](#" + anchor + ")"
}

// receiverLink links a receiver's name to its section under "By construct".
func (w *matrixWriter) receiverLink(r annotations.Receiver) string {
	return w.link(ReceiverName(r), sectionKey(r))
}

// para writes one block of registry or page text.
func (w *matrixWriter) para(text string) {
	w.b.WriteString(registryText(text, false) + "\n\n")
}

// table writes a GFM table. Every cell is escaped, so a "|" in a doc or an
// example cannot split a row.
func (w *matrixWriter) table(header []string, rows [][]string) {
	line := func(cells []string) {
		escaped := make([]string, len(cells))
		for i, c := range cells {
			escaped[i] = registryText(c, true)
		}
		w.b.WriteString("| " + strings.Join(escaped, " | ") + " |\n")
	}
	line(header)
	w.b.WriteString("|" + strings.Repeat("---|", len(header)) + "\n")
	for _, row := range rows {
		line(row)
	}
	w.b.WriteString("\n")
}

// fieldLead says, for a field receiver, where its fields are written: in the
// body of a concept, tool, prompt or builtin, or in the args block of the
// constructs that declare one.
func fieldLead(r annotations.Receiver) string {
	var body, args []string
	for _, c := range constructs() {
		if fieldReceiverFor(c) != r {
			continue
		}
		if BodyFieldReceiver(c.Keyword) == r {
			body = append(body, code(c.Keyword))
		} else {
			args = append(args, code(c.Keyword))
		}
	}
	var where []string
	if len(body) > 0 {
		where = append(where, "in the body of a "+joinWords(body, "or"))
	}
	if len(args) > 0 {
		where = append(where, "in the `args` block of a "+joinWords(args, "or"))
	}
	if len(where) == 0 {
		return ""
	}
	return "Written after a field's type " + strings.Join(where, ", and ") + "."
}

// fieldPointer points a construct's section at the section of its fields.
func (w *matrixWriter) fieldPointer(r annotations.Receiver) string {
	for _, c := range constructs() {
		if c.AnnotationReceiver != string(r) {
			continue
		}
		fields := fieldReceiverFor(c)
		if fields == "" {
			continue
		}
		which := "The fields of its `args` block"
		if BodyFieldReceiver(c.Keyword) == fields {
			which = "The fields in its body"
		}
		return which + " take the annotations under " + w.receiverLink(fields) + "."
	}
	return ""
}

// keysLead introduces one keys table.
func keysLead(name string, g []annotations.Placement, everywhere bool) string {
	lead := code("@"+name) + " takes these keys"
	if !everywhere {
		phrases := make([]string, 0, len(g))
		for _, p := range g {
			phrases = append(phrases, p.Receiver.Phrase())
		}
		lead += " on " + joinWords(phrases, "or")
	}
	for _, k := range g[0].Keys {
		if k.Type == "flag" {
			return lead + ". A key of type `flag` is written bare, and every other as `key=value`."
		}
	}
	return lead + ", each written as `key=value`."
}

// retiredName renders a retirement's name: the family rule, or the name,
// linked to its entry when another receiver accepts it.
func (w *matrixWriter) retiredName(ret annotations.Retirement) string {
	if ret.Prefix {
		return code("@"+ret.Name+"X") + ", for any `X` that starts with an upper-case letter"
	}
	if len(namePlacements(ret.Name)) > 0 {
		return w.link(code("@"+ret.Name), entryKey(ret.Name))
	}
	return code("@" + ret.Name)
}

// retiredWhere renders where a retirement applies: one receiver, or every
// receiver but the ones that accept the name.
func (w *matrixWriter) retiredWhere(ret annotations.Retirement) string {
	if ret.Receiver != "" {
		return w.receiverLink(ret.Receiver)
	}
	var live []string
	if !ret.Prefix {
		for _, p := range namePlacements(ret.Name) {
			live = append(live, w.receiverLink(p.Receiver))
		}
	}
	if len(live) == 0 {
		return "everywhere"
	}
	return "everywhere except " + joinWords(live, "and")
}

// pageReceivers is every receiver in page order: family by family, column by
// column.
func pageReceivers() []annotations.Receiver {
	var out []annotations.Receiver
	for _, fam := range matrixFamilies {
		for _, col := range fam.columns {
			out = append(out, col...)
		}
	}
	return out
}

// namePlacements returns every placement of name, in page order.
func namePlacements(name string) []annotations.Placement {
	var out []annotations.Placement
	for _, r := range pageReceivers() {
		if p, ok := annotations.Lookup(r, name); ok {
			out = append(out, p)
		}
	}
	return out
}

// acceptedNames returns every name some receiver accepts, alphabetically.
func acceptedNames() []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range annotations.Placements() {
		if !seen[p.Name] {
			seen[p.Name] = true
			out = append(out, p.Name)
		}
	}
	sort.Slice(out, func(i, j int) bool { return lessName(out[i], out[j]) })
	return out
}

// familyNames returns the names any column of the family accepts,
// alphabetically.
func familyNames(fam matrixFamily) []string {
	seen := map[string]bool{}
	var out []string
	for _, col := range fam.columns {
		for _, p := range columnPlacements(col) {
			if !seen[p.Name] {
				seen[p.Name] = true
				out = append(out, p.Name)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return lessName(out[i], out[j]) })
	return out
}

// columnPlacements returns every placement of the column's receivers,
// alphabetically.
func columnPlacements(col matrixColumn) []annotations.Placement {
	in := map[annotations.Receiver]bool{}
	for _, r := range col {
		in[r] = true
	}
	var out []annotations.Placement
	for _, p := range annotations.Placements() {
		if in[p.Receiver] {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return lessName(out[i].Name, out[j].Name) })
	return out
}

// glanceCell is one matrix cell: how the column writes the name, or empty.
func glanceCell(col matrixColumn, name string) string {
	var parts []string
	for _, r := range col {
		if p, ok := annotations.Lookup(r, name); ok {
			word := shortForm(p.Forms)
			if r == annotations.ConceptBody {
				word += ", in the body"
			}
			parts = append(parts, word)
		}
	}
	return strings.Join(parts, "; ")
}

// writtenAs is the "Written as" cell: the registry's words for the forms, and
// whether the annotation repeats or sits inside the concept's body.
func writtenAs(p annotations.Placement) string {
	s := p.Forms.String()
	if p.Repeatable {
		s += ", repeatable"
	}
	if p.Receiver == annotations.ConceptBody {
		s += ", in the body"
	}
	return s
}

// formTerm is one argument form as the page names it: the short word a matrix
// cell uses, and the registry's long words for it.
type formTerm struct{ word, meaning string }

// formTerms splits a form set into the terms a cell lists, in formWords order.
// One string or a list of them is the single term "strings" ("one or more
// strings"), the way Form.String reads the pair; the collapse is made here and
// nowhere else. A form the registry gained and formWords did not is printed in
// the registry's words rather than dropped, so its cell never reads as empty.
func formTerms(f annotations.Form) []formTerm {
	both := annotations.FormString | annotations.FormStrings
	var out []formTerm
	rest := f
	for _, fw := range formWords {
		if rest&fw.form == 0 {
			continue
		}
		switch {
		case fw.form == annotations.FormString && rest&both == both:
			continue // read below, with FormStrings, as one term
		case fw.form == annotations.FormStrings && rest&both == both:
			out = append(out, formTerm{fw.word, both.String()})
			rest &^= both
			continue
		}
		out = append(out, formTerm{fw.word, fw.form.String()})
		rest &^= fw.form
	}
	if rest != 0 {
		out = append(out, formTerm{rest.String(), rest.String()})
	}
	return out
}

// shortForm is the forms in the cell's short words: "number or keywords".
func shortForm(f annotations.Form) string {
	terms := formTerms(f)
	words := make([]string, 0, len(terms))
	for _, t := range terms {
		words = append(words, t.word)
	}
	return joinWords(words, "or")
}

// formLegend is the sentence that says what each cell word means, in the
// registry's own words, for the forms the registry uses.
func formLegend() string {
	long := map[string]string{}
	for _, p := range annotations.Placements() {
		for _, t := range formTerms(p.Forms) {
			if _, ok := long[t.word]; !ok {
				long[t.word] = t.meaning
			}
		}
	}
	var parts []string
	for _, fw := range formWords {
		if meaning, ok := long[fw.word]; ok {
			if len(parts) == 0 {
				parts = append(parts, code(fw.word)+" means "+meaning)
			} else {
				parts = append(parts, code(fw.word)+" "+meaning)
			}
		}
	}
	return "The words in a cell are argument forms: " + joinWords(parts, "and") + "."
}

// placementDoc is the doc a placement shows: its own, or the name's.
func placementDoc(p annotations.Placement) string {
	if p.Doc != "" {
		return p.Doc
	}
	return annotations.Docs[p.Name]
}

// keySignature identifies a key set, so placements sharing one share a table.
func keySignature(p annotations.Placement) string {
	var b strings.Builder
	for _, k := range p.Keys {
		b.WriteString(k.Name + "\x00" + k.Type + "\x00" + k.Doc + "\x00")
	}
	return b.String()
}

// groupPlacements groups placements by key, in the order each key first
// appears.
func groupPlacements(ps []annotations.Placement, key func(annotations.Placement) string) [][]annotations.Placement {
	index := map[string]int{}
	var out [][]annotations.Placement
	for _, p := range ps {
		k := key(p)
		i, ok := index[k]
		if !ok {
			i = len(out)
			index[k] = i
			out = append(out, nil)
		}
		out[i] = append(out[i], p)
	}
	return out
}

// lessName orders names alphabetically regardless of case, as a reader
// expects (@maximum before @maxLength), with the byte order breaking a tie.
func lessName(a, b string) bool {
	la, lb := strings.ToLower(a), strings.ToLower(b)
	if la != lb {
		return la < lb
	}
	return a < b
}

// joinWords joins with commas and a final conjunction: "a, b or c".
func joinWords(words []string, conjunction string) string {
	switch len(words) {
	case 0:
		return ""
	case 1:
		return words[0]
	}
	return strings.Join(words[:len(words)-1], ", ") + " " + conjunction + " " + words[len(words)-1]
}

// sentence starts a hint with a capital and ends it with a full stop.
func sentence(s string) string {
	if s == "" {
		return s
	}
	if s[0] >= 'a' && s[0] <= 'z' {
		s = strings.ToUpper(s[:1]) + s[1:]
	}
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}

// code renders s as a code span.
func code(s string) string {
	if strings.Contains(s, "`") {
		return "`` " + s + " ``"
	}
	return "`" + s + "`"
}

// registryText renders registry text as markdown. The registry's docs are
// markdown already (emphasis, code spans, paragraphs and lists pass through);
// three things are changed, outside code spans only:
//
//   - a docs/public page named by its repository path becomes a relative link,
//     since a path is useless to a reader of the published page;
//   - a "<" is written "&lt;", so a placeholder such as <field> prints instead
//     of being read as an HTML tag and dropped;
//   - in a table cell a line break becomes a space.
//
// In a table cell every "|" is also written "\|", inside a code span too, as
// GFM requires.
func registryText(s string, inCell bool) string {
	var b strings.Builder
	codeSegments(s, func(segment string, code bool) {
		if !code {
			segment = linkDocPages(segment)
			segment = strings.ReplaceAll(segment, "<", "&lt;")
			if inCell {
				segment = strings.ReplaceAll(segment, "\n", " ")
			}
		}
		if inCell {
			segment = strings.ReplaceAll(segment, "|", `\|`)
		}
		b.WriteString(segment)
	})
	return b.String()
}

// codeSegments walks s as a markdown renderer reads it: visit gets each code
// span (code=true, backticks included) and each stretch of text between them,
// in order. A backtick run opens a span only when a later run of the same
// length closes it; an unmatched run is literal text.
func codeSegments(s string, visit func(segment string, code bool)) {
	start := 0 // where the pending text segment began
	for i := 0; i < len(s); {
		if s[i] != '`' {
			i++
			continue
		}
		n := runLength(s, i)
		end := closingRun(s, i+n, n)
		if end < 0 {
			i += n
			continue
		}
		if start < i {
			visit(s[start:i], false)
		}
		visit(s[i:end+n], true)
		i = end + n
		start = i
	}
	if start < len(s) {
		visit(s[start:], false)
	}
}

// docPagePath matches a docs/public page named by its repository path, with an
// optional anchor. It ends at ".md" (or the anchor), so a sentence's full stop
// after the path stays outside the link.
var docPagePath = regexp.MustCompile(`docs/public/[A-Za-z0-9_./-]*\.md(?:#[A-Za-z0-9_-]+)?`)

// linkDocPages turns every docs/public path in a text segment into a link
// relative to the attribute matrix, named by the page's file name. A path that
// is already a link target -- right after "(" -- is left alone.
func linkDocPages(text string) string {
	var b strings.Builder
	last := 0
	for _, m := range docPagePath.FindAllStringIndex(text, -1) {
		if m[0] > 0 && strings.ContainsRune("(/[", rune(text[m[0]-1])) {
			continue
		}
		target := text[m[0]:m[1]]
		page, anchor, _ := strings.Cut(target, "#")
		if anchor != "" {
			anchor = "#" + anchor
		}
		b.WriteString(text[last:m[0]])
		b.WriteString("[" + path.Base(page) + "](" + relativeToMatrix(page) + anchor + ")")
		last = m[1]
	}
	b.WriteString(text[last:])
	return b.String()
}

// relativeToMatrix is page's path relative to the attribute matrix's
// directory: "../operate/auth/per-row-authz-audit.md".
func relativeToMatrix(page string) string {
	from := strings.Split(path.Dir(AttributeMatrixPath), "/")
	to := strings.Split(page, "/")
	common := 0
	for common < len(from) && common < len(to)-1 && from[common] == to[common] {
		common++
	}
	var parts []string
	for range from[common:] {
		parts = append(parts, "..")
	}
	return strings.Join(append(parts, to[common:]...), "/")
}

// indentContinuation indents every line of a list item after its first by the
// item's content column, so paragraphs and nested lists stay inside the item.
// A blank line stays empty: whitespace on it would be invisible noise in the
// page and a trailing-whitespace error in its diff.
func indentContinuation(item string) string {
	lines := strings.Split(item, "\n")
	for i := 1; i < len(lines); i++ {
		if lines[i] != "" {
			lines[i] = "  " + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}

// runLength counts the backticks in the run starting at i.
func runLength(s string, i int) int {
	n := 0
	for i+n < len(s) && s[i+n] == '`' {
		n++
	}
	return n
}

// closingRun returns where the next run of exactly n backticks at or after i
// starts, or -1.
func closingRun(s string, i, n int) int {
	for i < len(s) {
		if s[i] != '`' {
			i++
			continue
		}
		m := runLength(s, i)
		if m == n {
			return i
		}
		i += m
	}
	return -1
}

// slugger computes a heading's anchor the way GitHub does (github-slugger):
// lower-case, drop everything but letters, digits, spaces, hyphens and
// underscores, turn spaces into hyphens, and suffix a repeat with -1, -2, ...
// The page's headings are ASCII, so the ASCII rule is the whole rule.
type slugger struct {
	seen map[string]int
}

func (s *slugger) slug(heading string) string {
	if s.seen == nil {
		s.seen = map[string]int{}
	}
	var b strings.Builder
	for _, r := range strings.ToLower(heading) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	base := b.String()
	out := base
	for {
		if _, taken := s.seen[out]; !taken {
			break
		}
		s.seen[base]++
		out = fmt.Sprintf("%s-%d", base, s.seen[base])
	}
	s.seen[out] = 0
	return out
}
