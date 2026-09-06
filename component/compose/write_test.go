package compose

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func fixtureProvenance() Provenance {
	return Provenance{
		Title:         "Q3 report",
		Statement:     "Draft the Q3 report for Acme from the open invoices",
		AuthorName:    "Jo Rivera",
		AuthorId:      "u-7f2a",
		Instance:      "memql.example",
		CompositionId: "c-91bc",
		GoalId:        "g-44de",
		TemplateName:  "Acme quarterly",
		Sources: []Source{
			{Kind: "concept_row", Ref: "inv-1", Label: "INV-1001", CapturedAt: time.Unix(0, 0).UTC()},
			{Kind: "concept_row", Ref: "inv-2", Label: "INV-1002", CapturedAt: time.Unix(0, 0).UTC()},
			{Kind: "library_file", Ref: "f-3", Label: "notes.md", CapturedAt: time.Unix(0, 0).UTC()},
		},
		Models: []ModelContribution{
			{Provider: "anthropic", Model: "claude-sonnet-5", Calls: 1, Tokens: 4200},
		},
		CreatedAt: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC),
	}
}

func fixtureDraft() Draft {
	return Draft{
		Title:  "Q3 report",
		Body:   "Revenue rose 14%.\n\nThree invoices remain open.",
		Header: []string{"number", "total"},
		Rows: []map[string]any{
			{"number": "INV-1001", "total": float64(1200)},
			{"number": "INV-1002", "total": float64(980.5)},
		},
	}
}

// TestEveryFormatAnswersProvenance is the epic's headline claim as a
// property of a function over values: every format either embeds the
// provenance record in its own bytes or reports that it cannot, with a
// sentence naming the format and the reason. There is no third outcome
// and no silent omission.
//
// IT RUNS OVER Formats() RATHER THAN A LIST WRITTEN HERE, so a format
// added to the table without an answer fails this test rather than
// shipping. That is the reachable positive the empty-offender-list rule
// asks for: the test is about the table, not about seven names somebody
// remembered to type.
func TestEveryFormatAnswersProvenance(t *testing.T) {
	p := fixtureProvenance()
	d := fixtureDraft()
	for _, f := range Formats() {
		t.Run(string(f), func(t *testing.T) {
			res, err := Render(f, d, p)
			if err != nil {
				t.Fatalf("Render(%s): %v", f, err)
			}
			if len(res.Bytes) == 0 {
				t.Fatalf("Render(%s) produced no bytes", f)
			}
			if res.Embedded != f.CarriesProvenance() {
				t.Fatalf("Render(%s).Embedded = %v, format says %v", f, res.Embedded, f.CarriesProvenance())
			}
			if strings.TrimSpace(res.Note) == "" {
				t.Fatalf("Render(%s) carries no provenance note -- an unexplained flag reads as a failure", f)
			}
			if !res.Embedded && !strings.Contains(strings.ToUpper(res.Note), strings.ToUpper(string(f))) {
				t.Fatalf("Render(%s) note %q does not name the format; a reader cannot tell which file this is about", f, res.Note)
			}
			if res.SHA256() == "" {
				t.Fatalf("Render(%s) produced no digest", f)
			}
		})
	}
}

// TestFormatsWithNoChannelAreWrittenClean is design D4's other half, and
// the one a helpful future change would break: txt, csv and json must
// carry NOTHING of the provenance in their bytes. A "#"-prefixed CSV
// header and a {provenance, rows} JSON wrapper are the two obvious
// places to put it and are exactly what breaks the spreadsheet and the
// parser downstream.
func TestFormatsWithNoChannelAreWrittenClean(t *testing.T) {
	p := fixtureProvenance()
	d := fixtureDraft()
	// Every distinctive string from the provenance record. If any of
	// them appears in the bytes, something leaked.
	leaks := []string{p.CompositionId, p.GoalId, p.AuthorId, p.Instance, p.TemplateName, "MemQL", "claude-sonnet-5"}

	for _, f := range []Format{FormatText, FormatCSV, FormatJSON} {
		t.Run(string(f), func(t *testing.T) {
			res, err := Render(f, d, p)
			if err != nil {
				t.Fatalf("Render(%s): %v", f, err)
			}
			body := string(res.Bytes)
			for _, leak := range leaks {
				if strings.Contains(body, leak) {
					t.Fatalf("Render(%s) leaked %q into the bytes -- D4 says this format is written clean and the record is its only provenance", f, leak)
				}
			}
		})
	}

	// THE REACHABLE POSITIVE. Without it, the loop above would pass
	// against a Render that returned empty bytes for everything, and an
	// empty offender list would be a statement about the test rather
	// than about the writers.
	res, err := Render(FormatMarkdown, d, p)
	if err != nil {
		t.Fatalf("Render(markdown): %v", err)
	}
	for _, want := range leaks {
		if !strings.Contains(string(res.Bytes), want) {
			t.Fatalf("markdown, the control, does NOT carry %q -- the clean-format assertions above prove nothing", want)
		}
	}
}

