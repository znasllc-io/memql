package compose

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/znasllc-io/memql/core/num"
)

// Draft is what the compose step produced and the render step turns into
// bytes. It is deliberately ONE SHAPE for every format: a title, a body
// in Markdown-ish source, and -- for the two data formats -- rows.
//
// THE FORMAT IS HOW A DRAFT IS WRITTEN OUT, NOT WHAT IT IS. That is the
// whole reason a person can change the Target column's format without
// re-composing: the reasoning step produced the content, and the four
// deterministic steps after it produced the file. A second draft shape
// per format would have made the format a compose-time decision and put
// a model call behind every change of mind.
type Draft struct {
	// Title is the document's heading. Empty is allowed and renders no
	// heading rather than an empty one.
	Title string
	// Body is Markdown source. It is the content for every format
	// except CSV and JSON, which read Rows instead.
	Body string
	// Rows is the tabular content for csv and json. Header is the
	// column order -- WITHOUT it a map has no order, and a CSV whose
	// columns move between runs is one no downstream tool can read.
	Header []string
	Rows   []map[string]any
}

// Result is what a render produced.
type Result struct {
	Bytes []byte
	// Embedded reports whether the provenance went INTO these bytes.
	// It is the format's answer, recorded per composition so the row
	// and the app never have to re-derive it.
	Embedded bool
	// Note is the sentence explaining Embedded, either way.
	Note string
}

// SHA256 is the lowercase hex digest of the produced bytes.
//
// A CONTENT DIGEST, NEVER A CREDENTIAL DIGEST, and the distinction is
// worth stating because a scanner cannot tell them apart. It answers
// "are these the same bytes" -- it is what makes a re-run's output
// comparable to the last one's, and it is copied onto
// `v1:library:file.sha256`, which the Library's own header calls a dedup
// hint and an integrity check and explicitly NOT an access key. Nothing
// secret is ever hashed here: the input is a document this cluster
// composed and is about to store. SHA-256 is the right function for
// that, and a password hash would be the wrong one.
func (r Result) SHA256() string {
	sum := sha256.Sum256(r.Bytes)
	return hex.EncodeToString(sum[:])
}

// Render writes a draft as the named format, stamping provenance where
// the format has somewhere to put it.
//
// RENDER AND STAMP ARE ONE CALL HERE AND TWO STEPS IN THE RUN, and that
// is not a contradiction: the run records them separately because
// stamping is where D4's "this format has no channel" is DECIDED, and a
// decision with no receipt of its own is one nobody can audit. Inside a
// single format writer they are inseparable -- a PDF's XMP packet is
// written as part of writing the PDF.
func Render(f Format, d Draft, p Provenance) (Result, error) {
	note := f.ProvenanceNote()
	var (
		out []byte
		err error
	)
	switch f {
	case FormatMarkdown:
		out, err = renderMarkdown(d, p)
	case FormatHTML:
		out, err = renderHTML(d, p)
	case FormatText:
		out, err = renderText(d)
	case FormatCSV:
		out, err = renderCSV(d)
	case FormatJSON:
		out, err = renderJSON(d)
	case FormatDOCX:
		out, err = renderDOCX(d, p)
	case FormatPDF:
		out, err = renderPDF(d, p)
	default:
		return Result{}, fmt.Errorf("compose: no writer for format %q", f)
	}
	if err != nil {
		return Result{}, err
	}
	return Result{Bytes: out, Embedded: f.CarriesProvenance(), Note: note}, nil
}

// ---- markdown: YAML front matter ----

// renderMarkdown writes the body under a YAML front-matter block.
//
// FRONT MATTER IS THE FORMAT'S OWN CONVENTION rather than something
// invented here, which is what makes it provenance a reader's tools
// already understand: a static-site generator, an editor's outline view
// and `head -20` all read it.
func renderMarkdown(d Draft, p Provenance) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("---\n")
	writeYAML(&b, "title", firstNonEmpty(p.Title, d.Title))
	writeYAML(&b, "statement", p.Statement)
	writeYAML(&b, "author", p.AuthorName)
	writeYAML(&b, "authorId", p.AuthorId)
	writeYAML(&b, "instance", p.Instance)
	writeYAML(&b, "compositionId", p.CompositionId)
	writeYAML(&b, "goalId", p.GoalId)
	writeYAML(&b, "template", p.TemplateName)
	writeYAML(&b, "sources", p.SourceSummary())
	writeYAML(&b, "models", p.ModelSummary())
	writeYAML(&b, "createdAt", p.CreatedAt.UTC().Format(time.RFC3339))
	writeYAML(&b, "producedBy", "MemQL Materializer")
	b.WriteString("---\n\n")
	if title := strings.TrimSpace(d.Title); title != "" {
		b.WriteString("# " + title + "\n\n")
	}
	b.WriteString(strings.TrimRight(d.Body, "\n"))
	b.WriteString("\n")
	return b.Bytes(), nil
}

