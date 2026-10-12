package compose

import (
	"encoding/json"
	"math"
	"testing"

	pure "github.com/znasllc-io/memql/component/compose"
)

// pickedFile is a library_file source that resolved to the file row it names. The name
// and the size on that row are everything the Library's per-file rule reads, and the
// row carries the size the way the engine hands it back (a decoded JSON number).
func pickedFile(name string, size int64) Resolved {
	ref := "f-" + name
	return Resolved{
		Ref:   SourceRef{Kind: KindLibraryFile, Ref: ref, Label: name},
		Rows:  []map[string]any{{"id": ref, "name": name, "size": float64(size)}},
		Count: 1,
	}
}

// The Library takes a review attachment by its name and its size before it looks at its
// bytes (integrations/library/review_attachments.go): ".md" up to 32 KiB, or a PNG, JPEG,
// GIF or WebP picture up to 8 MiB, and never an empty one. This is compose's copy of that
// per-file rule, and TestComposeMirrorsTheReviewAttachmentRules (integrations/library)
// holds its constants to the Library's own.
func TestAttachableByLibraryIsTheLibrarysPerFileRule(t *testing.T) {
	for _, tc := range []struct {
		name  string
		size  int64
		sized bool
		want  bool
	}{
		// Taken.
		{"notes.md", 1, true, true},
		{"notes.md", 32768, true, true},
		{"NOTES.MD", 1200, true, true},
		{"draft.v2.md", 10, true, true},
		{"notes.md", 0, false, true}, // the row says no size: the name decides
		{"logo.png", 8388608, true, true},
		{"LOGO.PNG", 10, true, true},
		{"photo.jpg", 10, true, true}, // ".jpg" is read as ".jpeg"
		{"photo.jpeg", 10, true, true},
		{"anim.gif", 10, true, true},
		{"anim.G\u0130F", 10, true, true}, // a Turkish locale's capital dotted I is "i" to strings.ToLower, as the Library reads it
		{"pic.webp", 10, true, true},
		{"dir/notes.md", 10, true, true}, // the extension of the last element
		// Not taken: a size the Library refuses.
		{"notes.md", 32769, true, false},
		{"notes.md", 0, true, false}, // a file of no bytes is not attachable
		{"notes.md", -1, true, false},
		{"logo.png", 8388609, true, false},
		{"logo.png", 0, true, false},
		// Not taken: a name the Library refuses, at a size it would take.
		{"notes.markdown", 10, true, false}, // only ".md" is Markdown
		{"notes.txt", 10, true, false},
		{"numbers.csv", 10, true, false},
		{"data.json", 10, true, false},
		{"page.html", 10, true, false},
		{"site.zip", 10, true, false},
		{"vector.svg", 10, true, false}, // an SVG is text, not one of the four picture formats
		{"spec.pdf", 10, true, false},
		{"scan.tiff", 10, true, false},
		{"README", 10, true, false},
		{"bundle.png.zip", 10, true, false}, // the last extension counts
		{"notes.md.txt", 10, true, false},
		{"notes.", 10, true, false},
		{"", 10, true, false},
		{"spec.pdf", 0, false, false}, // no size does not rescue a name the Library refuses
	} {
		if got := attachableByLibrary(tc.name, tc.size, tc.sized); got != tc.want {
			t.Errorf("attachableByLibrary(%q, %d, sized=%v) = %v, want %v", tc.name, tc.size, tc.sized, got, tc.want)
		}
	}
}

// A file row's size is a number the engine decoded: float64 from JSON, a Go integer from
// a fake, json.Number from a decoder that kept the digits. Anything else is no size.
func TestRowSizeReadsAJSONNumber(t *testing.T) {
	for name, tc := range map[string]struct {
		row   map[string]any
		size  int64
		sized bool
	}{
		"a decoded JSON number":          {map[string]any{"size": float64(2048)}, 2048, true},
		"an int":                         {map[string]any{"size": 7}, 7, true},
		"an int64":                       {map[string]any{"size": int64(9)}, 9, true},
		"a json.Number":                  {map[string]any{"size": json.Number("12")}, 12, true},
		"a json.Number in exponent form": {map[string]any{"size": json.Number("1e3")}, 1000, true},
		"zero is a size":                 {map[string]any{"size": float64(0)}, 0, true},
		"a negative size":                {map[string]any{"size": float64(-4)}, -4, true},
		// A file's size is an int, so a fraction does not occur. It is read all the same,
		// rounded up, so that "more than 0 and at most the limit" gives the answer for it
		// that the SDK's `size > 0 && size <= limit` gives.
		"a fraction of a byte rounds up":      {map[string]any{"size": 0.25}, 1, true},
		"a fraction past the limit rounds up": {map[string]any{"size": 32768.5}, 32769, true},
		"a size that is too large to hold":    {map[string]any{"size": 1e30}, math.MaxInt64, true},
		"no row at all":                       {nil, 0, false},
		"no size":                             {map[string]any{"name": "notes.md"}, 0, false},
		"a null size":                         {map[string]any{"size": nil}, 0, false},
		"a string":                            {map[string]any{"size": "2048"}, 0, false},
		"a boolean":                           {map[string]any{"size": true}, 0, false},
		"a bad json.Number":                   {map[string]any{"size": json.Number("x")}, 0, false},
		"not a number":                        {map[string]any{"size": math.NaN()}, 0, false},
		"infinity":                            {map[string]any{"size": math.Inf(1)}, 0, false},
	} {
		if size, sized := rowSize(tc.row); size != tc.size || sized != tc.sized {
			t.Errorf("%s: rowSize = %d, sized=%v; want %d, sized=%v", name, size, sized, tc.size, tc.sized)
		}
	}
}