func TestMarkdownFrontMatterIsParseableAndQuoted(t *testing.T) {
	p := fixtureProvenance()
	p.Statement = `He said: "ship it" # now` + "\nand then left"
	res, err := Render(FormatMarkdown, fixtureDraft(), p)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	body := string(res.Bytes)
	if !strings.HasPrefix(body, "---\n") {
		t.Fatalf("front matter does not open the file:\n%s", body[:min(120, len(body))])
	}
	// The statement holds a colon, a hash and a newline -- the three
	// characters that turn an unquoted YAML scalar into a different
	// document. All three must survive escaped, on ONE line.
	if !strings.Contains(body, `statement: "He said: \"ship it\" # now\nand then left"`) {
		t.Fatalf("statement was not quoted and escaped as one scalar:\n%s", body)
	}
	if strings.Count(body, "\n---\n") != 1 {
		t.Fatalf("expected exactly one closing front-matter fence:\n%s", body)
	}
}

func TestCSVQuotesTheFourCharactersThatChangeTheColumnCount(t *testing.T) {
	d := Draft{
		Header: []string{"note", "n"},
		Rows: []map[string]any{
			{"note": `a,b`, "n": float64(1)},
			{"note": `say "hi"`, "n": float64(2)},
			{"note": "two\nlines", "n": float64(3)},
		},
	}
	res, err := Render(FormatCSV, d, fixtureProvenance())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	body := string(res.Bytes)
	for _, want := range []string{`"a,b",1`, `"say ""hi""",2`, "\"two\nlines\",3"} {
		if !strings.Contains(body, want) {
			t.Fatalf("csv did not quote correctly; want %q in:\n%s", want, body)
		}
	}
	// An integral float must not render a decimal tail -- a JSON number
	// arrives as float64, and "1.000000" in a spreadsheet cell is not
	// what anybody asked for.
	if strings.Contains(body, "1.000000") {
		t.Fatalf("integral float rendered with a decimal tail:\n%s", body)
	}
}

func TestCSVHeaderFallbackIsStable(t *testing.T) {
	// No Header supplied: the column order must be derived and STABLE,
	// because Go randomises map iteration and a CSV whose columns move
	// between two runs of one recipe is one no downstream tool reads.
	d := Draft{Rows: []map[string]any{{"zeta": 1, "alpha": 2, "mid": 3}}}
	var first string
	for i := range 12 {
		res, err := Render(FormatCSV, d, fixtureProvenance())
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		line := strings.SplitN(string(res.Bytes), "\r\n", 2)[0]
		if i == 0 {
			first = line
			continue
		}
		if line != first {
			t.Fatalf("header order moved between runs: %q then %q", first, line)
		}
	}
	if first != "alpha,mid,zeta" {
		t.Fatalf("derived header = %q, want the sorted order", first)
	}
}

