package emaildoc

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// luminanceOf is the WCAG 2.x relative luminance of a "#rrggbb" colour.
func luminanceOf(hex string) float64 {
	channel := func(from int) float64 {
		v, _ := strconv.ParseUint(hex[from:from+2], 16, 8)
		c := float64(v) / 255
		if c <= 0.03928 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*channel(1) + 0.7152*channel(3) + 0.0722*channel(5)
}

// contrastOf is the WCAG 2.x contrast ratio of two "#rrggbb" colours: the
// luminance of the lighter plus 0.05, over that of the darker plus 0.05.
func contrastOf(a, b string) float64 {
	la, lb := luminanceOf(a), luminanceOf(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func TestContrastRatio(t *testing.T) {
	for _, c := range []struct {
		fg, bg string
		want   float64
	}{
		{"#000000", "#ffffff", 21},
		{"#ffffff", "#000000", 21},
		{"#ffffff", "#ffffff", 1},
		{"#767676", "#ffffff", 4.54}, // the lightest grey that passes AA on white
		{"#777777", "#ffffff", 4.48}, // and the next one, which does not
		{"#0000ff", "#ffffff", 8.59},
	} {
		if got := contrastOf(c.fg, c.bg); math.Abs(got-c.want) > 0.005 {
			t.Errorf("%s on %s: want %.2f, got %.4f", c.fg, c.bg, c.want, got)
		}
	}
}

// textRun is some words of an email as a reader sees them: their colour, the
// colour behind them, their size, whether they are bold, and the block they
// belong to (a preview render marks each block's outer cell).
type textRun struct {
	block  string
	words  string
	fg, bg string
	size   float64
	bold   bool
}

// large says whether WCAG treats the words as large text: 24 pixels, or 18.66
// pixels (14 points) and bold.
func (r textRun) large() bool { return r.size >= 24 || (r.bold && r.size >= 18.66) }

// required is the contrast the words need: 4.5 to 1, or 3 to 1 for large text.
func (r textRun) required() float64 {
	if r.large() {
		return 3
	}
	return 4.5
}

// hexColour reads "#rgb" or "#rrggbb" as "#rrggbb"; anything else (a keyword,
// a function) is not a colour the renderer writes for text.
func hexColour(v string) (string, bool) {
	v = strings.ToLower(strings.TrimSpace(v))
	if len(v) == 4 && v[0] == '#' {
		return "#" + strings.Repeat(v[1:2], 2) + strings.Repeat(v[2:3], 2) + strings.Repeat(v[3:4], 2), true
	}
	if len(v) == 7 && v[0] == '#' {
		return v, true
	}
	return "", false
}

func attrOf(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// styleOf reads an element's style attribute into property -> value.
func styleOf(n *html.Node) map[string]string {
	decl := map[string]string{}
	for _, part := range strings.Split(attrOf(n, "style"), ";") {
		if name, value, ok := strings.Cut(part, ":"); ok {
			decl[strings.ToLower(strings.TrimSpace(name))] = strings.ToLower(strings.TrimSpace(value))
		}
	}
	return decl
}

// textRuns reads the words of an email the way a reader meets them. Colour,
// size and weight are inherited, as in a browser; the colour behind words is
// the nearest background of an element around them (a page, the canvas, a
// container, the cell of a button, the span of a highlight). What is not shown
// (the head, comments, the hidden preheader) is left out, and so are white
// space and the empty cells that hold only a no-break space.
func textRuns(t *testing.T, markup string) []textRun {
	t.Helper()
	page, err := html.Parse(strings.NewReader(markup))
	if err != nil {
		t.Fatal(err)
	}
	type state struct {
		fg, bg string
		size   float64
		bold   bool
		block  string
	}
	var runs []textRun
	var walk func(n *html.Node, st state)
	walk = func(n *html.Node, st state) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			switch c.Type {
			case html.TextNode:
				if words := strings.TrimSpace(c.Data); words != "" && st.size >= 1 {
					runs = append(runs, textRun{block: st.block, words: words, fg: st.fg, bg: st.bg, size: st.size, bold: st.bold})
				}
			case html.ElementNode:
				switch c.Data {
				case "head", "style", "script", "title", "noscript":
					continue
				}
				decl := styleOf(c)
				if strings.Contains(decl["display"], "none") {
					continue
				}
				next := st
				if v, ok := hexColour(decl["color"]); ok {
					next.fg = v
				}
				if v, ok := hexColour(decl["background-color"]); ok {
					next.bg = v
				} else if v, ok := hexColour(attrOf(c, "bgcolor")); ok {
					next.bg = v
				}
				if v, err := strconv.ParseFloat(strings.TrimSuffix(decl["font-size"], "px"), 64); err == nil && decl["font-size"] != "" {
					next.size = v
				}
				switch decl["font-weight"] {
				case "700", "bold":
					next.bold = true
				case "400", "normal":
					next.bold = false
				}
				if id := attrOf(c, "data-block"); id != "" {
					next.block = id
				}
				walk(c, next)
			}
		}
	}
	walk(page, state{fg: "#000000", bg: "#ffffff", size: 16})
	return runs
}

func TestTextRunsReadWhatAReaderSees(t *testing.T) {
	runs := textRuns(t, `<!DOCTYPE html><html><head><title>t</title><style>td{color:#ff0000}</style></head>`+
		`<body style="background-color:#eef0f3"><div style="display:none;color:#eef0f3">hidden</div>`+
		`<table style="background-color:#223344"><tbody><tr>`+
		`<td data-block="big" style="color:#111;font-size:30px;font-weight:700">Big <span style="font-weight:normal">plain</span></td>`+
		`<td data-block="small" bgcolor="#000000"><span style="color:#FFF;font-size:12.5px">Small</span></td>`+
		`<td style="font-size:0">&nbsp;</td><td style="font-size:0">gone</td>`+
		`</tr></tbody></table><!-- <p style="color:#ff0000">in a comment</p> --></body></html>`)
	var got []string
	for _, r := range runs {
		got = append(got, fmt.Sprintf("%s|%s|%s|%s|%g|%v", r.block, r.words, r.fg, r.bg, r.size, r.bold))
	}
	want := []string{
		"big|Big|#111111|#223344|30|true",
		"big|plain|#111111|#223344|30|false",
		"small|Small|#ffffff|#000000|12.5|false",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("want\n%s\ngot\n%s", strings.Join(want, "\n"), strings.Join(got, "\n"))
	}
	if !runs[0].large() || !runs[1].large() || runs[2].large() {
		t.Errorf("30px is large text, bold or not, and 12.5px is not: %+v", runs)
	}
}

// TestLargeTextNeedsLessContrast pins WCAG's large text: 24 pixels at any
// weight, or 18.66 pixels (14 points) and bold, which need 3 to 1; anything
// smaller needs 4.5 to 1.
func TestLargeTextNeedsLessContrast(t *testing.T) {
	for _, c := range []struct {
		size  float64
		bold  bool
		large bool
	}{
		{23.9, false, false}, {24, false, true}, {40, false, true},
		{18.65, true, false}, {18.66, true, true}, {18.66, false, false}, {18.7, false, false},
		{17.3, true, false}, {14.7, true, false}, {14.7, false, false},
	} {
		run := textRun{size: c.size, bold: c.bold}
		want := 4.5
		if c.large {
			want = 3
		}
		if run.large() != c.large || run.required() != want {
			t.Errorf("%gpx bold=%v: want large=%v needing %g to 1, got large=%v needing %g", c.size, c.bold, c.large, want, run.large(), run.required())
		}
	}
}

// roleNames says which colours of the theme a colour is, for a message a
// person can act on: "secondary" and not "#5b8fb9".
func roleNames(theme Theme, hex string) string {
	var names []string
	for _, role := range []struct{ name, value string }{
		{"page", theme.Page}, {"canvas", theme.Canvas}, {"primary", theme.Primary}, {"secondary", theme.Secondary},
		{"accent", theme.Accent}, {"tint", theme.Tint}, {"text", theme.Text}, {"muted", theme.Muted}, {"white", "#ffffff"},
	} {
		if strings.EqualFold(role.value, hex) {
			names = append(names, role.name)
		}
	}
	if len(names) == 0 {
		return hex
	}
	return strings.Join(names, "/") + " " + hex
}

// unreadBlocks lists, in document order, the blocks that have words (a
// heading, a text, a highlight, a button, a social block or a footer) of which
// no words are among the runs.
func unreadBlocks(blocks []Block, runs []textRun) []string {
	read := map[string]bool{}
	for _, run := range runs {
		read[run.block] = true
	}
	var silent []string
	walkBlocks(blocks, func(b Block) bool {
		switch b.Type {
		case typeHeading, typeText, typeHighlight, typeButton, typeSocial, typeFooter:
			if !read[b.ID] {
				silent = append(silent, b.ID)
			}
		}
		return true
	})
	return silent
}

func TestUnreadBlocksAreNoticed(t *testing.T) {
	blocks := []Block{
		{ID: "h", Type: typeHeading}, {ID: "t", Type: typeText}, {ID: "l", Type: typeHighlight},
		{ID: "b", Type: typeButton}, {ID: "s", Type: typeSocial}, {ID: "f", Type: typeFooter},
		{ID: "i", Type: typeImage}, {ID: "d", Type: typeDivider}, {ID: "p", Type: typeSpacer},
		{ID: "sec", Type: typeSection, Blocks: []Block{{ID: "inner", Type: typeText}}},
	}
	if got := strings.Join(unreadBlocks(blocks, []textRun{{block: "h"}}), " "); got != "t l b s f inner" {
		t.Errorf("want every block with words but the heading, got %q", got)
	}
	runs := []textRun{{block: "h"}, {block: "t"}, {block: "l"}, {block: "b"}, {block: "s"}, {block: "f"}, {block: "inner"}}
	if got := unreadBlocks(blocks, runs); len(got) != 0 {
		t.Errorf("every block with words was read, got %v", got)
	}
}

// TestBuiltinLayoutsMeetTextContrast checks every pair of words and the colour
// behind them that each built-in layout draws, as the renderer writes them
// (so the theme's roles as the blocks use them, the defaults of each block
// type included): 4.5 to 1 for text, 3 to 1 for large text (24 pixels, or
// 18.66 pixels and bold), the WCAG 2 level AA. These are the layouts people
// start from, and what they leave alone is what gets sent.
func TestBuiltinLayoutsMeetTextContrast(t *testing.T) {
	for _, info := range Layouts() {
		t.Run(info.ID, func(t *testing.T) {
			r, err := Render(context.Background(), info.Document, nil, RenderOptions{Preview: true})
			if err != nil {
				t.Fatal(err)
			}
			runs := textRuns(t, r.HTML)
			reported := map[string]bool{}
			for _, run := range runs {
				got := contrastOf(run.fg, run.bg)
				if got >= run.required() {
					continue
				}
				// One report for each block and colours, however many words.
				key := fmt.Sprintf("%s|%s|%s|%g|%v", run.block, run.fg, run.bg, run.size, run.bold)
				if reported[key] {
					continue
				}
				reported[key] = true
				kind := "text"
				if run.large() {
					kind = "large text"
				}
				weight := ""
				if run.bold {
					weight = ", bold"
				}
				t.Errorf("block %q: %s (%gpx%s) is %s on %s: %.2f to 1, %s needs %.1f to 1",
					run.block, strconv.Quote(clip(run.words, 30)), run.size, weight,
					roleNames(info.Document.Theme, run.fg), roleNames(info.Document.Theme, run.bg), got, kind, run.required())
			}
			// A check that read no words would pass for nothing: every block
			// that has words must have been read.
			if silent := unreadBlocks(info.Document.Blocks, runs); len(silent) > 0 {
				t.Errorf("no words of these blocks were read, so none were checked: %v", silent)
			}
		})
	}
}