// Whether a described request takes the Library's revision workflow is decided on the files
// it picked, and decided the same way on every one of them: the workflow can read only what
// the Library attaches, so one file it cannot attach sends the whole request to the composer
// (which reads every picked thing), and a file nobody can read is not one it can attach.
func TestDescribedMarkdownNeedsEveryPickedFileToBeOneTheLibraryAttaches(t *testing.T) {
	archived := pickedFile("old-notes.md", 100)
	archived.Rows[0]["archived"] = true
	notArchived := pickedFile("live-notes.md", 100)
	notArchived.Rows[0]["archived"] = false
	unsized := pickedFile("notes.md", 100)
	delete(unsized.Rows[0], "size")
	unreadable := Resolved{
		Ref:     SourceRef{Kind: KindLibraryFile, Ref: "f-gone", Label: "Gone"},
		Problem: "no file with that id is readable by you",
	}
	query := Resolved{Ref: SourceRef{Kind: KindQuery, Ref: "sourceRows()", Label: "Invoices"}, Rows: []map[string]any{{"id": "invoice-1"}}, Count: 1}
	conceptRow := Resolved{Ref: SourceRef{Kind: KindConceptRow, Ref: "v1:billing:invoice#invoice-1", Label: "INV-1"}, Rows: []map[string]any{{"id": "invoice-1"}}, Count: 1}
	twoRows := pickedFile("notes.md", 100)
	twoRows.Rows = append(twoRows.Rows, map[string]any{"id": "another", "name": "notes.md", "size": float64(100)})
	// A file's size is an int, so these do not occur; they are read as the SDK reads them.
	fraction := pickedFile("notes.md", 0)
	fraction.Rows[0]["size"] = 0.25
	pastTheLimit := pickedFile("notes.md", 0)
	pastTheLimit.Rows[0]["size"] = 32768.5

	for name, tc := range map[string]struct {
		picked []Resolved
		want   bool
	}{
		"nothing picked":                         {nil, true},
		"one Markdown file":                      {[]Resolved{pickedFile("notes.md", 2048)}, true},
		"a Markdown file and a picture":          {[]Resolved{pickedFile("notes.md", 2048), pickedFile("logo.png", 40<<10)}, true},
		"a file that is not archived":            {[]Resolved{notArchived}, true},
		"a Markdown file whose row says no size": {[]Resolved{unsized}, true},
		"a fraction of a byte":                   {[]Resolved{fraction}, true},
		"a fraction of a byte past the limit":    {[]Resolved{pastTheLimit}, false},
		"a spreadsheet":                          {[]Resolved{pickedFile("numbers.csv", 4096)}, false},
		"a Markdown file and a spreadsheet":      {[]Resolved{pickedFile("notes.md", 2048), pickedFile("numbers.csv", 4096)}, false},
		"a spreadsheet and a Markdown file":      {[]Resolved{pickedFile("numbers.csv", 4096), pickedFile("notes.md", 2048)}, false},
		"a Markdown file of 40 KiB":              {[]Resolved{pickedFile("notes.md", 40<<10)}, false},
		"a picture of 9 MiB":                     {[]Resolved{pickedFile("logo.png", 9<<20)}, false},
		"an archived Markdown file":              {[]Resolved{archived}, false},
		"a file whose row could not be read":     {[]Resolved{unreadable}, false},
		"a Markdown file and an unreadable one":  {[]Resolved{pickedFile("notes.md", 2048), unreadable}, false},
		"a source that found two rows":           {[]Resolved{twoRows}, false},
		"a query":                                {[]Resolved{query}, false},
		"a concept row":                          {[]Resolved{conceptRow}, false},
		"a Markdown file and a query":            {[]Resolved{pickedFile("notes.md", 2048), query}, false},
	} {
		if got := describedArgs().describedMarkdown(tc.picked); got != tc.want {
			t.Errorf("%s: describedMarkdown = %v, want %v", name, got, tc.want)
		}
	}

	// The files make no difference to a request that was never a description.
	for name, with := range map[string]func(*materializeArgs){
		"no statement": func(a *materializeArgs) { a.Statement = "" },
		"a template":   func(a *materializeArgs) { a.TemplateId = "template-1" },
		"not Markdown": func(a *materializeArgs) { a.Format = pure.FormatCSV },
	} {
		a := describedArgs()
		with(&a)
		if a.describedMarkdown([]Resolved{pickedFile("notes.md", 2048)}) {
			t.Errorf("a Markdown file made a request with %s a described one", name)
		}
	}
}