// writeYAML emits one scalar key, skipping empty values.
//
// AN EMPTY KEY IS OMITTED RATHER THAN WRITTEN BLANK. `template: ""` in a
// front-matter block reads as "there was a template and it has no name";
// an absent key reads as "there was no template", which is true.
func writeYAML(b *bytes.Buffer, key, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	b.WriteString(key)
	b.WriteString(": ")
	// Always quoted, always escaped. A value containing a colon, a
	// leading `#`, or a line break is otherwise a YAML document that
	// parses as something else -- and every one of these values is
	// free text somebody typed.
	b.WriteString(quoteYAML(value))
	b.WriteString("\n")
}

// quoteYAML renders a value as a double-quoted YAML scalar.
//
// THE BACKSLASH IS ESCAPED FIRST AND THE ORDER IS THE WHOLE CORRECTNESS
// ARGUMENT. Escaping the quote first would turn `"` into `\"` and then
// the second pass would turn that backslash into `\\`, giving `\\"` --
// a closed scalar followed by junk. Every function of this shape is
// right or wrong on that one line.
//
// EVERY REMAINING C0 CONTROL IS DROPPED, and that half is not
// belt-and-braces. YAML 1.2 forbids unescaped C0 controls in a
// double-quoted scalar exactly as XML 1.0 forbids them in an element,
// and this quoter feeds `memql-package.yaml` -- a document ANOTHER
// component parses and validates. A composition's name is free text
// somebody typed, so one stray 0x0B would produce a package the
// Deployables pipeline refuses with a parse error naming nothing. It is
// the same defect docx.go's escapeXML fixes one format along, and it was
// found by reading this function rather than by a test failing.
func quoteYAML(v string) string {
	v = strings.ReplaceAll(v, "\\", "\\\\")
	v = strings.ReplaceAll(v, "\"", "\\\"")
	v = strings.ReplaceAll(v, "\n", "\\n")
	v = strings.ReplaceAll(v, "\t", "\\t")

	var b strings.Builder
	b.Grow(len(v) + 2)
	b.WriteByte('"')
	for _, r := range v {
		// \n and \t are already the two-character escapes above, so
		// anything still in the C0 range here arrived as a raw byte.
		if r < 0x20 || (r >= 0x7F && r <= 0x9F) {
			continue
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

// ---- html: a meta block ----

func renderHTML(d Draft, p Provenance) ([]byte, error) {
	var b bytes.Buffer
	title := firstNonEmpty(d.Title, p.Title)
	b.WriteString("<!doctype html>\n<html lang=\"en\">\n<head>\n")
	b.WriteString("<meta charset=\"utf-8\">\n")
	b.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n")
	b.WriteString("<title>" + html.EscapeString(title) + "</title>\n")
	writeMeta(&b, "memql:statement", p.Statement)
	writeMeta(&b, "author", p.AuthorName)
	writeMeta(&b, "memql:authorId", p.AuthorId)
	writeMeta(&b, "memql:instance", p.Instance)
	writeMeta(&b, "memql:compositionId", p.CompositionId)
	writeMeta(&b, "memql:goalId", p.GoalId)
	writeMeta(&b, "memql:template", p.TemplateName)
	writeMeta(&b, "memql:sources", p.SourceSummary())
	writeMeta(&b, "memql:models", p.ModelSummary())
	writeMeta(&b, "memql:createdAt", p.CreatedAt.UTC().Format(time.RFC3339))
	writeMeta(&b, "generator", "MemQL Materializer")
	b.WriteString("</head>\n<body>\n")
	if title != "" {
		b.WriteString("<h1>" + html.EscapeString(title) + "</h1>\n")
	}
	// The body is Markdown source and is emitted as escaped
	// paragraphs. THIS IS NOT A MARKDOWN RENDERER and does not pretend
	// to be one: a half-implemented one that handles bold and not
	// tables produces documents that are wrong in ways nobody predicts,
	// and the honest small thing is paragraphs plus escaping. A real
	// renderer is a dependency decision, and it is not this epic's.
	for _, para := range splitParagraphs(d.Body) {
		b.WriteString("<p>" + html.EscapeString(para) + "</p>\n")
	}
	b.WriteString("</body>\n</html>\n")
	return b.Bytes(), nil
}

func writeMeta(b *bytes.Buffer, name, content string) {
	if strings.TrimSpace(content) == "" {
		return
	}
	b.WriteString("<meta name=\"" + html.EscapeString(name) + "\" content=\"" + html.EscapeString(content) + "\">\n")
}

// ---- txt / csv / json: written clean (design D4) ----

// renderText writes the body and NOTHING ELSE.
//
// No header comment, no trailing provenance block. A person who asks for
// a text file gets the text; the record is where the provenance lives,
// and the app says so beside the composition.
func renderText(d Draft) ([]byte, error) {
	var b bytes.Buffer
	if title := strings.TrimSpace(d.Title); title != "" {
		b.WriteString(title + "\n\n")
	}
	b.WriteString(strings.TrimRight(d.Body, "\n"))
	b.WriteString("\n")
	return b.Bytes(), nil
}

// renderCSV writes the rows and nothing else.
//
// A `#`-prefixed provenance header is the obvious place to put it and is
// exactly what breaks the spreadsheet somebody opens this in -- so it is
// not there, and the app says why.
func renderCSV(d Draft) ([]byte, error) {
	header := d.Header
	if len(header) == 0 {
		header = headerFromRows(d.Rows)
	}
	if len(header) == 0 {
		return nil, fmt.Errorf("compose: a csv needs columns -- the draft carried neither a header nor any rows")
	}
	var b bytes.Buffer
	writeCSVRow(&b, header)
	for _, row := range d.Rows {
		cells := make([]string, 0, len(header))
		for _, col := range header {
			cells = append(cells, cellString(row[col]))
		}
		writeCSVRow(&b, cells)
	}
	return b.Bytes(), nil
}

// writeCSVRow emits one RFC 4180 record. Quoting is unconditional for
// any cell holding a comma, a quote, a newline or a carriage return --
// the four characters that otherwise silently change the column count.
func writeCSVRow(b *bytes.Buffer, cells []string) {
	for i, cell := range cells {
		if i > 0 {
			b.WriteByte(',')
		}
		if strings.ContainsAny(cell, ",\"\n\r") {
			b.WriteByte('"')
			b.WriteString(strings.ReplaceAll(cell, "\"", "\"\""))
			b.WriteByte('"')
			continue
		}
		b.WriteString(cell)
	}
	b.WriteString("\r\n")
}

// renderJSON writes the rows as a JSON array and nothing else.
//
// A wrapper object carrying `{provenance, rows}` is the obvious place
// and is exactly what changes the shape every program parsing this has
// to handle -- so the array is the document, and the record is the
// provenance.
func renderJSON(d Draft) ([]byte, error) {
	rows := d.Rows
	if rows == nil {
		rows = []map[string]any{}
	}
	out, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("compose: writing json: %w", err)
	}
	return append(out, '\n'), nil
}

func headerFromRows(rows []map[string]any) []string {
	seen := map[string]bool{}
	var header []string
	for _, row := range rows {
		for k := range row {
			if !seen[k] {
				seen[k] = true
				header = append(header, k)
			}
		}
	}
	// SORTED, because map iteration is randomised per run and a CSV
	// whose columns move between two runs of one recipe is one no
	// downstream tool can read. A caller who cares about order supplies
	// Header; this is the fallback and it is at least STABLE.
	sortStrings(header)
	return header
}

func cellString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		// narrowing: GUARDED -- the bound is core/num's `WholeInt64`
		// rather than an inline one (memql#4779).
		//
		// Integral floats render without a decimal tail: a JSON number
		// arrives as float64, and "4" is what somebody expects in a
		// spreadsheet cell rather than "4.000000". The obvious test for
		// that is `t == float64(int64(t))`, which is UNDEFINED outside
		// int64 -- the inner conversion is implementation-defined, so
		// the comparison asks whether an arbitrary value equals the
		// original.
		//
		// NONE OF THE THREE SATURATING ANSWERS IS RIGHT HERE, which is
		// why this is guarded rather than clamped. A cell is a VALUE
		// being written out verbatim, so a number too large for an
		// int64 must render as the number it is; saturating would put
		// 9223372036854775807 in a file somebody sends to a client,
		// which is this writer silently changing their data.
		if whole, ok := num.WholeInt64(t); ok {
			return fmt.Sprintf("%d", whole)
		}
		return fmt.Sprintf("%g", t)
	case int, int32, int64:
		return fmt.Sprintf("%d", t)
	case time.Time:
		return t.UTC().Format(time.RFC3339)
	}
	// An object or an array in a cell is rendered as compact JSON
	// rather than as Go's %v, which prints `map[a:1]` -- a shape
	// nothing can parse and nobody asked for.
	if encoded, err := json.Marshal(v); err == nil {
		return string(encoded)
	}
	return fmt.Sprintf("%v", v)
}

func splitParagraphs(body string) []string {
	var out []string
	for _, block := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n\n") {
		if trimmed := strings.TrimSpace(block); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