func TestJSONIsTheRowsAndNothingElse(t *testing.T) {
	res, err := Render(FormatJSON, fixtureDraft(), fixtureProvenance())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// It must decode as an ARRAY. A {provenance, rows} wrapper is the
	// obvious place to put provenance and is exactly what changes the
	// shape every program parsing this has to handle.
	var rows []map[string]any
	if err := json.Unmarshal(res.Bytes, &rows); err != nil {
		t.Fatalf("json output is not a bare array: %v\n%s", err, res.Bytes)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
}

func TestJSONWithNoRowsIsAnEmptyArrayNotNull(t *testing.T) {
	res, err := Render(FormatJSON, Draft{}, fixtureProvenance())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := strings.TrimSpace(string(res.Bytes)); got != "[]" {
		// `null` decodes to a nil slice in every consumer and reads as
		// "the read failed" rather than "there was nothing".
		t.Fatalf("empty json = %q, want []", got)
	}
}

func TestDocxIsAZipCarryingItsSixParts(t *testing.T) {
	res, err := Render(FormatDOCX, fixtureDraft(), fixtureProvenance())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(res.Bytes), int64(len(res.Bytes)))
	if err != nil {
		t.Fatalf("docx is not a readable zip: %v", err)
	}
	parts := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("opening %s: %v", f.Name, err)
		}
		body, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("reading %s: %v", f.Name, err)
		}
		parts[f.Name] = string(body)
	}
	for _, want := range []string{
		"[Content_Types].xml", "_rels/.rels", "word/document.xml",
		"docProps/core.xml", "docProps/app.xml", "docProps/custom.xml",
	} {
		if _, ok := parts[want]; !ok {
			t.Fatalf("docx is missing %s; Word refuses a package without it", want)
		}
	}
	// The provenance must be in the two parts a reader actually
	// surfaces: core.xml is the File -> Properties dialog and
	// custom.xml is where the traceable ids live.
	if !strings.Contains(parts["docProps/core.xml"], "Jo Rivera") {
		t.Fatalf("core.xml carries no author:\n%s", parts["docProps/core.xml"])
	}
	if !strings.Contains(parts["docProps/custom.xml"], "c-91bc") {
		t.Fatalf("custom.xml carries no composition id:\n%s", parts["docProps/custom.xml"])
	}
	if !strings.Contains(parts["word/document.xml"], "Revenue rose 14%") {
		t.Fatalf("document.xml carries no body:\n%s", parts["word/document.xml"])
	}
}

// TestDocxSurvivesAControlByteInFreeText pins the failure mode that
// produces a file Word refuses to open with a message naming nothing.
// Every provenance value is free text -- a goal statement, a row's own
// field -- and XML 1.0 forbids a lone 0x0B outright: escaping does not
// help, because `&#11;` is equally invalid.
func TestDocxSurvivesAControlByteInFreeText(t *testing.T) {
	p := fixtureProvenance()
	p.Statement = "before\x0bafter"
	d := fixtureDraft()
	d.Body = "body\x07with a bell"

	res, err := Render(FormatDOCX, d, p)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(res.Bytes), int64(len(res.Bytes)))
	if err != nil {
		t.Fatalf("docx is not a readable zip: %v", err)
	}
	// THE PARTS ARE DEFLATED, so every assertion here reads the
	// DECOMPRESSED body. Scanning the zip's raw bytes finds neither the
	// control character nor the sentence around it, and would pass
	// against a writer that emitted both.
	var all strings.Builder
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("opening %s: %v", f.Name, err)
		}
		body, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("reading %s: %v", f.Name, err)
		}
		for _, b := range body {
			if b < 0x20 && b != '\t' && b != '\n' && b != '\r' {
				t.Fatalf("%s carries raw control byte %#x -- Word refuses the package", f.Name, b)
			}
		}
		if bytes.Contains(body, []byte("&#11;")) || bytes.Contains(body, []byte("&#7;")) {
			t.Fatalf("%s escaped a forbidden control character instead of stripping it; &#11; is equally invalid in XML 1.0", f.Name)
		}
		all.Write(body)
	}
	// And the surrounding TEXT must survive: the byte is stripped, the
	// sentence is not. Without this half, a writer that dropped every
	// value carrying a control character would pass the loop above.
	for _, want := range []string{"beforeafter", "bodywith a bell"} {
		if !strings.Contains(all.String(), want) {
			t.Fatalf("stripping the control byte took the surrounding text with it; %q is not in the package", want)
		}
	}
}

func TestPDFIsAPDFAndCarriesBothChannels(t *testing.T) {
	res, err := Render(FormatPDF, fixtureDraft(), fixtureProvenance())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !bytes.HasPrefix(res.Bytes, []byte("%PDF-")) {
		t.Fatalf("output does not begin with the PDF magic: %q", res.Bytes[:min(16, len(res.Bytes))])
	}
	body := string(res.Bytes)
	// The XMP packet is the machine-readable channel.
	if !strings.Contains(body, "<?xpacket begin=") {
		t.Fatal("pdf carries no XMP packet -- a machine reading provenance has nothing to read")
	}
	if !strings.Contains(body, "c-91bc") {
		t.Fatal("pdf's XMP carries no composition id, so a file in a downloads folder traces back to nothing")
	}
	// modelsUsed must be present even though it could be summarised
	// away: on a catalog hit it reads "no model", which is the claim.
	if !strings.Contains(body, "modelsUsed") {
		t.Fatal("pdf's XMP omits modelsUsed")
	}
	// The Info dictionary is the person-readable channel.
	if !strings.Contains(body, "/Producer") && !strings.Contains(body, "/Creator") {
		t.Fatal("pdf carries no Info dictionary entries, so Document Properties shows nothing")
	}
}

// TestPDFFoldsTypographyRatherThanDroppingIt pins the silent failure the
// core PDF fonts produce: they are single-byte encoded, so a curly quote
// or an em dash renders as nothing at all -- and composed prose is made
// of exactly those characters.
func TestPDFFoldsTypographyRatherThanDroppingIt(t *testing.T) {
	got := tr("It’s a “quote” — and an ellipsis…")
	want := `It's a "quote" -- and an ellipsis...`
	if got != want {
		t.Fatalf("tr() = %q, want %q", got, want)
	}
	// An unfoldable character becomes a visible '?' rather than
	// vanishing: a missing character somebody can SEE is a bug report,
	// one that silently disappears is a document that is quietly wrong.
	if got := tr("emoji \U0001F600 here"); !strings.Contains(got, "?") {
		t.Fatalf("tr() dropped an out-of-range rune silently: %q", got)
	}
}

// TestHTMLEscapesEveryContextItWritesInto pins what the html/template
// refactor bought (epic memql#4977). Every value in this page is free text
// somebody typed -- a title, a goal statement, a person's name -- and each
// lands in a DIFFERENT context: an attribute value, an element body, a
// <title>. The concatenated version escaped them all identically and was
// safe by care; the template escapes them by where they land.
func TestHTMLEscapesEveryContextItWritesInto(t *testing.T) {
	p := fixtureProvenance()
	p.Title = `A "quoted" title`
	p.Statement = `He said "ship it" & <b>meant</b> it`
	p.AuthorName = `O'Brien`
	d := Draft{Title: p.Title, Body: `A <script>alert(1)</script> paragraph.`}

	res, err := Render(FormatHTML, d, p)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := string(res.Bytes)

	// NOTHING ESCAPES ITS CONTEXT. A raw `<script>` in the body or an
	// unescaped quote closing an attribute are the two failures this
	// whole refactor is about.
	for _, leak := range []string{"<script>", `content="He said "ship`, `<title>A "quoted"`} {
		if strings.Contains(out, leak) {
			t.Fatalf("%q reached the output unescaped:\n%s", leak, out)
		}
	}

	// THE REACHABLE POSITIVE: the values are PRESENT, escaped, rather
	// than dropped. A renderer that emitted nothing would pass the
	// assertions above.
	for _, want := range []string{"&lt;script&gt;", "&#34;quoted&#34;", "meant", "MemQL Materializer"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in the escaped output:\n%s", want, out)
		}
	}

	// And an absent value is an ABSENT TAG rather than an empty one, for
	// the front matter's reason.
	p.TemplateName = ""
	res2, err := Render(FormatHTML, d, p)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(string(res2.Bytes), `name="memql:template"`) {
		t.Fatalf("an empty template name wrote a blank meta tag:\n%s", res2.Bytes)
	}
}

func TestParseFormatRefusesRatherThanDefaulting(t *testing.T) {
	if _, err := ParseFormat("markdown"); err != nil {
		t.Fatalf("markdown must parse: %v", err)
	}
	if _, err := ParseFormat("nonsense"); err == nil {
		t.Fatal("an unknown format must be an error -- defaulting to markdown answers a request for something else with a text file and calls it success")
	}
	// audio and video are NAMED in the brief and unoffered, so their
	// refusal says which it is rather than "unknown format".
	for _, name := range []string{"audio", "video"} {
		_, err := ParseFormat(name)
		if err == nil {
			t.Fatalf("%s must be refused", name)
		}
		if !strings.Contains(err.Error(), "not offered") {
			t.Fatalf("%s refusal reads as an unknown-format error, not as a deliberate absence: %v", name, err)
		}
	}
}

func TestModelSummaryStatesTheCatalogHitRatherThanBlanking(t *testing.T) {
	p := fixtureProvenance()
	p.Models = nil
	got := p.ModelSummary()
	if !strings.Contains(got, "no model") {
		t.Fatalf("an empty model list rendered %q; a re-run that reached no provider is the product's headline claim, not a missing value", got)
	}
}

func TestSourceSummaryPluralisesAndSorts(t *testing.T) {
	p := fixtureProvenance()
	if got, want := p.SourceSummary(), "2 rows, 1 file"; got != want {
		t.Fatalf("SourceSummary() = %q, want %q", got, want)
	}
}
